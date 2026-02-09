package mask

import (
	"strings"
	"testing"
)

func TestByKey(t *testing.T) {
	cases := []struct {
		key, in, want string
		sensitive     bool
	}{
		{"phone", "13812345678", "138****5678", true},
		{"Mobile", "13812345678", "138****5678", true},
		{"id_card", "110101199001011234", "110***********1234", true},
		{"card-no", "6222020000001234", "************1234", true},
		{"email", "alice@example.com", "a****@example.com", true},
		{"password", "P@ssw0rd!", "***", true},
		{"Authorization", "Bearer eyJhbGciOiJFZERTQSJ9.payload.sig", "Bearer eyJhbG***", true},
		{"token", "short", "***", true},
		{"nickname", "玩家一号", "玩家一号", false},
	}
	for _, c := range cases {
		got, sensitive := ByKey(c.key, c.in)
		if got != c.want || sensitive != c.sensitive {
			t.Errorf("ByKey(%q,%q) = (%q,%v), want (%q,%v)", c.key, c.in, got, sensitive, c.want, c.sensitive)
		}
	}
}

func TestMiddleShortValueFullyMasked(t *testing.T) {
	if got := Phone("1381"); got != "****" {
		t.Fatalf("short phone should be fully masked, got %q", got)
	}
}

func TestJSON(t *testing.T) {
	in := `{"order_no":"A1","phone":"13812345678","pay":{"card_no":"6222020000001234","cvv":123},"items":[{"token":"abcdefghijklmnopqrstuvwxyz"}]}`
	out := string(JSON([]byte(in)))
	for _, leak := range []string{"13812345678", "6222020000001234", "123}", "abcdefghijklmnopqrstuvwxyz"} {
		if strings.Contains(out, leak) {
			t.Fatalf("masked json still contains %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, `"order_no":"A1"`) {
		t.Fatalf("non-sensitive field should be kept: %s", out)
	}
	if got := string(JSON([]byte("not json phone=13812345678"))); strings.Contains(got, "1381234") {
		t.Fatalf("non-json body must be omitted, got %s", got)
	}
}

func TestText(t *testing.T) {
	in := `login failed phone=13812345678 Authorization: Bearer abc.def.ghi password="hunter2" api_key=sk-123`
	out := Text(in)
	for _, leak := range []string{"13812345678", "abc.def.ghi", "hunter2", "sk-123"} {
		if strings.Contains(out, leak) {
			t.Fatalf("Text() leaked %q: %s", leak, out)
		}
	}
}
