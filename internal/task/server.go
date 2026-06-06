package task

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	taskv1 "gamepub/api/gen/task/v1"
)

// GRPCServer gRPC 适配层。
type GRPCServer struct {
	taskv1.UnimplementedTaskServiceServer
	svc *Service
}

// NewGRPCServer 创建适配层。
func NewGRPCServer(svc *Service) *GRPCServer { return &GRPCServer{svc: svc} }

func toStatus(err error) error {
	switch {
	case errors.Is(err, ErrTaskNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrNotCompleted), errors.Is(err, ErrAlreadyClaimed):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, ErrRewardPending):
		return status.Error(codes.Unavailable, err.Error())
	}
	return err
}

func (g *GRPCServer) ListTasks(ctx context.Context, req *taskv1.ListTasksRequest) (*taskv1.ListTasksResponse, error) {
	if req.GetUserId() == 0 {
		return nil, status.Error(codes.InvalidArgument, "user_id required")
	}
	rows, err := g.svc.ListTasks(ctx, req.GetUserId())
	if err != nil {
		return nil, toStatus(err)
	}
	out := make([]*taskv1.TaskProgress, 0, len(rows))
	for _, p := range rows {
		out = append(out, &taskv1.TaskProgress{
			TaskId: p.ID, Title: p.Title, Target: p.Target, Progress: p.Progress,
			Status: p.Status, RewardCoin: p.RewardCoin, Period: p.PeriodKey,
		})
	}
	return &taskv1.ListTasksResponse{Tasks: out}, nil
}

func (g *GRPCServer) ClaimReward(ctx context.Context, req *taskv1.ClaimRewardRequest) (*taskv1.ClaimRewardResponse, error) {
	reward, balance, err := g.svc.ClaimReward(ctx, req.GetUserId(), req.GetTaskId())
	if err != nil {
		return nil, toStatus(err)
	}
	return &taskv1.ClaimRewardResponse{RewardCoin: reward, BalanceAfter: balance}, nil
}
