// Package resilience 第三方/下游 HTTP 调用的统一治理：超时、重试、熔断、响应体上限。
//
// 审计中常见的“下游拖垮主链路”问题：无超时的 http.DefaultClient、对非幂等请求盲目重试、
// 全局一个熔断器导致一个下游故障波及所有下游、io.ReadAll 读取无上限的响应体。
package resilience

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2" // nosemgrep: math-random-used -- 仅用于重试退避抖动
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"
)

// ErrCircuitOpen 熔断器处于打开状态，请求被快速失败。
var ErrCircuitOpen = errors.New("resilience: circuit open")

// Response 已读取完毕的响应（Body 受 MaxBodyBytes 限制）。
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// StatusError 下游返回可重试的失败状态码（5xx / 429）。
type StatusError struct{ Resp *Response }

func (e *StatusError) Error() string {
	return fmt.Sprintf("resilience: upstream status %d", e.Resp.StatusCode)
}

// Config 治理参数。
type Config struct {
	MaxAttempts        int           // 幂等请求的最大尝试次数（含首次）
	PerTryTimeout      time.Duration // 单次尝试超时；整体仍受调用方 ctx 约束
	BaseBackoff        time.Duration
	MaxBackoff         time.Duration
	MaxBodyBytes       int64
	BreakerFailures    uint32        // 连续失败多少次后熔断
	BreakerOpenTimeout time.Duration // 熔断持续多久后进入半开探测
}

// DefaultConfig 默认参数。
func DefaultConfig() Config {
	return Config{
		MaxAttempts:        3,
		PerTryTimeout:      3 * time.Second,
		BaseBackoff:        100 * time.Millisecond,
		MaxBackoff:         time.Second,
		MaxBodyBytes:       1 << 20,
		BreakerFailures:    5,
		BreakerOpenTimeout: 30 * time.Second,
	}
}

// Client 带治理能力的 HTTP 客户端。熔断按目标 host 隔离，一个游戏的回调地址故障不影响其它游戏。
type Client struct {
	http     *http.Client
	cfg      Config
	mu       sync.Mutex
	breakers map[string]*gobreaker.CircuitBreaker[*Response]
}

// New 包装底层 http.Client（生产中应传入 netguard 生成的受保护客户端）。
func New(hc *http.Client, cfg Config) *Client {
	return &Client{http: hc, cfg: cfg, breakers: make(map[string]*gobreaker.CircuitBreaker[*Response])}
}

func (c *Client) breaker(host string) *gobreaker.CircuitBreaker[*Response] {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cb, ok := c.breakers[host]; ok {
		return cb
	}
	threshold := c.cfg.BreakerFailures
	cb := gobreaker.NewCircuitBreaker[*Response](gobreaker.Settings{
		Name:        host,
		MaxRequests: 1,
		Interval:    time.Minute,
		Timeout:     c.cfg.BreakerOpenTimeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool { return counts.ConsecutiveFailures >= threshold },
	})
	c.breakers[host] = cb
	return cb
}

// Request 出站请求。
type Request struct {
	Method string
	URL    string
	Body   []byte
	Header http.Header
	// Idempotent=false 时绝不重试，避免重复下单/重复发货；需要重试的写请求应携带幂等键
	// （如 Idempotency-Key 头）并由调用方显式声明。
	Idempotent bool
	// Prepare 每次尝试发出前调用，用于逐次签名：带 nonce 的签名请求如果原样重试，
	// 会被接收方的防重放校验拒绝。
	Prepare func(*http.Request)
}

// Do 发送请求。
func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	u, err := url.Parse(r.URL)
	if err != nil {
		return nil, fmt.Errorf("resilience: parse url: %w", err)
	}
	cb := c.breaker(u.Host)
	attempts := 1
	if r.Idempotent && c.cfg.MaxAttempts > 1 {
		attempts = c.cfg.MaxAttempts
	}

	var (
		last    *Response
		lastErr error
	)
	for i := 0; i < attempts; i++ {
		if i > 0 {
			if err := sleep(ctx, c.backoff(i)); err != nil {
				return last, err
			}
		}
		resp, err := cb.Execute(func() (*Response, error) { return c.once(ctx, r) })
		if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
			return nil, fmt.Errorf("%w: %s", ErrCircuitOpen, u.Host)
		}
		if err == nil {
			return resp, nil
		}
		last, lastErr = resp, err
		if ctx.Err() != nil {
			return last, ctx.Err()
		}
	}
	return last, lastErr
}

func (c *Client) once(ctx context.Context, r Request) (*Response, error) {
	tctx, cancel := context.WithTimeout(ctx, c.cfg.PerTryTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(tctx, r.Method, r.URL, bytes.NewReader(r.Body))
	if err != nil {
		return nil, err
	}
	if r.Header != nil {
		req.Header = r.Header.Clone()
	}
	if r.Prepare != nil {
		r.Prepare(req)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxBodyBytes))
	if err != nil {
		return nil, err
	}
	out := &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: data}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return out, &StatusError{Resp: out}
	}
	// 4xx 属于调用方问题，重试无意义，也不计入熔断
	return out, nil
}

// backoff 指数退避 + 全抖动，避免大量客户端同时重试形成“重试风暴”。
func (c *Client) backoff(attempt int) time.Duration {
	d := c.cfg.BaseBackoff << (attempt - 1)
	if d <= 0 || d > c.cfg.MaxBackoff {
		d = c.cfg.MaxBackoff
	}
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d))) // #nosec G404 -- 退避抖动无需密码学随机数
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
