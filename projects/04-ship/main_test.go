package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{ShutdownTimeout: 2 * time.Second, TokenInterval: 20 * time.Millisecond}
}

func start(t *testing.T, cfg Config) (url string, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- Run(ctx, ln, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	url = "http://" + ln.Addr().String()
	waitOK(t, url+"/healthz")
	return url, cancel, errc
}

func waitOK(t *testing.T, url string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if res, err := http.Get(url); err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never became ready", url)
}

func TestLoadConfig(t *testing.T) {
	env := map[string]string{"ADDR": ":9000", "SHUTDOWN_TIMEOUT": "3s", "TOKEN_INTERVAL_MS": "5"}
	c, err := LoadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":9000" || c.ShutdownTimeout != 3*time.Second || c.TokenInterval != 5*time.Millisecond {
		t.Fatalf("got %+v", c)
	}

	_, err = LoadConfig(func(k string) string {
		if k == "SHUTDOWN_TIMEOUT" {
			return "soon"
		}
		return ""
	})
	if err == nil {
		t.Fatal("want error for bad duration")
	}
}

func TestHealthAndReady(t *testing.T) {
	url, cancel, done := start(t, testConfig())
	defer func() { cancel(); <-done }()
	for _, p := range []string{"/healthz", "/readyz", "/version"} {
		res, err := http.Get(url + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%s = %d", p, res.StatusCode)
		}
	}
}

func readTokens(t *testing.T, body io.Reader) (toks []string, sawDone bool) {
	t.Helper()
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		line := sc.Text()
		if line == "data: [DONE]" {
			return toks, true
		}
		if strings.HasPrefix(line, "data: ") {
			toks = append(toks, line)
		}
	}
	return toks, false
}

func TestChatStreams(t *testing.T) {
	url, cancel, done := start(t, testConfig())
	defer func() { cancel(); <-done }()
	res, err := http.Post(url+"/chat", "application/json", strings.NewReader(`{"prompt":"a b c"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type %q", ct)
	}
	toks, ok := readTokens(t, res.Body)
	if !ok || len(toks) != 4 { // "echo:" a b c
		t.Fatalf("toks=%v done=%v", toks, ok)
	}
}

// 核心测试：收到退出信号时，进行中的流要能完整结束，新连接被拒绝。
func TestGracefulShutdownWaitsForInflight(t *testing.T) {
	cfg := testConfig()
	cfg.TokenInterval = 50 * time.Millisecond
	url, cancel, done := start(t, cfg)

	res, err := http.Post(url+"/chat", "application/json", strings.NewReader(`{"prompt":"1 2 3 4 5"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	time.Sleep(60 * time.Millisecond) // 让流先开始
	cancel()                          // 相当于收到 SIGTERM

	toks, ok := readTokens(t, res.Body)
	if !ok || len(toks) != 6 {
		t.Fatalf("in-flight stream was cut: toks=%d done=%v", len(toks), ok)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if _, err := http.Get(url + "/healthz"); err == nil {
		t.Fatal("server still accepting after shutdown")
	}
}

// 超过 ShutdownTimeout 还没结束的请求会被强制断开，Run 返回 DeadlineExceeded。
func TestShutdownTimeoutForcesClose(t *testing.T) {
	cfg := testConfig()
	cfg.TokenInterval = 200 * time.Millisecond
	cfg.ShutdownTimeout = 100 * time.Millisecond
	url, cancel, done := start(t, cfg)

	res, err := http.Post(url+"/chat", "application/json", strings.NewReader(`{"prompt":"1 2 3 4 5 6 7 8"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	time.Sleep(50 * time.Millisecond)
	cancel()

	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if _, ok := readTokens(t, res.Body); ok {
		t.Fatal("stream should have been cut")
	}
}

func TestDrainingReadyz(t *testing.T) {
	s := NewServer(testConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := &http.Server{Handler: s.Handler()}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	defer srv.Close()
	url := "http://" + ln.Addr().String()
	waitOK(t, url+"/readyz")

	s.draining.Store(true)
	res, err := http.Get(url + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz while draining = %d", res.StatusCode)
	}
	res, _ = http.Get(url + "/healthz")
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal("healthz must stay 200 while draining")
	}
}
