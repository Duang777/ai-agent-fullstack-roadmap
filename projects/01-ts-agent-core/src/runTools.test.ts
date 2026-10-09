import { test, expect } from "vitest";
import { runTools, type Tool } from "./runTools.js";
import { sleep } from "./sleep.js";

const fakeTool = (ms: number): Tool => {
  return async (_args, signal) => {
    await sleep(ms, signal);
    return `done ${ms}`;
  };
};

test("慢工具超时，其他成功", async () => {
  const start = Date.now();

  const res = await runTools(
    [
      { id: "a", name: "fast", args: "{}" },
      { id: "b", name: "mid", args: "{}" },
      { id: "c", name: "slow", args: "{}" },
    ],
    { fast: fakeTool(50), mid: fakeTool(100), slow: fakeTool(5000) },
    { signal: new AbortController().signal, perToolMs: 200 },
  );

  expect(res.map((r) => r.isError)).toEqual([false, false, true]);
  expect(Date.now() - start).toBeLessThan(400);
});

test("未知工具返回错误结果而不是抛异常", async () => {
  const res = await runTools([{ id: "x", name: "nope", args: "{}" }], {}, {
    signal: new AbortController().signal,
  });
  expect(res[0]?.isError).toBe(true);
});

test("整体取消时 runTools 抛错", async () => {
  const ctrl = new AbortController();
  setTimeout(() => ctrl.abort(), 30);
  await expect(
    runTools([{ id: "a", name: "slow", args: "{}" }], { slow: fakeTool(5000) }, { signal: ctrl.signal }),
  ).rejects.toThrow();
});
