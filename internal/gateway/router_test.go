package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountv1 "gamepub/api/gen/account/v1"
	orderv1 "gamepub/api/gen/order/v1"
	taskv1 "gamepub/api/gen/task/v1"
	walletv1 "gamepub/api/gen/wallet/v1"
	"gamepub/pkg/auth"
	"gamepub/pkg/logx"
	"gamepub/pkg/ratelimit"
	"gamepub/pkg/upload"
)

// ---- fakes：嵌入接口，只实现测试关心的方法 ----

type fakeAccount struct {
	accountv1.AccountServiceClient
	mu      sync.Mutex
	users   map[uint64]*accountv1.User
	updates []*accountv1.UpdateProfileRequest
}

func (f *fakeAccount) GetUser(_ context.Context, in *accountv1.GetUserRequest, _ ...grpc.CallOption) (*accountv1.GetUserResponse, error) {
	u, ok := f.users[in.GetUserId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "user not found")
	}
	return &accountv1.GetUserResponse{User: u}, nil
}

func (f *fakeAccount) UpdateProfile(_ context.Context, in *accountv1.UpdateProfileRequest, _ ...grpc.CallOption) (*accountv1.UpdateProfileResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, in)
	return &accountv1.UpdateProfileResponse{User: f.users[in.GetUserId()]}, nil
}

func (f *fakeAccount) Login(context.Context, *accountv1.LoginRequest, ...grpc.CallOption) (*accountv1.LoginResponse, error) {
	return nil, status.Error(codes.Unauthenticated, "invalid phone or password")
}

type fakeOrder struct {
	orderv1.OrderServiceClient
	mu       sync.Mutex
	lastUser uint64
}

func (f *fakeOrder) record(uid uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastUser = uid
}

func (f *fakeOrder) GetOrder(_ context.Context, in *orderv1.GetOrderRequest, _ ...grpc.CallOption) (*orderv1.GetOrderResponse, error) {
	f.record(in.GetUserId())
	return &orderv1.GetOrderResponse{Order: &orderv1.Order{OrderNo: in.GetOrderNo(), UserId: in.GetUserId()}}, nil
}

func (f *fakeOrder) CreateOrder(_ context.Context, in *orderv1.CreateOrderRequest, _ ...grpc.CallOption) (*orderv1.CreateOrderResponse, error) {
	f.record(in.GetUserId())
	return &orderv1.CreateOrderResponse{Order: &orderv1.Order{UserId: in.GetUserId()}}, nil
}

func (f *fakeOrder) AdminListOrders(context.Context, *orderv1.AdminListOrdersRequest, ...grpc.CallOption) (*orderv1.AdminListOrdersResponse, error) {
	return &orderv1.AdminListOrdersResponse{}, nil
}

func (f *fakeOrder) HandlePayNotify(_ context.Context, in *orderv1.HandlePayNotifyRequest, _ ...grpc.CallOption) (*orderv1.HandlePayNotifyResponse, error) {
	if in.GetHeaders()["X-Signature"] == "" {
		return nil, status.Error(codes.Unauthenticated, "rejected")
	}
	return &orderv1.HandlePayNotifyResponse{Reply: "success"}, nil
}

type fakeWallet struct {
	walletv1.WalletServiceClient
	err error
}

func (f *fakeWallet) GetBalance(context.Context, *walletv1.GetBalanceRequest, ...grpc.CallOption) (*walletv1.GetBalanceResponse, error) {
	return nil, f.err
}

type fakeTask struct{ taskv1.TaskServiceClient }

// ---- 测试环境 ----

const (
	uidPlayer   = 1
	uidCS       = 2
	uidFinance  = 3
	uidDisabled = 4
	uidOps      = 5
)

type testEnv struct {
	router  *gin.Engine
	issuer  *auth.Issuer
	account *fakeAccount
	order   *fakeOrder
	wallet  *fakeWallet
	storage string
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	iss, _ := auth.NewIssuer(bytes.Repeat([]byte{9}, ed25519.SeedSize), "gamepub-account", "gamepub-api", time.Hour)
	ver, _ := auth.NewVerifier(iss.PublicKey(), "gamepub-account", "gamepub-api")
	acc := &fakeAccount{users: map[uint64]*accountv1.User{
		uidPlayer:   {UserId: uidPlayer, Roles: []string{"player"}, Status: "active"},
		uidCS:       {UserId: uidCS, Roles: []string{"player", "cs"}, Status: "active"},
		uidFinance:  {UserId: uidFinance, Roles: []string{"finance"}, Status: "active"},
		uidDisabled: {UserId: uidDisabled, Roles: []string{"admin"}, Status: "disabled"},
		uidOps:      {UserId: uidOps, Roles: []string{"ops"}, Status: "active"},
	}}
	generous := ratelimit.NewLocal(ratelimit.Rule{Rate: 1000, Burst: 1000})
	dir := t.TempDir()
	e := &testEnv{issuer: iss, account: acc, order: &fakeOrder{}, wallet: &fakeWallet{}, storage: dir}
	r, err := NewRouter(Deps{
		Account: acc, Wallet: e.wallet, Order: e.order, Task: &fakeTask{}, Verifier: ver,
		Limiters: Limiters{
			Login: ratelimit.NewLocal(ratelimit.Rule{Rate: 0.001, Burst: 3}), User: generous, Batch: generous, Notify: generous,
		},
		Storage: upload.LocalStorage{Root: dir},
		Logger:  logx.Discard(),
	}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	e.router = r
	return e
}

func (e *testEnv) token(t *testing.T, uid uint64, roles ...string) string {
	t.Helper()
	tok, _, err := e.issuer.Issue(uid, roles)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (e *testEnv) do(method, path, token string, body []byte, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func concrete(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, ":") {
			parts[i] = "x"
		}
	}
	return strings.Join(parts, "/")
}

// ---- 用例 ----

// TestEveryNonPublicRouteRequiresAuth 鉴权中间件遗漏的“回归网”：新增任何路由，只要不在白名单，未登录必须 401。
func TestEveryNonPublicRouteRequiresAuth(t *testing.T) {
	e := newEnv(t)
	routes := e.router.Routes()
	if len(routes) < 15 {
		t.Fatalf("unexpectedly few routes: %d", len(routes))
	}
	for _, rt := range routes {
		key := rt.Method + " " + rt.Path
		if publicRoutes[key] {
			continue
		}
		for _, tok := range []string{"", "not-a-jwt", e.token(t, uidPlayer) + "tampered"} {
			if w := e.do(rt.Method, concrete(rt.Path), tok, []byte("{}"), nil); w.Code != http.StatusUnauthorized {
				t.Errorf("%s with token %.10q: status %d, want 401", key, tok, w.Code)
			}
		}
	}
}

func TestAdminRoutesRequireStaffAndPermission(t *testing.T) {
	e := newEnv(t)
	for _, rt := range e.router.Routes() {
		if !strings.HasPrefix(rt.Path, "/admin/") {
			continue
		}
		// 普通玩家；以及令牌里自称 admin、但账户服务中已停用的员工
		for _, tok := range []string{e.token(t, uidPlayer, "player"), e.token(t, uidDisabled, "admin")} {
			if w := e.do(rt.Method, concrete(rt.Path), tok, []byte("{}"), nil); w.Code != http.StatusForbidden {
				t.Errorf("%s %s: status %d, want 403", rt.Method, rt.Path, w.Code)
			}
		}
	}

	cs := e.token(t, uidCS)
	if w := e.do("GET", "/admin/v1/orders", cs, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("cs should read orders: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/admin/v1/wallet/adjust", cs, []byte(`{}`), nil); w.Code != http.StatusForbidden {
		t.Fatalf("cs must not adjust wallet: %d", w.Code)
	}
	ops := e.token(t, uidOps)
	if w := e.do("GET", "/admin/v1/reconcile/diffs", ops, nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("ops must not read reconcile diffs: %d", w.Code)
	}
	// 令牌中的角色声明不被信任：玩家自签不了令牌，但即使令牌里写了 admin，也以账户服务为准
	if w := e.do("GET", "/admin/v1/orders", e.token(t, uidPlayer, "admin"), nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("roles in token must not grant access: %d", w.Code)
	}
}

func TestUserIDAlwaysComesFromToken(t *testing.T) {
	e := newEnv(t)
	tok := e.token(t, uidPlayer)

	e.do("GET", "/api/v1/orders/ABC?user_id=999", tok, nil, nil)
	if e.order.lastUser != uidPlayer {
		t.Fatalf("GetOrder used user_id %d from request, want %d from token", e.order.lastUser, uidPlayer)
	}
	e.do("POST", "/api/v1/orders", tok, []byte(`{"user_id":999,"game_id":"g","product_id":"p","channel":"c","amount":1}`), nil)
	if e.order.lastUser != uidPlayer {
		t.Fatalf("CreateOrder used user_id %d from body", e.order.lastUser)
	}
}

func TestMassAssignmentIgnored(t *testing.T) {
	e := newEnv(t)
	body := []byte(`{"nickname":"new","avatar_key":"avatars/2/stolen.png","roles":["admin"],"user_id":2}`)
	if w := e.do("PATCH", "/api/v1/me", e.token(t, uidPlayer), body, nil); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	got := e.account.updates[len(e.account.updates)-1]
	if got.GetUserId() != uidPlayer || got.GetAvatarKey() != "" || got.GetNickname() != "new" {
		t.Fatalf("unexpected fields forwarded: %+v", got)
	}
}

func TestInternalErrorsNotLeaked(t *testing.T) {
	e := newEnv(t)
	tok := e.token(t, uidPlayer)
	for _, err := range []error{
		errors.New("Error 1045: Access denied for user 'gamepub'@'10.0.3.17'"),
		status.Error(codes.Unavailable, "connection error: dial tcp 10.0.3.17:9000: connect: connection refused"),
		status.Error(codes.Internal, "panic: runtime error at wallet/service.go:142"),
	} {
		e.wallet.err = err
		w := e.do("GET", "/api/v1/wallet", tok, nil, nil)
		if w.Code < 500 || strings.Contains(w.Body.String(), "10.0.3.17") || strings.Contains(w.Body.String(), ".go:") {
			t.Errorf("leaked internal detail (%d): %s", w.Code, w.Body)
		}
	}
}

func TestBatchQueryLimit(t *testing.T) {
	e := newEnv(t)
	nos := make([]string, 51)
	for i := range nos {
		nos[i] = "X"
	}
	body, _ := json.Marshal(gin.H{"order_nos": nos})
	if w := e.do("POST", "/api/v1/orders/batch-query", e.token(t, uidPlayer), body, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("51 items: status %d, want 400", w.Code)
	}
}

func (e *testEnv) login(phone, remoteAddr, xff string) int {
	req := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{"phone":"`+phone+`","password":"guess-123"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteAddr
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w.Code
}

func TestLoginRateLimitedPerPhone(t *testing.T) {
	e := newEnv(t)
	var last int
	for i := 1; i <= 5; i++ {
		// 每次换一个真实来源 IP（模拟代理池撞库），手机号维度的限流依然生效
		last = e.login("13812345678", fmt.Sprintf("203.0.113.%d:4000", i), "")
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("expected per-phone rate limit, last status %d", last)
	}
}

func TestSpoofedForwardedForIgnored(t *testing.T) {
	e := newEnv(t)
	var last int
	for i := 1; i <= 5; i++ {
		// 同一来源、不同手机号、伪造不同的 X-Forwarded-For：未配置可信代理时不能借此绕过按 IP 限流
		last = e.login(fmt.Sprintf("1381234560%d", i), "198.51.100.7:4000", fmt.Sprintf("10.0.0.%d", i))
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("spoofed X-Forwarded-For bypassed per-IP limit, last status %d", last)
	}
}

func TestRequestIDSanitized(t *testing.T) {
	e := newEnv(t)
	w := e.do("GET", "/healthz", "", nil, map[string]string{"X-Request-ID": "abc\" injected=\"1"})
	if got := w.Header().Get("X-Request-ID"); strings.Contains(got, "injected") || got == "" {
		t.Fatalf("request id not sanitized: %q", got)
	}
}

func TestPayNotifyForwardsSignatureHeaders(t *testing.T) {
	e := newEnv(t)
	if w := e.do("POST", "/api/v1/pay/notify/mockpay", "", []byte(`{}`), nil); w.Code != http.StatusBadRequest || w.Body.String() != "fail" {
		t.Fatalf("unsigned notify: %d %q", w.Code, w.Body)
	}
	w := e.do("POST", "/api/v1/pay/notify/mockpay", "", []byte(`{}`), map[string]string{"X-Signature": "abc"})
	if w.Code != http.StatusOK || w.Body.String() != "success" {
		t.Fatalf("signed notify: %d %q", w.Code, w.Body)
	}
}

func multipartFile(t *testing.T, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", filename)
	_, _ = fw.Write(data)
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestAvatarUpload(t *testing.T) {
	e := newEnv(t)
	tok := e.token(t, uidPlayer)
	send := func(filename string, data []byte) *httptest.ResponseRecorder {
		body, ct := multipartFile(t, filename, data)
		req := httptest.NewRequest("POST", "/api/v1/me/avatar", body)
		req.Header.Set("Content-Type", ct)
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		e.router.ServeHTTP(w, req)
		return w
	}

	if w := send("avatar.png", []byte("<html><script>alert(document.cookie)</script></html>")); w.Code != http.StatusBadRequest {
		t.Fatalf("html disguised as png: %d", w.Code)
	}
	if w := send("big.png", bytes.Repeat([]byte{0}, 4<<20)); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload: %d", w.Code)
	}

	var img bytes.Buffer
	_ = png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 8, 8)))
	w := send("../../../etc/cron.d/evil.php", img.Bytes())
	if w.Code != http.StatusOK {
		t.Fatalf("valid png: %d %s", w.Code, w.Body)
	}
	var resp struct {
		Data struct {
			AvatarKey string `json:"avatar_key"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	key := resp.Data.AvatarKey
	if !strings.HasPrefix(key, "avatars/1/") || !strings.HasSuffix(key, ".png") || strings.Contains(key, "evil") {
		t.Fatalf("server must generate its own key, got %q", key)
	}
	if _, err := os.Stat(filepath.Join(e.storage, filepath.FromSlash(key))); err != nil {
		t.Fatalf("file not stored: %v", err)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	w := e.do("GET", "/api/v1/me", e.token(t, uidPlayer), nil, nil)
	for _, h := range []string{"X-Content-Type-Options", "X-Frame-Options", "Content-Security-Policy", "Cache-Control"} {
		if w.Header().Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
}
