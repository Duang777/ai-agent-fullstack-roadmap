// SSE（Server-Sent Events）解析器：按规范处理任意切块、CRLF、多行 data、注释、id、retry。
// 规范：https://html.spec.whatwg.org/multipage/server-sent-events.html

export interface SSEEvent {
  event: string; // 没写 event: 时默认 "message"
  data: string; // 多行 data 用 "\n" 拼接
  id?: string;
  retry?: number;
}

export class SSEParser {
  private buf = ""; // 还没凑成完整一行的残余文本
  private data: string[] = [];
  private eventType = "";
  private lastId: string | undefined;
  private retry: number | undefined;
  private pendingCR = false; // 上一块以 \r 结尾，下一块若以 \n 开头要吞掉

  /** 喂入一段文本，返回这段文本里凑齐的所有事件。 */
  feed(chunk: string): SSEEvent[] {
    if (chunk === "") return []; // 空块不能清掉 pendingCR，否则被切开的 \r\n 会多出一个空行
    if (this.pendingCR && chunk.startsWith("\n")) chunk = chunk.slice(1);
    this.pendingCR = false;
    this.buf += chunk;

    const out: SSEEvent[] = [];
    // 一行可以以 \r\n、\n 或 \r 结尾
    for (;;) {
      const m = /\r\n|\n|\r/.exec(this.buf);
      if (!m) break;
      // 结尾正好是 \r：可能是 \r\n 被切开了，先当作行尾，并记住要吞掉下一个 \n
      if (m[0] === "\r" && m.index === this.buf.length - 1) this.pendingCR = true;
      const line = this.buf.slice(0, m.index);
      this.buf = this.buf.slice(m.index + m[0].length);
      const ev = this.line(line);
      if (ev) out.push(ev);
    }
    return out;
  }

  /** 流结束时调用：规范要求丢弃没有空行结尾的半个事件。 */
  end(): void {
    this.buf = "";
    this.data = [];
    this.eventType = "";
  }

  private line(line: string): SSEEvent | undefined {
    if (line === "") return this.dispatch(); // 空行：一个事件结束
    if (line.startsWith(":")) return undefined; // 注释，常用作心跳

    const i = line.indexOf(":");
    const field = i === -1 ? line : line.slice(0, i);
    let value = i === -1 ? "" : line.slice(i + 1);
    if (value.startsWith(" ")) value = value.slice(1); // 冒号后只去掉一个空格

    switch (field) {
      case "data":
        this.data.push(value);
        break;
      case "event":
        this.eventType = value;
        break;
      case "id":
        if (!value.includes("\0")) this.lastId = value;
        break;
      case "retry":
        if (/^\d+$/.test(value)) this.retry = Number(value);
        break;
      // 其他字段名按规范忽略
    }
    return undefined;
  }

  private dispatch(): SSEEvent | undefined {
    if (this.data.length === 0) {
      this.eventType = "";
      return undefined; // 没有 data 的事件不派发
    }
    const ev: SSEEvent = { event: this.eventType || "message", data: this.data.join("\n") };
    if (this.lastId !== undefined) ev.id = this.lastId;
    if (this.retry !== undefined) ev.retry = this.retry;
    this.data = [];
    this.eventType = "";
    return ev;
  }
}

/** 把字节流解析成 SSE 事件。signal 中止时立即停止并释放底层连接。 */
export async function* parseSSE(
  body: ReadableStream<Uint8Array>,
  signal?: AbortSignal,
): AsyncGenerator<SSEEvent> {
  const reader = body.getReader();
  const decoder = new TextDecoder(); // stream: true 能正确处理被切开的多字节中文
  const parser = new SSEParser();
  const onAbort = () => void reader.cancel().catch(() => {});
  signal?.addEventListener("abort", onAbort, { once: true });
  try {
    for (;;) {
      if (signal?.aborted) return;
      const { value, done } = await reader.read();
      if (done) break;
      for (const ev of parser.feed(decoder.decode(value, { stream: true }))) yield ev;
    }
    for (const ev of parser.feed(decoder.decode())) yield ev;
    parser.end();
  } finally {
    signal?.removeEventListener("abort", onAbort);
    reader.releaseLock();
  }
}

/** 编码一个 SSE 事件。data 里的换行会被拆成多行 data:。 */
export function encodeSSE(ev: { data: string; event?: string; id?: string; retry?: number }): string {
  let s = "";
  if (ev.event) s += `event: ${ev.event}\n`;
  if (ev.id !== undefined) s += `id: ${ev.id}\n`;
  if (ev.retry !== undefined) s += `retry: ${ev.retry}\n`;
  for (const line of ev.data.split(/\r\n|\n|\r/)) s += `data: ${line}\n`;
  return s + "\n";
}
