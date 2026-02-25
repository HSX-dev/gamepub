package auth

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func newPair(t *testing.T) (*Issuer, *Verifier) {
	t.Helper()
	iss, err := NewIssuer(bytes.Repeat([]byte{1}, ed25519.SeedSize), "gamepub-account", "gamepub-api", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ver, err := NewVerifier(iss.PublicKey(), "gamepub-account", "gamepub-api")
	if err != nil {
		t.Fatal(err)
	}
	return iss, ver
}

func TestIssueVerify(t *testing.T) {
	iss, ver := newPair(t)
	tok, _, err := iss.Issue(42, []string{"player"})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := ver.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := claims.UserID(); id != 42 {
		t.Fatalf("uid = %d", id)
	}
}

func TestRejectsAlgNone(t *testing.T) {
	_, ver := newPair(t)
	claims := jwt.MapClaims{"sub": "1", "iss": "gamepub-account", "aud": "gamepub-api", "exp": time.Now().Add(time.Hour).Unix()}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ver.Verify(tok); err == nil {
		t.Fatal("alg=none token must be rejected")
	}
}

func TestRejectsHS256SignedWithPublicKey(t *testing.T) {
	// 算法混淆：攻击者用公钥当 HMAC 密钥签名，期望服务端误用公钥做 HS256 校验
	iss, ver := newPair(t)
	claims := jwt.MapClaims{"sub": "1", "iss": "gamepub-account", "aud": "gamepub-api", "exp": time.Now().Add(time.Hour).Unix()}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(iss.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ver.Verify(tok); err == nil {
		t.Fatal("HS256 token must be rejected")
	}
}

func TestRejectsExpiredAndWrongAudience(t *testing.T) {
	iss, ver := newPair(t)
	iss.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	expired, _, _ := iss.Issue(1, nil)
	if _, err := ver.Verify(expired); err == nil {
		t.Fatal("expired token must be rejected")
	}

	other, _ := NewIssuer(bytes.Repeat([]byte{1}, ed25519.SeedSize), "gamepub-account", "other-api", time.Hour)
	tok, _, _ := other.Issue(1, nil)
	if _, err := ver.Verify(tok); err == nil {
		t.Fatal("token for another audience must be rejected")
	}
}

func TestRejectsTampered(t *testing.T) {
	iss, ver := newPair(t)
	tok, _, _ := iss.Issue(1, []string{"player"})
	parts := strings.Split(tok, ".")
	parts[1] = parts[1][:len(parts[1])-2] + "AA"
	if _, err := ver.Verify(strings.Join(parts, ".")); err == nil {
		t.Fatal("tampered token must be rejected")
	}
}
