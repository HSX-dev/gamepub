// Package signature 回调验签与防重放：HMAC-SHA256(timestamp \n nonce \n body) + 时间窗口 + nonce 去重。
//
// 用于支付渠道异步通知、平台向游戏服下发的发货回调。审计要点：
//   - 必须对原始报文验签，而不是对反序列化后再序列化的结构体；
//   - 比较签名必须常量时间（hmac.Equal），防止时序侧信道；
//   - 只验签不防重放，攻击者可截获一次合法通知反复投递；
//   - nonce 应在验签通过后再占用，否则伪造请求能提前“烧掉”合法 nonce。
package signature

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	ErrMissing = errors.New("signature: missing signature headers")
	ErrExpired = errors.New("signature: timestamp outside allowed window")
	ErrInvalid = errors.New("signature: invalid signature")
	ErrReplay  = errors.New("signature: nonce already used")
)

// 标准头部名。
const (
	HeaderTimestamp = "X-Signature-Timestamp"
	HeaderNonce     = "X-Signature-Nonce"
	HeaderSignature = "X-Signature"
)

func mac(secret []byte, timestamp, nonce string, body []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(timestamp))
	m.Write([]byte{'\n'})
	m.Write([]byte(nonce))
	m.Write([]byte{'\n'})
	m.Write(body)
	return m.Sum(nil)
}

// Sign 计算签名（十六进制）。
func Sign(secret []byte, timestamp, nonce string, body []byte) string {
	return hex.EncodeToString(mac(secret, timestamp, nonce, body))
}

// NonceStore nonce 去重存储；Claim 返回 true 表示首次出现。
type NonceStore interface {
	Claim(ctx context.Context, key string, ttl time.Duration) (bool, error)
	Release(ctx context.Context, key string) error
}

// Verifier 验签器。
type Verifier struct {
	scope  string
	secret []byte
	window time.Duration
	store  NonceStore
	now    func() time.Time
}

// NewVerifier scope 用于隔离不同渠道的 nonce 空间。
func NewVerifier(scope string, secret []byte, window time.Duration, store NonceStore) *Verifier {
	return &Verifier{scope: scope, secret: secret, window: window, store: store, now: time.Now}
}

// Verify 校验签名、时间窗口与 nonce。
func (v *Verifier) Verify(ctx context.Context, timestamp, nonce, sig string, body []byte) error {
	if timestamp == "" || nonce == "" || sig == "" {
		return ErrMissing
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrInvalid
	}
	if d := v.now().Sub(time.Unix(ts, 0)); d > v.window || d < -v.window {
		return ErrExpired
	}
	if len(nonce) < 8 || len(nonce) > 64 {
		return ErrInvalid
	}
	got, err := hex.DecodeString(sig)
	if err != nil || !hmac.Equal(got, mac(v.secret, timestamp, nonce, body)) {
		return ErrInvalid
	}
	// nonce TTL 覆盖整个时间窗口的两倍：窗口外的旧请求已被时间戳拦截
	first, err := v.store.Claim(ctx, v.scope+":"+nonce, 2*v.window)
	if err != nil {
		// 去重存储不可用时拒绝（fail closed）：资金类回调宁可让渠道重试，也不能放过重放
		return fmt.Errorf("signature: nonce store unavailable: %w", err)
	}
	if !first {
		return ErrReplay
	}
	return nil
}

// Release 业务处理失败时释放 nonce。支付渠道的重试通常原样重发报文（nonce 不变），
// 若不释放，合法重试会被当成重放拒绝，导致“用户已付款、订单永远不到账”。
func (v *Verifier) Release(ctx context.Context, nonce string) error {
	return v.store.Release(ctx, v.scope+":"+nonce)
}

// RedisNonceStore 基于 SET NX PX 的 nonce 存储，多副本共享。
type RedisNonceStore struct {
	rdb    redis.UniversalClient
	prefix string
}

// NewRedisNonceStore 创建 Redis nonce 存储。
func NewRedisNonceStore(rdb redis.UniversalClient, prefix string) *RedisNonceStore {
	return &RedisNonceStore{rdb: rdb, prefix: prefix}
}

// Claim 实现 NonceStore。
func (s *RedisNonceStore) Claim(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return s.rdb.SetNX(ctx, s.prefix+key, 1, ttl).Result()
}

// Release 实现 NonceStore。
func (s *RedisNonceStore) Release(ctx context.Context, key string) error {
	return s.rdb.Del(ctx, s.prefix+key).Err()
}

// MemoryNonceStore 单机内存实现，仅用于测试与本地开发。
type MemoryNonceStore struct {
	mu sync.Mutex
	m  map[string]time.Time
}

// NewMemoryNonceStore 创建内存 nonce 存储。
func NewMemoryNonceStore() *MemoryNonceStore { return &MemoryNonceStore{m: make(map[string]time.Time)} }

// Claim 实现 NonceStore。
func (s *MemoryNonceStore) Claim(_ context.Context, key string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if exp, ok := s.m[key]; ok && now.Before(exp) {
		return false, nil
	}
	s.m[key] = now.Add(ttl)
	return true, nil
}

// Release 实现 NonceStore。
func (s *MemoryNonceStore) Release(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return nil
}
