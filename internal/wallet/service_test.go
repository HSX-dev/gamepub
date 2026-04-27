package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"gamepub/internal/events"
	"gamepub/internal/testkit"
	"gamepub/pkg/logx"
	"gamepub/pkg/mq"
)

func newService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	db := testkit.NewDB(t, Models()...)
	return NewService(db, logx.Discard()), db
}

func credit(t *testing.T, s *Service, uid uint64, amount int64, bizID string) *ChangeResult {
	t.Helper()
	res, err := s.Credit(context.Background(), ChangeRequest{UserID: uid, Amount: amount, BizType: BizRecharge, BizID: bizID})
	if err != nil {
		t.Fatalf("credit %s: %v", bizID, err)
	}
	return res
}

func assertConsistent(t *testing.T, db *gorm.DB, uid uint64, wantAvailable, wantFrozen int64) {
	t.Helper()
	var acc Account
	if err := db.Where("user_id = ?", uid).Take(&acc).Error; err != nil {
		t.Fatal(err)
	}
	var sum int64
	db.Model(&Ledger{}).Select("COALESCE(SUM(amount),0)").Where("user_id = ?", uid).Scan(&sum)
	if acc.Available != wantAvailable || acc.Frozen != wantFrozen {
		t.Fatalf("balance = (%d,%d), want (%d,%d)", acc.Available, acc.Frozen, wantAvailable, wantFrozen)
	}
	if sum != acc.Available+acc.Frozen {
		t.Fatalf("ledger sum %d != account total %d", sum, acc.Available+acc.Frozen)
	}
}

func TestCreditIsIdempotent(t *testing.T) {
	s, db := newService(t)
	first := credit(t, s, 1, 100, "O1")
	second := credit(t, s, 1, 100, "O1")
	if first.Duplicated || !second.Duplicated || second.LedgerID != first.LedgerID {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	assertConsistent(t, db, 1, 100, 0)

	_, err := s.Credit(context.Background(), ChangeRequest{UserID: 1, Amount: 999, BizType: BizRecharge, BizID: "O1"})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("reusing biz_id with different amount: got %v", err)
	}
	_, err = s.Credit(context.Background(), ChangeRequest{UserID: 2, Amount: 100, BizType: BizRecharge, BizID: "O1"})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("reusing biz_id for another user: got %v", err)
	}
}

func TestValidation(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	cases := []struct {
		req  ChangeRequest
		want error
	}{
		{ChangeRequest{UserID: 1, Amount: -100, BizType: BizRecharge, BizID: "x"}, ErrInvalidAmount},
		{ChangeRequest{UserID: 1, Amount: 0, BizType: BizRecharge, BizID: "x"}, ErrInvalidAmount},
		{ChangeRequest{UserID: 1, Amount: MaxAmount + 1, BizType: BizRecharge, BizID: "x"}, ErrInvalidAmount},
		{ChangeRequest{UserID: 1, Amount: 1, BizType: BizPurchase, BizID: "x"}, ErrInvalidBiz}, // 消费类型不能用于入账
		{ChangeRequest{UserID: 1, Amount: 1, BizType: BizRecharge, BizID: ""}, ErrInvalidBiz},
		{ChangeRequest{UserID: 0, Amount: 1, BizType: BizRecharge, BizID: "x"}, ErrInvalidArgument},
	}
	for _, c := range cases {
		if _, err := s.Credit(ctx, c.req); !errors.Is(err, c.want) {
			t.Errorf("Credit(%+v) = %v, want %v", c.req, err, c.want)
		}
	}
}

func TestDebitInsufficientBalance(t *testing.T) {
	s, db := newService(t)
	credit(t, s, 1, 50, "O1")
	_, err := s.Debit(context.Background(), ChangeRequest{UserID: 1, Amount: 51, BizType: BizPurchase, BizID: "P1"})
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("got %v", err)
	}
	_, err = s.Debit(context.Background(), ChangeRequest{UserID: 99, Amount: 1, BizType: BizPurchase, BizID: "P2"})
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("debit without account: got %v", err)
	}
	assertConsistent(t, db, 1, 50, 0)
}

func TestConcurrentDebitsNeverOverdraw(t *testing.T) {
	s, db := newService(t)
	credit(t, s, 1, 100, "O1")

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ok, low int
	)
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Debit(context.Background(), ChangeRequest{UserID: 1, Amount: 10, BizType: BizPurchase, BizID: fmt.Sprintf("P%d", i)})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrInsufficientBalance):
				low++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 10 || low != 20 {
		t.Fatalf("succeeded=%d insufficient=%d, want 10/20", ok, low)
	}
	assertConsistent(t, db, 1, 0, 0)
}

func TestConcurrentSameBizIDCreditsOnce(t *testing.T) {
	s, db := newService(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Credit(context.Background(), ChangeRequest{UserID: 1, Amount: 60, BizType: BizRecharge, BizID: "O-dup"}); err != nil {
				t.Errorf("credit: %v", err)
			}
		}()
	}
	wg.Wait()
	assertConsistent(t, db, 1, 60, 0)
}

func TestOptimisticLockConflictRetries(t *testing.T) {
	s, db := newService(t)
	credit(t, s, 1, 100, "O1")

	calls := 0
	s.afterSnapshot = func(tx *gorm.DB) {
		calls++
		if calls == 1 {
			// 模拟另一个请求在快照之后修改了账户
			tx.Exec("UPDATE wallet_account SET available = available + 5, version = version + 1 WHERE user_id = ?", 1)
		}
	}
	res, err := s.Debit(context.Background(), ChangeRequest{UserID: 1, Amount: 30, BizType: BizPurchase, BizID: "P1"})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || res.BalanceAfter != 70 {
		t.Fatalf("expected one retry with fresh snapshot: calls=%d balance_after=%d", calls, res.BalanceAfter)
	}
	assertConsistent(t, db, 1, 70, 0)
}

func TestTCCTryConfirm(t *testing.T) {
	s, db := newService(t)
	ctx := context.Background()
	credit(t, s, 1, 100, "O1")

	if st, err := s.TryFreeze(ctx, "X1", 1, 40); err != nil || st != BranchTried {
		t.Fatalf("try: %s %v", st, err)
	}
	if st, _ := s.TryFreeze(ctx, "X1", 1, 40); st != BranchTried {
		t.Fatal("try must be idempotent")
	}
	assertConsistent(t, db, 1, 60, 40)

	if st, err := s.ConfirmFreeze(ctx, "X1"); err != nil || st != BranchConfirmed {
		t.Fatalf("confirm: %s %v", st, err)
	}
	if st, err := s.ConfirmFreeze(ctx, "X1"); err != nil || st != BranchConfirmed {
		t.Fatalf("confirm must be idempotent: %s %v", st, err)
	}
	assertConsistent(t, db, 1, 60, 0)

	if _, err := s.CancelFreeze(ctx, "X1"); !errors.Is(err, ErrBranchRejected) {
		t.Fatalf("cancel after confirm: got %v", err)
	}
}

func TestTCCTryCancel(t *testing.T) {
	s, db := newService(t)
	ctx := context.Background()
	credit(t, s, 1, 100, "O1")

	_, _ = s.TryFreeze(ctx, "X1", 1, 40)
	if st, err := s.CancelFreeze(ctx, "X1"); err != nil || st != BranchCancelled {
		t.Fatalf("cancel: %s %v", st, err)
	}
	if st, err := s.CancelFreeze(ctx, "X1"); err != nil || st != BranchCancelled {
		t.Fatalf("cancel must be idempotent: %s %v", st, err)
	}
	if _, err := s.ConfirmFreeze(ctx, "X1"); !errors.Is(err, ErrBranchRejected) {
		t.Fatalf("confirm after cancel: got %v", err)
	}
	assertConsistent(t, db, 1, 100, 0)
}

func TestTCCEmptyRollbackAndSuspension(t *testing.T) {
	s, db := newService(t)
	ctx := context.Background()
	credit(t, s, 1, 100, "O1")

	// Try 丢失，协调者超时后发起 Cancel：空回滚成功
	if st, err := s.CancelFreeze(ctx, "X-lost"); err != nil || st != BranchCancelled {
		t.Fatalf("empty rollback: %s %v", st, err)
	}
	// 迟到的 Try 到达：必须拒绝，否则资金被永久冻结（悬挂）
	if _, err := s.TryFreeze(ctx, "X-lost", 1, 40); !errors.Is(err, ErrBranchRejected) {
		t.Fatalf("suspended try: got %v", err)
	}
	assertConsistent(t, db, 1, 100, 0)
}

func TestTCCTryInsufficientRollsBackBranch(t *testing.T) {
	s, db := newService(t)
	ctx := context.Background()
	credit(t, s, 1, 10, "O1")

	if _, err := s.TryFreeze(ctx, "X1", 1, 40); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("got %v", err)
	}
	var n int64
	db.Model(&TCCBranch{}).Where("xid = ?", "X1").Count(&n)
	if n != 0 {
		t.Fatal("failed try must not leave a branch record")
	}
	if st, err := s.CancelFreeze(ctx, "X1"); err != nil || st != BranchCancelled {
		t.Fatalf("cancel after failed try should be an empty rollback: %s %v", st, err)
	}
}

func TestReconcileDetectsMismatch(t *testing.T) {
	s, db := newService(t)
	credit(t, s, 1, 100, "O1")
	credit(t, s, 2, 50, "O2")
	if _, err := s.Debit(context.Background(), ChangeRequest{UserID: 2, Amount: 20, BizType: BizPurchase, BizID: "P1"}); err != nil {
		t.Fatal(err)
	}
	r := NewReconciler(db, nil, time.Minute, logx.Discard())
	if n, err := r.RunOnce(context.Background(), nil); err != nil || n != 0 {
		t.Fatalf("consistent data should have no diffs: n=%d err=%v", n, err)
	}

	// 绕过服务直接改余额（模拟 bug 或越权篡改）
	db.Exec("UPDATE wallet_account SET available = available + 1000 WHERE user_id = ?", 1)
	n, err := r.RunOnce(context.Background(), nil)
	if err != nil || n != 1 {
		t.Fatalf("expected 1 diff, got n=%d err=%v", n, err)
	}
	diffs, _ := s.ListReconcileDiffs(context.Background(), 10)
	if len(diffs) != 1 || diffs[0].UserID != 1 || diffs[0].WalletBalance != 1100 || diffs[0].LedgerSum != 100 {
		t.Fatalf("unexpected diff record: %+v", diffs)
	}
}

func TestHandleOrderPaid(t *testing.T) {
	s, db := newService(t)
	evt := events.OrderPaid{EventID: "O1", OrderNo: "O1", UserID: 7, Amount: 600, Coin: 60, PaidAt: time.Now()}
	body, _ := json.Marshal(evt)

	for i := 0; i < 3; i++ { // 重复投递
		if err := s.HandleOrderPaid(context.Background(), mq.Message{Value: body}); err != nil {
			t.Fatal(err)
		}
	}
	assertConsistent(t, db, 7, 60, 0)

	if err := s.HandleOrderPaid(context.Background(), mq.Message{Value: []byte("{bad")}); !mq.IsPermanent(err) {
		t.Fatalf("malformed payload should be permanent: %v", err)
	}
	bad, _ := json.Marshal(events.OrderPaid{EventID: "O2", OrderNo: "O2", Amount: 600, Coin: 60, PaidAt: time.Now()})
	if err := s.HandleOrderPaid(context.Background(), mq.Message{Value: bad}); !mq.IsPermanent(err) {
		t.Fatalf("event without user_id should be permanent: %v", err)
	}
}

func TestListLedgerSortWhitelist(t *testing.T) {
	s, _ := newService(t)
	credit(t, s, 1, 10, "O1")
	credit(t, s, 1, 30, "O2")
	credit(t, s, 2, 99, "O3")

	rows, total, err := s.ListLedger(context.Background(), LedgerQuery{UserID: 1, SortBy: "amount", Desc: true})
	if err != nil || total != 2 || rows[0].Amount != 30 {
		t.Fatalf("rows=%+v total=%d err=%v", rows, total, err)
	}
	// 注入载荷回落到默认排序，而不是报 SQL 错误或执行
	if _, _, err := s.ListLedger(context.Background(), LedgerQuery{UserID: 1, SortBy: "amount DESC; DROP TABLE wallet_ledger"}); err != nil {
		t.Fatalf("malicious sort should fall back safely: %v", err)
	}
	if _, _, err := s.ListLedger(context.Background(), LedgerQuery{UserID: 1, BizType: "' OR 1=1 --"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unknown biz_type: %v", err)
	}
}
