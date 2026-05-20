package order

import (
	"time"

	"gamepub/pkg/mq"
)

// 订单状态。
const (
	StatusPending = "pending"
	StatusPaid    = "paid"
	StatusClosed  = "closed"
)

// Order 支付订单。
//   - uk_order_no：订单号唯一；
//   - uk_user_idem (user_id, idempotency_key)：客户端重复提交下单只生成一笔订单；
//   - uk_channel_trade (channel, trade_no)：同一笔渠道交易只能核销一个订单，防止“一次付款多单到账”。
type Order struct {
	ID             uint64     `gorm:"primaryKey"`
	OrderNo        string     `gorm:"size:32;not null;uniqueIndex:uk_order_no"`
	UserID         uint64     `gorm:"not null;index:idx_user_status,priority:1;uniqueIndex:uk_user_idem,priority:1"`
	IdempotencyKey *string    `gorm:"size:64;uniqueIndex:uk_user_idem,priority:2"`
	GameID         string     `gorm:"size:32;not null;index"`
	ProductID      string     `gorm:"size:32;not null"`
	Amount         int64      `gorm:"not null"`
	Coin           int64      `gorm:"not null"`
	Channel        string     `gorm:"size:16;not null;uniqueIndex:uk_channel_trade,priority:1"`
	TradeNo        *string    `gorm:"size:64;uniqueIndex:uk_channel_trade,priority:2"`
	Status         string     `gorm:"size:16;not null;index:idx_user_status,priority:2"`
	PaidAt         *time.Time `gorm:""`
	CreatedAt      time.Time  `gorm:"index"`
	UpdatedAt      time.Time
}

// TableName 表名。
func (Order) TableName() string { return "order_order" }

// GameApp 游戏接入配置。DeliveryURL 由运营配置、由平台服务端发起请求，是典型的 SSRF 入口。
type GameApp struct {
	GameID      string `gorm:"primaryKey;size:32"`
	Name        string `gorm:"size:64;not null"`
	DeliveryURL string `gorm:"size:512"`
	UpdatedBy   string `gorm:"size:64"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TableName 表名。
func (GameApp) TableName() string { return "order_game_app" }

// Outbox 状态。
const (
	outboxPending = "pending"
	outboxSent    = "sent"
)

// Outbox 事务性发件箱：与订单状态变更在同一事务写入，再由 Relay 异步投递到 Kafka。
// 避免“订单已改为已支付，但发消息失败/进程崩溃”导致用户付了钱却不到账。
type Outbox struct {
	ID        uint64     `gorm:"primaryKey;index:idx_status_id,priority:2"`
	Topic     string     `gorm:"size:128;not null"`
	MsgKey    string     `gorm:"size:128"`
	Payload   string     `gorm:"type:text;not null"`
	Status    string     `gorm:"size:16;not null;index:idx_status_id,priority:1"`
	Attempts  int        `gorm:"not null;default:0"`
	CreatedAt time.Time  `gorm:""`
	SentAt    *time.Time `gorm:""`
}

// TableName 表名。
func (Outbox) TableName() string { return "order_outbox" }

// Models 需要迁移的模型。
func Models() []any { return []any{&Order{}, &GameApp{}, &Outbox{}, &mq.Consumed{}} }

// Product 商品。价格只来自服务端目录，绝不信任客户端传入的金额。
type Product struct {
	ID    string
	Title string
	Price int64 // 分
	Coin  int64
}

var catalog = map[string]Product{
	"coin_60":   {ID: "coin_60", Title: "60 平台币", Price: 600, Coin: 60},
	"coin_300":  {ID: "coin_300", Title: "300 平台币", Price: 3000, Coin: 300},
	"coin_980":  {ID: "coin_980", Title: "1000 平台币", Price: 9800, Coin: 1000},
	"coin_6480": {ID: "coin_6480", Title: "6800 平台币", Price: 64800, Coin: 6800},
}

// LookupProduct 查询商品。
func LookupProduct(id string) (Product, bool) {
	p, ok := catalog[id]
	return p, ok
}
