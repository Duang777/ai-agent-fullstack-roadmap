# 28-ts-ecosystem · 4.5 TS 生态：Vercel AI SDK、Mastra 与 OpenAI Agents SDK (JS)

对应课件：[`notes/04-frameworks/05-ts-ecosystem.md`](../../notes/04-frameworks/05-ts-ecosystem.md)

和 4.4 一样直接用真实的库：

| 依赖 | 版本 |
|---|---|
| `ai`（Vercel AI SDK） | 7.0.183 |
| `@mastra/core` | 1.86.0 |
| `@openai/agents` | 0.22.0 |
| `zod` | 4.6.7 |

测试里的模型都是按脚本回复的假模型，不需要 API key。需要 Node 22 以上。

| 文件 | 内容 |
|---|---|
| `src/script.ts` | 继承 AI SDK `MockLanguageModelV4` 的脚本模型，支持流式；AI SDK 和 Mastra 共用 |
| `src/shop.ts` | 三个框架共用的订单库、退款账本、并发计数 |
| `src/aisdk.ts` | `ToolLoopAgent`、`toolApproval` 策略、审批请求与答复的辅助函数、可选的审批签名 |
| `src/mastra.ts` | `createTool`（`requireApproval` 与 `suspend()`）、`Mastra` 实例与 storage、工单分流 workflow |
| `src/openai.ts` | 实现 `Model` 接口的脚本模型、`needsApproval` 工具、`RunState` 恢复 |
| `src/*.test.ts` | 25 个测试 |

```bash
npm install
npm run typecheck
npm test
```

练习 2（三个框架统一的审批接口）和练习 3（硬性拒绝）需要你自己写，参考答案在课件末尾。
