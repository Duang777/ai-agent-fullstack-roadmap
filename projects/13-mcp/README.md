# 13-mcp · 2.2 MCP 练习

对应课件：[`notes/02-tools/02-mcp.md`](../../notes/02-tools/02-mcp.md)

按 MCP **2026-07-28** 规范实现的最小 Server 和 Client，纯 Go 标准库。只覆盖核心子集，用来理解机制，不是完整 SDK。

| 文件 | 内容 |
|---|---|
| `jsonrpc.go` | JSON-RPC 2.0 消息、错误码、`_meta` 键 |
| `server.go` | 每请求 `_meta` 校验、`server/discover`、tools / resources / prompts、MRTR 与 `requestState` HMAC |
| `demo.go` | 示例 Server：天气、需要确认的 `delete_repo`、用句柄的购物篮、资源和提示词 |
| `stdio.go` | stdio 传输：一行一条消息、并发处理、按 id 对应、取消通知 |
| `http.go` | Streamable HTTP：只接受 POST、Origin 校验、镜像头校验、base64 头编码 |
| `client.go` | Client（自动带 `_meta`、分页、MRTR 循环）和 Host（多 Server 聚合、加前缀、路由） |
| `mcp_test.go` | 13 个测试 |

```bash
make test
```

练习 2（stdio 真正的取消）和练习 3（版本协商）需要你自己写，参考答案在课件末尾。
