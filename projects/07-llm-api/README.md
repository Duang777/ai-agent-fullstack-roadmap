# 07-llm-api · 1.2 主流模型 API 练习

对应课件：[`notes/01-llm/02-mainstream-apis.md`](../../notes/01-llm/02-mainstream-apis.md)

一个只用 Go 标准库写成的统一客户端，同时对接 OpenAI Responses、Anthropic Messages、Gemini Interactions。原则：**同家无损（原样回放原始 JSON），跨家降级（只转换文本、工具调用、工具结果）**。

| 文件 | 内容 |
|---|---|
| `llm.go` | 统一类型（`Message` / `Part` / `Response` / `Event`）、`Provider` 接口、`APIError` 与重试分类、HTTP 与三层检查、`runStream`、`Collect` |
| `sse.go` | SSE 解析器 |
| `openai.go` | Responses：item 编码、reasoning `encrypted_content` 回放、语义事件流 |
| `anthropic.go` | Messages：内容块、`tool_result` 规则、thinking `signature`、按 index 拼装流、缓存用量换算 |
| `gemini.go` | Interactions：steps 时间线、`requires_action`、thought `signature`、两种参数增量写法容错 |
| `agent.go` | `RunTools`：对三家完全相同的工具循环 |
| `llm_test.go` | 20 个测试 |
| `testdata/` | 按 2026 年 10 月官方文档格式手写的响应与 SSE fixture |
| `cmd/chat/` | 用真实 API Key 试一下 |

```bash
make test
go run ./cmd/chat -p anthropic -m <模型名> "用一句话解释 KV Cache"
go run ./cmd/chat -p gemini -m <模型名> -tools "现在几点？再算一下 17*23"
```

练习 3（`retry.go`：指数退避 + 全抖动 + Retry-After，流式只在未推出事件前重试）需要你自己写，参考答案在课件末尾。
