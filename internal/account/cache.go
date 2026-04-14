package account

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2" // nosemgrep: math-random-used -- 仅用于 TTL 抖动，不涉及安全
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

const nullMarker = "null"

// profileCache Cache-Aside 用户资料缓存。
//
// 审计关注点与对应处理：
//   - 缓存击穿（热点 key 过期瞬间大量回源）→ singleflight 合并同 key 的并发回源；
//   - 缓存穿透（查询不存在的用户）→ 缓存空值标记，短 TTL；
//   - 缓存雪崩（大量 key 同时过期）→ TTL 加随机抖动；
//   - 一致性：更新时“先写库、再删缓存”，并延迟二次删除，覆盖“删缓存后、写库前有旧值回填”的竞态；
//   - Redis 故障 → 直接回源数据库，缓存只是加速层，不能成为单点。
//   - 只缓存 Profile，不缓存口令哈希与手机号密文。
type profileCache struct {
	rdb         redis.UniversalClient
	ttl         time.Duration
	nullTTL     time.Duration
	doubleDelay time.Duration
	sf          singleflight.Group
	logger      *slog.Logger
}

func newProfileCache(rdb redis.UniversalClient, logger *slog.Logger) *profileCache {
	return &profileCache{rdb: rdb, ttl: 10 * time.Minute, nullTTL: 30 * time.Second, doubleDelay: 500 * time.Millisecond, logger: logger}
}

func cacheKey(id uint64) string { return "account:profile:" + strconv.FormatUint(id, 10) }

// get 读取资料；load 为回源函数，返回 ErrUserNotFound 时写入空值标记。
func (c *profileCache) get(ctx context.Context, id uint64, load func(context.Context) (*Profile, error)) (*Profile, error) {
	key := cacheKey(id)
	val, err := c.rdb.Get(ctx, key).Result()
	switch {
	case err == nil && val == nullMarker:
		return nil, ErrUserNotFound
	case err == nil:
		var p Profile
		if json.Unmarshal([]byte(val), &p) == nil {
			return &p, nil
		}
	case !errors.Is(err, redis.Nil):
		c.logger.WarnContext(ctx, "account cache: redis get failed, falling back to db", "err", err)
		return load(ctx)
	}

	v, err, _ := c.sf.Do(key, func() (any, error) {
		// 与发起者的 ctx 解耦：否则第一个请求被取消会让所有合并等待的请求一起失败
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		p, err := load(lctx)
		switch {
		case errors.Is(err, ErrUserNotFound):
			c.rdb.Set(lctx, key, nullMarker, c.nullTTL)
		case err == nil:
			if data, mErr := json.Marshal(p); mErr == nil {
				c.rdb.Set(lctx, key, data, c.jitter(c.ttl))
			}
		}
		return p, err
	})
	if err != nil {
		return nil, err
	}
	p, _ := v.(*Profile)
	return p, nil
}

// invalidate 写库成功后调用：立即删除 + 延迟二次删除。
func (c *profileCache) invalidate(ctx context.Context, id uint64) {
	key := cacheKey(id)
	if err := c.rdb.Del(ctx, key).Err(); err != nil {
		c.logger.WarnContext(ctx, "account cache: delete failed", "user_id", id, "err", err)
	}
	bg := context.WithoutCancel(ctx)
	time.AfterFunc(c.doubleDelay, func() {
		dctx, cancel := context.WithTimeout(bg, time.Second)
		defer cancel()
		c.rdb.Del(dctx, key)
	})
}

func (c *profileCache) jitter(d time.Duration) time.Duration {
	return d + time.Duration(rand.Int64N(int64(d/5))) // #nosec G404 -- TTL 抖动，不涉及安全
}
