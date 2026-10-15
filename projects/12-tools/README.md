# 12-tools · 2.1 Function Calling / Tool Use 练习

对应课件：[`notes/02-tools/01-function-calling.md`](../../notes/02-tools/01-function-calling.md)

纯 Go 标准库，不调真实 API。

| 文件 | 内容 |
|---|---|
| `schema.go` | 工具定义、Lint（名字、描述、参数说明、strict 必填）、渲染成两家格式、参数校验 |
| `wire.go` | 解析两家的调用；生成两家的结果；检查 Anthropic 消息历史的格式规则 |
| `stream.go` | 按 index 拼接交错到达的流式参数片段 |
| `exec.go` | 执行器：只读并发（有上限、去重）、副作用串行失败即停、超时、panic、未知工具都变成错误结果 |
| `loop.go` | 最小的工具调用循环 |
| `tools_test.go` | 13 个测试 |

```bash
make test
```

练习 2（`Plan`）和练习 3（`ToAnthropic`）需要你自己写，参考答案在课件末尾。
