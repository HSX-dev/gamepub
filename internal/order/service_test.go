package order

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gamepub/internal/events"
	"gamepub/internal/testkit"
	"gamepub/pkg/logx"
	"gamepub/pkg/mq"
	"gamepub/pkg/netguard"
	"gamepub/pkg/resilience"
	"gamepub/pkg/signature"
)

var (
	paySecret      = []byte("mockpay-secret-0123456789abcdef")
	deliverySecret = []byte("delivery-secret-0123456789abcdef")
)

type fixture struct {
	svc *Service
	db  *gorm.DB
}

func setup(t *testing.T) *fixture {
	t.Helper()
	db := testkit.NewDB(t, Models()...)
	cfg := resilience.DefaultConfig()
	cfg.BaseBackoff, cfg.MaxBackoff = time.Millisecond, time.Millisecond
	svc := NewService(Options{
		DB: db,
		PayVerifiers: map[string]*signature.Verifier{
			"mockpay": signature.NewVerifier("mockpay", paySecret, 5*time.Minute, signature.NewMemoryNonceStore()),
		},
		Guard:      netguard.Default(),
		Notifier:   NewNotifier(resilience.New(http.DefaultClient, cfg), deliverySecret),
		PayBaseURL: "https://pay.example.com/checkout",
		Logger:     logx.Discard(),
	})
	if _, err := svc.UpsertGameApp(context.Background(), "demo_game", "Demo", "", "tester"); err != nil {
		t.Fatal(err)
	}
	return &fixture{svc: svc, db: db}
}

func (f *fixture) create(t *testing.T, uid uint64) *Order {
	t.Helper()
	o, _, err := f.svc.CreateOrder(context.Background(), CreateOrderInput{UserID: uid, GameID: "demo_game", ProductID: "coin_60", Channel: "mockpay"})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func notify(f *fixture, body any, nonce string) (string, error) {
	raw, _ := json.Marshal(body)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	return f.svc.HandlePayNotify(context.Background(), "mockpay", raw, map[string]string{
		signature.HeaderTimestamp: ts,
		signature.HeaderNonce:     nonce,
		signature.HeaderSignature: signature.Sign(paySecret, ts, nonce, raw),
	})
}

func TestCreateOrderUsesServerSidePrice(t *testing.T) {
	f := setup(t)
	o := f.create(t, 1)
	if o.Amount != 600 || o.Coin != 60 || o.Status != StatusPending || !orderNoPattern.MatchString(o.OrderNo) {
		t.Fatalf("unexpected order: %+v", o)
	}
	ctx := context.Background()
	cases := []struct {
		in   CreateOrderInput
		want error
	}{
		{CreateOrderInput{UserID: 1, GameID: "demo_game", ProductID: "coin_free", Channel: "mockpay"}, ErrProductNotFound},
		{CreateOrderInput{UserID: 1, GameID: "no_such_game", ProductID: "coin_60", Channel: "mockpay"}, ErrGameNotFound},
		{CreateOrderInput{UserID: 1, GameID: "demo_game", ProductID: "coin_60", Channel: "unknownpay"}, ErrChannelNotSupported},
		{CreateOrderInput{UserID: 1, GameID: "demo'; DROP TABLE x;--", ProductID: "coin_60", Channel: "mockpay"}, ErrInvalidArgument},
	}
	for _, c := range cases {
		if _, _, err := f.svc.CreateOrder(ctx, c.in); !errors.Is(err, c.want) {
			t.Errorf("CreateOrder(%+v) = %v, want %v", c.in, err, c.want)
		}
	}
}

func TestCreateOrderIdempotencyKey(t *testing.T) {
	f := setup(t)
	in := CreateOrderInput{UserID: 1, GameID: "demo_game", ProductID: "coin_300", Channel: "mockpay", IdempotencyKey: "client-req-0001"}
	a, _, err := f.svc.CreateOrder(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := f.svc.CreateOrder(context.Background(), in)
	if a.OrderNo != b.OrderNo {
		t.Fatal("same idempotency key must return the same order")
	}
	in.UserID = 2 // 不同用户使用相同的 key 互不影响
	c, _, _ := f.svc.CreateOrder(context.Background(), in)
	if c.OrderNo == a.OrderNo {
		t.Fatal("idempotency key must be scoped per user")
	}
}

func TestHorizontalPrivilegeEscalationBlocked(t *testing.T) {
	f := setup(t)
	mine := f.create(t, 1)
	theirs := f.create(t, 2)
	ctx := context.Background()

	if _, err := f.svc.GetOrder(ctx, 1, mine.OrderNo); err != nil {
		t.Fatalf("owner should read own order: %v", err)
	}
	if _, err := f.svc.GetOrder(ctx, 1, theirs.OrderNo); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("reading another user's order must look like not-found, got %v", err)
	}
	rows, err := f.svc.BatchGetOrders(ctx, 1, []string{mine.OrderNo, theirs.OrderNo, mine.OrderNo, "' OR 1=1 --"})
	if err != nil || len(rows) != 1 || rows[0].OrderNo != mine.OrderNo {
		t.Fatalf("batch must only return caller's orders: %+v %v", rows, err)
	}
	many := make([]string, MaxBatch+1)
	if _, err := f.svc.BatchGetOrders(ctx, 1, many); !errors.Is(err, ErrTooManyOrders) {
		t.Fatalf("batch limit: %v", err)
	}
}

func TestPayNotifyHappyPathAndIdempotency(t *testing.T) {
	f := setup(t)
	o := f.create(t, 1)
	body := payNotify{OrderNo: o.OrderNo, TradeNo: "T001", Amount: 600, Status: "SUCCESS"}

	if reply, err := notify(f, body, "nonce-0000001"); err != nil || reply != ReplySuccess {
		t.Fatalf("notify: %q %v", reply, err)
	}
	// 渠道重复通知（新 nonce）：幂等成功，不重复写 outbox
	if _, err := notify(f, body, "nonce-0000002"); err != nil {
		t.Fatalf("duplicate notify: %v", err)
	}
	// 截获原报文重放：拒绝
	if _, err := notify(f, body, "nonce-0000001"); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("replay: %v", err)
	}

	var got Order
	f.db.Where("order_no = ?", o.OrderNo).Take(&got)
	if got.Status != StatusPaid || got.TradeNo == nil || *got.TradeNo != "T001" || got.PaidAt == nil {
		t.Fatalf("order not paid: %+v", got)
	}
	var outbox []Outbox
	f.db.Find(&outbox)
	if len(outbox) != 1 || outbox[0].Topic != events.TopicOrderPaid {
		t.Fatalf("expected exactly one outbox event, got %+v", outbox)
	}
	var evt events.OrderPaid
	if err := json.Unmarshal([]byte(outbox[0].Payload), &evt); err != nil || evt.Validate() != nil || evt.Coin != 60 {
		t.Fatalf("bad event payload: %+v %v", evt, err)
	}
}

func TestPayNotifyRejections(t *testing.T) {
	f := setup(t)
	o := f.create(t, 1)
	other := f.create(t, 2)

	// 金额被篡改（签名本身合法，例如渠道侧配置错误或商户密钥泄露）
	if _, err := notify(f, payNotify{OrderNo: o.OrderNo, TradeNo: "T1", Amount: 1, Status: "SUCCESS"}, "nonce-a-0001"); !errors.Is(err, ErrAmountMismatch) {
		t.Fatalf("amount mismatch: %v", err)
	}
	// 业务失败后 nonce 已释放，渠道用原 nonce 重试可以被处理
	if _, err := notify(f, payNotify{OrderNo: o.OrderNo, TradeNo: "T1", Amount: 600, Status: "SUCCESS"}, "nonce-a-0001"); err != nil {
		t.Fatalf("retry with same nonce after failure should pass: %v", err)
	}
	// 同一笔渠道交易号试图核销第二个订单
	if _, err := notify(f, payNotify{OrderNo: other.OrderNo, TradeNo: "T1", Amount: 600, Status: "SUCCESS"}, "nonce-b-0001"); !errors.Is(err, ErrTradeReused) {
		t.Fatalf("trade reuse: %v", err)
	}
	// 签名错误
	raw, _ := json.Marshal(payNotify{OrderNo: other.OrderNo, TradeNo: "T2", Amount: 600, Status: "SUCCESS"})
	_, err := f.svc.HandlePayNotify(context.Background(), "mockpay", raw, map[string]string{
		signature.HeaderTimestamp: strconv.FormatInt(time.Now().Unix(), 10), signature.HeaderNonce: "nonce-c-0001", signature.HeaderSignature: "00",
	})
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("bad signature: %v", err)
	}

	var got Order
	f.db.Where("order_no = ?", other.OrderNo).Take(&got)
	if got.Status != StatusPending {
		t.Fatalf("rejected notifications must not change order state: %+v", got)
	}
}

type fakePublisher struct {
	fail bool
	got  []mq.OutMessage
}

func (p *fakePublisher) Publish(_ context.Context, msgs ...mq.OutMessage) error {
	if p.fail {
		return errors.New("kafka unavailable")
	}
	p.got = append(p.got, msgs...)
	return nil
}
func (p *fakePublisher) Close() error { return nil }

func TestOutboxRelay(t *testing.T) {
	f := setup(t)
	o := f.create(t, 1)
	if _, err := notify(f, payNotify{OrderNo: o.OrderNo, TradeNo: "T1", Amount: 600, Status: "SUCCESS"}, "nonce-r-0001"); err != nil {
		t.Fatal(err)
	}
	pub := &fakePublisher{fail: true}
	relay := NewRelay(f.db, pub, nil, time.Second, logx.Discard())

	if _, err := relay.RunOnce(context.Background()); err == nil {
		t.Fatal("publish failure should surface")
	}
	var row Outbox
	f.db.First(&row)
	if row.Status != outboxPending || row.Attempts != 1 {
		t.Fatalf("failed publish must keep event pending: %+v", row)
	}

	pub.fail = false
	if n, err := relay.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("relay: n=%d err=%v", n, err)
	}
	f.db.First(&row)
	if row.Status != outboxSent || len(pub.got) != 1 || string(pub.got[0].Key) != "1" {
		t.Fatalf("event not relayed: %+v %+v", row, pub.got)
	}
}

func TestGameAppDeliveryURLSSRF(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, u := range []string{
		"http://127.0.0.1:8080/admin",
		"http://169.254.169.254/latest/meta-data/iam/",
		"http://10.0.0.8/internal",
		"file:///etc/passwd",
		"http://user:pass@93.184.216.34/",
	} {
		if _, err := f.svc.UpsertGameApp(ctx, "demo_game", "Demo", u, "ops-1"); !errors.Is(err, ErrInvalidDeliveryURL) {
			t.Errorf("delivery url %q: got %v", u, err)
		}
	}
	if _, err := f.svc.UpsertGameApp(ctx, "demo_game", "Demo", "https://93.184.216.34/delivery", "ops-1"); err != nil {
		t.Fatalf("public url should be accepted: %v", err)
	}
}

func TestDeliveryIsSignedAndRetried(t *testing.T) {
	f := setup(t)
	verifier := signature.NewVerifier("delivery", deliverySecret, time.Minute, signature.NewMemoryNonceStore())
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := verifier.Verify(r.Context(), r.Header.Get(signature.HeaderTimestamp), r.Header.Get(signature.HeaderNonce), r.Header.Get(signature.HeaderSignature), body); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Idempotency-Key") == "" || hits.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // 第一次模拟游戏服抖动
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 测试环境放行本地回环地址；生产使用默认策略
	f.svc.guard = &netguard.Guard{Schemes: map[string]bool{"http": true}, AllowAddr: func(netip.AddrPort) bool { return true }}
	f.db.Model(&GameApp{}).Where("game_id = ?", "demo_game").Update("delivery_url", srv.URL+"/delivery")

	evt := events.OrderPaid{EventID: "e1", OrderNo: "20261015000000ABCDEF123456", UserID: 1, GameID: "demo_game", ProductID: "coin_60", Amount: 600, Coin: 60, PaidAt: time.Now()}
	raw, _ := json.Marshal(evt)
	if err := f.svc.HandleOrderPaidDelivery(context.Background(), mq.Message{Value: raw}); err != nil {
		t.Fatalf("delivery: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected 1 retry after 503, hits=%d", hits.Load())
	}

	evt.GameID = "unknown_game"
	raw, _ = json.Marshal(evt)
	if err := f.svc.HandleOrderPaidDelivery(context.Background(), mq.Message{Value: raw}); !mq.IsPermanent(err) {
		t.Fatalf("unknown game should be permanent: %v", err)
	}
}

func TestAdminListOrdersFilters(t *testing.T) {
	f := setup(t)
	for i := 0; i < 3; i++ {
		f.create(t, uint64(i+1))
	}
	ctx := context.Background()
	rows, total, err := f.svc.AdminListOrders(ctx, AdminQuery{UserID: 2, SortBy: "created_at", Desc: true})
	if err != nil || total != 1 || rows[0].UserID != 2 {
		t.Fatalf("rows=%+v total=%d err=%v", rows, total, err)
	}
	for _, q := range []AdminQuery{
		{SortBy: "amount; DELETE FROM order_order"},
		{Status: "paid' OR '1'='1"},
		{GameID: "x' UNION SELECT"},
	} {
		if _, _, err := f.svc.AdminListOrders(ctx, q); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("AdminListOrders(%+v) = %v, want ErrInvalidArgument", q, err)
		}
	}
	var n int64
	f.db.Model(&Order{}).Count(&n)
	if n != 3 {
		t.Fatalf("orders table altered: %d rows", n)
	}
}
