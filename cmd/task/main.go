// task 任务活动服务：gRPC + 充值进度消费者 + 领奖补偿。
package main

import (
	"context"
	"time"

	"golang.org/x/sync/errgroup"

	taskv1 "gamepub/api/gen/task/v1"
	walletv1 "gamepub/api/gen/wallet/v1"
	"gamepub/internal/app"
	"gamepub/internal/events"
	"gamepub/internal/task"
	"gamepub/pkg/config"
	"gamepub/pkg/logx"
	"gamepub/pkg/mq"
)

func main() {
	logger := logx.New("task", config.String("LOG_LEVEL", "info"))
	app.Main(logger, func(ctx context.Context, g *errgroup.Group) error {
		db, err := app.OpenDB(task.Models()...)
		if err != nil {
			return err
		}
		walletConn, err := app.Dial("WALLET_ADDR", "dns:///wallet:9002")
		if err != nil {
			return err
		}
		svc := task.NewService(db, walletv1.NewWalletServiceClient(walletConn), logger)

		srv, err := app.GRPCServer(logger)
		if err != nil {
			return err
		}
		taskv1.RegisterTaskServiceServer(srv, task.NewGRPCServer(svc))
		if err := app.ServeGRPC(ctx, g, srv, config.String("GRPC_ADDR", ":9004"), logger); err != nil {
			return err
		}

		brokers := config.List("KAFKA_BROKERS", []string{"127.0.0.1:9092"})
		reader := mq.NewKafkaReader(brokers, "task-progress", events.TopicOrderPaid)
		dlq := mq.NewKafkaPublisher(brokers)
		consumer := mq.NewConsumer(mq.ConsumerConfig{Name: "task-progress", DLQTopic: events.TopicOrderPaidDLQ, MaxAttempts: 5}, reader, dlq, svc.HandleOrderPaid, logger)
		app.Background(g, "progress-consumer", logger, func() error {
			defer app.Close(logger, "kafka reader", reader)
			defer app.Close(logger, "dlq publisher", dlq)
			defer app.Close(logger, "wallet grpc conn", walletConn)
			return consumer.Run(ctx)
		})
		app.Background(g, "claim-compensator", logger, func() error { return svc.RunCompensator(ctx, time.Minute) })
		return nil
	})
}
