# 09-reasoning · 1.4 推理模型练习

对应课件：[`notes/01-llm/04-reasoning-models.md`](../../notes/01-llm/04-reasoning-models.md)

纯 Go 标准库，不调真实 API。

| 文件 | 内容 |
|---|---|
| `effort.go` | 统一推理强度枚举、模型能力表（2026-10 文档摘录）、`Resolve` 映射与降级 |
| `usage.go` | 三家推理 token 用量统一、成本、两种截断的诊断 |
| `history.go` | 用 HMAC 模拟思考块签名：绑定模型与前缀、跨模型丢弃、只保留最近一轮 |
| `value.go` | 逐级升级与多数投票的期望正确率和 token |
| `reasoning_test.go` | 17 个测试 |

```bash
make test
```

练习 3（不完美验证器 `EscalateVerified`）需要你自己写，参考答案在课件末尾。
