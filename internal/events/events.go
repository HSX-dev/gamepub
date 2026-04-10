// Package events 服务间异步事件定义（Kafka 消息体）。
package events

import (
	"errors"
	"time"
)

// 主题命名：<系统>.<领域>.<事件>；死信主题统一加 .dlq 后缀。
const (
	TopicOrderPaid    = "gamepub.order.paid"
	TopicOrderPaidDLQ = "gamepub.order.paid.dlq"
)

// OrderPaid 订单支付成功事件。EventID 全局唯一，消费者以其做幂等。
type OrderPaid struct {
	EventID   string    `json:"event_id"`
	OrderNo   string    `json:"order_no"`
	UserID    uint64    `json:"user_id"`
	GameID    string    `json:"game_id"`
	ProductID string    `json:"product_id"`
	Amount    int64     `json:"amount"`
	Coin      int64     `json:"coin"`
	PaidAt    time.Time `json:"paid_at"`
}

// Validate 消费端在处理前校验：上游 bug 或被投毒的消息不能进入资金链路。
func (e *OrderPaid) Validate() error {
	switch {
	case e.EventID == "" || e.OrderNo == "":
		return errors.New("events: missing event_id or order_no")
	case e.UserID == 0:
		return errors.New("events: missing user_id")
	case e.Amount <= 0 || e.Coin < 0:
		return errors.New("events: invalid amount")
	case e.PaidAt.IsZero():
		return errors.New("events: missing paid_at")
	}
	return nil
}
