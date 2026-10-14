# 11-context · 1.6 上下文工程练习

对应课件：[`notes/01-llm/06-context-engineering.md`](../../notes/01-llm/06-context-engineering.md)

纯 Go 标准库，不调真实 API。

| 文件 | 内容 |
|---|---|
| `budget.go` | 窗口检查（放得下 / 可能中途截断 / 直接 400）、token 粗估 |
| `degrade.go` | 玩具退化模型：长度退化 + 位置效应；全塞与检索的对比（参数是编的，只看形状） |
| `assemble.go` | 预算内组装上下文：必需项、优先级、去重、稳定内容在前；工具输出保留头尾 |
| `compact.go` | 60 轮长任务模拟：全部保留、清旧工具结果、清除前记笔记、摘要压缩；`ClearAtLeast` |
| `evalharness.go` | 提示词回归评测：通过率 + 修好 / 弄坏的用例 |
| `context_test.go` | 12 个测试 |

```bash
make test
```

练习 2（`Render`）和练习 3（`Remaining`）需要你自己写，参考答案在课件末尾。
