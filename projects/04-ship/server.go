package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// version 在构建时注入：go build -ldflags "-X main.version=$(git rev-parse --short HEAD)"
var version = "dev"

type Server struct {
	cfg      Config
	log      *slog.Logger
	draining atomic.Bool // 收到 SIGTERM 后置为 true，/readyz 开始返回 503
	inflight atomic.Int64
}

func NewServer(cfg Config, log *slog.Logger) *Server {
	return &Server{cfg: cfg, log: log}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, version)
	})
	mux.HandleFunc("POST /chat", s.chat)
	return s.logRequests(mux)
}

// healthz：进程活着就 200。K8s 的 liveness 探针用它，失败会重启容器。
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

// readyz：能不能接新流量。正在下线时返回 503，负载均衡就不再转新请求过来。
func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	if s.draining.Load() {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ready")
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	s.inflight.Add(1)
	defer s.inflight.Add(-1)

	var req struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	rc := http.NewResponseController(w) // 能穿透中间件包装器拿到 Flush
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	ctx := r.Context()
	for _, tok := range strings.Fields("echo: " + req.Prompt) {
		select {
		case <-ctx.Done():
			s.log.Info("client gone", "err", ctx.Err())
			return
		case <-time.After(s.cfg.TokenInterval):
		}
		b, _ := json.Marshal(map[string]string{"text": tok})
		fmt.Fprintf(w, "event: token\ndata: %s\n\n", b)
		if err := rc.Flush(); err != nil {
			return
		}
	}
	fmt.Fprint(w, "event: done\ndata: [DONE]\n\n")
	_ = rc.Flush()
}

// statusRecorder 记录状态码。注意它必须实现 Unwrap，否则 NewResponseController 拿不到 Flush。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// logRequests：每个请求一行结构化日志（JSON），方便 docker logs / 日志平台检索。
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			return // 探针每几秒一次，不记，免得淹没日志
		}
		s.log.Info("request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "dur_ms", time.Since(start).Milliseconds())
	})
}
