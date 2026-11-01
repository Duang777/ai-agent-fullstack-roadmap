# 21-state · 3.5 Agent 状态与数据模型：事件溯源与完整重放

对应课件：[`notes/03-agents/05-agent-state.md`](../../notes/03-agents/05-agent-state.md)

把 Agent 循环改成“先写事件、再往下走”。run 的全部历史只存在一条只追加的事件流里，其余状态都是投影。模型和工具用脚本化假实现。

| 文件 | 内容 |
|---|---|
| `events.go` | 事件类型与数据结构；`EventStore` 接口；`MemStore`（`expect` 冲突检测、全局位置）；`upcast` 读时升级 |
| `projection.go` | `RunView.Apply`（序号、版本、调用配对检查）；`Open` / `FirstToolError`；`Fold` / `Load`（快照）；`SessionIndex` 追赶；会话历史 |
| `agent.go` | 每一步先写事件的 `Agent`；请求指纹 `RequestHash`；`Replay` 录像重放与分叉检测；`SameEvents` |
| `schema.sql` | Postgres 参考表结构（事实层 `events` + 投影层），练习 2 用 |
| `state_test.go` | 15 个测试：并发追加只有一个成功、快照+尾部等于全量、v1 事件升级、崩溃后找出未完成调用、重放不调用模型和工具、改版本/改工具集合检测分叉、失败 run 定位、会话跨 run |

```bash
make test
```

练习 2（Postgres 事件库 + 投影同事务 + Rebuild）和练习 3（从某一步分叉）需要你自己写，参考答案在课件末尾。
