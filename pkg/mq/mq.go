// Package mq 消息队列抽象与可靠消费框架。
//
// 消费语义：至少一次（at-least-once）。框架保证“处理成功或已转入死信”之后才提交位点；
// 业务侧必须幂等（唯一索引 / 消费记录表），这是重复投递下资金正确性的最后防线。
package mq

import (
	"context"
	"errors"
)

// Message 消费到的消息。
type Message struct {
	Topic     string
	Partition int
	Offset    int64
	Key       []byte
	Value     []byte
	Headers   map[string]string

	raw any // 底层实现的原始消息，用于提交位点
}

// OutMessage 待发送的消息。
type OutMessage struct {
	Topic   string
	Key     []byte
	Value   []byte
	Headers map[string]string
}

// Reader 消费端抽象。
type Reader interface {
	Fetch(ctx context.Context) (Message, error)
	Commit(ctx context.Context, msgs ...Message) error
	Close() error
}

// Publisher 生产端抽象。
type Publisher interface {
	Publish(ctx context.Context, msgs ...OutMessage) error
	Close() error
}

// Handler 业务处理函数。返回 Permanent 包装的错误表示重试无意义，直接转入死信。
type Handler func(ctx context.Context, msg Message) error

type permanentError struct{ err error }

func (e permanentError) Error() string { return "permanent: " + e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent 标记不可重试错误（报文格式错误、业务校验失败等）。
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

// IsPermanent 判断是否为不可重试错误。
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}
