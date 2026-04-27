package wallet

import (
	"time"

	"gamepub/pkg/mq"
)

// Account 钱包账户。总额 = Available + Frozen；Version 用于乐观锁。
type Account struct {
	ID        uint64 `gorm:"primaryKey"`
	UserID    uint64 `gorm:"not null;uniqueIndex:uk_user"`
	Available int64  `gorm:"not null;default:0"`
	Frozen    int64  `gorm:"not null;default:0"`
	Version   int64  `gorm:"not null;default:0"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 表名。
func (Account) TableName() string { return "wallet_account" }

// Ledger 资金流水。(biz_type, biz_id) 唯一索引是幂等的最终保障：
// 同一业务单据无论被重复请求多少次、无论并发与否，只可能落一条流水。
type Ledger struct {
	ID           uint64    `gorm:"primaryKey"`
	UserID       uint64    `gorm:"not null;index:idx_user_created,priority:1"`
	BizType      string    `gorm:"size:32;not null;uniqueIndex:uk_biz,priority:1"`
	BizID        string    `gorm:"size:64;not null;uniqueIndex:uk_biz,priority:2"`
	Amount       int64     `gorm:"not null"` // 有符号：入账为正，扣减为负
	BalanceAfter int64     `gorm:"not null"` // 变更后账户总额（可用+冻结），对账依据
	Remark       string    `gorm:"size:255"`
	CreatedAt    time.Time `gorm:"index:idx_user_created,priority:2"`
}

// TableName 表名。
func (Ledger) TableName() string { return "wallet_ledger" }

// TCC 分支状态。
const (
	BranchTried     = "tried"
	BranchConfirmed = "confirmed"
	BranchCancelled = "cancelled"
)

// TCCBranch TCC 事务分支记录，xid 唯一。它同时承担三件事：Try 幂等、空回滚记录、防悬挂判定。
type TCCBranch struct {
	ID        uint64 `gorm:"primaryKey"`
	XID       string `gorm:"column:xid;size:64;not null;uniqueIndex:uk_xid"`
	UserID    uint64 `gorm:"not null"`
	Amount    int64  `gorm:"not null"`
	Status    string `gorm:"size:16;not null"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 表名。
func (TCCBranch) TableName() string { return "wallet_tcc_branch" }

// ReconcileDiff 对账差异记录。
type ReconcileDiff struct {
	ID            uint64    `gorm:"primaryKey"`
	UserID        uint64    `gorm:"not null;index"`
	WalletBalance int64     `gorm:"not null"`
	LedgerSum     int64     `gorm:"not null"`
	CheckedAt     time.Time `gorm:"not null;index"`
}

// TableName 表名。
func (ReconcileDiff) TableName() string { return "wallet_reconcile_diff" }

// Models 需要迁移的模型。
func Models() []any {
	return []any{&Account{}, &Ledger{}, &TCCBranch{}, &ReconcileDiff{}, &mq.Consumed{}}
}
