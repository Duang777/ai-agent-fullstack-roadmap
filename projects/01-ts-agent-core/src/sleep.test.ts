import { test, expect } from "vitest";
import { sleep } from "./sleep.js";

test("sleep 正常结束", async () => {
  const start = Date.now();
  await sleep(50);
  expect(Date.now() - start).toBeGreaterThanOrEqual(45);
});

test("abort 立刻打断 sleep", async () => {
  const ctrl = new AbortController();
  const start = Date.now();
  setTimeout(() => ctrl.abort(), 20); // 20ms 后取消
  await expect(sleep(1000, ctrl.signal)).rejects.toThrow();
  expect(Date.now() - start).toBeLessThan(100); // 远小于 1000ms
});

test("已取消的 signal 直接 reject", async () => {
  const ctrl = new AbortController();
  ctrl.abort();
  await expect(sleep(1000, ctrl.signal)).rejects.toThrow();
});
