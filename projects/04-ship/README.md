# 04-ship · 0.5 工程基础练习

对应课件：[`notes/00-foundations/05-engineering.md`](../../notes/00-foundations/05-engineering.md)

一个可以直接上线的最小 Agent 服务骨架（只用标准库）：

| 文件 | 内容 |
|---|---|
| `config.go` | 配置全部来自环境变量，密钥不进镜像 |
| `server.go` | `/healthz`、`/readyz`、`/version`、`POST /chat`（SSE）；JSON 结构化日志中间件 |
| `main.go` | `signal.NotifyContext` 接 SIGTERM；draining → Shutdown → 超时强关 |
| `main_test.go` | 优雅退出、超时强关、draining 时 readyz 返回 503 等 6 个测试 |
| `Dockerfile` | 多阶段构建 + distroless nonroot，镜像约 10 MB |
| `../../.github/workflows/ci.yml` | 每次 push 跑 Go / TS 测试并构建镜像 |

```bash
make test                 # go vet + go test -race
make run                  # 本地运行，另开终端：curl -N -X POST localhost:8080/chat -d '{"prompt":"你好 Agent"}'
make docker-build docker-run
```
