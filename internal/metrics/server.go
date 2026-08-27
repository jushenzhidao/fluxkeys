package metrics

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Server 在独立端口暴露 /metrics。
//
// 与业务端口分离的理由: 业务端口通常要暴露给外部或经过网关，而指标含有
// Key 用量分布这类内部信息，不应与用户流量共享入口。同时指标抓取不应受
// 业务端口的限流、鉴权中间件影响。
type Server struct {
	srv *http.Server
}

// NewServer 构造指标服务。addr 为空时返回 nil，表示不启用。
func NewServer(addr string, m *Metrics) *Server {
	if addr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	// 指标端口自身的存活检查，便于容器探针复用该端口
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	return &Server{srv: &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}}
}

// Start 在后台监听。返回的 channel 会收到非 ErrServerClosed 的监听错误。
func (s *Server) Start() <-chan error {
	ch := make(chan error, 1)
	if s == nil {
		close(ch)
		return ch
	}
	go func() {
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			ch <- err
			return
		}
		close(ch)
	}()
	return ch
}

// Addr 返回监听地址。
func (s *Server) Addr() string {
	if s == nil {
		return ""
	}
	return s.srv.Addr
}

// Shutdown 优雅关闭指标服务。
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}
