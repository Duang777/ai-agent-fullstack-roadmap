package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)) // 日志写 stdout，交给容器运行时收集
	cfg, err := LoadConfig(os.Getenv)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	// docker stop / K8s 删除 Pod 发的是 SIGTERM；本地 Ctrl+C 是 SIGINT
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	if err := Run(ctx, ln, cfg, log); err != nil {
		log.Error("run", "err", err)
		os.Exit(1)
	}
}

// Run 启动服务，ctx 取消后优雅退出。拆出来是为了能在测试里调用。
func Run(ctx context.Context, ln net.Listener, cfg Config, log *slog.Logger) error {
	s := NewServer(cfg, log)
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second, // 防慢速攻击；不设 WriteTimeout，否则长 SSE 会被掐断
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", ln.Addr().String(), "version", version)
		errc <- srv.Serve(ln)
	}()

	select {
	case err := <-errc: // 启动就失败了
		return err
	case <-ctx.Done():
	}

	// 优雅退出三步：
	// 1. 标记 draining，/readyz 返回 503，让负载均衡摘掉这台
	s.draining.Store(true)
	log.Info("shutting down", "inflight", s.inflight.Load())

	// 2. Shutdown：关闭监听，不接新连接，等进行中的请求结束（最多 ShutdownTimeout）
	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	err := srv.Shutdown(sctx)

	// 3. 超时还没结束的，强制关闭
	if errors.Is(err, context.DeadlineExceeded) {
		log.Warn("shutdown timeout, forcing close", "inflight", s.inflight.Load())
		_ = srv.Close()
	}
	if e := <-errc; e != nil && !errors.Is(e, http.ErrServerClosed) {
		return e
	}
	log.Info("bye")
	return err
}
