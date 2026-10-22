# 18-loop · 3.2 Agent Loop 设计：停止条件、预算、重试

对应课件：[`notes/03-agents/02-agent-loop.md`](../../notes/03-agents/02-agent-loop.md)

在 3.1 的 ReAct 循环外面加上生产需要的三层。模型用脚本化的假实现代替，时钟、随机数和 `Sleep` 都可注入，测试不真的等待。

| 文件 | 内容 |
|---|---|
| `stop.go` | 把 Anthropic `stop_reason` 和 OpenAI Responses `status` / `incomplete_details` 归一成 7 种停止原因 |
| `budget.go` | 五个维度的预算（步数、工具次数、token、费用、墙钟），整数微美元计费、向上取整 |
| `retry.go` | 可重试错误分类（含额度类 429）、全抖动指数退避、retry-after 下限、总等待上限、可取消等待 |
| `loop.go` | Agent Loop：按停止原因分支（续写、原样发回、拒绝、窗口满）、预算用完时的收尾调用、工具超时 |
| `loop_test.go` | 14 个测试 |

```bash
make test
```

练习 2（硬性费用上限 `Reserve`）和练习 3（备用模型）需要你自己写，参考答案在课件末尾。
