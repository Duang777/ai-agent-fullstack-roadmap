// 一个最小的“模型流式输出”SSE 服务：POST /chat 逐个吐 token。
// 演示：正确的响应头、心跳、断点续传（Last-Event-ID）、客户端断开即停止生成。

import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { encodeSSE } from "./sse.ts";

export interface ServerOptions {
  tokens: string[]; // 模拟模型要输出的 token
  tokenIntervalMs: number; // 每个 token 间隔
  heartbeatMs: number; // 心跳间隔
}

export interface StreamServer {
  server: Server;
  url: string;
  /** 开始 / 正常结束 / 被客户端中途断开的流数量（测试用） */
  stats: { started: number; completed: number; aborted: number };
  close(): Promise<void>;
}

export async function startServer(opts: ServerOptions): Promise<StreamServer> {
  const stats = { started: 0, completed: 0, aborted: 0 };

  const server = createServer((req, res) => {
    if (req.method !== "POST" || req.url !== "/chat") {
      res.writeHead(404).end();
      return;
    }
    void handleChat(req, res, opts, stats);
  });

  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address();
  if (addr === null || typeof addr === "string") throw new Error("unexpected address");

  return {
    server,
    url: `http://127.0.0.1:${addr.port}`,
    stats,
    close: () =>
      new Promise((resolve) => {
        server.closeAllConnections();
        server.close(() => resolve());
      }),
  };
}

async function handleChat(
  req: IncomingMessage,
  res: ServerResponse,
  opts: ServerOptions,
  stats: StreamServer["stats"],
): Promise<void> {
  // 1. 读完请求体（真实服务里这里是 messages、model 等参数）
  for await (const _ of req) {
    // 本例不使用请求体
  }

  // 2. 断点续传：浏览器 EventSource 重连会自动带上 Last-Event-ID；fetch 客户端手动带
  const lastId = Number(req.headers["last-event-id"] ?? -1);
  const start = Number.isInteger(lastId) && lastId >= 0 ? lastId + 1 : 0;

  // 3. 响应头：三个都不能少
  res.writeHead(200, {
    "Content-Type": "text/event-stream; charset=utf-8",
    "Cache-Control": "no-cache, no-transform", // no-transform：别让代理压缩/改写
    "X-Accel-Buffering": "no", // 告诉 Nginx 不要缓冲
    Connection: "keep-alive",
  });
  res.write(encodeSSE({ event: "meta", data: "start", retry: 1000 })); // retry：告诉浏览器断线后 1 秒重连
  stats.started++;

  // 4. 客户端断开：用 AbortController 把“断开”变成取消信号
  const ac = new AbortController();
  res.on("close", () => {
    if (!res.writableFinished) ac.abort();
  });

  // 5. 心跳：注释行，客户端解析器直接忽略，但能防止代理/负载均衡因空闲断开
  const hb = setInterval(() => res.write(": ping\n\n"), opts.heartbeatMs);

  try {
    for (let i = start; i < opts.tokens.length; i++) {
      await sleep(opts.tokenIntervalMs, ac.signal);
      res.write(encodeSSE({ event: "token", id: String(i), data: JSON.stringify({ text: opts.tokens[i] }) }));
    }
    res.write(encodeSSE({ event: "done", data: "[DONE]" }));
    res.end();
    stats.completed++;
  } catch (err) {
    if (ac.signal.aborted) stats.aborted++; // 客户端走了：停止生成，不再写
    else throw err;
  } finally {
    clearInterval(hb);
  }
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) return reject(signal.reason);
    const t = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(t);
      reject(signal.reason);
    };
    signal.addEventListener("abort", onAbort, { once: true });
  });
}
