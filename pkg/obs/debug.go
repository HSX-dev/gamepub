// Package obs 运维观测端口：pprof 与健康检查。
package obs

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"time"
)

// ServeDebug 在独立端口暴露 pprof 与 /healthz。
//
// 该端口只允许 Pod 内或运维网段访问（NetworkPolicy 限制），绝不能挂到对外网关：
// pprof 会泄露内存内容与调用栈，/debug/pprof/profile 还能被用来消耗 CPU。
// 注意这里使用独立的 ServeMux，而不是 import _ "net/http/pprof" 注册到 DefaultServeMux。
func ServeDebug(ctx context.Context, addr string, logger *slog.Logger) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	logger.Info("debug server listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
