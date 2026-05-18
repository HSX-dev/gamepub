// wallet 钱包资金服务：gRPC + 充值到账消费者 + 定时对账。
package main

import (
	"context"
	"time"

	"golang.org/x/sync/errgroup"

	walletv1 "gamepub/api/gen/wallet/v1"
	"gamepub/internal/app"
	"gamepub/internal/events"
	"gamepub/internal/wallet"
	"gamepub/pkg/config"
	"gamepub/pkg/logx"
	"gamepub/pkg/mq"
	"gamepub/pkg/redislock"
)

func main() {
	logger := logx.New("wallet", config.String("LOG_LEVEL", "info"))
	app.Main(logger, func(ctx context.Context, g *errgroup.Group) error {
		db, err := app.OpenDB(wallet.Models()...)
		if err != nil {
			return err
		}
		rdb, err := app.OpenRedis(ctx)
		if err != nil {
			return err
		}
		svc := wallet.NewService(db, logger)

		srv, err := app.GRPCServer(logger)
		if err != nil {
			return err
		}
		walletv1.RegisterWalletServiceServer(srv, wallet.NewGRPCServer(svc))
		if err := app.ServeGRPC(ctx, g, srv, config.String("GRPC_ADDR", ":9002"), logger); err != nil {
			return err
		}

		brokers := config.List("KAFKA_BROKERS", []string{"127.0.0.1:9092"})
		reader := mq.NewKafkaReader(brokers, "wallet-recharge", events.TopicOrderPaid)
		dlq := mq.NewKafkaPublisher(brokers)
		consumer := mq.NewConsumer(mq.ConsumerConfig{Name: "wallet-recharge", DLQTopic: events.TopicOrderPaidDLQ, MaxAttempts: 5}, reader, dlq, svc.HandleOrderPaid, logger)
		app.Background(g, "recharge-consumer", logger, func() error {
			defer app.Close(logger, "kafka reader", reader)
			defer app.Close(logger, "dlq publisher", dlq)
			return consumer.Run(ctx)
		})

		reconciler := wallet.NewReconciler(db, redislock.New(rdb), config.Duration("RECONCILE_INTERVAL", 10*time.Minute), logger)
		app.Background(g, "reconciler", logger, func() error { return reconciler.Run(ctx) })
		return nil
	})
}
