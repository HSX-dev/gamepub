// gateway 对外 HTTP 网关。
package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"

	accountv1 "gamepub/api/gen/account/v1"
	orderv1 "gamepub/api/gen/order/v1"
	taskv1 "gamepub/api/gen/task/v1"
	walletv1 "gamepub/api/gen/wallet/v1"
	"gamepub/internal/app"
	"gamepub/internal/gateway"
	"gamepub/pkg/auth"
	"gamepub/pkg/config"
	"gamepub/pkg/logx"
	"gamepub/pkg/ratelimit"
	"gamepub/pkg/upload"
)

func main() {
	logger := logx.New("gateway", config.String("LOG_LEVEL", "info"))
	app.Main(logger, func(ctx context.Context, g *errgroup.Group) error {
		// 网关只持有公钥：被攻破也无法签发令牌
		pub, err := config.SecretBytes("JWT_ED25519_PUBLIC", ed25519.PublicKeySize)
		if err != nil {
			return err
		}
		verifier, err := auth.NewVerifier(pub, "gamepub-account", "gamepub-api")
		if err != nil {
			return err
		}
		rdb, err := app.OpenRedis(ctx)
		if err != nil {
			return err
		}

		accountConn, err1 := app.Dial("ACCOUNT_ADDR", "dns:///account:9001")
		walletConn, err2 := app.Dial("WALLET_ADDR", "dns:///wallet:9002")
		orderConn, err3 := app.Dial("ORDER_ADDR", "dns:///order:9003")
		taskConn, err4 := app.Dial("TASK_ADDR", "dns:///task:9004")
		if err := errors.Join(err1, err2, err3, err4); err != nil {
			return err
		}

		router, err := gateway.NewRouter(gateway.Deps{
			Account:  accountv1.NewAccountServiceClient(accountConn),
			Wallet:   walletv1.NewWalletServiceClient(walletConn),
			Order:    orderv1.NewOrderServiceClient(orderConn),
			Task:     taskv1.NewTaskServiceClient(taskConn),
			Verifier: verifier,
			Limiters: gateway.Limiters{
				Login:  ratelimit.NewRedis(rdb, "rl:login:", ratelimit.Rule{Rate: 0.2, Burst: 5}, logger),
				User:   ratelimit.NewRedis(rdb, "rl:user:", ratelimit.Rule{Rate: 20, Burst: 40}, logger),
				Batch:  ratelimit.NewRedis(rdb, "rl:batch:", ratelimit.Rule{Rate: 1, Burst: 3}, logger),
				Notify: ratelimit.NewRedis(rdb, "rl:notify:", ratelimit.Rule{Rate: 50, Burst: 100}, logger),
			},
			Storage: upload.LocalStorage{Root: config.String("UPLOAD_DIR", "/data/uploads")},
			Logger:  logger,
		}, gateway.Config{TrustedProxies: config.List("TRUSTED_PROXIES", nil)})
		if err != nil {
			return err
		}

		srv := &http.Server{
			Addr:              config.String("HTTP_ADDR", ":8080"),
			Handler:           router,
			ReadHeaderTimeout: 5 * time.Second, // 防 Slowloris
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      15 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    32 << 10,
		}
		g.Go(func() error {
			logger.Info("http listening", "addr", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
		g.Go(func() error {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			err := srv.Shutdown(sctx)
			for _, c := range []interface{ Close() error }{accountConn, walletConn, orderConn, taskConn} {
				_ = c.Close()
			}
			return err
		})
		return nil
	})
}
