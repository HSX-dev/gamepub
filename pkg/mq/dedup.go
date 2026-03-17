package mq

import (
	"time"

	"gorm.io/gorm"

	"gamepub/pkg/dbx"
)

// Consumed 消费记录表：(consumer, msg_id) 唯一，与业务写操作在同一事务内插入。
type Consumed struct {
	ID        uint64    `gorm:"primaryKey"`
	Consumer  string    `gorm:"size:64;not null;uniqueIndex:uk_consumer_msg,priority:1"`
	MsgID     string    `gorm:"size:128;not null;uniqueIndex:uk_consumer_msg,priority:2"`
	CreatedAt time.Time `gorm:"not null"`
}

// TableName 表名。
func (Consumed) TableName() string { return "mq_consumed" }

// MarkConsumed 在业务事务内调用。返回 false 表示该消息已被处理过（重复投递），调用方应直接返回 nil。
//
// 必须与业务变更处于同一个事务：若先标记再处理，处理失败会导致消息被永久跳过；
// 若先处理再标记，两步之间崩溃会导致重复处理。
func MarkConsumed(tx *gorm.DB, consumer, msgID string) (bool, error) {
	err := tx.Create(&Consumed{Consumer: consumer, MsgID: msgID}).Error
	if dbx.IsDuplicate(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
