package grpcx

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"gamepub/pkg/logx"
)

const token = "0123456789abcdef0123456789abcdef"

type echoServer struct{ err error }

func (e *echoServer) handle(context.Context, any) (any, error) {
	return &healthpb.HealthCheckResponse{}, e.err
}

// register 注册一个手写 ServiceDesc，避免为测试引入额外 proto。
func register(s *grpc.Server, impl *echoServer) {
	s.RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Echo",
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Call",
			Handler: func(srv any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
				in := new(healthpb.HealthCheckRequest)
				if err := dec(in); err != nil {
					return nil, err
				}
				return ic(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/test.Echo/Call"}, impl.handle)
			},
		}},
	}, impl)
}

func start(t *testing.T, impl *echoServer) *grpc.ClientConn {
	t.Helper()
	srv, err := NewServer(ServerOptions{InternalToken: token, Logger: logx.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	register(srv, impl)
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///buf",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func call(ctx context.Context, conn *grpc.ClientConn) error {
	return conn.Invoke(ctx, "/test.Echo/Call", &healthpb.HealthCheckRequest{}, &healthpb.HealthCheckResponse{})
}

func TestInternalTokenRequired(t *testing.T) {
	conn := start(t, &echoServer{})

	if err := call(context.Background(), conn); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("call without token: %v", err)
	}
	bad := metadata.AppendToOutgoingContext(context.Background(), MDInternalToken, strings.Repeat("x", 32))
	if err := call(bad, conn); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("call with wrong token: %v", err)
	}
	good := metadata.AppendToOutgoingContext(context.Background(), MDInternalToken, token)
	if err := call(good, conn); err != nil {
		t.Fatalf("call with token: %v", err)
	}
}

func TestInternalErrorsAreSanitized(t *testing.T) {
	conn := start(t, &echoServer{err: errors.New("Error 1146: Table 'gamepub.wallet_account' doesn't exist (10.0.3.17:3306)")})

	ctx := metadata.AppendToOutgoingContext(context.Background(), MDInternalToken, token)
	st := status.Convert(call(ctx, conn))
	if st.Code() != codes.Internal || strings.Contains(st.Message(), "wallet_account") || strings.Contains(st.Message(), "10.0.3.17") {
		t.Fatalf("internal error leaked to caller: %v", st)
	}
}

func TestDomainStatusPassesThrough(t *testing.T) {
	conn := start(t, &echoServer{err: status.Error(codes.NotFound, "order not found")})

	ctx := metadata.AppendToOutgoingContext(context.Background(), MDInternalToken, token)
	if st := status.Convert(call(ctx, conn)); st.Code() != codes.NotFound || st.Message() != "order not found" {
		t.Fatalf("explicit status should pass through: %v", st)
	}
}

func TestShortTokenRejected(t *testing.T) {
	if _, err := NewServer(ServerOptions{InternalToken: "short", Logger: logx.Discard()}); err == nil {
		t.Fatal("short internal token must be rejected at startup")
	}
}
