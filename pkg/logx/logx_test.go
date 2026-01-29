package logx

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestSensitiveFieldsMasked(t *testing.T) {
	var buf bytes.Buffer
	log := NewWithWriter(&buf, "test", "info").With("token", "eyJhbGciOiJFZERTQSJ9.long.token")
	ctx := WithRequestID(context.Background(), "req-1")
	log.InfoContext(ctx, "login", "phone", "13812345678", "password", "hunter2", "user_id", 42)

	out := buf.String()
	for _, leak := range []string{"13812345678", "hunter2", "long.token"} {
		if strings.Contains(out, leak) {
			t.Fatalf("log leaked %q: %s", leak, out)
		}
	}
	for _, want := range []string{`"phone":"138****5678"`, `"request_id":"req-1"`, `"user_id":42`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %s: %s", want, out)
		}
	}
}
