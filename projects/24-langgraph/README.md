# 24-langgraph · 4.1 LangGraph：状态图、持久化与 interrupt

对应课件：[`notes/04-frameworks/01-langgraph.md`](../../notes/04-frameworks/01-langgraph.md)

用 Go 重写 LangGraph 运行时的核心，只保留和设计有关的部分，纯标准库。

| 文件 | 内容 | 对应 LangGraph |
|---|---|---|
| `graph.go` | 通道与 reducer（`Append`、`Overwrite`）、`Send`、`Command`、`Builder`、编译期结构检查 | `StateGraph`、`Annotated[..., reducer]`、`Send`、`Command`、`compile()` |
| `checkpoint.go` | `Checkpoint`、`Write`（pending writes）、`Checkpointer` 接口、`MemSaver`（JSON 往返，强制可序列化） | `StateSnapshot`、`checkpoint_writes`、`BaseCheckpointSaver`、`InMemorySaver` |
| `pregel.go` | super-step 主循环、`Invoke` / `Continue` / `Resume` / `Replay` / `UpdateState`、`Runtime.Interrupt` | `invoke`、`Command(resume=)`、`update_state`、`get_state_history`、`interrupt()` |
| `graph_test.go` | 15 个测试 | |

```bash
make test
```

练习 2（durability 模式）和练习 3（task：恢复时不重复的副作用）需要你自己写，参考答案在课件末尾。
