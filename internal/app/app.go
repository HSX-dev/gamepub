// Package app 各服务 main 共用的启动骨架：信号处理、gRPC 服务、调试端口、后台任务的统一生命周期管理。
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"gorm.io/gorm"

	"gamepub/pkg/config"
	"gamepub/pkg/dbx"
	"gamepub/pkg/grpcx"
	"gamepub/pkg/obs"
)

// Main 运行服务并在失败时以非零码退出。
func Main(logger *slog.Logger, run func(ctx context.Context, g *errgroup.Group) error) {
	if err := serve(logger, run); err != nil {
		logger.Error("service stopped with error", "err", err)
		os.Exit(1)
	}
	logger.Info("service stopped")
}

func serve(logger *slog.Logger, run func(ctx context.Context, g *errgroup.Group) error) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g, gctx := errgroup.WithContext(ctx)
	if addr := config.String("DEBUG_ADDR", "127.0.0.1:6060"); addr != "off" {
		g.Go(func() error { return obs.ServeDebug(gctx, addr, logger) })
	}
	if err := run(gctx, g); err != nil {
		stop() // 让已启动的组件退出，再返回启动错误
		_ = g.Wait()
		return fmt.Errorf("startup: %w", err)
	}
	return g.Wait()
}

// Close 关闭资源并记录错误。Kafka 生产者的 Close 会冲刷缓冲区，错误意味着可能有消息未发出，不能静默忽略。
func Close(logger *slog.Logger, name string, c io.Closer) {
	if err := c.Close(); err != nil {
		logger.Warn("close failed", "resource", name, "err", err)
	}
}

// ServeGRPC 启动 gRPC 服务，ctx 取消时优雅停止。
func ServeGRPC(ctx context.Context, g *errgroup.Group, srv *grpc.Server, addr string, logger *slog.Logger) error {
	lis, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	g.Go(func() error {
		logger.Info("grpc listening", "addr", addr)
		if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		grpcx.GracefulStop(srv, 15*time.Second)
		return nil
	})
	return nil
}

// Background 启动后台任务；任务返回错误会触发整个进程退出（由 K8s 重启），而不是静默停摆。
func Background(g *errgroup.Group, name string, logger *slog.Logger, fn func() error) {
	g.Go(func() error {
		logger.Info("background task started", "task", name)
		if err := fn(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	})
}

// OpenDB 从 MYSQL_DSN 打开数据库；AUTO_MIGRATE=true 时执行迁移（仅限开发环境，生产使用 migrations/*.sql）。
func OpenDB(models ...any) (*gorm.DB, error) {
	dsn, err := config.Secret("MYSQL_DSN", 10)
	if err != nil {
		return nil, err
	}
	pool := dbx.DefaultPool()
	pool.MaxOpen = config.Int("DB_MAX_OPEN", pool.MaxOpen)
	db, err := dbx.OpenMySQL(dsn, pool)
	if err != nil {
		return nil, err
	}
	if config.Bool("AUTO_MIGRATE", false) {
		if err := db.AutoMigrate(models...); err != nil {
			return nil, fmt.Errorf("auto migrate: %w", err)
		}
	}
	return db, nil
}

// OpenRedis 从 REDIS_ADDR / REDIS_PASSWORD 打开 Redis，并做一次连通性检查。
// 口令为必填：nonce 防重放、限流、分布式锁都依赖 Redis，无认证的 Redis 可被内网任意 Pod 篡改。
func OpenRedis(ctx context.Context) (*redis.Client, error) {
	password, err := config.Secret("REDIS_PASSWORD", 16)
	if err != nil {
		return nil, err
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:         config.String("REDIS_ADDR", "127.0.0.1:6379"),
		Password:     password,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  time.Second,
		WriteTimeout: time.Second,
		PoolSize:     config.Int("REDIS_POOL_SIZE", 50),
	})
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := rdb.Ping(pctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return rdb, nil
}

// GRPCServer 创建带统一拦截器的 gRPC 服务端。
func GRPCServer(logger *slog.Logger) (*grpc.Server, error) {
	token, err := config.Secret("INTERNAL_TOKEN", 32)
	if err != nil {
		return nil, err
	}
	return grpcx.NewServer(grpcx.ServerOptions{InternalToken: token, Logger: logger})
}

// Dial 创建到其它服务的 gRPC 连接。
func Dial(targetEnv, def string) (*grpc.ClientConn, error) {
	token, err := config.Secret("INTERNAL_TOKEN", 32)
	if err != nil {
		return nil, err
	}
	return grpcx.Dial(config.String(targetEnv, def), grpcx.ClientOptions{
		InternalToken:  token,
		DefaultTimeout: config.Duration("RPC_TIMEOUT", 3*time.Second),
	})
}
