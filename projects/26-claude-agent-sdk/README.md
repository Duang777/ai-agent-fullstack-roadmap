# 26-claude-agent-sdk · 4.3 Claude Agent SDK：agent 循环、六步权限、hooks、子 agent 与会话

对应课件：[`notes/04-frameworks/03-claude-agent-sdk.md`](../../notes/04-frameworks/03-claude-agent-sdk.md)

用 Go 重写 Claude Agent SDK 运行时的核心，纯标准库，测试用按脚本返回的假模型，不需要 API key。

| 文件 | 内容 | 对应 SDK |
|---|---|---|
| `types.go` | 内容块、消息、`Tool`（只读 / 写文件 / 规则匹配的参数 / 工具自批）、事件流、`Result` | 消息类型、`ResultMessage` |
| `permissions.go` | 六步权限判定、权限模式、规则语法、hooks 的 matcher 与聚合、`canUseTool` | `permission_mode`、`allowed_tools` / `disallowed_tools`、`hooks`、`can_use_tool` |
| `query.go` | agent 循环、工具分组执行、`PostToolUse`、`defer`、子 agent 与三个上限、权限模式继承 | `query()`、`AgentDefinition` |
| `session.go` | 会话的保存与 resume / fork / continue | 会话文件、`SessionStore` |
| `sdk_test.go` | 22 个测试 | |

```
make test
```

练习 2（基于 defer 的异步审批）和练习 3（自动压缩与 PreCompact）需要你自己写，参考答案在课件末尾。
