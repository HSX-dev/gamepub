package gateway

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"gamepub/pkg/logx"
)

type envelope struct {
	Code      string `json:"code"`
	Message   string `json:"message,omitempty"`
	Data      any    `json:"data,omitempty"`
	RequestID string `json:"request_id"`
}

func ok(c *gin.Context, data any) {
	c.JSON(http.StatusOK, envelope{Code: "ok", Data: data, RequestID: logx.RequestID(c.Request.Context())})
}

func fail(c *gin.Context, httpStatus int, code, msg string) {
	c.AbortWithStatusJSON(httpStatus, envelope{Code: code, Message: msg, RequestID: logx.RequestID(c.Request.Context())})
}

// grpcFail 将下游错误映射为 HTTP 响应。
// 4xx 类错误的文案由领域层显式构造，可以透出；5xx 类一律使用通用文案——
// gRPC 传输层错误（如 "connection refused 10.0.3.17:9000"）同样会以 Unavailable 返回，绝不能原样透出。
func grpcFail(c *gin.Context, err error) {
	st := status.Convert(err)
	switch st.Code() {
	case codes.InvalidArgument:
		fail(c, http.StatusBadRequest, "invalid_argument", st.Message())
	case codes.Unauthenticated:
		fail(c, http.StatusUnauthorized, "unauthenticated", st.Message())
	case codes.PermissionDenied:
		fail(c, http.StatusForbidden, "forbidden", "permission denied")
	case codes.NotFound:
		fail(c, http.StatusNotFound, "not_found", st.Message())
	case codes.AlreadyExists, codes.Aborted:
		fail(c, http.StatusConflict, "conflict", st.Message())
	case codes.FailedPrecondition:
		fail(c, http.StatusUnprocessableEntity, "failed_precondition", st.Message())
	case codes.ResourceExhausted:
		fail(c, http.StatusTooManyRequests, "rate_limited", "too many requests")
	case codes.Unavailable:
		fail(c, http.StatusServiceUnavailable, "unavailable", "service temporarily unavailable, please retry")
	case codes.DeadlineExceeded:
		fail(c, http.StatusGatewayTimeout, "timeout", "request timed out, please retry")
	default:
		fail(c, http.StatusInternalServerError, "internal", "internal error")
	}
}

func badRequest(c *gin.Context, msg string) { fail(c, http.StatusBadRequest, "invalid_argument", msg) }

func queryInt32(c *gin.Context, key string) int32 {
	n, err := strconv.ParseInt(c.Query(key), 10, 32)
	if err != nil {
		return 0
	}
	return int32(n)
}
