# 03-streaming · 0.4 流式协议练习

对应课件：[`notes/00-foundations/04-streaming.md`](../../notes/00-foundations/04-streaming.md)

| 文件 | 内容 |
|---|---|
| `src/sse.ts` | 符合规范的 SSE 解析器：任意切块、三种行尾、多行 data、注释、id、retry；字节流解析与编码 |
| `src/server.ts` | `POST /chat` SSE 服务：响应头、心跳、`Last-Event-ID` 续传、客户端断开即停 |
| `src/client.ts` | `fetch` + SSE 客户端：状态码 / Content-Type 校验，未收到 `done` 视为失败 |
| `src/*.test.ts` | 13 个用例，含“任意两点切块”模糊测试和 1 字节一块的中文测试 |

```bash
npm install
npm run typecheck && npm test
npm run dev   # 终端里逐字打印
```

运行时零依赖，只需要 Node 22+。
