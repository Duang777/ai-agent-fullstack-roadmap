# 01-ts-agent-core · 0.2 TypeScript 练习

| 文件 | 内容 |
|---|---|
| `src/events.ts` | 练习 1：Zod 可辨识联合 + `parseEvent` |
| `src/sleep.ts` | 练习 2：可取消的 `sleep(ms, signal)` |
| `src/runTools.ts` | 练习 3：worker 池并行工具，限流 + 单工具超时 |
| `src/sse.ts` | AsyncGenerator 解析 SSE |
| `src/sse.test.ts` | 练习 4：客户端 abort 后断言服务端收到 close |

```bash
npm install
npm run typecheck   # tsc --noEmit，零错误
npm test            # vitest run：4 files, 9 tests passed
```

课件：[notes/00-foundations/02-typescript.md](../../notes/00-foundations/02-typescript.md)
