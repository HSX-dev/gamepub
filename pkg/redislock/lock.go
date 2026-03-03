// Package redislock 基于 Redis 的分布式锁（单实例语义）。
//
// 审计中的典型误用：
//   - SETNX 与 EXPIRE 分两步执行，进程在中间崩溃导致死锁 → 用 SET NX PX 原子完成；
//   - 释放锁直接 DEL：业务执行超过 TTL 后锁已被他人持有，DEL 会误删他人的锁 → 用随机 token + Lua 比对删除；
//   - 长任务不续期 → 提供 Refresh，由持有者在执行过程中续约；
//   - 把锁当成正确性的唯一保障 → 锁只用于“减少并发”，资金正确性仍依赖数据库唯一约束与条件更新。
package redislock

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"gamepub/pkg/cryptox"
)

var (
	// ErrNotAcquired 锁已被他人持有。
	ErrNotAcquired = errors.New("redislock: not acquired")
	// ErrLockLost 锁已过期或被他人持有，当前持有者不应继续执行临界区操作。
	ErrLockLost = errors.New("redislock: lock no longer held")
)

var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0`)

var refreshScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0`)

// Locker 锁工厂。
type Locker struct{ rdb redis.UniversalClient }

// New 创建 Locker。
func New(rdb redis.UniversalClient) *Locker { return &Locker{rdb: rdb} }

// Lock 已持有的锁。
type Lock struct {
	rdb   redis.UniversalClient
	key   string
	token string
}

// TryLock 非阻塞加锁。
func (l *Locker) TryLock(ctx context.Context, key string, ttl time.Duration) (*Lock, error) {
	token := cryptox.RandomHex(16)
	ok, err := l.rdb.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotAcquired
	}
	return &Lock{rdb: l.rdb, key: key, token: token}, nil
}

// Unlock 仅释放自己持有的锁。
func (lk *Lock) Unlock(ctx context.Context) error {
	n, err := releaseScript.Run(ctx, lk.rdb, []string{lk.key}, lk.token).Int64()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLockLost
	}
	return nil
}

// Refresh 续期；返回 ErrLockLost 时调用方必须立即停止临界区操作。
func (lk *Lock) Refresh(ctx context.Context, ttl time.Duration) error {
	n, err := refreshScript.Run(ctx, lk.rdb, []string{lk.key}, lk.token, ttl.Milliseconds()).Int64()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLockLost
	}
	return nil
}
