package task

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"gorm.io/gorm"

	walletv1 "gamepub/api/gen/wallet/v1"
	"gamepub/internal/events"
	"gamepub/internal/testkit"
	"gamepub/pkg/logx"
	"gamepub/pkg/mq"
)

// fakeWallet 模拟钱包的幂等入账语义。
type fakeWallet struct {
	mu      sync.Mutex
	fail    bool
	credits map[string]int64
	calls   int
}

func (w *fakeWallet) Credit(_ context.Context, in *walletv1.CreditRequest, _ ...grpc.CallOption) (*walletv1.CreditResponse, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.fail {
		return nil, errors.New("wallet unavailable")
	}
	_, dup := w.credits[in.GetBizId()]
	if !dup {
		w.credits[in.GetBizId()] = in.GetAmount()
	}
	var total int64
	for _, v := range w.credits {
		total += v
	}
	return &walletv1.CreditResponse{BalanceAfter: total, Duplicated: dup}, nil
}

func (w *fakeWallet) total() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	var t int64
	for _, v := range w.credits {
		t += v
	}
	return t
}

var now = time.Date(2026, 10, 15, 12, 0, 0, 0, bizZone)

func setup(t *testing.T) (*Service, *fakeWallet, *gorm.DB) {
	t.Helper()
	db := testkit.NewDB(t, Models()...)
	w := &fakeWallet{credits: map[string]int64{}}
	s := NewService(db, w, logx.Discard())
	s.now = func() time.Time { return now }
	return s, w, db
}

func paid(t *testing.T, s *Service, eventID string, uid uint64, amount int64) {
	t.Helper()
	body, _ := json.Marshal(events.OrderPaid{EventID: eventID, OrderNo: eventID, UserID: uid, Amount: amount, Coin: amount / 10, PaidAt: now})
	if err := s.HandleOrderPaid(context.Background(), mq.Message{Value: body}); err != nil {
		t.Fatal(err)
	}
}

func progressOf(t *testing.T, s *Service, uid uint64) map[string]Progress {
	t.Helper()
	list, err := s.ListTasks(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]Progress{}
	for _, p := range list {
		m[p.ID] = p
	}
	return m
}

func TestPeriodKey(t *testing.T) {
	daily := Definition{Period: PeriodDaily}
	// UTC 16:30 已是北京时间次日 00:30
	if got := daily.PeriodKey(time.Date(2026, 10, 15, 16, 30, 0, 0, time.UTC)); got != "20261016" {
		t.Fatalf("daily key = %s", got)
	}
	if got := (Definition{Period: PeriodWeekly}).PeriodKey(now); got != "2026W42" {
		t.Fatalf("weekly key = %s", got)
	}
}

func TestProgressFromEventsIsIdempotent(t *testing.T) {
	s, _, _ := setup(t)
	paid(t, s, "order-paid:A", 1, 6000)
	paid(t, s, "order-paid:A", 1, 6000) // 重复投递
	p := progressOf(t, s, 1)
	if p["weekly_recharge_100"].Progress != 6000 || p["weekly_recharge_100"].Status != StatusInProgress {
		t.Fatalf("duplicate event must not double count: %+v", p["weekly_recharge_100"])
	}
	if p["first_recharge"].Status != StatusCompleted || p["daily_recharge"].Status != StatusCompleted {
		t.Fatalf("count tasks should complete: %+v", p)
	}

	paid(t, s, "order-paid:B", 1, 6000)
	if p := progressOf(t, s, 1)["weekly_recharge_100"]; p.Progress != 12000 || p.Status != StatusCompleted {
		t.Fatalf("weekly task should complete after reaching target: %+v", p)
	}
	if p := progressOf(t, s, 2)["first_recharge"]; p.Progress != 0 {
		t.Fatalf("other users unaffected: %+v", p)
	}
}

func TestClaimReward(t *testing.T) {
	s, w, _ := setup(t)
	ctx := context.Background()
	if _, _, err := s.ClaimReward(ctx, 1, "first_recharge"); !errors.Is(err, ErrNotCompleted) {
		t.Fatalf("claim before completion: %v", err)
	}
	if _, _, err := s.ClaimReward(ctx, 1, "no_such_task"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("unknown task: %v", err)
	}
	paid(t, s, "order-paid:A", 1, 600)
	reward, _, err := s.ClaimReward(ctx, 1, "first_recharge")
	if err != nil || reward != 50 {
		t.Fatalf("claim: %d %v", reward, err)
	}
	if _, _, err := s.ClaimReward(ctx, 1, "first_recharge"); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("second claim: %v", err)
	}
	if w.total() != 50 {
		t.Fatalf("wallet credited %d", w.total())
	}
}

func TestConcurrentClaimsCreditOnce(t *testing.T) {
	s, w, _ := setup(t)
	paid(t, s, "order-paid:A", 1, 600)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = s.ClaimReward(context.Background(), 1, "first_recharge")
		}()
	}
	wg.Wait()
	if w.total() != 50 {
		t.Fatalf("concurrent claims credited %d, want 50", w.total())
	}
}

func TestWalletFailureLeavesClaimingAndRetrySucceeds(t *testing.T) {
	s, w, db := setup(t)
	ctx := context.Background()
	paid(t, s, "order-paid:A", 1, 600)

	w.fail = true
	if _, _, err := s.ClaimReward(ctx, 1, "first_recharge"); !errors.Is(err, ErrRewardPending) {
		t.Fatalf("wallet down: %v", err)
	}
	var row UserTask
	db.Where("user_id = ? AND task_id = ?", 1, "first_recharge").Take(&row)
	if row.Status != StatusClaiming {
		t.Fatalf("status = %s, want claiming", row.Status)
	}

	w.fail = false
	if _, _, err := s.ClaimReward(ctx, 1, "first_recharge"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if w.total() != 50 {
		t.Fatalf("credited %d", w.total())
	}
}

func TestCompensateStuckClaims(t *testing.T) {
	s, w, db := setup(t)
	paid(t, s, "order-paid:A", 1, 600)
	w.fail = true
	_, _, _ = s.ClaimReward(context.Background(), 1, "daily_recharge")
	w.fail = false

	// 时间推进到第二天：用户已无法对昨天的每日任务发起重试，只能靠补偿
	now = now.Add(24 * time.Hour)
	defer func() { now = now.Add(-24 * time.Hour) }()
	n, err := s.CompensateClaims(context.Background(), time.Minute)
	if err != nil || n != 1 || w.total() != 10 {
		t.Fatalf("compensated=%d err=%v credited=%d", n, err, w.total())
	}
	var row UserTask
	db.Where("task_id = ?", "daily_recharge").Take(&row)
	if row.Status != StatusClaimed {
		t.Fatalf("status = %s", row.Status)
	}
}

func TestMalformedEventIsPermanent(t *testing.T) {
	s, _, _ := setup(t)
	if err := s.HandleOrderPaid(context.Background(), mq.Message{Value: []byte(`{"event_id":""}`)}); !mq.IsPermanent(err) {
		t.Fatalf("got %v", err)
	}
}
