package account

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountv1 "gamepub/api/gen/account/v1"
)

// GRPCServer gRPC 适配层。
type GRPCServer struct {
	accountv1.UnimplementedAccountServiceServer
	svc *Service
}

// NewGRPCServer 创建适配层。
func NewGRPCServer(svc *Service) *GRPCServer { return &GRPCServer{svc: svc} }

func toStatus(err error) error {
	switch {
	case errors.Is(err, ErrInvalidPhone), errors.Is(err, ErrWeakPassword), errors.Is(err, ErrInvalidNickname),
		errors.Is(err, ErrInvalidAvatar), errors.Is(err, ErrInvalidRole):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrPhoneTaken):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, ErrInvalidCredentials):
		return status.Error(codes.Unauthenticated, err.Error())
	case errors.Is(err, ErrUserNotFound):
		return status.Error(codes.NotFound, err.Error())
	}
	return err
}

func toPB(p *Profile) *accountv1.User {
	return &accountv1.User{
		UserId: p.ID, PhoneMasked: p.PhoneMasked, Nickname: p.Nickname, Roles: p.Roles,
		CreatedAt: p.CreatedAt.Unix(), AvatarKey: p.AvatarKey, Status: p.Status,
	}
}

func (g *GRPCServer) Register(ctx context.Context, req *accountv1.RegisterRequest) (*accountv1.RegisterResponse, error) {
	id, err := g.svc.Register(ctx, req.GetPhone(), req.GetPassword(), req.GetNickname())
	if err != nil {
		return nil, toStatus(err)
	}
	return &accountv1.RegisterResponse{UserId: id}, nil
}

func (g *GRPCServer) Login(ctx context.Context, req *accountv1.LoginRequest) (*accountv1.LoginResponse, error) {
	token, exp, err := g.svc.Login(ctx, req.GetPhone(), req.GetPassword())
	if err != nil {
		return nil, toStatus(err)
	}
	return &accountv1.LoginResponse{AccessToken: token, ExpiresAt: exp.Unix()}, nil
}

func (g *GRPCServer) GetUser(ctx context.Context, req *accountv1.GetUserRequest) (*accountv1.GetUserResponse, error) {
	p, err := g.svc.GetProfile(ctx, req.GetUserId())
	if err != nil {
		return nil, toStatus(err)
	}
	return &accountv1.GetUserResponse{User: toPB(p)}, nil
}

func (g *GRPCServer) UpdateProfile(ctx context.Context, req *accountv1.UpdateProfileRequest) (*accountv1.UpdateProfileResponse, error) {
	p, err := g.svc.UpdateProfile(ctx, req.GetUserId(), req.GetNickname(), req.GetAvatarKey())
	if err != nil {
		return nil, toStatus(err)
	}
	return &accountv1.UpdateProfileResponse{User: toPB(p)}, nil
}
