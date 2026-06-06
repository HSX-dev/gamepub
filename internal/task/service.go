// Package task 任务活动：Kafka 事件驱动进度累计，奖励经钱包服务幂等发放。
package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	walletv1 "gamepub/api/gen/wallet/v1"
	"gamepub/internal/events"
	"gamepub/pkg/mq"
)

var (
	ErrTaskNotFound   = errors.New("task: task not found")
	ErrNotCompleted   = errors.New("task: task not completed")
	ErrAlreadyClaimed = errors.New("task: reward already claimed")
	ErrRewardPending  = errors.New("task: reward is being issued, please retry later")
)

// 与钱包服务约定的业务类型。
const walletBizTaskReward = "task_reward"

// consumerName 消费记录表中的消费者标识。
const consumerName = "task-service"

// WalletClient 只依赖需要的方法，便于测试替换，也让依赖面一目了然。
type WalletClient interface {
	Credit(ctx context.Context, in *walletv1.CreditRequest, opts ...grpc.CallOption) (*walletv1.CreditResponse, error)
}

// Service 任务领域服务。
type Service struct {
	db     *gorm.DB
	wallet WalletClient
	defs   map[string]Definition
	logger *slog.Logger
	now    func() time.Time
}

// NewService 创建服务。
func NewService(db *gorm.DB, wallet WalletClient, logger *slog.Logger) *Service {
	defs := make(map[string]Definition, len(definitions))
	for _, d := range definitions {
		defs[d.ID] = d
	}
	return &Service{db: db, wallet: wallet, defs: defs, logger: logger, now: time.Now}
}

// Progress 用户视角的任务进度。
type Progress struct {
	Definition
	PeriodKey string
	Progress  int64
	Status    string
}

// ListTasks 当前周期的任务进度。
func (s *Service) ListTasks(ctx context.Context, userID uint64) ([]Progress, error) {
	now := s.now()
	keys := make([]string, 0, len(definitions))
	for _, d := range definitions {
		keys = append(keys, d.PeriodKey(now))
	}
	var rows []UserTask
	if err := s.db.WithContext(ctx).Where("user_id = ? AND period IN ?", userID, keys).Find(&rows).Error; err != nil {
		return nil, err
	}
	byKey := make(map[string]UserTask, len(rows))
	for _, r := range rows {
		byKey[r.TaskID+"|"+r.Period] = r
	}
	out := make([]Progress, 0, len(definitions))
	for _, d := range definitions {
		p := Progress{Definition: d, PeriodKey: d.PeriodKey(now), Status: StatusInProgress}
		if r, ok := byKey[d.ID+"|"+p.PeriodKey]; ok {
			p.Progress, p.Status = r.Progress, r.Status
		}
		out = append(out, p)
	}
	return out, nil
}

func rewardBizID(taskID, period string, userID uint64) string {
	return fmt.Sprintf("task:%s:%s:%d", taskID, period, userID)
}

// ClaimReward 领取奖励。
//
// 高峰期常见问题是“连点/并发请求重复发奖”。这里不依赖分布式锁，而是：
//  1. 状态机 CAS：只有 completed/claiming 的任务能进入发放；
//  2. 钱包以 task:{id}:{period}:{uid} 为幂等键入账，并发或重试最多入账一次；
//  3. 入账成功后 CAS claiming → claimed。若进程在 2、3 之间崩溃，补偿任务会以同一幂等键补齐状态。
func (s *Service) ClaimReward(ctx context.Context, userID uint64, taskID string) (int64, int64, error) {
	def, ok := s.defs[taskID]
	if !ok || userID == 0 {
		return 0, 0, ErrTaskNotFound
	}
	period := def.PeriodKey(s.now())
	db := s.db.WithContext(ctx)
	r := db.Model(&UserTask{}).
		Where("user_id = ? AND task_id = ? AND period = ? AND status IN ?", userID, taskID, period, []string{StatusCompleted, StatusClaiming}).
		Updates(map[string]any{"status": StatusClaiming, "updated_at": s.now()})
	if r.Error != nil {
		return 0, 0, r.Error
	}
	if r.RowsAffected == 0 {
		var t UserTask
		err := db.Where("user_id = ? AND task_id = ? AND period = ?", userID, taskID, period).Take(&t).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			return 0, 0, ErrNotCompleted
		case err != nil:
			return 0, 0, err
		case t.Status == StatusClaimed:
			return 0, 0, ErrAlreadyClaimed
		default:
			return 0, 0, ErrNotCompleted
		}
	}

	balance, err := s.issue(ctx, userID, def, period)
	if err != nil {
		s.logger.WarnContext(ctx, "task: reward credit failed, left in claiming", "user_id", userID, "task_id", taskID, "err", err)
		return 0, 0, ErrRewardPending
	}
	return def.RewardCoin, balance, nil
}

func (s *Service) issue(ctx context.Context, userID uint64, def Definition, period string) (int64, error) {
	resp, err := s.wallet.Credit(ctx, &walletv1.CreditRequest{
		UserId: userID, Amount: def.RewardCoin, BizType: walletBizTaskReward,
		BizId: rewardBizID(def.ID, period, userID), Remark: def.Title,
	})
	if err != nil {
		return 0, err
	}
	err = s.db.WithContext(ctx).Model(&UserTask{}).
		Where("user_id = ? AND task_id = ? AND period = ? AND status = ?", userID, def.ID, period, StatusClaiming).
		Updates(map[string]any{"status": StatusClaimed, "updated_at": s.now()}).Error
	if err != nil {
		// 奖励已到账，仅状态未推进：补偿任务会修正，不能因此向用户报错导致其反复点击
		s.logger.ErrorContext(ctx, "task: mark claimed failed", "user_id", userID, "task_id", def.ID, "err", err)
	}
	return resp.GetBalanceAfter(), nil
}

// CompensateClaims 补偿停留在 claiming 超过 olderThan 的任务（例如跨周期后用户无法再触发重试）。
// 钱包入账幂等，所以多副本同时执行也只会入账一次，无需分布式锁。
func (s *Service) CompensateClaims(ctx context.Context, olderThan time.Duration) (int, error) {
	var stuck []UserTask
	err := s.db.WithContext(ctx).
		Where("status = ? AND updated_at < ?", StatusClaiming, s.now().Add(-olderThan)).
		Order("id").Limit(200).Find(&stuck).Error
	if err != nil {
		return 0, err
	}
	fixed := 0
	for _, t := range stuck {
		def, ok := s.defs[t.TaskID]
		if !ok {
			continue
		}
		if _, err := s.issue(ctx, t.UserID, def, t.Period); err != nil {
			s.logger.WarnContext(ctx, "task: compensation failed", "id", t.ID, "err", err)
			continue
		}
		fixed++
	}
	return fixed, nil
}

// RunCompensator 周期执行补偿。
func (s *Service) RunCompensator(ctx context.Context, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if n, err := s.CompensateClaims(ctx, time.Minute); err != nil {
				s.logger.ErrorContext(ctx, "task: compensation run failed", "err", err)
			} else if n > 0 {
				s.logger.InfoContext(ctx, "task: compensated stuck claims", "count", n)
			}
		}
	}
}

// HandleOrderPaid 消费 order.paid 累计充值类任务进度。
// 消费记录与进度更新在同一事务：重复投递时 MarkConsumed 返回 false，整条消息被跳过。
func (s *Service) HandleOrderPaid(ctx context.Context, msg mq.Message) error {
	var evt events.OrderPaid
	if err := json.Unmarshal(msg.Value, &evt); err != nil {
		return mq.Permanent(err)
	}
	if err := evt.Validate(); err != nil {
		return mq.Permanent(err)
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		first, err := mq.MarkConsumed(tx, consumerName, evt.EventID)
		if err != nil || !first {
			return err
		}
		now := s.now()
		for _, d := range definitions {
			var inc int64
			switch d.Kind {
			case KindRechargeCount:
				inc = 1
			case KindRechargeAmount:
				inc = evt.Amount
			default:
				continue
			}
			period := d.PeriodKey(evt.PaidAt)
			row := UserTask{UserID: evt.UserID, TaskID: d.ID, Period: period, Progress: inc, Target: d.Target, Status: StatusInProgress}
			// 原子累加：INSERT ... ON DUPLICATE KEY UPDATE progress = progress + ?，避免“读-改-写”丢失更新
			err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "user_id"}, {Name: "task_id"}, {Name: "period"}},
				DoUpdates: clause.Assignments(map[string]any{"progress": gorm.Expr("progress + ?", inc), "updated_at": now}),
			}).Create(&row).Error
			if err != nil {
				return err
			}
			err = tx.Model(&UserTask{}).
				Where("user_id = ? AND task_id = ? AND period = ? AND status = ? AND progress >= target", evt.UserID, d.ID, period, StatusInProgress).
				Updates(map[string]any{"status": StatusCompleted, "updated_at": now}).Error
			if err != nil {
				return err
			}
		}
		return nil
	})
}
