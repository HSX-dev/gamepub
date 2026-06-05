// order 订单服务：gRPC + 发件箱投递 + 发货通知消费者。
package main

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sync/errgroup"

	orderv1 "gamepub/api/gen/order/v1"
	"gamepub/internal/app"
	"gamepub/internal/events"
	"gamepub/internal/order"
	"gamepub/pkg/config"
	"gamepub/pkg/logx"
	"gamepub/pkg/mq"
	"gamepub/pkg/netguard"
	"gamepub/pkg/redislock"
	"gamepub/pkg/resilience"
	"gamepub/pkg/signature"
)

func main() {
	logger := logx.New("order", config.String("LOG_LEVEL", "info"))
	app.Main(logger, func(ctx context.Context, g *errgroup.Group) error {
		paySecret, err1 := config.Secret("PAY_MOCKPAY_SECRET", 32)
		deliverySecret, err2 := config.Secret("DELIVERY_SIGN_SECRET", 32)
		if err := errors.Join(err1, err2); err != nil {
			return err
		}
		db, err := app.OpenDB(order.Models()...)
		if err != nil {
			return err
		}
		rdb, err := app.OpenRedis(ctx)
		if err != nil {
			return err
		}

		guard := netguard.Default()
		httpClient := resilience.New(guard.HTTPClient(5*time.Second), resilience.DefaultConfig())
		svc := order.NewService(order.Options{
			DB: db,
			PayVerifiers: map[string]*signature.Verifier{
				"mockpay": signature.NewVerifier("mockpay", []byte(paySecret), 5*time.Minute, signature.NewRedisNonceStore(rdb, "pay:nonce:")),
			},
			Guard:      guard,
			Notifier:   order.NewNotifier(httpClient, []byte(deliverySecret)),
			PayBaseURL: config.String("PAY_BASE_URL", "https://pay.example.com/checkout"),
			Logger:     logger,
		})

		srv, err := app.GRPCServer(logger)
		if err != nil {
			return err
		}
		orderv1.RegisterOrderServiceServer(srv, order.NewGRPCServer(svc))
		if err := app.ServeGRPC(ctx, g, srv, config.String("GRPC_ADDR", ":9003"), logger); err != nil {
			return err
		}

		brokers := config.List("KAFKA_BROKERS", []string{"127.0.0.1:9092"})
		pub := mq.NewKafkaPublisher(brokers)
		relay := order.NewRelay(db, pub, redislock.New(rdb), config.Duration("OUTBOX_INTERVAL", time.Second), logger)
		app.Background(g, "outbox-relay", logger, func() error { return relay.Run(ctx) })

		reader := mq.NewKafkaReader(brokers, "order-delivery", events.TopicOrderPaid)
		consumer := mq.NewConsumer(mq.ConsumerConfig{Name: "order-delivery", DLQTopic: events.TopicOrderPaidDLQ, MaxAttempts: 5, HandlerTimeout: 30 * time.Second}, reader, pub, svc.HandleOrderPaidDelivery, logger)
		app.Background(g, "delivery-consumer", logger, func() error {
			defer app.Close(logger, "kafka reader", reader)
			defer app.Close(logger, "kafka publisher", pub)
			return consumer.Run(ctx)
		})
		return nil
	})
}
