# 25-agents-sdk · 4.2 OpenAI Agents SDK：Runner 循环、handoff、guardrail 与 RunState

对应课件：[`notes/04-frameworks/02-openai-agents-sdk.md`](../../notes/04-frameworks/02-openai-agents-sdk.md)

用 Go 重写 OpenAI Agents SDK 运行时的核心，纯标准库，测试用一个按脚本返回的假模型，不需要 API key。

| 文件 | 内容 | 对应 SDK |
|---|---|---|
| `types.go` | `Item`、`Agent`、`FunctionTool`、`Handoff`（`transfer_to_<name>`）、`RemoveAllTools`、`AsTool`、三种 guardrail | `Agent`、`function_tool`、`handoff()`、`handoff_filters`、`Agent.as_tool()`、guardrails |
| `state.go` | `RunState`（`Approve` / `Reject` / `ToJSON` / `StateFromJSON`）、sticky 决定、`Session`、`MemSession` | `RunState`、`ToolApprovalItem`、`Session` |
| `runner.go` | Runner 循环、工具并发与限流、审批暂停与恢复、handoff 与 input filter、输出 guardrail 与 session 写入 | `Runner.run()`、`RunConfig`、`error_handlers` |
| `agents_test.go` | 18 个测试 | |

```bash
make test
```

练习 2（服务端审批存储与防重放）和练习 3（有副作用工具的幂等键）需要你自己写，参考答案在课件末尾。
