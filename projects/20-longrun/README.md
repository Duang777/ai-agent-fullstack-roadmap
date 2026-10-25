# 20-longrun · 3.4 长任务 Agent：检查点、可恢复执行、Human-in-the-loop

对应课件：[`notes/03-agents/04-long-running.md`](../../notes/03-agents/04-long-running.md)

一个能暂停、崩溃后能接着跑的 agent 运行时。模型用脚本化假实现，外部系统用一个按幂等键去重的假银行。

| 文件 | 内容 |
|---|---|
| `checkpoint.go` | 消息、检查点、决定的类型；`Store` 接口（按 seq 比较并交换）；`MemStore`；`FileStore`（临时文件 + fsync + rename、runID 校验）；`Ledger` 工具结果账本 |
| `runner.go` | `Start` / `Continue` / `Resume`；每次模型调用和每个工具结果后写检查点；fail closed 的审批判断；同意 / 拒绝 / 改参数、部分决定、过期按拒绝；版本检查；账本 + 幂等键 |
| `longrun_test.go` | 15 个测试：并发 Resume 只有一个生效、两种崩溃时刻、跨进程恢复、版本不一致拒绝等 |

```bash
make test
```

练习 2（Postgres 存储，账本和检查点同一事务）和练习 3（自动审批裁判）需要你自己写，参考答案在课件末尾。
