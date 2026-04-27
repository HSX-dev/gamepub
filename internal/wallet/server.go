package wallet

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	walletv1 "gamepub/api/gen/wallet/v1"
)

// GRPCServer gRPC 适配层：只做参数转换与错误映射，业务逻辑在 Service。
type GRPCServer struct {
	walletv1.UnimplementedWalletServiceServer
	svc *Service
}

// NewGRPCServer 创建适配层。
func NewGRPCServer(svc *Service) *GRPCServer { return &GRPCServer{svc: svc} }

// toStatus 领域错误 → gRPC 状态码。未识别的错误原样返回，由拦截器统一替换为 internal error。
func toStatus(err error) error {
	switch {
	case errors.Is(err, ErrInvalidArgument), errors.Is(err, ErrInvalidAmount), errors.Is(err, ErrInvalidBiz):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrInsufficientBalance), errors.Is(err, ErrBranchRejected):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, ErrIdempotencyConflict):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, ErrConcurrentUpdate):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, ErrBranchNotFound):
		return status.Error(codes.NotFound, err.Error())
	}
	return err
}

func (g *GRPCServer) GetBalance(ctx context.Context, req *walletv1.GetBalanceRequest) (*walletv1.GetBalanceResponse, error) {
	if req.GetUserId() == 0 {
		return nil, toStatus(ErrInvalidArgument)
	}
	b, err := g.svc.GetBalance(ctx, req.GetUserId())
	if err != nil {
		return nil, toStatus(err)
	}
	return &walletv1.GetBalanceResponse{UserId: req.GetUserId(), Available: b.Available, Frozen: b.Frozen}, nil
}

func (g *GRPCServer) ListLedger(ctx context.Context, req *walletv1.ListLedgerRequest) (*walletv1.ListLedgerResponse, error) {
	rows, total, err := g.svc.ListLedger(ctx, LedgerQuery{
		UserID: req.GetUserId(), BizType: req.GetBizType(), SortBy: req.GetSortBy(), Desc: req.GetDesc(),
		Page: req.GetPage(), PageSize: req.GetPageSize(),
	})
	if err != nil {
		return nil, toStatus(err)
	}
	out := make([]*walletv1.LedgerEntry, 0, len(rows))
	for _, l := range rows {
		out = append(out, &walletv1.LedgerEntry{
			Id: l.ID, BizType: l.BizType, BizId: l.BizID, Amount: l.Amount,
			BalanceAfter: l.BalanceAfter, Remark: l.Remark, CreatedAt: l.CreatedAt.Unix(),
		})
	}
	return &walletv1.ListLedgerResponse{Entries: out, Total: total}, nil
}

func (g *GRPCServer) Credit(ctx context.Context, req *walletv1.CreditRequest) (*walletv1.CreditResponse, error) {
	res, err := g.svc.Credit(ctx, ChangeRequest{
		UserID: req.GetUserId(), Amount: req.GetAmount(), BizType: req.GetBizType(), BizID: req.GetBizId(), Remark: req.GetRemark(),
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return &walletv1.CreditResponse{LedgerId: res.LedgerID, BalanceAfter: res.BalanceAfter, Duplicated: res.Duplicated}, nil
}

func (g *GRPCServer) Debit(ctx context.Context, req *walletv1.DebitRequest) (*walletv1.DebitResponse, error) {
	res, err := g.svc.Debit(ctx, ChangeRequest{
		UserID: req.GetUserId(), Amount: req.GetAmount(), BizType: req.GetBizType(), BizID: req.GetBizId(), Remark: req.GetRemark(),
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return &walletv1.DebitResponse{LedgerId: res.LedgerID, BalanceAfter: res.BalanceAfter, Duplicated: res.Duplicated}, nil
}

func (g *GRPCServer) TryFreeze(ctx context.Context, req *walletv1.TryFreezeRequest) (*walletv1.TryFreezeResponse, error) {
	st, err := g.svc.TryFreeze(ctx, req.GetXid(), req.GetUserId(), req.GetAmount())
	if err != nil {
		return nil, toStatus(err)
	}
	return &walletv1.TryFreezeResponse{Status: st}, nil
}

func (g *GRPCServer) ConfirmFreeze(ctx context.Context, req *walletv1.ConfirmFreezeRequest) (*walletv1.ConfirmFreezeResponse, error) {
	st, err := g.svc.ConfirmFreeze(ctx, req.GetXid())
	if err != nil {
		return nil, toStatus(err)
	}
	return &walletv1.ConfirmFreezeResponse{Status: st}, nil
}

func (g *GRPCServer) CancelFreeze(ctx context.Context, req *walletv1.CancelFreezeRequest) (*walletv1.CancelFreezeResponse, error) {
	st, err := g.svc.CancelFreeze(ctx, req.GetXid())
	if err != nil {
		return nil, toStatus(err)
	}
	return &walletv1.CancelFreezeResponse{Status: st}, nil
}

func (g *GRPCServer) ListReconcileDiffs(ctx context.Context, req *walletv1.ListReconcileDiffsRequest) (*walletv1.ListReconcileDiffsResponse, error) {
	rows, err := g.svc.ListReconcileDiffs(ctx, int(req.GetLimit()))
	if err != nil {
		return nil, toStatus(err)
	}
	out := make([]*walletv1.ReconcileDiff, 0, len(rows))
	for _, d := range rows {
		out = append(out, &walletv1.ReconcileDiff{
			UserId: d.UserID, WalletBalance: d.WalletBalance, LedgerSum: d.LedgerSum, CheckedAt: d.CheckedAt.Unix(),
		})
	}
	return &walletv1.ListReconcileDiffsResponse{Diffs: out}, nil
}
