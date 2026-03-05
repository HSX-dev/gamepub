// Package grpcx gRPC 服务端/客户端的统一治理：内部鉴权、request_id 透传、panic 恢复、错误脱敏、默认超时与重试。
package grpcx

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"gamepub/pkg/cryptox"
	"gamepub/pkg/logx"
)

// 元数据键。
const (
	MDInternalToken = "x-internal-token"
	MDRequestID     = "x-request-id"
)

// ServerOptions 服务端参数。
type ServerOptions struct {
	// InternalToken 服务间调用凭据（纵深防御；传输层由服务网格 mTLS 保护）
	InternalToken string
	Logger        *slog.Logger
}

// NewServer 创建带统一拦截器链的 gRPC Server，并注册健康检查。
func NewServer(o ServerOptions, extra ...grpc.ServerOption) (*grpc.Server, error) {
	if len(o.InternalToken) < 32 {
		return nil, errors.New("grpcx: internal token must be at least 32 bytes")
	}
	opts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(
			recoveryInterceptor(o.Logger),
			requestIDInterceptor(),
			loggingInterceptor(o.Logger),
			authInterceptor(o.InternalToken),
			sanitizeInterceptor(o.Logger),
		),
		grpc.MaxRecvMsgSize(4 << 20),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle:     5 * time.Minute,
			MaxConnectionAge:      30 * time.Minute, // 定期断开长连接，配合客户端 round_robin 让扩容后的 Pod 分到流量
			MaxConnectionAgeGrace: 10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	}
	s := grpc.NewServer(append(opts, extra...)...)
	healthpb.RegisterHealthServer(s, health.NewServer())
	return s, nil
}

// GracefulStop 优雅停止，超时后强制停止，避免 Pod 终止被未结束的流卡住。
func GracefulStop(s *grpc.Server, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		s.GracefulStop()
		close(done)
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		s.Stop()
	}
}

func recoveryInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.ErrorContext(ctx, "rpc panic", "method", info.FullMethod, "panic", r, "stack", string(debug.Stack()))
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(ctx, req)
	}
}

func requestIDInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		id := firstMD(ctx, MDRequestID)
		if id == "" || len(id) > 64 {
			id = cryptox.RandomHex(8)
		}
		return handler(logx.WithRequestID(ctx, id), req)
	}
}

func loggingInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		code := status.Code(err)
		level := slog.LevelInfo
		if code == codes.Internal || code == codes.Unknown || code == codes.DataLoss {
			level = slog.LevelError
		}
		// 只记录方法名、状态码与耗时；请求体可能含手机号/口令，不落日志
		logger.Log(ctx, level, "rpc", "method", info.FullMethod, "code", code.String(), "duration_ms", time.Since(start).Milliseconds())
		return resp, err
	}
}

func authInterceptor(token string) grpc.UnaryServerInterceptor {
	want := []byte(token)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if strings.HasPrefix(info.FullMethod, "/grpc.health.v1.Health/") {
			return handler(ctx, req)
		}
		if subtle.ConstantTimeCompare([]byte(firstMD(ctx, MDInternalToken)), want) != 1 {
			return nil, status.Error(codes.Unauthenticated, "unauthenticated")
		}
		return handler(ctx, req)
	}
}

// sanitizeInterceptor 只有显式构造的 gRPC status 才原样返回；其余错误（SQL 错误、驱动错误等）
// 记录完整日志后统一替换为 "internal error"，防止表结构、内网地址等细节泄露给调用方。
func sanitizeInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if err == nil {
			return resp, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, status.FromContextError(err).Err()
		}
		if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown && st.Code() != codes.Internal {
			return nil, err
		}
		logger.ErrorContext(ctx, "rpc internal error", "method", info.FullMethod, "err", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
}

func firstMD(ctx context.Context, key string) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	if v := md.Get(key); len(v) > 0 {
		return v[0]
	}
	return ""
}
