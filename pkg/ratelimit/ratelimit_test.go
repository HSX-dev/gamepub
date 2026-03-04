package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"gamepub/pkg/logx"
)

func TestRedisTokenBucket(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	l := NewRedis(rdb, "rl:", Rule{Rate: 1, Burst: 3}, logx.Discard())
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if d, err := l.Allow(ctx, "u1"); err != nil || !d.Allowed {
			t.Fatalf("request %d should pass: %+v %v", i, d, err)
		}
	}
	d, _ := l.Allow(ctx, "u1")
	if d.Allowed || d.RetryAfter <= 0 {
		t.Fatalf("4th request should be limited with RetryAfter: %+v", d)
	}
	if d, _ := l.Allow(ctx, "u2"); !d.Allowed {
		t.Fatal("other keys must not be affected")
	}
	now = now.Add(time.Second)
	if d, _ := l.Allow(ctx, "u1"); !d.Allowed {
		t.Fatal("token should be refilled after 1s")
	}
}

func TestFallbackWhenRedisDown(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	defer rdb.Close()
	mr.Close()

	// 速率取极小值：客户端拨号重试本身耗时，避免测试期间本地令牌被回填
	l := NewRedis(rdb, "rl:", Rule{Rate: 0.001, Burst: 2}, logx.Discard())
	ctx := context.Background()
	allowed := 0
	for i := 0; i < 5; i++ {
		d, err := l.Allow(ctx, "u1")
		if err != nil {
			t.Fatalf("fallback must not return error: %v", err)
		}
		if d.Allowed {
			allowed++
		}
	}
	if allowed != 2 {
		t.Fatalf("local fallback should still enforce burst=2, allowed=%d", allowed)
	}
}
