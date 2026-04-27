// Package wallet 钱包资金服务（平台币）。
//
// 资金正确性的三层保障：
//  1. 幂等键：每笔变更必须携带 (biz_type, biz_id)，先查后写，命中直接返回原结果；
//  2. 唯一索引：并发的同单号请求在插入流水时由数据库唯一约束兜底，失败方整体回滚；
//  3. 乐观锁 + 条件更新：UPDATE ... WHERE version = ? AND available + delta >= 0，
//     既防止并发覆盖（丢失更新），又在 SQL 层面杜绝透支。
//
// 外加定时对账（reconcile.go）核对 SUM(流水) 与账户总额，兜底发现任何遗漏。
package wallet

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2" // nosemgrep: math-random-used -- 仅用于乐观锁重试退避抖动
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"gamepub/pkg/dbx"
	"gamepub/pkg/sqlsafe"
)

// 业务类型。
const (
	BizRecharge    = "recharge"     // 充值到账
	BizTaskReward  = "task_reward"  // 任务奖励
	BizAdminAdjust = "admin_adjust" // 运营调账（需 wallet:adjust 权限）
	BizPurchase    = "purchase"     // 消费
	BizTCCConfirm  = "tcc_confirm"  // TCC 确认扣减
)

var (
	creditBizTypes = map[string]bool{BizRecharge: true, BizTaskReward: true, BizAdminAdjust: true}
	debitBizTypes  = map[string]bool{BizPurchase: true, BizAdminAdjust: true}
	ledgerBizTypes = map[string]bool{BizRecharge: true, BizTaskReward: true, BizAdminAdjust: true, BizPurchase: true, BizTCCConfirm: true}
	ledgerSort     = sqlsafe.SortFields{"created_at": "created_at", "amount": "amount", "id": "id"}
)

// MaxAmount 单笔变更上限：防止 int64 溢出与“多打几个零”的误操作。
const MaxAmount int64 = 10_000_000

const maxRetries = 5

var (
	ErrInvalidArgument     = errors.New("wallet: invalid argument")
	ErrInvalidAmount       = errors.New("wallet: invalid amount")
	ErrInvalidBiz          = errors.New("wallet: invalid biz_type or biz_id")
	ErrInsufficientBalance = errors.New("wallet: insufficient balance")
	ErrIdempotencyConflict = errors.New("wallet: biz_id already used with different parameters")
	ErrConcurrentUpdate    = errors.New("wallet: too many concurrent updates, retry later")
	ErrBranchNotFound      = errors.New("wallet: tcc branch not found")
	ErrBranchRejected      = errors.New("wallet: tcc branch state does not allow this operation")
)

// 内部控制流错误，不会返回给调用方。
var (
	errRetry     = errors.New("wallet: retry")
	errNoAccount = errors.New("wallet: account not created")
)

// Service 钱包领域服务。
type Service struct {
	db     *gorm.DB
	logger *slog.Logger
	now    func() time.Time

	// afterSnapshot 测试注入点：在读取账户快照之后、条件更新之前执行，用于模拟并发修改。
	afterSnapshot func(tx *gorm.DB)
}

// NewService 创建服务。
func NewService(db *gorm.DB, logger *slog.Logger) *Service {
	return &Service{db: db, logger: logger, now: time.Now}
}

// ChangeRequest 余额变更请求。
type ChangeRequest struct {
	UserID  uint64
	Amount  int64
	BizType string
	BizID   string
	Remark  string
}

// ChangeResult 余额变更结果。
type ChangeResult struct {
	LedgerID     uint64
	BalanceAfter int64
	Duplicated   bool
}

// Balance 余额。
type Balance struct {
	Available int64
	Frozen    int64
}

func validateChange(req ChangeRequest, allowed map[string]bool) error {
	if req.UserID == 0 {
		return ErrInvalidArgument
	}
	if req.Amount <= 0 || req.Amount > MaxAmount {
		return ErrInvalidAmount
	}
	if !allowed[req.BizType] || req.BizID == "" || len(req.BizID) > 64 || len(req.Remark) > 255 {
		return ErrInvalidBiz
	}
	return nil
}

// Credit 入账。
func (s *Service) Credit(ctx context.Context, req ChangeRequest) (*ChangeResult, error) {
	if err := validateChange(req, creditBizTypes); err != nil {
		return nil, err
	}
	return s.change(ctx, req, req.Amount)
}

// Debit 扣减。
func (s *Service) Debit(ctx context.Context, req ChangeRequest) (*ChangeResult, error) {
	if err := validateChange(req, debitBizTypes); err != nil {
		return nil, err
	}
	return s.change(ctx, req, -req.Amount)
}

func (s *Service) change(ctx context.Context, req ChangeRequest, delta int64) (*ChangeResult, error) {
	var res *ChangeResult
	err := s.retry(ctx, func() error {
		r, err := s.changeOnce(ctx, req, delta)
		if errors.Is(err, errNoAccount) {
			// 在事务外建账户再重试：MySQL RR 隔离级别下，事务内“查不到→插入冲突→再查”仍会读到旧快照
			if err := s.ensureAccount(ctx, req.UserID); err != nil {
				return err
			}
			return errRetry
		}
		res = r
		return err
	})
	return res, err
}

func (s *Service) changeOnce(ctx context.Context, req ChangeRequest, delta int64) (*ChangeResult, error) {
	var res *ChangeResult
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1. 幂等：同一业务单据已入账则直接返回原结果；参数不一致说明调用方复用了单号，拒绝
		var dup Ledger
		err := tx.Where("biz_type = ? AND biz_id = ?", req.BizType, req.BizID).Take(&dup).Error
		if err == nil {
			if dup.UserID != req.UserID || dup.Amount != delta {
				return ErrIdempotencyConflict
			}
			res = &ChangeResult{LedgerID: dup.ID, BalanceAfter: dup.BalanceAfter, Duplicated: true}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		// 2. 读取账户快照
		var acc Account
		if err := tx.Where("user_id = ?", req.UserID).Take(&acc).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				if delta < 0 {
					return ErrInsufficientBalance
				}
				return errNoAccount
			}
			return err
		}
		if acc.Available+delta < 0 {
			return ErrInsufficientBalance
		}
		if s.afterSnapshot != nil {
			s.afterSnapshot(tx)
		}

		// 3. 乐观锁条件更新：快照过期（version 变化）则整个事务重来；余额条件在 SQL 层二次兜底防透支
		r := tx.Model(&Account{}).
			Where("id = ? AND version = ? AND available + ? >= 0", acc.ID, acc.Version, delta).
			Updates(map[string]any{
				"available":  gorm.Expr("available + ?", delta),
				"version":    gorm.Expr("version + 1"),
				"updated_at": s.now(),
			})
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected == 0 {
			return errRetry
		}

		// 4. 写流水：并发的同单号请求在这里被唯一索引拦下，整个事务回滚（含第 3 步的扣减）
		l := Ledger{
			UserID: req.UserID, BizType: req.BizType, BizID: req.BizID, Amount: delta,
			BalanceAfter: acc.Available + acc.Frozen + delta, Remark: req.Remark,
		}
		if err := tx.Create(&l).Error; err != nil {
			if dbx.IsDuplicate(err) {
				return errRetry // 重试时会命中第 1 步的幂等分支
			}
			return err
		}
		res = &ChangeResult{LedgerID: l.ID, BalanceAfter: l.BalanceAfter}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Service) ensureAccount(ctx context.Context, userID uint64) error {
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&Account{UserID: userID}).Error
}

// retry 处理乐观锁冲突：有限次数 + 随机退避，超过次数返回 ErrConcurrentUpdate 让调用方稍后重试。
func (s *Service) retry(ctx context.Context, fn func() error) error {
	for i := 0; i < maxRetries; i++ {
		err := fn()
		if !errors.Is(err, errRetry) {
			return err
		}
		d := time.Duration(rand.Int64N(int64(5*time.Millisecond) << i)) // #nosec G404 -- 退避抖动，不涉及安全
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	s.logger.WarnContext(ctx, "wallet: optimistic lock retries exhausted")
	return ErrConcurrentUpdate
}

// GetBalance 查询余额，账户不存在视为 0。
func (s *Service) GetBalance(ctx context.Context, userID uint64) (Balance, error) {
	var acc Account
	err := s.db.WithContext(ctx).Where("user_id = ?", userID).Take(&acc).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Balance{}, nil
	}
	if err != nil {
		return Balance{}, err
	}
	return Balance{Available: acc.Available, Frozen: acc.Frozen}, nil
}

// LedgerQuery 流水查询条件。
type LedgerQuery struct {
	UserID   uint64
	BizType  string
	SortBy   string
	Desc     bool
	Page     int32
	PageSize int32
}

// ListLedger 查询本人流水。user_id 由网关从令牌注入，排序字段走白名单。
func (s *Service) ListLedger(ctx context.Context, q LedgerQuery) ([]Ledger, int64, error) {
	if q.UserID == 0 {
		return nil, 0, ErrInvalidArgument
	}
	db := s.db.WithContext(ctx).Model(&Ledger{}).Where("user_id = ?", q.UserID)
	if q.BizType != "" {
		if !ledgerBizTypes[q.BizType] {
			return nil, 0, ErrInvalidArgument
		}
		db = db.Where("biz_type = ?", q.BizType)
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	offset, limit := sqlsafe.Page(q.Page, q.PageSize, 20)
	var rows []Ledger
	err := db.Order(ledgerSort.OrderBy(q.SortBy, q.Desc, "id")).Offset(offset).Limit(limit).Find(&rows).Error
	return rows, total, err
}

// ListReconcileDiffs 最近的对账差异。
func (s *Service) ListReconcileDiffs(ctx context.Context, limit int) ([]ReconcileDiff, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows []ReconcileDiff
	err := s.db.WithContext(ctx).Order("id DESC").Limit(limit).Find(&rows).Error
	return rows, err
}
