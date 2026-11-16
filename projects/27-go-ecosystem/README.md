# 27-go-ecosystem · 4.4 Go 生态：Eino、Genkit Go 与官方 MCP Go SDK

对应课件：[`notes/04-frameworks/04-go-ecosystem.md`](../../notes/04-frameworks/04-go-ecosystem.md)

前三课是用 Go 重写别人的运行时，这一课直接用真实的库：

| 依赖 | 版本 |
|---|---|
| `github.com/cloudwego/eino` | v0.9.23 |
| `github.com/firebase/genkit/go` | v1.14.0 |
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 |

测试里的模型都是按脚本回复的假模型，不需要 API key，也不访问网络（下载依赖除外）。需要 Go 1.25 以上。

| 文件 | 内容 |
|---|---|
| `eino_model.go` | 实现 Eino `ToolCallingChatModel` 的脚本模型，支持流式分片 |
| `eino_graph.go` | 用 `compose.Graph` 写的确定性工单分流图（局部状态 + 分支），以及把图包装成工具 |
| `eino_agent.go` | ADK 的 `ChatModelAgent` + `Runner`、事件收集、需要人工审批的退款工具（中断 / 恢复 / checkpoint） |
| `genkit_part.go` | Genkit 的脚本模型、`tool.Fail` 与普通错误、可恢复的转账工具、flow |
| `mcp_bridge.go` | 用官方 SDK 写 MCP 服务端（类型化工具、错误结果、MRTR 确认），以及 MCP → Eino、MCP → Genkit 两个适配器 |
| `*_test.go` | 22 个测试 |

```
make test
```

练习 2（把 MCP 的多轮请求变成 Eino 中断）和练习 3（Genkit 版）需要你自己写，参考答案在课件末尾。
