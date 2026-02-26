package signature

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

var secret = []byte("test-channel-secret-0123456789")

func signed(body string, at time.Time, nonce string) (string, string, string, []byte) {
	ts := strconv.FormatInt(at.Unix(), 10)
	return ts, nonce, Sign(secret, ts, nonce, []byte(body)), []byte(body)
}

func TestVerify(t *testing.T) {
	ctx := context.Background()
	v := NewVerifier("mockpay", secret, 5*time.Minute, NewMemoryNonceStore())
	now := time.Now()

	ts, nonce, sig, body := signed(`{"order_no":"A1","amount":600}`, now, "nonce-0001")
	if err := v.Verify(ctx, ts, nonce, sig, body); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if err := v.Verify(ctx, ts, nonce, sig, body); !errors.Is(err, ErrReplay) {
		t.Fatalf("replayed request: got %v, want ErrReplay", err)
	}

	ts, nonce, sig, _ = signed(`{"order_no":"A1","amount":600}`, now, "nonce-0002")
	if err := v.Verify(ctx, ts, nonce, sig, []byte(`{"order_no":"A1","amount":1}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tampered body: got %v, want ErrInvalid", err)
	}

	ts, nonce, sig, body = signed(`{}`, now.Add(-10*time.Minute), "nonce-0003")
	if err := v.Verify(ctx, ts, nonce, sig, body); !errors.Is(err, ErrExpired) {
		t.Fatalf("stale request: got %v, want ErrExpired", err)
	}

	if err := v.Verify(ctx, "", "", "", nil); !errors.Is(err, ErrMissing) {
		t.Fatalf("missing headers: got %v", err)
	}
}

func TestForgedRequestDoesNotBurnNonce(t *testing.T) {
	ctx := context.Background()
	v := NewVerifier("mockpay", secret, 5*time.Minute, NewMemoryNonceStore())
	ts, nonce, sig, body := signed(`{"ok":true}`, time.Now(), "nonce-race")

	if err := v.Verify(ctx, ts, nonce, "deadbeef", body); !errors.Is(err, ErrInvalid) {
		t.Fatalf("forged signature: got %v", err)
	}
	if err := v.Verify(ctx, ts, nonce, sig, body); err != nil {
		t.Fatalf("legit request after forged one must pass: %v", err)
	}
}

type failingStore struct{}

func (failingStore) Claim(context.Context, string, time.Duration) (bool, error) {
	return false, errors.New("redis down")
}

func (failingStore) Release(context.Context, string) error { return errors.New("redis down") }

func TestNonceStoreFailureFailsClosed(t *testing.T) {
	v := NewVerifier("mockpay", secret, 5*time.Minute, failingStore{})
	ts, nonce, sig, body := signed(`{}`, time.Now(), "nonce-0004")
	if err := v.Verify(context.Background(), ts, nonce, sig, body); err == nil {
		t.Fatal("must fail closed when nonce store is unavailable")
	}
}
