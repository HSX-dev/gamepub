package order

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	orderv1 "gamepub/api/gen/order/v1"
)

// GRPCServer gRPC 适配层。
type GRPCServer struct {
	orderv1.UnimplementedOrderServiceServer
	svc *Service
}

// NewGRPCServer 创建适配层。
func NewGRPCServer(svc *Service) *GRPCServer { return &GRPCServer{svc: svc} }

func toStatus(err error) error {
	switch {
	case errors.Is(err, ErrInvalidArgument), errors.Is(err, ErrInvalidDeliveryURL), errors.Is(err, ErrTooManyOrders):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrProductNotFound), errors.Is(err, ErrGameNotFound), errors.Is(err, ErrOrderNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrChannelNotSupported):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrBadSignature):
		return status.Error(codes.Unauthenticated, err.Error())
	case errors.Is(err, ErrAmountMismatch), errors.Is(err, ErrChannelMismatch), errors.Is(err, ErrTradeReused):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, errConcurrentPaid):
		return status.Error(codes.Aborted, err.Error())
	}
	return err
}

func toPB(o *Order) *orderv1.Order {
	out := &orderv1.Order{
		OrderNo: o.OrderNo, UserId: o.UserID, GameId: o.GameID, ProductId: o.ProductID, Amount: o.Amount,
		Coin: o.Coin, Status: o.Status, Channel: o.Channel, CreatedAt: o.CreatedAt.Unix(),
	}
	if o.PaidAt != nil {
		out.PaidAt = o.PaidAt.Unix()
	}
	return out
}

func toPBList(rows []Order) []*orderv1.Order {
	out := make([]*orderv1.Order, 0, len(rows))
	for i := range rows {
		out = append(out, toPB(&rows[i]))
	}
	return out
}

func (g *GRPCServer) CreateOrder(ctx context.Context, req *orderv1.CreateOrderRequest) (*orderv1.CreateOrderResponse, error) {
	o, payURL, err := g.svc.CreateOrder(ctx, CreateOrderInput{
		UserID: req.GetUserId(), GameID: req.GetGameId(), ProductID: req.GetProductId(),
		Channel: req.GetChannel(), IdempotencyKey: req.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return &orderv1.CreateOrderResponse{Order: toPB(o), PayUrl: payURL}, nil
}

func (g *GRPCServer) GetOrder(ctx context.Context, req *orderv1.GetOrderRequest) (*orderv1.GetOrderResponse, error) {
	o, err := g.svc.GetOrder(ctx, req.GetUserId(), req.GetOrderNo())
	if err != nil {
		return nil, toStatus(err)
	}
	return &orderv1.GetOrderResponse{Order: toPB(o)}, nil
}

func (g *GRPCServer) ListOrders(ctx context.Context, req *orderv1.ListOrdersRequest) (*orderv1.ListOrdersResponse, error) {
	rows, total, err := g.svc.ListOrders(ctx, req.GetUserId(), req.GetStatus(), req.GetPage(), req.GetPageSize())
	if err != nil {
		return nil, toStatus(err)
	}
	return &orderv1.ListOrdersResponse{Orders: toPBList(rows), Total: total}, nil
}

func (g *GRPCServer) BatchGetOrders(ctx context.Context, req *orderv1.BatchGetOrdersRequest) (*orderv1.BatchGetOrdersResponse, error) {
	rows, err := g.svc.BatchGetOrders(ctx, req.GetUserId(), req.GetOrderNos())
	if err != nil {
		return nil, toStatus(err)
	}
	return &orderv1.BatchGetOrdersResponse{Orders: toPBList(rows)}, nil
}

func (g *GRPCServer) HandlePayNotify(ctx context.Context, req *orderv1.HandlePayNotifyRequest) (*orderv1.HandlePayNotifyResponse, error) {
	reply, err := g.svc.HandlePayNotify(ctx, req.GetChannel(), req.GetBody(), req.GetHeaders())
	if err != nil {
		return nil, toStatus(err)
	}
	return &orderv1.HandlePayNotifyResponse{Reply: reply}, nil
}

func (g *GRPCServer) AdminListOrders(ctx context.Context, req *orderv1.AdminListOrdersRequest) (*orderv1.AdminListOrdersResponse, error) {
	rows, total, err := g.svc.AdminListOrders(ctx, AdminQuery{
		UserID: req.GetUserId(), Status: req.GetStatus(), GameID: req.GetGameId(), SortBy: req.GetSortBy(),
		Desc: req.GetDesc(), Page: req.GetPage(), PageSize: req.GetPageSize(),
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return &orderv1.AdminListOrdersResponse{Orders: toPBList(rows), Total: total}, nil
}

func (g *GRPCServer) UpsertGameApp(ctx context.Context, req *orderv1.UpsertGameAppRequest) (*orderv1.UpsertGameAppResponse, error) {
	app, err := g.svc.UpsertGameApp(ctx, req.GetGameId(), req.GetName(), req.GetDeliveryUrl(), req.GetOperator())
	if err != nil {
		return nil, toStatus(err)
	}
	return &orderv1.UpsertGameAppResponse{App: &orderv1.GameApp{
		GameId: app.GameID, Name: app.Name, DeliveryUrl: app.DeliveryURL, UpdatedAt: app.UpdatedAt.Unix(), UpdatedBy: app.UpdatedBy,
	}}, nil
}

func (g *GRPCServer) TestDelivery(ctx context.Context, req *orderv1.TestDeliveryRequest) (*orderv1.TestDeliveryResponse, error) {
	code, latency, err := g.svc.TestDelivery(ctx, req.GetGameId())
	if err != nil {
		return nil, toStatus(err)
	}
	return &orderv1.TestDeliveryResponse{StatusCode: int32(code), LatencyMs: latency.Milliseconds()}, nil // #nosec G115 -- HTTP 状态码不会溢出 int32
}
