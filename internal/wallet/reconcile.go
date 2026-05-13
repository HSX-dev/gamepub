package wallet

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"gamepub/pkg/redislock"
)

// Reconciler 定时对账：逐批核对 SUM(流水.amount) 与 账户总额（可用 + 冻结）。
// 多副本部署时用分布式锁保证同一时刻只有一个副本执行，并在批次间续约；锁丢失立即停止。
type Reconciler struct {
	db       *gorm.DB
	locker   *redislock.Locker
	interval time.Duration
	lockTTL  time.Duration
	batch    int
	logger   *slog.Logger
	now      func() time.Time
}

// NewReconciler 创建对账任务。
func NewReconciler(db *gorm.DB, locker *redislock.Locker, interval time.Duration, logger *slog.Logger) *Reconciler {
	return &Reconciler{db: db, locker: locker, interval: interval, lockTTL: time.Minute, batch: 500, logger: logger, now: time.Now}
}

// Run 周期执行直到 ctx 取消。
func (r *Reconciler) Run(ctx context.Context) error {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			r.tick(ctx)
		}
	}
}

func (r *Reconciler) tick(ctx context.Context) {
	lock, err := r.locker.TryLock(ctx, "wallet:reconcile", r.lockTTL)
	if errors.Is(err, redislock.ErrNotAcquired) {
		return
	}
	if err != nil {
		r.logger.ErrorContext(ctx, "reconcile: acquire lock failed", "err", err)
		return
	}
	defer func() {
		// 用 WithoutCancel：进程退出时 ctx 已取消，但仍应尽力释放锁
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if err := lock.Unlock(uctx); err != nil && !errors.Is(err, redislock.ErrLockLost) {
			r.logger.WarnContext(ctx, "reconcile: release lock failed", "err", err)
		}
	}()
	start := r.now()
	diffs, err := r.RunOnce(ctx, func(ctx context.Context) error { return lock.Refresh(ctx, r.lockTTL) })
	if err != nil {
		r.logger.ErrorContext(ctx, "reconcile: run failed", "err", err)
		return
	}
	r.logger.InfoContext(ctx, "reconcile: finished", "diffs", diffs, "duration_ms", r.now().Sub(start).Milliseconds())
}

// RunOnce 执行一轮对账，返回差异数。keepalive 在每批之后调用，返回错误时中止。
func (r *Reconciler) RunOnce(ctx context.Context, keepalive func(context.Context) error) (int, error) {
	var (
		lastID uint64
		diffs  int
	)
	for {
		var accs []Account
		if err := r.db.WithContext(ctx).Where("id > ?", lastID).Order("id").Limit(r.batch).Find(&accs).Error; err != nil {
			return diffs, err
		}
		if len(accs) == 0 {
			return diffs, nil
		}
		ids := make([]uint64, len(accs))
		for i, a := range accs {
			ids[i] = a.UserID
		}
		type sumRow struct {
			UserID uint64
			Total  int64
		}
		var sums []sumRow
		if err := r.db.WithContext(ctx).Model(&Ledger{}).
			Select("user_id, COALESCE(SUM(amount), 0) AS total").
			Where("user_id IN ?", ids).Group("user_id").Scan(&sums).Error; err != nil {
			return diffs, err
		}
		sumOf := make(map[uint64]int64, len(sums))
		for _, s := range sums {
			sumOf[s.UserID] = s.Total
		}
		for _, a := range accs {
			if a.Available+a.Frozen == sumOf[a.UserID] {
				continue
			}
			// 批量读取期间账户可能正常变动，单用户在一个事务内复核（MySQL RR 下为一致性快照）
			diff, err := r.recheck(ctx, a.UserID)
			if err != nil {
				return diffs, err
			}
			if diff != nil {
				diffs++
				r.logger.ErrorContext(ctx, "reconcile: balance mismatch",
					"user_id", diff.UserID, "wallet_balance", diff.WalletBalance, "ledger_sum", diff.LedgerSum)
			}
		}
		lastID = accs[len(accs)-1].ID
		if keepalive != nil {
			if err := keepalive(ctx); err != nil {
				return diffs, err
			}
		}
	}
}

func (r *Reconciler) recheck(ctx context.Context, userID uint64) (*ReconcileDiff, error) {
	var diff *ReconcileDiff
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var acc Account
		if err := tx.Where("user_id = ?", userID).Take(&acc).Error; err != nil {
			return err
		}
		var sum int64
		if err := tx.Model(&Ledger{}).Select("COALESCE(SUM(amount), 0)").Where("user_id = ?", userID).Scan(&sum).Error; err != nil {
			return err
		}
		if acc.Available+acc.Frozen == sum {
			return nil
		}
		diff = &ReconcileDiff{UserID: userID, WalletBalance: acc.Available + acc.Frozen, LedgerSum: sum, CheckedAt: r.now()}
		return tx.Create(diff).Error
	})
	return diff, err
}
