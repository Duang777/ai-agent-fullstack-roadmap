# 19-multiagent · 3.3 多智能体：Supervisor、Handoff、Swarm

对应课件：[`notes/03-agents/03-multi-agent.md`](../../notes/03-agents/03-multi-agent.md)

一个能跑多个 agent 的运行时。模型用按 agent 分的脚本化假实现代替。

| 文件 | 内容 |
|---|---|
| `agent.go` | 消息、Agent、共享预算 `Meter`（并发安全、按 agent 记账）、`transfer_to_<名字>` 命名、`RemoveTools` 过滤器 |
| `runtime.go` | Handoff / Swarm 运行时：活跃 agent 跨轮保留、只认第一个转交、转交时不执行其他工具、来回转交上限、并行工具执行 |
| `supervisor.go` | `AsTool`：把 agent 包成工具，隔离上下文、禁止转交、嵌套深度上限 |
| `multiagent_test.go` | 12 个测试 |

```bash
make test
```

练习 2（带元数据的转交、摘要过滤器）和练习 3（ArtifactStore 代替传话）需要你自己写，参考答案在课件末尾。
