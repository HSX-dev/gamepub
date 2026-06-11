// Package gateway 对外 HTTP 网关（Gin）：鉴权、权限、限流、参数校验后经 gRPC 转发到领域服务。
//
// 路由组织原则（针对“鉴权中间件遗漏”这一高频审计问题）：
//   - 公开接口在最前面显式列出，数量越少越好；
//   - 鉴权、限流挂在分组上，新增接口只要放进分组就自动受保护，不依赖开发者逐个记得加；
//   - router_test.go 遍历全部路由断言“非白名单接口未登录必须 401、后台接口无权限必须 403”，
//     CI 中任何人漏挂中间件都会让测试失败。
package gateway

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	accountv1 "gamepub/api/gen/account/v1"
	orderv1 "gamepub/api/gen/order/v1"
	taskv1 "gamepub/api/gen/task/v1"
	walletv1 "gamepub/api/gen/wallet/v1"
	"gamepub/internal/rbac"
	"gamepub/pkg/auth"
	"gamepub/pkg/ratelimit"
	"gamepub/pkg/upload"
)

// Limiters 各类接口的限流器。
type Limiters struct {
	Login  ratelimit.Limiter // 注册/登录：按 IP，登录另按手机号
	User   ratelimit.Limiter // 登录用户的常规接口：按用户
	Batch  ratelimit.Limiter // 批量查询：按用户，配额更小
	Notify ratelimit.Limiter // 支付回调：按来源 IP
}

// Deps 网关依赖。
type Deps struct {
	Account  accountv1.AccountServiceClient
	Wallet   walletv1.WalletServiceClient
	Order    orderv1.OrderServiceClient
	Task     taskv1.TaskServiceClient
	Verifier *auth.Verifier
	Limiters Limiters
	Storage  upload.Storage
	Logger   *slog.Logger
}

// Config 网关配置。
type Config struct {
	// TrustedProxies 可信反向代理网段。为空表示不信任任何代理：
	// Gin 默认信任所有代理，攻击者伪造 X-Forwarded-For 即可绕过按 IP 的限流与风控。
	TrustedProxies []string
}

// publicRoutes 无需登录的接口白名单（测试据此校验其余接口都要求鉴权）。
var publicRoutes = map[string]bool{
	"GET /healthz":                     true,
	"POST /api/v1/auth/register":       true,
	"POST /api/v1/auth/login":          true,
	"POST /api/v1/pay/notify/:channel": true, // 由支付渠道签名鉴权
}

// NewRouter 构建路由。
func NewRouter(d Deps, cfg Config) (*gin.Engine, error) {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	if err := r.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		return nil, err
	}
	r.Use(requestID(), recovery(d.Logger), securityHeaders(), accessLog(d.Logger))
	h := &handlers{Deps: d}

	r.GET("/healthz", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	// ---- 公开接口 ----
	pub := r.Group("/api/v1", bodyLimit(16<<10))
	pub.POST("/auth/register", rateLimit(d.Limiters.Login, byIP), h.register)
	pub.POST("/auth/login", rateLimit(d.Limiters.Login, byIP), h.login)
	pub.POST("/pay/notify/:channel", rateLimit(d.Limiters.Notify, byIP), h.payNotify)

	// ---- 玩家接口：登录 + 按用户限流 ----
	user := r.Group("/api/v1", authn(d.Verifier), rateLimit(d.Limiters.User, byUser))
	api := user.Group("", bodyLimit(64<<10))
	api.GET("/me", h.me)
	api.PATCH("/me", h.updateMe)
	api.GET("/wallet", h.balance)
	api.GET("/wallet/ledger", h.ledger)
	api.POST("/orders", h.createOrder)
	api.GET("/orders", h.listOrders)
	api.GET("/orders/:order_no", h.getOrder)
	api.POST("/orders/batch-query", rateLimit(d.Limiters.Batch, byUser), h.batchOrders)
	api.GET("/tasks", h.listTasks)
	api.POST("/tasks/:task_id/claim", h.claimTask)
	user.POST("/me/avatar", bodyLimit(3<<20), h.uploadAvatar)

	// ---- 运营后台：登录 + 实时角色校验 + 权限点 + 审计日志 ----
	admin := r.Group("/admin/v1", bodyLimit(64<<10), authn(d.Verifier), loadStaff(d.Account), auditLog(d.Logger))
	admin.GET("/orders", requirePerm(rbac.PermOrderRead), h.adminOrders)
	admin.PUT("/games/:game_id", requirePerm(rbac.PermGameWrite), h.adminUpsertGame)
	admin.POST("/games/:game_id/test-delivery", requirePerm(rbac.PermGameWrite), h.adminTestDelivery)
	admin.POST("/wallet/adjust", requirePerm(rbac.PermWalletAdjust), h.adminAdjustWallet)
	admin.GET("/reconcile/diffs", requirePerm(rbac.PermWalletRead), h.adminReconcileDiffs)

	r.NoRoute(func(c *gin.Context) { fail(c, http.StatusNotFound, "not_found", "not found") })
	return r, nil
}

type handlers struct{ Deps }
