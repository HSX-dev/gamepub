package task

import (
	"fmt"
	"time"

	"gamepub/pkg/mq"
)

// 用户任务状态机：in_progress → completed → claiming → claimed。
// claiming 是“奖励发放中”的中间态：钱包调用失败时停留在此，重试/补偿以同一幂等键再次入账。
const (
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusClaiming   = "claiming"
	StatusClaimed    = "claimed"
)

// 任务类型与周期。
const (
	KindRechargeCount  = "recharge_count"
	KindRechargeAmount = "recharge_amount"

	PeriodDaily  = "daily"
	PeriodWeekly = "weekly"
	PeriodOnce   = "once"
)

// Definition 任务定义。
type Definition struct {
	ID         string
	Title      string
	Kind       string
	Period     string
	Target     int64
	RewardCoin int64
}

// 运营活动的任务配置；生产中可迁移到配置中心，此处静态声明便于评审。
var definitions = []Definition{
	{ID: "first_recharge", Title: "首次充值", Kind: KindRechargeCount, Period: PeriodOnce, Target: 1, RewardCoin: 50},
	{ID: "daily_recharge", Title: "每日充值任意金额", Kind: KindRechargeCount, Period: PeriodDaily, Target: 1, RewardCoin: 10},
	{ID: "weekly_recharge_100", Title: "本周累计充值 100 元", Kind: KindRechargeAmount, Period: PeriodWeekly, Target: 10000, RewardCoin: 100},
}

// 统一使用业务时区计算周期，避免 Pod 时区配置不同导致跨天边界不一致。
var bizZone = time.FixedZone("CST", 8*3600)

// PeriodKey 计算任务周期键。
func (d Definition) PeriodKey(t time.Time) string {
	t = t.In(bizZone)
	switch d.Period {
	case PeriodDaily:
		return t.Format("20060102")
	case PeriodWeekly:
		y, w := t.ISOWeek()
		return fmt.Sprintf("%dW%02d", y, w)
	default:
		return PeriodOnce
	}
}

// UserTask 用户任务进度，(user_id, task_id, period) 唯一。
type UserTask struct {
	ID        uint64 `gorm:"primaryKey"`
	UserID    uint64 `gorm:"not null;uniqueIndex:uk_user_task_period,priority:1"`
	TaskID    string `gorm:"size:32;not null;uniqueIndex:uk_user_task_period,priority:2"`
	Period    string `gorm:"size:16;not null;uniqueIndex:uk_user_task_period,priority:3"`
	Progress  int64  `gorm:"not null;default:0"`
	Target    int64  `gorm:"not null"`
	Status    string `gorm:"size:16;not null;index"`
	CreatedAt time.Time
	UpdatedAt time.Time `gorm:"index"`
}

// TableName 表名。
func (UserTask) TableName() string { return "task_user_task" }

// Models 需要迁移的模型。
func Models() []any { return []any{&UserTask{}, &mq.Consumed{}} }
