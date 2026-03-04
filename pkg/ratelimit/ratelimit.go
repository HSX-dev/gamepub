// Package ratelimit 分布式令牌桶限流（Redis + Lua），Redis 故障时降级为单机限流。
package ratelimit

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"
)

// Decision 限流结果。
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
}

// Limiter 限流器。
type Limiter interface {
	Allow(ctx context.Context, key string) (Decision, error)
}

// Rule 令牌桶参数：每秒补充 Rate 个令牌，桶容量 Burst。
type Rule struct {
	Rate  float64
	Burst int
}

// 令牌桶在 Redis 中原子计算，避免“读-算-写”三步之间被并发请求穿透。
var tokenBucket = redis.NewScript(`
local rate  = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now   = tonumber(ARGV[3])
local cost  = tonumber(ARGV[4])
local data = redis.call("HMGET", KEYS[1], "tokens", "ts")
local tokens = tonumber(data[1])
local ts = tonumber(data[2])
if tokens == nil then
  tokens = burst
  ts = now
end
local elapsed = math.max(0, now - ts)
tokens = math.min(burst, tokens + elapsed * rate / 1000)
local allowed = 0
if tokens >= cost then
  tokens = tokens - cost
  allowed = 1
end
redis.call("HSET", KEYS[1], "tokens", tokens, "ts", now)
redis.call("PEXPIRE", KEYS[1], math.ceil(burst / rate * 1000) + 1000)
return {allowed, tostring(tokens)}`)

// RedisLimiter 多副本共享配额的限流器。
type RedisLimiter struct {
	rdb      redis.Scripter
	prefix   string
	rule     Rule
	fallback *Local
	logger   *slog.Logger
	now      func() time.Time
}

// NewRedis 创建 Redis 限流器。
func NewRedis(rdb redis.Scripter, prefix string, rule Rule, logger *slog.Logger) *RedisLimiter {
	return &RedisLimiter{rdb: rdb, prefix: prefix, rule: rule, fallback: NewLocal(rule), logger: logger, now: time.Now}
}

// Allow 实现 Limiter。
func (l *RedisLimiter) Allow(ctx context.Context, key string) (Decision, error) {
	res, err := tokenBucket.Run(ctx, l.rdb, []string{l.prefix + key}, l.rule.Rate, l.rule.Burst, l.now().UnixMilli(), 1).Slice()
	if err != nil || len(res) != 2 {
		// 降级而非放行：Redis 故障时仍保留单机维度的保护
		l.logger.WarnContext(ctx, "ratelimit: redis unavailable, falling back to local limiter", "err", err)
		return l.fallback.Allow(ctx, key)
	}
	allowed, _ := res[0].(int64)
	if allowed == 1 {
		return Decision{Allowed: true}, nil
	}
	s, _ := res[1].(string)
	tokens, _ := strconv.ParseFloat(s, 64)
	wait := time.Duration(math.Ceil((1-tokens)/l.rule.Rate*1000)) * time.Millisecond
	return Decision{Allowed: false, RetryAfter: wait}, nil
}

// Local 单机限流器。key 数量设上限，防止攻击者用海量随机 key 撑爆内存。
type Local struct {
	mu   sync.Mutex
	rule Rule
	m    map[string]*rate.Limiter
	max  int
}

// NewLocal 创建单机限流器。
func NewLocal(rule Rule) *Local {
	return &Local{rule: rule, m: make(map[string]*rate.Limiter), max: 100000}
}

// Allow 实现 Limiter。
func (l *Local) Allow(_ context.Context, key string) (Decision, error) {
	l.mu.Lock()
	lim, ok := l.m[key]
	if !ok {
		if len(l.m) >= l.max {
			clear(l.m)
		}
		lim = rate.NewLimiter(rate.Limit(l.rule.Rate), l.rule.Burst)
		l.m[key] = lim
	}
	l.mu.Unlock()
	if lim.Allow() {
		return Decision{Allowed: true}, nil
	}
	return Decision{Allowed: false, RetryAfter: time.Duration(float64(time.Second) / l.rule.Rate)}, nil
}
