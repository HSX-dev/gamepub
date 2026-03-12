package mq

import (
	"context"
	"time"

	"github.com/segmentio/kafka-go"
)

// KafkaReader 基于消费组的 Reader，关闭自动提交，由 Consumer 在处理完成后显式提交。
type KafkaReader struct{ r *kafka.Reader }

// NewKafkaReader 创建 Kafka 消费者。
func NewKafkaReader(brokers []string, groupID, topic string) *KafkaReader {
	return &KafkaReader{r: kafka.NewReader(kafka.ReaderConfig{
		Brokers:     brokers,
		GroupID:     groupID,
		Topic:       topic,
		MinBytes:    1,
		MaxBytes:    10 << 20,
		MaxWait:     500 * time.Millisecond,
		StartOffset: kafka.FirstOffset,
		// CommitInterval=0：同步提交，CommitMessages 返回即已落到 broker
		CommitInterval: 0,
	})}
}

// Fetch 实现 Reader（FetchMessage 不会自动提交位点）。
func (k *KafkaReader) Fetch(ctx context.Context) (Message, error) {
	m, err := k.r.FetchMessage(ctx)
	if err != nil {
		return Message{}, err
	}
	headers := make(map[string]string, len(m.Headers))
	for _, h := range m.Headers {
		headers[h.Key] = string(h.Value)
	}
	return Message{
		Topic: m.Topic, Partition: m.Partition, Offset: m.Offset,
		Key: m.Key, Value: m.Value, Headers: headers, raw: m,
	}, nil
}

// Commit 实现 Reader。
func (k *KafkaReader) Commit(ctx context.Context, msgs ...Message) error {
	raws := make([]kafka.Message, 0, len(msgs))
	for _, m := range msgs {
		if km, ok := m.raw.(kafka.Message); ok {
			raws = append(raws, km)
		}
	}
	return k.r.CommitMessages(ctx, raws...)
}

// Close 实现 Reader。
func (k *KafkaReader) Close() error { return k.r.Close() }

// KafkaPublisher 生产者：acks=all，按 key 哈希分区以保证同一用户的事件有序。
type KafkaPublisher struct{ w *kafka.Writer }

// NewKafkaPublisher 创建 Kafka 生产者。
func NewKafkaPublisher(brokers []string) *KafkaPublisher {
	return &KafkaPublisher{w: &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Balancer:               &kafka.Hash{},
		RequiredAcks:           kafka.RequireAll,
		BatchTimeout:           10 * time.Millisecond,
		WriteTimeout:           5 * time.Second,
		AllowAutoTopicCreation: false,
	}}
}

// Publish 实现 Publisher。
func (k *KafkaPublisher) Publish(ctx context.Context, msgs ...OutMessage) error {
	out := make([]kafka.Message, 0, len(msgs))
	for _, m := range msgs {
		km := kafka.Message{Topic: m.Topic, Key: m.Key, Value: m.Value}
		for hk, hv := range m.Headers {
			km.Headers = append(km.Headers, kafka.Header{Key: hk, Value: []byte(hv)})
		}
		out = append(out, km)
	}
	return k.w.WriteMessages(ctx, out...)
}

// Close 实现 Publisher。
func (k *KafkaPublisher) Close() error { return k.w.Close() }
