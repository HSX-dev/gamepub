// account 账户中心服务。
//
// 运维子命令：account grant-role <phone> <role>（仅在 Pod 内通过 kubectl exec 执行，不暴露为网络接口）。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sync/errgroup"

	accountv1 "gamepub/api/gen/account/v1"
	"gamepub/internal/account"
	"gamepub/internal/app"
	"gamepub/pkg/auth"
	"gamepub/pkg/config"
	"gamepub/pkg/cryptox"
	"gamepub/pkg/logx"
)

func main() {
	logger := logx.New("account", config.String("LOG_LEVEL", "info"))
	if len(os.Args) > 1 {
		if err := runCommand(logger, os.Args[1:]); err != nil {
			logger.Error("command failed", "err", err)
			os.Exit(1)
		}
		return
	}
	app.Main(logger, func(ctx context.Context, g *errgroup.Group) error {
		svc, err := build(ctx, logger)
		if err != nil {
			return err
		}
		srv, err := app.GRPCServer(logger)
		if err != nil {
			return err
		}
		accountv1.RegisterAccountServiceServer(srv, account.NewGRPCServer(svc))
		return app.ServeGRPC(ctx, g, srv, config.String("GRPC_ADDR", ":9001"), logger)
	})
}

func build(ctx context.Context, logger *slog.Logger) (*account.Service, error) {
	seed, err1 := config.SecretBytes("JWT_ED25519_SEED", 32)
	aesKey, err2 := config.SecretBytes("PII_AES_KEY", 32)
	pepper, err3 := config.Secret("PHONE_PEPPER", 16)
	if err := errors.Join(err1, err2, err3); err != nil {
		return nil, err
	}
	issuer, err := auth.NewIssuer(seed, "gamepub-account", "gamepub-api", config.Duration("JWT_TTL", 2*time.Hour))
	if err != nil {
		return nil, err
	}
	sealer, err := cryptox.NewSealer(aesKey)
	if err != nil {
		return nil, err
	}
	db, err := app.OpenDB(&account.User{})
	if err != nil {
		return nil, err
	}
	rdb, err := app.OpenRedis(ctx)
	if err != nil {
		return nil, err
	}
	return account.NewService(account.Options{
		DB: db, Redis: rdb, Issuer: issuer, Sealer: sealer, PhonePepper: []byte(pepper), Logger: logger,
	})
}

func runCommand(logger *slog.Logger, args []string) error {
	if len(args) != 3 || args[0] != "grant-role" {
		return errors.New("usage: account grant-role <phone> <role>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	svc, err := build(ctx, logger)
	if err != nil {
		return err
	}
	if err := svc.GrantRole(ctx, args[1], args[2]); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}
