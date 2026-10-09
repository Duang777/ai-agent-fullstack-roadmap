export type ToolCall = { id: string; name: string; args: string };
export type ToolResult = { id: string; output: string; isError: boolean };
export type Tool = (args: string, signal: AbortSignal) => Promise<string>;

export type RunOptions = {
  signal: AbortSignal;
  limit?: number;
  perToolMs?: number;
};

/** 并行执行工具：最多 limit 个并发，单工具超时，单个失败作为结果回传给模型。 */
export async function runTools(
  calls: ToolCall[],
  tools: Record<string, Tool>,
  opts: RunOptions,
): Promise<ToolResult[]> {
  const { signal, limit = 4, perToolMs = 20_000 } = opts;
  const results: ToolResult[] = new Array(calls.length);
  let next = 0;

  async function worker(): Promise<void> {
    while (true) {
      const i = next++; // JS 单线程，这里不需要锁
      const c = calls[i];
      if (!c) return;

      const tool = tools[c.name];
      if (!tool) {
        results[i] = { id: c.id, output: `未知工具 ${c.name}`, isError: true };
        continue;
      }

      try {
        const s = AbortSignal.any([signal, AbortSignal.timeout(perToolMs)]);
        const output = await tool(c.args, s);
        results[i] = { id: c.id, output, isError: false };
      } catch (err) {
        if (signal.aborted) throw err; // 整体取消：向上抛
        results[i] = { id: c.id, output: String(err), isError: true }; // 单个失败：回传
      }
    }
  }

  const n = Math.min(limit, calls.length);
  const workers: Promise<void>[] = [];
  for (let k = 0; k < n; k++) {
    workers.push(worker());
  }
  await Promise.all(workers);

  return results;
}
