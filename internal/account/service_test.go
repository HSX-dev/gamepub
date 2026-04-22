package account

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"gamepub/internal/testkit"
	"gamepub/pkg/auth"
	"gamepub/pkg/cryptox"
	"gamepub/pkg/logx"
)

type env struct {
	svc *Service
	db  *gorm.DB
	mr  *miniredis.Miniredis
	ver *auth.Verifier
}

func setup(t *testing.T) *env {
	t.Helper()
	db := testkit.NewDB(t, &User{})
	mr, rdb := testkit.NewRedis(t)
	iss, _ := auth.NewIssuer(bytes.Repeat([]byte{1}, ed25519.SeedSize), "gamepub-account", "gamepub-api", time.Hour)
	ver, _ := auth.NewVerifier(iss.PublicKey(), "gamepub-account", "gamepub-api")
	sealer, _ := cryptox.NewSealer(bytes.Repeat([]byte{2}, 32))
	svc, err := NewService(Options{
		DB: db, Redis: rdb, Issuer: iss, Sealer: sealer, PhonePepper: []byte("pepper-0123456789"),
		BcryptCost: bcrypt.MinCost, Logger: logx.Discard(),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.cache.doubleDelay = 10 * time.Millisecond
	return &env{svc: svc, db: db, mr: mr, ver: ver}
}

func TestRegisterStoresNoPlaintextPhone(t *testing.T) {
	e := setup(t)
	id, err := e.svc.Register(context.Background(), "13812345678", "Passw0rd!", "")
	if err != nil {
		t.Fatal(err)
	}
	var u User
	e.db.Take(&u, id)
	for _, col := range []string{u.PhoneHash, u.PhoneEnc, u.PhoneMasked, u.PasswordHash} {
		if strings.Contains(col, "13812345678") {
			t.Fatalf("plaintext phone persisted: %+v", u)
		}
	}
	if u.PhoneMasked != "138****5678" || u.Roles != "player" {
		t.Fatalf("unexpected user: %+v", u)
	}
	if _, err := e.svc.Register(context.Background(), "13812345678", "Passw0rd!", ""); !errors.Is(err, ErrPhoneTaken) {
		t.Fatalf("duplicate phone: %v", err)
	}
}

func TestRegisterValidation(t *testing.T) {
	e := setup(t)
	cases := []struct {
		phone, pw, nick string
		want            error
	}{
		{"1381234567", "Passw0rd!", "", ErrInvalidPhone},
		{"13812345678' OR '1'='1", "Passw0rd!", "", ErrInvalidPhone},
		{"13812345678", "short1", "", ErrWeakPassword},
		{"13812345678", "allletters", "", ErrWeakPassword},
		{"13812345678", strings.Repeat("a1", 40), "", ErrWeakPassword}, // > 72 字节会被 bcrypt 截断
		{"13812345678", "Passw0rd!", "<script>alert(1)</script>", ErrInvalidNickname},
		{"13812345678", "Passw0rd!", "bad\x00name", ErrInvalidNickname},
	}
	for _, c := range cases {
		if _, err := e.svc.Register(context.Background(), c.phone, c.pw, c.nick); !errors.Is(err, c.want) {
			t.Errorf("Register(%q,%q,%q) = %v, want %v", c.phone, c.pw, c.nick, err, c.want)
		}
	}
}

func TestLogin(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id, _ := e.svc.Register(ctx, "13812345678", "Passw0rd!", "")

	tok, _, err := e.svc.Login(ctx, "13812345678", "Passw0rd!")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := e.ver.Verify(tok)
	if uid, _ := claims.UserID(); err != nil || uid != id {
		t.Fatalf("token uid mismatch: %v", err)
	}

	// 口令错误与用户不存在返回同一错误，不暴露手机号是否注册
	_, _, errWrong := e.svc.Login(ctx, "13812345678", "WrongPass1")
	_, _, errNone := e.svc.Login(ctx, "13900000000", "Passw0rd!")
	if !errors.Is(errWrong, ErrInvalidCredentials) || !errors.Is(errNone, ErrInvalidCredentials) {
		t.Fatalf("errWrong=%v errNone=%v", errWrong, errNone)
	}

	e.db.Model(&User{}).Where("id = ?", id).Update("status", StatusDisabled)
	if _, _, err := e.svc.Login(ctx, "13812345678", "Passw0rd!"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("disabled user must not log in: %v", err)
	}
}

func TestProfileCacheInvalidationAndPenetration(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id, _ := e.svc.Register(ctx, "13812345678", "Passw0rd!", "旧昵称")

	p, _ := e.svc.GetProfile(ctx, id)
	if p.Nickname != "旧昵称" || !e.mr.Exists(cacheKey(id)) {
		t.Fatal("profile should be cached after first read")
	}
	if cached, _ := e.mr.Get(cacheKey(id)); strings.Contains(cached, "password") || strings.Contains(cached, "phone_enc") {
		t.Fatalf("sensitive fields cached: %s", cached)
	}

	p, err := e.svc.UpdateProfile(ctx, id, "新昵称", "")
	if err != nil || p.Nickname != "新昵称" {
		t.Fatalf("update: %+v %v", p, err)
	}

	// 不存在的用户：写入空值标记，后续请求不再打到数据库
	if _, err := e.svc.GetProfile(ctx, 999); !errors.Is(err, ErrUserNotFound) {
		t.Fatal(err)
	}
	if v, _ := e.mr.Get(cacheKey(999)); v != nullMarker {
		t.Fatalf("null marker not cached: %q", v)
	}
}

func TestSingleflightCollapsesConcurrentMisses(t *testing.T) {
	e := setup(t)
	var loads atomic.Int32
	load := func(context.Context) (*Profile, error) {
		loads.Add(1)
		time.Sleep(50 * time.Millisecond)
		return &Profile{ID: 1, Nickname: "hot"}, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.svc.cache.get(context.Background(), 1, load); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := loads.Load(); n != 1 {
		t.Fatalf("expected 1 db load for 50 concurrent misses, got %d", n)
	}
}

func TestAvatarMustBelongToUser(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id, _ := e.svc.Register(ctx, "13812345678", "Passw0rd!", "")
	for _, key := range []string{"avatars/2/abc.png", "../etc/passwd", "avatars/1/../2/x.png", "https://evil.example/x.png"} {
		if _, err := e.svc.UpdateProfile(ctx, id, "", key); !errors.Is(err, ErrInvalidAvatar) {
			t.Errorf("avatar %q: got %v", key, err)
		}
	}
}

func TestGrantRole(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id, _ := e.svc.Register(ctx, "13812345678", "Passw0rd!", "")
	if err := e.svc.GrantRole(ctx, "13812345678", "superuser"); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("unknown role: %v", err)
	}
	_, _ = e.svc.GetProfile(ctx, id) // 预热缓存
	if err := e.svc.GrantRole(ctx, "13812345678", "finance"); err != nil {
		t.Fatal(err)
	}
	p, _ := e.svc.GetProfile(ctx, id)
	if strings.Join(p.Roles, ",") != "player,finance" {
		t.Fatalf("roles not refreshed after grant (stale cache?): %v", p.Roles)
	}
}
