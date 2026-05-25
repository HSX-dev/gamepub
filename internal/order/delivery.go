package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"gorm.io/gorm"

	"gamepub/internal/events"
	"gamepub/pkg/cryptox"
	"gamepub/pkg/mq"
	"gamepub/pkg/resilience"
	"gamepub/pkg/signature"
)

// ErrDeliveryRejected 游戏服明确拒绝（4xx），重试无意义。
var ErrDeliveryRejected = errors.New("order: delivery rejected by game server")

// Notifier 平台 → 游戏服的发货通知。
// 底层客户端由 netguard（SSRF 防护）+ resilience（超时/重试/按 host 熔断）组成，
// 某个游戏服故障只会熔断它自己，不会拖垮订单主链路或其它游戏的发货。
type Notifier struct {
	client *resilience.Client
	secret []byte
	now    func() time.Time
}

// NewNotifier 创建通知器。
func NewNotifier(client *resilience.Client, secret []byte) *Notifier {
	return &Notifier{client: client, secret: secret, now: time.Now}
}

func (n *Notifier) post(ctx context.Context, url string, body []byte, idemKey string, idempotent bool) (*resilience.Response, error) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "gamepub-delivery/1.0")
	if idemKey != "" {
		h.Set("Idempotency-Key", idemKey)
	}
	return n.client.Do(ctx, resilience.Request{
		Method: http.MethodPost, URL: url, Body: body, Header: h, Idempotent: idempotent,
		// 每次尝试重新生成时间戳与 nonce：游戏服若实现了防重放，原样重试会被拒绝
		Prepare: func(r *http.Request) {
			ts := strconv.FormatInt(n.now().Unix(), 10)
			nonce := cryptox.RandomHex(16)
			r.Header.Set(signature.HeaderTimestamp, ts)
			r.Header.Set(signature.HeaderNonce, nonce)
			r.Header.Set(signature.HeaderSignature, signature.Sign(n.secret, ts, nonce, body))
		},
	})
}

type deliveryPayload struct {
	Type      string `json:"type"`
	OrderNo   string `json:"order_no,omitempty"`
	UserID    uint64 `json:"user_id,omitempty"`
	ProductID string `json:"product_id,omitempty"`
	Coin      int64  `json:"coin,omitempty"`
	PaidAt    int64  `json:"paid_at,omitempty"`
}

// Deliver 发送发货通知。游戏服须按 Idempotency-Key（订单号）去重，因此可以安全重试。
func (n *Notifier) Deliver(ctx context.Context, url string, evt *events.OrderPaid) error {
	body, err := json.Marshal(deliveryPayload{
		Type: "order.paid", OrderNo: evt.OrderNo, UserID: evt.UserID, ProductID: evt.ProductID,
		Coin: evt.Coin, PaidAt: evt.PaidAt.Unix(),
	})
	if err != nil {
		return err
	}
	resp, err := n.post(ctx, url, body, evt.OrderNo, true)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%w: status %d", ErrDeliveryRejected, resp.StatusCode)
	}
	return nil
}

// Ping 探测请求，不重试，只返回状态码（响应体不回显，避免成为 SSRF 读取通道）。
func (n *Notifier) Ping(ctx context.Context, url string) (int, error) {
	body, _ := json.Marshal(deliveryPayload{Type: "ping"})
	resp, err := n.post(ctx, url, body, "", false)
	if resp != nil {
		return resp.StatusCode, nil
	}
	return 0, err
}

// HandleOrderPaidDelivery 消费 order.paid，通知游戏服发货。
func (s *Service) HandleOrderPaidDelivery(ctx context.Context, msg mq.Message) error {
	var evt events.OrderPaid
	if err := json.Unmarshal(msg.Value, &evt); err != nil {
		return mq.Permanent(err)
	}
	if err := evt.Validate(); err != nil {
		return mq.Permanent(err)
	}
	var app GameApp
	if err := s.db.WithContext(ctx).Where("game_id = ?", evt.GameID).Take(&app).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return mq.Permanent(ErrGameNotFound)
		}
		return err
	}
	if app.DeliveryURL == "" {
		return nil
	}
	// 投递前再次校验：库中地址可能保存于规则收紧之前
	if _, err := s.guard.ValidateURL(ctx, app.DeliveryURL); err != nil {
		return mq.Permanent(fmt.Errorf("%w: %w", ErrInvalidDeliveryURL, err))
	}
	err := s.notifier.Deliver(ctx, app.DeliveryURL, &evt)
	if errors.Is(err, ErrDeliveryRejected) {
		return mq.Permanent(err)
	}
	return err
}
