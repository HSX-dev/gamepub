package mq

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gamepub/pkg/logx"
)

type fakeReader struct {
	mu      sync.Mutex
	queue   []Message
	commits []int64
	events  []string
}

func (r *fakeReader) Fetch(ctx context.Context) (Message, error) {
	for {
		r.mu.Lock()
		if len(r.queue) > 0 {
			m := r.queue[0]
			r.queue = r.queue[1:]
			r.mu.Unlock()
			return m, nil
		}
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return Message{}, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (r *fakeReader) Commit(_ context.Context, msgs ...Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range msgs {
		r.commits = append(r.commits, m.Offset)
		r.events = append(r.events, "commit")
	}
	return nil
}

func (r *fakeReader) Close() error { return nil }

func (r *fakeReader) committed() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.commits...)
}

type fakeDLQ struct {
	mu       sync.Mutex
	failLeft int
	got      []OutMessage
	reader   *fakeReader
}

func (p *fakeDLQ) Publish(_ context.Context, msgs ...OutMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failLeft > 0 {
		p.failLeft--
		return errors.New("broker unavailable")
	}
	p.got = append(p.got, msgs...)
	p.reader.mu.Lock()
	p.reader.events = append(p.reader.events, "dlq")
	p.reader.mu.Unlock()
	return nil
}

func (p *fakeDLQ) Close() error { return nil }

func run(t *testing.T, msgs []Message, dlqFail int, h Handler, wantCommits int) (*fakeReader, *fakeDLQ) {
	t.Helper()
	r := &fakeReader{queue: msgs}
	dlq := &fakeDLQ{failLeft: dlqFail, reader: r}
	c := NewConsumer(ConsumerConfig{
		Name: "test", DLQTopic: "t.dlq", MaxAttempts: 3, HandlerTimeout: time.Second,
		Backoff: func(int) time.Duration { return time.Millisecond },
	}, r, dlq, h, logx.Discard())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	deadline := time.After(3 * time.Second)
	for len(r.committed()) < wantCommits {
		select {
		case <-deadline:
			cancel()
			t.Fatalf("timeout waiting for %d commits, got %v", wantCommits, r.committed())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return r, dlq
}

func TestRetryThenSucceed(t *testing.T) {
	calls := 0
	r, dlq := run(t, []Message{{Offset: 1}}, 0, func(context.Context, Message) error {
		calls++
		if calls < 3 {
			return errors.New("db timeout")
		}
		return nil
	}, 1)
	if calls != 3 || len(dlq.got) != 0 || r.committed()[0] != 1 {
		t.Fatalf("calls=%d dlq=%d commits=%v", calls, len(dlq.got), r.committed())
	}
}

func TestExhaustedRetriesGoToDLQ(t *testing.T) {
	calls := 0
	_, dlq := run(t, []Message{{Topic: "orders", Offset: 7, Value: []byte("x")}}, 0, func(context.Context, Message) error {
		calls++
		return errors.New("always failing")
	}, 1)
	if calls != 3 || len(dlq.got) != 1 {
		t.Fatalf("calls=%d dlq=%d", calls, len(dlq.got))
	}
	if h := dlq.got[0].Headers; h["x-dlq-source-offset"] != "7" || h["x-dlq-source-topic"] != "orders" {
		t.Fatalf("dlq headers missing source info: %v", h)
	}
}

func TestPermanentErrorAndPanicSkipRetry(t *testing.T) {
	calls := 0
	_, dlq := run(t, []Message{{Offset: 1}, {Offset: 2}}, 0, func(_ context.Context, m Message) error {
		calls++
		if m.Offset == 1 {
			return Permanent(errors.New("malformed payload"))
		}
		panic("nil pointer")
	}, 2)
	if calls != 2 || len(dlq.got) != 2 {
		t.Fatalf("permanent errors must not be retried: calls=%d dlq=%d", calls, len(dlq.got))
	}
}

func TestNoCommitBeforeDLQSucceeds(t *testing.T) {
	r, dlq := run(t, []Message{{Offset: 1}}, 2, func(context.Context, Message) error {
		return Permanent(errors.New("bad"))
	}, 1)
	if len(dlq.got) != 1 {
		t.Fatalf("dlq should eventually succeed, got %d", len(dlq.got))
	}
	if r.events[0] != "dlq" || r.events[1] != "commit" {
		t.Fatalf("offset committed before DLQ write: %v", r.events)
	}
}

func TestCancelDuringRetryDoesNotCommit(t *testing.T) {
	r := &fakeReader{queue: []Message{{Offset: 1}}}
	c := NewConsumer(ConsumerConfig{Name: "t", DLQTopic: "t.dlq", MaxAttempts: 5,
		Backoff: func(int) time.Duration { return time.Hour }}, r, &fakeDLQ{reader: r},
		func(context.Context, Message) error { return errors.New("fail") }, logx.Discard())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.committed()) != 0 {
		t.Fatalf("message must not be committed when shutting down mid-retry: %v", r.committed())
	}
}
