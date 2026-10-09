import { test, expect } from "vitest";
import http from "node:http";
import { once } from "node:events";
import type { AddressInfo } from "node:net";
import { sseEvents } from "./sse.js";

test("客户端 abort 后服务端收到 close", async () => {
  // 1. 准备一个“等服务端关闭”的 Promise
  let markClosed!: () => void;
  const closed = new Promise<void>((resolve) => {
    markClosed = resolve;
  });

  // 2. 起一个假 SSE 服务：每 20ms 发一条
  const server = http.createServer((req, res) => {
    res.writeHead(200, { "Content-Type": "text/event-stream" });
    let n = 0;
    const timer = setInterval(() => {
      res.write(`data: {"type":"text","delta":"t${n++}"}\n\n`);
    }, 20);
    res.on("close", () => {
      clearInterval(timer);
      markClosed();
    });
  });
  server.listen(0);
  await once(server, "listening");
  const { port } = server.address() as AddressInfo;

  // 3. 客户端读 3 条后取消
  const ctrl = new AbortController();
  const res = await fetch(`http://127.0.0.1:${port}`, { signal: ctrl.signal });
  let count = 0;
  try {
    for await (const _ of sseEvents(res)) {
      count++;
      if (count === 3) ctrl.abort();
    }
  } catch (err) {
    expect((err as Error).name).toBe("AbortError");
  }

  // 4. 断言服务端真的感知到了断开
  await closed;
  expect(count).toBe(3);
  server.close();
});
