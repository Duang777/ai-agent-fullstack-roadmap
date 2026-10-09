/** 把 SSE 响应体解析成 data 字符串流；遇到 [DONE] 或流结束时停止。 */
export async function* sseEvents(res: Response): AsyncGenerator<string> {
  if (!res.body) throw new Error("no body");

  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
  let buf = "";

  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) return;
      buf += value;

      let idx: number;
      while ((idx = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, idx).trim();
        buf = buf.slice(idx + 1);

        if (!line.startsWith("data:")) continue;
        const data = line.slice(5).trim();
        if (data === "[DONE]") return;
        yield data;
      }
    }
  } finally {
    reader.releaseLock();
  }
}
