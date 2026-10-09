// fetch + SSE 客户端：EventSource 只支持 GET 且不能带请求体，调模型 API 一律用这种写法。

import { parseSSE } from "./sse.ts";

export type ChatEvent =
  | { type: "token"; id: string; text: string }
  | { type: "done" };

export interface StreamChatOptions {
  signal?: AbortSignal;
  lastEventId?: string; // 断点续传：从这个 id 之后继续
}

export async function* streamChat(
  url: string,
  body: unknown,
  opts: StreamChatOptions = {},
): AsyncGenerator<ChatEvent> {
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    Accept: "text/event-stream",
  };
  if (opts.lastEventId !== undefined) headers["Last-Event-ID"] = opts.lastEventId;

  const res = await fetch(url, { method: "POST", headers, body: JSON.stringify(body), signal: opts.signal });
  if (!res.ok) throw new Error(`HTTP ${res.status}`); // 先查状态码：错误响应往往是 JSON 而不是 SSE
  const ct = res.headers.get("content-type") ?? "";
  if (!ct.startsWith("text/event-stream")) throw new Error(`unexpected content-type: ${ct}`);
  if (!res.body) throw new Error("empty body");

  for await (const ev of parseSSE(res.body, opts.signal)) {
    if (ev.event === "token") {
      const { text } = JSON.parse(ev.data) as { text: string };
      yield { type: "token", id: ev.id ?? "", text };
    } else if (ev.event === "done") {
      yield { type: "done" };
      return;
    }
    // meta 等其他事件忽略
  }
  throw new Error("stream ended without done"); // 没收到 done 就断了：当作失败，交给上层重试
}
