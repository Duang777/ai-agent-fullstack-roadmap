# 14-a2a · 2.3 A2A 练习

对应课件：[`notes/02-tools/03-a2a.md`](../../notes/02-tools/03-a2a.md)

按 A2A **1.0** 规范实现的最小远程 Agent 和客户端，纯 Go 标准库，只做 JSON-RPC 绑定的核心子集。生产环境用官方 SDK `github.com/a2aproject/a2a-go/v2`。

| 文件 | 内容 |
|---|---|
| `types.go` | Part / Message / Task / Artifact / StreamResponse / AgentCard、状态和错误码 |
| `server.go` | 任务状态机、contextId 与 taskId 规则、messageId 幂等、阻塞与 returnImmediately、SSE、取消、ListTasks、版本头、Agent Card |
| `client.go` | 取 Agent Card、Send / Stream / GetTask / Cancel，以及把远程 Agent 包装成工具的 `RemoteAgentTool` |
| `demo.go` | 示例 Agent：订机票（input-required）、写行程（流式分块）、慢任务、失败、ping |
| `a2a_test.go` | 13 个测试 |

```bash
make test
```

练习 2（SubscribeToTask）和练习 3（选择接口）需要你自己写，参考答案在课件末尾。
