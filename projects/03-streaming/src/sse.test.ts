import { describe, expect, it } from "vitest";
import { encodeSSE, parseSSE, SSEParser, type SSEEvent } from "./sse.ts";

function parseAll(chunks: string[]): SSEEvent[] {
  const p = new SSEParser();
  return chunks.flatMap((c) => p.feed(c));
}

describe("SSEParser", () => {
  it("解析基础事件，默认类型为 message", () => {
    expect(parseAll(["data: hello\n\n"])).toEqual([{ event: "message", data: "hello" }]);
  });

  it("多行 data 用换行拼接，event / id / retry 生效", () => {
    const evs = parseAll(["event: token\nid: 7\nretry: 3000\ndata: a\ndata: b\n\n"]);
    expect(evs).toEqual([{ event: "token", data: "a\nb", id: "7", retry: 3000 }]);
  });

  it("忽略注释行（心跳），没有 data 的事件不派发", () => {
    expect(parseAll([": ping\n\n", "event: x\n\n", "data: ok\n\n"])).toEqual([{ event: "message", data: "ok" }]);
  });

  it("冒号后只去掉一个空格；没有冒号的行值为空", () => {
    expect(parseAll(["data:  two\ndata\n\n"])).toEqual([{ event: "message", data: " two\n" }]);
  });

  it("支持 CRLF 与单独的 CR 作为行尾", () => {
    expect(parseAll(["data: a\r\n\r\ndata: b\r\r"])).toEqual([
      { event: "message", data: "a" },
      { event: "message", data: "b" },
    ]);
  });

  it("任意位置切块，结果都与整块一致", () => {
    const raw = 'event: token\r\nid: 1\r\ndata: {"text":"你好"}\r\n\r\n: ping\n\ndata: x\ndata: y\n\n';
    const whole = parseAll([raw]);
    expect(whole).toHaveLength(2);
    for (let i = 0; i <= raw.length; i++) {
      for (let j = i; j <= raw.length; j++) {
        expect(parseAll([raw.slice(0, i), raw.slice(i, j), raw.slice(j)])).toEqual(whole);
      }
    }
  });

  it("没有空行结尾的半个事件不派发", () => {
    const p = new SSEParser();
    expect(p.feed("data: half")).toEqual([]);
    p.end();
    expect(p.feed("\n\n")).toEqual([]);
  });

  it("encodeSSE 与解析器互逆，换行会拆成多行 data", () => {
    const s = encodeSSE({ event: "token", id: "3", data: "line1\nline2" });
    expect(s).toBe("event: token\nid: 3\ndata: line1\ndata: line2\n\n");
    expect(parseAll([s])).toEqual([{ event: "token", id: "3", data: "line1\nline2" }]);
  });
});

describe("parseSSE（字节流）", () => {
  it("多字节中文被切在字节中间也能正确解码", async () => {
    const bytes = new TextEncoder().encode("data: 你好世界\n\n");
    const body = new ReadableStream<Uint8Array>({
      start(c) {
        for (const b of bytes) c.enqueue(new Uint8Array([b])); // 每次只给 1 个字节
        c.close();
      },
    });
    const evs: SSEEvent[] = [];
    for await (const ev of parseSSE(body)) evs.push(ev);
    expect(evs).toEqual([{ event: "message", data: "你好世界" }]);
  });
});
