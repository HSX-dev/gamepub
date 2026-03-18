package resilience

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.BaseBackoff = time.Millisecond
	cfg.MaxBackoff = 2 * time.Millisecond
	cfg.PerTryTimeout = time.Second
	return cfg
}

func get(url string) Request { return Request{Method: http.MethodGet, URL: url, Idempotent: true} }

func TestRetryOn5xxThenSuccess(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := New(srv.Client(), testConfig())
	resp, err := c.Do(context.Background(), Request{Method: http.MethodPost, URL: srv.URL, Idempotent: true})
	if err != nil || resp.StatusCode != http.StatusOK || hits.Load() != 3 {
		t.Fatalf("resp=%v err=%v hits=%d", resp, err, hits.Load())
	}
}

func TestPrepareRunsPerAttempt(t *testing.T) {
	var (
		mu     sync.Mutex
		nonces = map[string]bool{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		n := r.Header.Get("X-Nonce")
		if nonces[n] {
			w.WriteHeader(http.StatusUnauthorized) // 接收方防重放
			return
		}
		nonces[n] = true
		if len(nonces) < 3 {
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer srv.Close()

	var seq atomic.Int32
	c := New(srv.Client(), testConfig())
	resp, err := c.Do(context.Background(), Request{
		Method: http.MethodPost, URL: srv.URL, Idempotent: true,
		Prepare: func(r *http.Request) { r.Header.Set("X-Nonce", strconv.Itoa(int(seq.Add(1)))) },
	})
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("each retry must be re-signed with a fresh nonce: resp=%v err=%v", resp, err)
	}
}

func TestNoRetryForNonIdempotentOr4xx(t *testing.T) {
	var hits atomic.Int32
	var status atomic.Int32
	status.Store(http.StatusServiceUnavailable)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	c := New(srv.Client(), testConfig())

	if _, err := c.Do(context.Background(), Request{Method: http.MethodPost, URL: srv.URL}); err == nil || hits.Load() != 1 {
		t.Fatalf("non-idempotent request must not retry: hits=%d err=%v", hits.Load(), err)
	}

	hits.Store(0)
	status.Store(http.StatusBadRequest)
	resp, err := c.Do(context.Background(), get(srv.URL))
	if err != nil || resp.StatusCode != http.StatusBadRequest || hits.Load() != 1 {
		t.Fatalf("4xx must not retry: hits=%d err=%v", hits.Load(), err)
	}
}

func TestBreakerOpensPerHost(t *testing.T) {
	var hits atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer good.Close()

	cfg := testConfig()
	cfg.MaxAttempts = 1
	cfg.BreakerFailures = 2
	cfg.BreakerOpenTimeout = time.Minute
	c := New(&http.Client{Timeout: time.Second}, cfg)

	for i := 0; i < 2; i++ {
		_, _ = c.Do(context.Background(), get(bad.URL))
	}
	if _, err := c.Do(context.Background(), get(bad.URL)); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("open breaker must fail fast without hitting upstream, hits=%d", hits.Load())
	}
	if _, err := c.Do(context.Background(), get(good.URL)); err != nil {
		t.Fatalf("breaker of another host must stay closed: %v", err)
	}
}

func TestBodyLimitAndTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		_, _ = w.Write(bytes.Repeat([]byte("a"), 1<<20))
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxBodyBytes = 1024
	cfg.PerTryTimeout = 50 * time.Millisecond
	cfg.MaxAttempts = 1
	c := New(srv.Client(), cfg)

	resp, err := c.Do(context.Background(), get(srv.URL))
	if err != nil || len(resp.Body) != 1024 {
		t.Fatalf("body should be truncated to 1024 bytes: err=%v", err)
	}
	start := time.Now()
	if _, err := c.Do(context.Background(), get(srv.URL+"/slow")); err == nil {
		t.Fatal("slow upstream should time out")
	}
	if time.Since(start) > time.Second {
		t.Fatal("per-try timeout not enforced")
	}
}
