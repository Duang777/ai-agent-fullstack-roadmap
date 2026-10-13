# 10-prompt-cache · 1.5 Prompt Caching 练习

对应课件：[`notes/01-llm/05-prompt-caching.md`](../../notes/01-llm/05-prompt-caching.md)

纯 Go 标准库，不调真实 API。用累计前缀哈希模拟一台机器上的 KV 缓存索引。

| 文件 | 内容 |
|---|---|
| `cache.go` | 累计前缀哈希、最小长度、TTL 与命中续期；Anthropic 显式断点 + 自动缓存 + 20 块回看窗口；OpenAI implicit / explicit 两种模式 |
| `cost.go` | 读写倍率（2026-10 文档摘录）、“普通输入当量”、最小长度陷阱的盈亏平衡公式、Gemini 显式缓存估算 |
| `diagnose.go` | 两次请求从哪一块开始分叉；断点前的时间戳、断点数量、最小长度检查；map 键顺序与稳定 JSON |
| `sim.go` | 30 轮 Agent 会话四种断点策略对比；5 分钟与 1 小时 TTL 对比 |
| `cache_test.go` | 17 个测试 |

```bash
make test
```

练习 2（`PlaceBreakpoints`）和练习 3（混合 TTL 计费）需要你自己写，参考答案在课件末尾。
