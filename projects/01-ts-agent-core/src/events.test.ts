import { test, expect } from "vitest";
import { parseEvent } from "./events.js";

test("合法事件", () => {
  const r = parseEvent('{"type":"text","delta":"你好"}');
  expect(r.ok).toBe(true);
  if (r.ok) expect(r.event).toEqual({ type: "text", delta: "你好" });
});

test("非法事件都返回 ok: false", () => {
  expect(parseEvent('{"type":"text"}').ok).toBe(false);    // 缺 delta
  expect(parseEvent('{"type":"unknown"}').ok).toBe(false); // 未知类型
  expect(parseEvent("not json").ok).toBe(false);           // 不是 JSON
});
