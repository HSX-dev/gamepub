package redislock

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func setup(t *testing.T) (*miniredis.Miniredis, *Locker) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, New(rdb)
}

func TestMutualExclusion(t *testing.T) {
	_, l := setup(t)
	ctx := context.Background()
	a, err := l.TryLock(ctx, "job", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.TryLock(ctx, "job", time.Second); !errors.Is(err, ErrNotAcquired) {
		t.Fatalf("second TryLock = %v, want ErrNotAcquired", err)
	}
	if err := a.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := l.TryLock(ctx, "job", time.Second); err != nil {
		t.Fatalf("lock should be free after unlock: %v", err)
	}
}

// 持有者执行超时、锁过期后被 B 获取，A 的 Unlock 不能删掉 B 的锁。
func TestExpiredHolderCannotReleaseOthersLock(t *testing.T) {
	mr, l := setup(t)
	ctx := context.Background()
	a, _ := l.TryLock(ctx, "job", time.Second)
	mr.FastForward(2 * time.Second)

	b, err := l.TryLock(ctx, "job", time.Second)
	if err != nil {
		t.Fatalf("B should acquire expired lock: %v", err)
	}
	if err := a.Unlock(ctx); !errors.Is(err, ErrLockLost) {
		t.Fatalf("A.Unlock = %v, want ErrLockLost", err)
	}
	if err := a.Refresh(ctx, time.Second); !errors.Is(err, ErrLockLost) {
		t.Fatalf("A.Refresh = %v, want ErrLockLost", err)
	}
	if !mr.Exists("job") {
		t.Fatal("B's lock was deleted by A")
	}
	if err := b.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshExtendsTTL(t *testing.T) {
	mr, l := setup(t)
	ctx := context.Background()
	a, _ := l.TryLock(ctx, "job", time.Second)
	mr.FastForward(800 * time.Millisecond)
	if err := a.Refresh(ctx, time.Second); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(800 * time.Millisecond)
	if !mr.Exists("job") {
		t.Fatal("refreshed lock should still exist")
	}
}
