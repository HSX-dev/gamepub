// Package logx 基于 log/slog 的结构化日志：统一 JSON 输出、按字段名自动脱敏、自动携带 request_id。
package logx

import (
	"context"
	"io"
	"log/slog"
	"os"

	"gamepub/pkg/mask"
)

type requestIDKey struct{}

// WithRequestID 将请求 ID 写入 ctx，后续 *Context 日志会自动带上。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID 读取 ctx 中的请求 ID。
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// New 创建输出到 stdout 的 logger。level 取值 debug/info/warn/error，非法值按 info 处理。
func New(service, level string) *slog.Logger {
	return NewWithWriter(os.Stdout, service, level)
}

// NewWithWriter 便于测试注入输出。
func NewWithWriter(w io.Writer, service, level string) *slog.Logger {
	var lv slog.Level
	_ = lv.UnmarshalText([]byte(level))
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv, ReplaceAttr: maskAttr})
	return slog.New(ctxHandler{h}).With("service", service)
}

// Discard 返回丢弃所有输出的 logger，供测试使用。
func Discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// maskAttr 在序列化前统一脱敏：业务代码即使误把 password / phone / token 打进日志也不会明文落盘。
func maskAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup || !mask.IsSensitiveKey(a.Key) {
		return a
	}
	masked, _ := mask.ByKey(a.Key, a.Value.String())
	return slog.String(a.Key, masked)
}

type ctxHandler struct{ slog.Handler }

func (h ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestID(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h ctxHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return ctxHandler{h.Handler.WithAttrs(attrs)}
}

func (h ctxHandler) WithGroup(name string) slog.Handler {
	return ctxHandler{h.Handler.WithGroup(name)}
}
