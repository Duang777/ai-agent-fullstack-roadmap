import { afterEach, describe, expect, it } from "vitest";
import { streamChat } from "./client.ts";
import { startServer, type StreamServer } from "./server.ts";

const tokens = ["你", "好", "，", "世", "界"];
let srv: StreamServer | undefined;

afterEach(async () => {
  await srv?.close();
  srv = undefined;
});

async function waitFor(cond: () => boolean, ms = 1000): Promise<void> {
  const until = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > until) throw new Error("timeout");
    await new Promise((r) => setTimeout(r, 5));
  }
}

describe("SSE 服务 + fetch 客户端", () => {
  it("完整收到全部 token 和 done，心跳不会混进结果", async () => {
    srv = await startServer({ tokens, tokenIntervalMs: 15, heartbeatMs: 5 });
    let text = "";
    let done = false;
    for await (const ev of streamChat(`${srv.url}/chat`, {})) {
      if (ev.type === "token") text += ev.text;
      else done = true;
    }
    expect(text).toBe("你好，世界");
    expect(done).toBe(true);
    expect(srv.stats.completed).toBe(1);
  });

  it("客户端中途取消，服务端停止生成", async () => {
    srv = await startServer({ tokens, tokenIntervalMs: 30, heartbeatMs: 1000 });
    const ac = new AbortController();
    const got: string[] = [];
    await expect(async () => {
      for await (const ev of streamChat(`${srv!.url}/chat`, {}, { signal: ac.signal })) {
        if (ev.type === "token") {
          got.push(ev.text);
          if (got.length === 2) ac.abort();
        }
      }
    }).rejects.toThrow();
    expect(got).toEqual(["你", "好"]);
    await waitFor(() => srv!.stats.aborted === 1);
    expect(srv.stats.completed).toBe(0);
  });

  it("带 Last-Event-ID 从断点之后继续", async () => {
    srv = await startServer({ tokens, tokenIntervalMs: 5, heartbeatMs: 1000 });
    const ids: string[] = [];
    let text = "";
    for await (const ev of streamChat(`${srv.url}/chat`, {}, { lastEventId: "1" })) {
      if (ev.type === "token") {
        ids.push(ev.id);
        text += ev.text;
      }
    }
    expect(ids).toEqual(["2", "3", "4"]);
    expect(text).toBe("，世界");
  });

  it("非 SSE 响应直接报错", async () => {
    srv = await startServer({ tokens, tokenIntervalMs: 5, heartbeatMs: 1000 });
    await expect(async () => {
      for await (const _ of streamChat(`${srv!.url}/nope`, {})) {
        // unreachable
      }
    }).rejects.toThrow("HTTP 404");
  });
});
