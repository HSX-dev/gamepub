package gateway

import (
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountv1 "gamepub/api/gen/account/v1"
	"gamepub/internal/rbac"
	"gamepub/pkg/auth"
	"gamepub/pkg/cryptox"
	"gamepub/pkg/logx"
	"gamepub/pkg/ratelimit"
)

const (
	ctxUserID     = "gp.uid"
	ctxStaffRoles = "gp.staff_roles"
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// requestID 透传或生成请求 ID。外部传入的值必须通过格式校验，防止换行等字符注入日志。
func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if !requestIDPattern.MatchString(id) {
			id = cryptox.RandomHex(8)
		}
		c.Header("X-Request-ID", id)
		c.Request = c.Request.WithContext(logx.WithRequestID(c.Request.Context(), id))
		c.Next()
	}
}

// recovery 捕获 panic：完整堆栈只进日志，响应只给通用错误。
func recovery(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				logger.ErrorContext(c.Request.Context(), "http panic", "panic", r, "route", c.FullPath(), "stack", string(debug.Stack()))
				fail(c, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		c.Next()
	}
}

func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		c.Next()
	}
}

// accessLog 记录路由模板而非原始 URL：原始 URL 的路径参数与查询串里可能有订单号、手机号、token。
func accessLog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		logger.InfoContext(c.Request.Context(), "http",
			"method", c.Request.Method, "route", c.FullPath(), "status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(), "client_ip", c.ClientIP(), "user_id", c.GetUint64(ctxUserID))
	}
}

func bodyLimit(n int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, n)
		c.Next()
	}
}

// authn 校验 Bearer 令牌，并把用户 ID 写入上下文。之后所有 handler 只从这里取用户 ID。
func authn(v *auth.Verifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if !ok || raw == "" {
			fail(c, http.StatusUnauthorized, "unauthenticated", "missing bearer token")
			return
		}
		claims, err := v.Verify(raw)
		if err != nil {
			fail(c, http.StatusUnauthorized, "unauthenticated", "invalid or expired token")
			return
		}
		uid, err := claims.UserID()
		if err != nil {
			fail(c, http.StatusUnauthorized, "unauthenticated", "invalid or expired token")
			return
		}
		c.Set(ctxUserID, uid)
		c.Next()
	}
}

// loadStaff 后台权限以账户服务的实时数据为准，而不是令牌里的角色：
// 员工离职/降权、账号停用后立即生效，不必等令牌过期。
func loadStaff(account accountv1.AccountServiceClient) gin.HandlerFunc {
	return func(c *gin.Context) {
		resp, err := account.GetUser(c.Request.Context(), &accountv1.GetUserRequest{UserId: userID(c)})
		if err != nil {
			if status.Code(err) == codes.NotFound {
				fail(c, http.StatusUnauthorized, "unauthenticated", "user not found")
				return
			}
			grpcFail(c, err)
			return
		}
		u := resp.GetUser()
		if u.GetStatus() != "active" || !rbac.IsStaff(u.GetRoles()) {
			fail(c, http.StatusForbidden, "forbidden", "permission denied")
			return
		}
		c.Set(ctxStaffRoles, u.GetRoles())
		c.Next()
	}
}

func requirePerm(perm string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !rbac.HasPerm(c.GetStringSlice(ctxStaffRoles), perm) {
			fail(c, http.StatusForbidden, "forbidden", "permission denied")
			return
		}
		c.Next()
	}
}

// auditLog 后台操作审计：谁、在什么时候、对哪个资源、做了什么、结果如何。
func auditLog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		params := make(map[string]string, len(c.Params))
		for _, p := range c.Params {
			params[p.Key] = p.Value
		}
		logger.InfoContext(c.Request.Context(), "audit",
			"operator", c.GetUint64(ctxUserID), "roles", c.GetStringSlice(ctxStaffRoles),
			"method", c.Request.Method, "route", c.FullPath(), "params", params,
			"status", c.Writer.Status(), "client_ip", c.ClientIP())
	}
}

func rateLimit(l ratelimit.Limiter, key func(*gin.Context) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		d, err := l.Allow(c.Request.Context(), c.FullPath()+"|"+key(c))
		if err == nil && !d.Allowed {
			c.Header("Retry-After", strconv.Itoa(int(math.Ceil(d.RetryAfter.Seconds()))))
			fail(c, http.StatusTooManyRequests, "rate_limited", "too many requests")
			return
		}
		c.Next()
	}
}

func byIP(c *gin.Context) string   { return c.ClientIP() }
func byUser(c *gin.Context) string { return strconv.FormatUint(userID(c), 10) }

func userID(c *gin.Context) uint64 { return c.GetUint64(ctxUserID) }
