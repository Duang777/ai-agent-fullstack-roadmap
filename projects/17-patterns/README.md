# 17-patterns · 3.1 ReAct、Plan-and-Execute、Reflexion 练习

对应课件：[`notes/03-agents/01-react-plan-reflexion.md`](../../notes/03-agents/01-react-plan-reflexion.md)

三种基本 Agent 模式的最小实现。模型用脚本化的假实现（`Script`）代替，测试只关心循环本身写对了没有。

| 文件 | 内容 |
|---|---|
| `model.go` | 消息、工具调用、`Model` 接口与脚本化模型 |
| `react.go` | ReAct 循环：取消、每个调用都有结果、工具错误变观察、步数上限；论文原版文本格式解析 |
| `plan.go` | Plan-and-Execute：`#E1` 引用、计划校验、按依赖分波并行、失败后带着已完成结果重新规划 |
| `reflexion.go` | Reflexion：外部评估、反思记忆窗口、如实返回失败 |
| `patterns_test.go` | 12 个测试 |

```bash
make test
```

练习 2（ReAct 并行执行工具）和练习 3（绕圈检测）需要你自己写，参考答案在课件末尾。
