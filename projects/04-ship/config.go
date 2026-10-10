package main

import (
	"fmt"
	"strconv"
	"time"
)

// Config 全部来自环境变量：同一个镜像，换环境只换变量（12-Factor 第 III 条）。
type Config struct {
	Addr            string        // 监听地址，如 ":8080"
	ShutdownTimeout time.Duration // 收到 SIGTERM 后最多等多久让进行中的请求结束
	TokenInterval   time.Duration // 模拟模型每个 token 的间隔
	APIKey          string        // 上游模型的密钥：只从环境变量读，绝不写进镜像
}

func LoadConfig(getenv func(string) string) (Config, error) {
	c := Config{
		Addr:            envOr(getenv, "ADDR", ":8080"),
		ShutdownTimeout: 25 * time.Second,
		TokenInterval:   100 * time.Millisecond,
		APIKey:          getenv("LLM_API_KEY"),
	}
	if v := getenv("SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("SHUTDOWN_TIMEOUT: %w", err)
		}
		c.ShutdownTimeout = d
	}
	if v := getenv("TOKEN_INTERVAL_MS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Config{}, fmt.Errorf("TOKEN_INTERVAL_MS: invalid %q", v)
		}
		c.TokenInterval = time.Duration(n) * time.Millisecond
	}
	return c, nil
}

func envOr(getenv func(string) string, k, def string) string {
	if v := getenv(k); v != "" {
		return v
	}
	return def
}
