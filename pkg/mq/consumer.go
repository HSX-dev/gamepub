package mq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// ConsumerConfig 消费者参数。
type ConsumerConfig struct {
	Name           string        // 消费者名，用于日志与幂等记录
	DLQTopic       string        // 死信主题
	MaxAttempts    int           // 单条消息最大处理次数（含首次）
	HandlerTimeout time.Duration // 单次处理超时，防止一条慢消息卡死整个分区
	Backoff        func(attempt int) time.Duration
}

// Consumer 可靠消费者：有限重试 → 死信 → 提交位点。
type Consumer struct {
	cfg     ConsumerConfig
	reader  Reader
	dlq     Publisher
	handler Handler
	logger  *slog.Logger
}

// NewConsumer 创建消费者。
func NewConsumer(cfg ConsumerConfig, reader Reader, dlq Publisher, handler Handler, logger *slog.Logger) *Consumer {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.HandlerTimeout <= 0 {
		cfg.HandlerTimeout = 10 * time.Second
	}
	if cfg.Backoff == nil {
		cfg.Backoff = func(attempt int) time.Duration { return time.Duration(attempt) * 200 * time.Millisecond }
	}
	return &Consumer{cfg: cfg, reader: reader, dlq: dlq, handler: handler, logger: logger.With("consumer", cfg.Name)}
}

// Run 阻塞消费直到 ctx 取消。ctx 取消时正在处理的消息不提交，重启后会被重新投递。
func (c *Consumer) Run(ctx context.Context) error {
	if c.cfg.DLQTopic == "" || c.dlq == nil {
		return errors.New("mq: consumer requires a dead letter topic")
	}
	for {
		msg, ok := c.fetch(ctx)
		if !ok || !c.process(ctx, msg) {
			return nil // 正常关闭：已取到但未处理完的消息不提交，重启后重新投递
		}
		if err := c.reader.Commit(ctx, msg); err != nil && ctx.Err() == nil {
			// 提交失败不影响正确性：消息会被再次投递，由业务幂等兜底
			c.logger.WarnContext(ctx, "mq commit failed", "topic", msg.Topic, "partition", msg.Partition, "offset", msg.Offset, "err", err)
		}
	}
}

// fetch 拉取下一条消息，失败时退避重试；返回 false 表示 ctx 已取消，应停止消费。
func (c *Consumer) fetch(ctx context.Context) (Message, bool) {
	for {
		msg, err := c.reader.Fetch(ctx)
		if err == nil {
			return msg, true
		}
		if ctx.Err() != nil {
			return Message{}, false
		}
		c.logger.ErrorContext(ctx, "mq fetch failed", "err", err)
		if !sleep(ctx, time.Second) {
			return Message{}, false
		}
	}
}

// process 处理单条消息，直到“成功”或“已写入死信”才返回 true；仅在 ctx 取消时返回 false。
func (c *Consumer) process(ctx context.Context, msg Message) bool {
	var err error
	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		if err = c.invoke(ctx, msg); err == nil {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		c.logger.WarnContext(ctx, "mq handle failed", "topic", msg.Topic, "offset", msg.Offset, "attempt", attempt, "err", err)
		if IsPermanent(err) {
			break
		}
		if attempt < c.cfg.MaxAttempts && !sleep(ctx, c.cfg.Backoff(attempt)) {
			return false
		}
	}

	// Kafka 位点按分区单调提交：如果跳过这条继续消费，后续位点一提交，这条消息就被“隐式确认”而永久丢失。
	// 因此死信写入成功之前绝不前进，宁可阻塞该分区并告警。
	for i := 1; ; i++ {
		derr := c.toDLQ(ctx, msg, err)
		if derr == nil {
			c.logger.ErrorContext(ctx, "mq message moved to DLQ", "topic", msg.Topic, "offset", msg.Offset, "err", err)
			return true
		}
		c.logger.ErrorContext(ctx, "mq publish to DLQ failed, partition blocked", "attempt", i, "err", derr)
		if !sleep(ctx, c.cfg.Backoff(min(i, 5))) {
			return false
		}
	}
}

func (c *Consumer) invoke(ctx context.Context, msg Message) (err error) {
	hctx, cancel := context.WithTimeout(ctx, c.cfg.HandlerTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			// panic 通常是代码缺陷，重试无意义；recover 防止一条坏消息让整个进程崩溃重启循环
			err = Permanent(fmt.Errorf("panic: %v", r))
		}
	}()
	return c.handler(hctx, msg)
}

func (c *Consumer) toDLQ(ctx context.Context, msg Message, cause error) error {
	headers := make(map[string]string, len(msg.Headers)+5)
	for k, v := range msg.Headers {
		headers[k] = v
	}
	headers["x-dlq-source-topic"] = msg.Topic
	headers["x-dlq-source-partition"] = strconv.Itoa(msg.Partition)
	headers["x-dlq-source-offset"] = strconv.FormatInt(msg.Offset, 10)
	headers["x-dlq-consumer"] = c.cfg.Name
	if cause != nil {
		headers["x-dlq-error"] = truncate(cause.Error(), 512)
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.dlq.Publish(pctx, OutMessage{Topic: c.cfg.DLQTopic, Key: msg.Key, Value: msg.Value, Headers: headers})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
