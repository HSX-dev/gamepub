package grpcx

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"gamepub/pkg/logx"
)

// retryServiceConfig 仅对 UNAVAILABLE 重试（请求大概率未到达服务端）。
// 写接口之所以可以重试，是因为钱包/订单的所有写 RPC 都以业务单号为幂等键——
// 新增非幂等写接口时必须为其单独配置不重试。
const retryServiceConfig = `{
  "loadBalancingConfig": [{"round_robin": {}}],
  "methodConfig": [{
    "name": [{}],
    "retryPolicy": {
      "maxAttempts": 3,
      "initialBackoff": "0.1s",
      "maxBackoff": "1s",
      "backoffMultiplier": 2,
      "retryableStatusCodes": ["UNAVAILABLE"]
    }
  }]
}`

// ClientOptions 客户端参数。
type ClientOptions struct {
	InternalToken  string
	DefaultTimeout time.Duration
	// TLS 为空时使用明文：集群内由服务网格 Sidecar 提供 mTLS；未接入网格的环境必须配置 TLS
	TLS credentials.TransportCredentials
}

// Dial 创建客户端连接。target 建议使用 dns:///svc.ns.svc.cluster.local:9000 + headless Service，
// 配合 round_robin 实现客户端负载均衡（否则 HTTP/2 长连接会让流量粘在单个 Pod 上）。
func Dial(target string, o ClientOptions) (*grpc.ClientConn, error) {
	creds := o.TLS
	if creds == nil {
		creds = insecure.NewCredentials()
	}
	if o.DefaultTimeout <= 0 {
		o.DefaultTimeout = 3 * time.Second
	}
	return grpc.NewClient(target,
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultServiceConfig(retryServiceConfig),
		grpc.WithChainUnaryInterceptor(timeoutInterceptor(o.DefaultTimeout), metadataInterceptor(o.InternalToken)),
	)
}

// timeoutInterceptor 调用方未设置 deadline 时补默认超时：没有超时的下游调用是雪崩的起点。
func timeoutInterceptor(d time.Duration) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func metadataInterceptor(token string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		kv := []string{MDInternalToken, token}
		if id := logx.RequestID(ctx); id != "" {
			kv = append(kv, MDRequestID, id)
		}
		return invoker(metadata.AppendToOutgoingContext(ctx, kv...), method, req, reply, cc, opts...)
	}
}
