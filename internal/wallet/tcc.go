package wallet

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"gamepub/pkg/dbx"
)

// TCC 三阶段的异常场景（审计必查）：
//
//   - 幂等：协调者超时重试，Try/Confirm/Cancel 都可能被重复调用；
//   - 空回滚：Try 因网络原因未到达或未执行，协调者却发起了 Cancel，此时没有可回滚的冻结；
//   - 悬挂：Cancel（空回滚）先执行完，迟到的 Try 才到达。若 Try 仍冻结资金，
//     这笔冻结再也不会有 Confirm/Cancel 来处理，资金被永久冻住。
//
// 解法：分支表以 xid 唯一。空回滚时插入 cancelled 状态的分支记录；
// 迟到的 Try 插入时撞唯一键，读到 cancelled 即拒绝。

func validXID(xid string) bool { return xid != "" && len(xid) <= 64 }

// TryFreeze 冻结资金：available → frozen。
func (s *Service) TryFreeze(ctx context.Context, xid string, userID uint64, amount int64) (string, error) {
	if !validXID(xid) || userID == 0 {
		return "", ErrInvalidArgument
	}
	if amount <= 0 || amount > MaxAmount {
		return "", ErrInvalidAmount
	}
	var status string
	err := s.retry(ctx, func() error {
		return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			b := TCCBranch{XID: xid, UserID: userID, Amount: amount, Status: BranchTried}
			if err := tx.Create(&b).Error; err != nil {
				if !dbx.IsDuplicate(err) {
					return err
				}
				existing, err := s.branch(tx, xid)
				if err != nil {
					return err
				}
				if existing.Status == BranchCancelled {
					s.logger.WarnContext(ctx, "tcc: reject suspended try after cancel", "xid", xid)
					return ErrBranchRejected
				}
				if existing.UserID != userID || existing.Amount != amount {
					return ErrIdempotencyConflict
				}
				status = existing.Status
				return nil
			}
			r := tx.Model(&Account{}).
				Where("user_id = ? AND available >= ?", userID, amount).
				Updates(map[string]any{
					"available":  gorm.Expr("available - ?", amount),
					"frozen":     gorm.Expr("frozen + ?", amount),
					"version":    gorm.Expr("version + 1"), // 与 Credit/Debit 的乐观锁共用版本号
					"updated_at": s.now(),
				})
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected == 0 {
				return ErrInsufficientBalance // 回滚分支记录，之后的 Cancel 走空回滚
			}
			status = BranchTried
			return nil
		})
	})
	return status, err
}

// ConfirmFreeze 确认扣减冻结资金，并记一笔流水。
func (s *Service) ConfirmFreeze(ctx context.Context, xid string) (string, error) {
	if !validXID(xid) {
		return "", ErrInvalidArgument
	}
	var status string
	err := s.retry(ctx, func() error {
		return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			b, err := s.branch(tx, xid)
			if err != nil {
				return err
			}
			switch b.Status {
			case BranchConfirmed:
				status = BranchConfirmed
				return nil
			case BranchCancelled:
				return ErrBranchRejected
			}
			if err := s.transit(tx, b.ID, BranchTried, BranchConfirmed); err != nil {
				return err
			}
			r := tx.Model(&Account{}).
				Where("user_id = ? AND frozen >= ?", b.UserID, b.Amount).
				Updates(map[string]any{
					"frozen":     gorm.Expr("frozen - ?", b.Amount),
					"version":    gorm.Expr("version + 1"),
					"updated_at": s.now(),
				})
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected == 0 {
				return fmt.Errorf("wallet: frozen balance inconsistent for xid %s", xid)
			}
			var acc Account
			if err := tx.Where("user_id = ?", b.UserID).Take(&acc).Error; err != nil {
				return err
			}
			l := Ledger{
				UserID: b.UserID, BizType: BizTCCConfirm, BizID: xid, Amount: -b.Amount,
				BalanceAfter: acc.Available + acc.Frozen, Remark: "tcc confirm",
			}
			if err := tx.Create(&l).Error; err != nil {
				if dbx.IsDuplicate(err) {
					return errRetry
				}
				return err
			}
			status = BranchConfirmed
			return nil
		})
	})
	return status, err
}

// CancelFreeze 释放冻结资金；Try 未执行时做空回滚。
func (s *Service) CancelFreeze(ctx context.Context, xid string) (string, error) {
	if !validXID(xid) {
		return "", ErrInvalidArgument
	}
	var status string
	err := s.retry(ctx, func() error {
		return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			b, err := s.branch(tx, xid)
			if errors.Is(err, ErrBranchNotFound) {
				// 空回滚：记录 cancelled 分支，本次 Cancel 返回成功，迟到的 Try 将被拒绝
				if err := tx.Create(&TCCBranch{XID: xid, Status: BranchCancelled}).Error; err != nil {
					if dbx.IsDuplicate(err) {
						return errRetry // Try 恰好并发插入，重新按其状态处理
					}
					return err
				}
				s.logger.InfoContext(ctx, "tcc: empty rollback recorded", "xid", xid)
				status = BranchCancelled
				return nil
			}
			if err != nil {
				return err
			}
			switch b.Status {
			case BranchCancelled:
				status = BranchCancelled
				return nil
			case BranchConfirmed:
				return ErrBranchRejected
			}
			if err := s.transit(tx, b.ID, BranchTried, BranchCancelled); err != nil {
				return err
			}
			r := tx.Model(&Account{}).
				Where("user_id = ? AND frozen >= ?", b.UserID, b.Amount).
				Updates(map[string]any{
					"available":  gorm.Expr("available + ?", b.Amount),
					"frozen":     gorm.Expr("frozen - ?", b.Amount),
					"version":    gorm.Expr("version + 1"),
					"updated_at": s.now(),
				})
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected == 0 {
				return fmt.Errorf("wallet: frozen balance inconsistent for xid %s", xid)
			}
			status = BranchCancelled
			return nil
		})
	})
	return status, err
}

func (s *Service) branch(tx *gorm.DB, xid string) (*TCCBranch, error) {
	var b TCCBranch
	err := tx.Where("xid = ?", xid).Take(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrBranchNotFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// transit 状态机 CAS：只有处于 from 状态才能迁移，并发的 Confirm 与 Cancel 只有一个能成功。
func (s *Service) transit(tx *gorm.DB, id uint64, from, to string) error {
	r := tx.Model(&TCCBranch{}).Where("id = ? AND status = ?", id, from).
		Updates(map[string]any{"status": to, "updated_at": s.now()})
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected == 0 {
		return errRetry
	}
	return nil
}
