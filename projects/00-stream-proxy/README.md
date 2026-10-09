# 00-stream-proxy · 0.1 Go 并发练习

| 文件 | 内容 |
|---|---|
| `fanin.go` | 练习 1：`fanIn` 合并多个 channel，ctx 取消后全部退出 |
| `tools.go` | 练习 2：errgroup 并行工具调用，限流 + 单工具超时 |
| `stream.go` | 可取消的 SSE 流式读取 `streamTokens` |
| `stream_test.go` | 练习 3：客户端取消后断言服务端感知断开 |

```bash
go test -race -count=1 ./...
# ok  github.com/Duang777/ai-agent-fullstack-roadmap/projects/00-stream-proxy  3.522s
```

课件：[notes/00-foundations/01-go-concurrency.md](../../notes/00-foundations/01-go-concurrency.md)
