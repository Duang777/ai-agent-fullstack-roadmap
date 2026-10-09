# 0.2 TypeScript：给 Agent 套上类型的安全带

> 第 0 部分 · 第 2 课 ｜ 预计 2–3 小时 ｜ 前置：会写基本 TS/JS，Node 22+

## 为什么学这个

Go 那一课解决的是“并发不出错”，这一课解决的是“数据不出错”。Agent 里最危险的数据有三种：

```
LLM 返回的 tool_call 参数（模型随时可能编错 JSON）
流式事件（text / tool_call / done / error 混在一个流里）
外部取消（用户点了“停止”、超时、上游断开）
```

对应的 TS 武器：**可辨识联合** 管事件类型，**Zod** 管运行时校验，**AsyncGenerator** 管流，**AbortController** 管取消。本课结束时，你能写出 TS 版的“流式解析 + 工具参数校验 + 可取消的并行工具执行”。

---

## 1. 严格模式：先把编译器调到最凶

`tsconfig.json` 最少这样开：

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "module": "NodeNext",
    "moduleResolution": "NodeNext",
    "strict": true,
    "noUncheckedIndexedAccess": true,
    "exactOptionalPropertyTypes": true,
    "noImplicitOverride": true,
    "verbatimModuleSyntax": true
  }
}
```

两个最值得的开关：

- `noUncheckedIndexedAccess`：`arr[0]` 的类型变成 `T | undefined`。模型返回 `choices[0]` 为空时，编译期就逼你处理。
- `exactOptionalPropertyTypes`：`{ a?: string }` 不再允许显式写 `a: undefined`，避免“传了 undefined 却以为没传”。

规范：
- 外部数据一律先当 `unknown`，校验后才变成具体类型。**禁止 `as` 强转外部数据。**
- `any` 只能出现在你明确写了注释的地方。

## 2. 可辨识联合：一个流里的多种事件

```ts
type StreamEvent =
  | { type: "text"; delta: string }
  | { type: "tool_call"; id: string; name: string; args: string }
  | { type: "done"; usage: { input: number; output: number } }
  | { type: "error"; message: string };

function handle(ev: StreamEvent) {
  switch (ev.type) {
    case "text":
      process.stdout.write(ev.delta); // 这里 ev 自动收窄，只有 delta
      break;
    case "tool_call":
      console.log(ev.name, ev.args);
      break;
    case "done":
      console.log("tokens:", ev.usage.output);
      break;
    case "error":
      throw new Error(ev.message);
    default:
      assertNever(ev); // 新增事件类型忘了处理 → 编译报错
  }
}

function assertNever(x: never): never {
  throw new Error(`unhandled: ${JSON.stringify(x)}`);
}
```

`type` 字段就是“标签”。OpenAI Responses、Anthropic Messages 的流式事件本质上都是这种结构，你自己的 Agent 内部事件也应该这么定义。

## 3. Zod：运行时校验 + 类型推导 + JSON Schema 一次写完

TS 的类型在运行时消失，而 LLM 的输出只在运行时出现。Zod 补的就是这一段。

```ts
import { z } from "zod"; // zod v4

const SearchArgs = z.object({
  query: z.string().min(1).describe("搜索关键词"),
  topK: z.number().int().min(1).max(20).default(5),
});

type SearchArgs = z.infer<typeof SearchArgs>; // { query: string; topK: number }

// 1) 发给模型的工具定义：直接从 Zod 生成 JSON Schema，不再手写两份
const toolDef = {
  name: "search",
  description: "搜索内部文档",
  parameters: z.toJSONSchema(SearchArgs),
};

// 2) 模型返回的参数：先 JSON.parse，再 safeParse
function parseArgs(raw: string): { ok: true; data: SearchArgs } | { ok: false; error: string } {
  let json: unknown;
  try {
    json = JSON.parse(raw);
  } catch {
    return { ok: false, error: "参数不是合法 JSON" };
  }
  const r = SearchArgs.safeParse(json);
  return r.success ? { ok: true, data: r.data } : { ok: false, error: z.prettifyError(r.error) };
}
```

关键点：**校验失败不抛异常，而是把错误文本作为 tool result 回传给模型**，让它自己修正参数重试。这和 Go 课里“工具失败作为观察回传”是同一个思想。

`parse` vs `safeParse`：信任边界外（模型、用户、第三方 API）用 `safeParse`；内部配置启动时用 `parse`，错了直接崩。

## 4. AsyncGenerator：把 SSE 变成 `for await`

```ts
async function* sseEvents(res: Response, signal: AbortSignal): AsyncGenerator<string> {
  if (!res.body) throw new Error("no body");
  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
  let buf = "";
  try {
    while (true) {
      const { value, done } = await reader.read(); // signal 取消时这里会 reject
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
    reader.releaseLock(); // 不管正常结束、break、还是异常，都会执行
  }
}

// 使用
const ctrl = new AbortController();
const res = await fetch(url, { method: "POST", body, signal: ctrl.signal });
for await (const data of sseEvents(res, ctrl.signal)) {
  const ev = JSON.parse(data) as unknown; // 下一步交给 Zod 校验成 StreamEvent
  // ...
  if (shouldStop) break; // break 会触发 generator 的 finally
}
```

### 逐段解读

- `TextDecoderStream`：网络按字节到达，一个汉字可能被拆在两个 chunk，必须用流式解码，不能每个 chunk 单独 `decode`。
- `buf` + `indexOf("\n")`：一个 chunk 可能半行，也可能多行，所以要攒缓冲按行切。和 Go 课里 `bufio.Scanner` 做的是一件事。
- `finally`：`for await` 里 `break` / `return` / 抛错时，JS 会调用 generator 的 `return()`，于是 `finally` 执行。这是 TS 版的 `defer`。
- 取消：`fetch` 绑定了 `signal`，`ctrl.abort()` 后 `reader.read()` 会以 `AbortError` reject，连接被断开，上游停止计费。

## 5. AbortController：TS 版的 context

| Go | TS |
|---|---|
| `context.WithCancel` | `new AbortController()` |
| `context.WithTimeout(ctx, d)` | `AbortSignal.timeout(ms)` |
| 子 ctx 继承父 ctx | `AbortSignal.any([parent, AbortSignal.timeout(ms)])` |
| `<-ctx.Done()` | `signal.aborted` / `signal.addEventListener("abort", ...)` |
| `ctx.Err()` | `signal.reason` |

```ts
// 整轮对话 60 秒；用户点停止也能取消
async function runTurn(userSignal: AbortSignal) {
  const signal = AbortSignal.any([userSignal, AbortSignal.timeout(60_000)]);
  const res = await fetch(LLM_URL, { method: "POST", body: "...", signal });
  // ... signal 一路往下传给每个工具
}
```

规范（和 Go 一致）：
- `signal` 作为参数显式往下传（习惯放在最后一个 options 参数里）。
- 自己写的长循环里要检查：`signal.throwIfAborted()`。
- 区分取消和真错误：`err.name === "AbortError"`（手动 abort）或 `"TimeoutError"`（`AbortSignal.timeout`）。

## 6. 并行工具调用：带并发上限 + 单工具超时

```ts
type ToolCall = { id: string; name: string; args: string };
type ToolResult = { id: string; output: string; isError: boolean };
type Tool = (args: string, signal: AbortSignal) => Promise<string>;

async function runTools(
  calls: ToolCall[],
  tools: Record<string, Tool>,
  opts: { signal: AbortSignal; limit?: number; perToolMs?: number },
): Promise<ToolResult[]> {
  const { signal, limit = 4, perToolMs = 20_000 } = opts;
  const results: ToolResult[] = new Array(calls.length);
  let next = 0;

  async function worker() {
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
        results[i] = { id: c.id, output: await tool(c.args, s), isError: false };
      } catch (err) {
        if (signal.aborted) throw err; // 整体取消：向上抛，整轮结束
        results[i] = { id: c.id, output: String(err), isError: true }; // 单个失败：回传给模型
      }
    }
  }

  await Promise.all(Array.from({ length: Math.min(limit, calls.length) }, worker));
  return results;
}
```

对照 Go 版：`limit` 个 worker = `g.SetLimit`；每个 worker 写自己的下标 = 无锁写切片；单工具失败写进结果、整体取消才抛 = `return nil` vs `return err`。

`Promise.all` vs `Promise.allSettled`：这里用 `all` 是故意的，只有整体取消才会 reject，单个工具失败已经在 `try/catch` 里变成了结果。

## 7. 三个高频坑

1. **忘了 `await` 的 Promise**：`tool(args)` 没 `await` 就返回，异常变成 unhandled rejection，Node 直接退出。开 ESLint 的 `@typescript-eslint/no-floating-promises`。
2. **`as` 骗编译器**：`JSON.parse(x) as ToolCall` 编译通过，运行时字段可能全是 undefined。外部数据只能 Zod 校验后再用。
3. **abort 了但资源没释放**：自己写的工具如果用了 `setTimeout` 或子进程，要监听 `signal` 的 `abort` 事件去清理，否则请求取消了，后台还在跑。

## 练习（今天做完）

放在 `projects/01-ts-agent-core/`，用 `vitest` 测试。

1. 用 Zod 定义 `StreamEvent` 可辨识联合（`z.discriminatedUnion("type", [...])`），写 `parseEvent(data: string)`，非法输入返回错误而不是抛异常
2. 写可取消的 `sleep(ms, signal)`，abort 时立刻 reject 并清掉定时器
3. 用 `runTools` 跑三个假工具（50ms / 100ms / 5s），`perToolMs = 200`，断言前两个成功、第三个 `isError: true`、总耗时 < 400ms
4. 用 `node:http` 起一个假 SSE 服务，客户端读到 3 个事件后 `abort()`，断言服务端收到 `close` 事件
5. `tsc --noEmit` 零错误，`vitest run` 全绿

## 自测题

1. 为什么 `switch` 的 `default` 里要调 `assertNever`？
2. 工具参数 Zod 校验失败，为什么不抛异常而是回传给模型？
3. `for await` 里 `break`，generator 里的 `finally` 会执行吗？为什么？
4. `AbortSignal.any` 解决了什么问题？对应 Go 的什么？
5. 为什么 `runTools` 里 `next++` 不需要锁，而 Go 版需要每个 goroutine 写自己的下标？

---

# 参考答案与详解

## 自测题答案

### 1. `assertNever` 的作用
`default` 分支里 `ev` 的类型应该是 `never`（所有情况都处理完了）。以后有人往 `StreamEvent` 里加了 `{ type: "thinking" }` 却忘了加 `case`，`ev` 就不再是 `never`，传给 `assertNever(x: never)` 会编译报错。把“漏处理”从运行时 bug 变成编译错误。

### 2. 校验失败为什么回传给模型
模型编错参数是常态，不是异常。回传类似 `topK: 应小于等于 20` 的错误文本，模型下一轮通常能自己修正；抛异常会让整轮对话失败，用户什么也拿不到。只有“系统坏了”（网络断、配置错、被取消）才应该抛。

### 3. `break` 后 `finally` 会执行吗
会。`for await...of` 提前退出时会调用迭代器的 `return()` 方法，generator 收到后会执行挂起点之后的 `finally`。所以把 `releaseLock`、关连接放 `finally` 是安全的。注意：如果你手动 `.next()` 迭代又中途丢掉不管，`finally` 不会执行。

### 4. `AbortSignal.any`
把多个取消源合成一个：用户停止 OR 整体超时 OR 单工具超时，任一触发就取消。对应 Go 里“子 ctx 继承父 ctx + 自己的 WithTimeout”。没有它之前，要手写监听器把多个 signal 转发到一个新 controller，容易漏清理。

### 5. 为什么不需要锁
JS 是单线程事件循环，`const i = next++` 是同步执行的，两个 worker 不可能同时执行这一行；它们只在 `await` 处让出控制权。Go 的 goroutine 是真并行的，多个 goroutine 同时 `next++` 会数据竞争，所以 Go 版用“每个 goroutine 预先分到自己的下标”来避免共享变量。

## 练习参考实现

### 练习 1：`parseEvent`

```ts
import { z } from "zod";

export const StreamEventSchema = z.discriminatedUnion("type", [
  z.object({ type: z.literal("text"), delta: z.string() }),
  z.object({ type: z.literal("tool_call"), id: z.string(), name: z.string(), args: z.string() }),
  z.object({ type: z.literal("done"), usage: z.object({ input: z.number(), output: z.number() }) }),
  z.object({ type: z.literal("error"), message: z.string() }),
]);
export type StreamEvent = z.infer<typeof StreamEventSchema>;

export function parseEvent(data: string):
  | { ok: true; event: StreamEvent }
  | { ok: false; error: string } {
  let json: unknown;
  try {
    json = JSON.parse(data);
  } catch {
    return { ok: false, error: "invalid json" };
  }
  const r = StreamEventSchema.safeParse(json);
  return r.success ? { ok: true, event: r.data } : { ok: false, error: z.prettifyError(r.error) };
}
```

测试要点：合法的四种各一条；`{"type":"text"}`（缺 delta）、`{"type":"unknown"}`、`"not json"` 都返回 `ok: false`。

### 练习 2：可取消的 `sleep`

```ts
export function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason);
    const t = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    function onAbort() {
      clearTimeout(t); // 坑 3：不清定时器，进程会多挂 ms 毫秒
      reject(signal!.reason);
    }
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}
```

### 练习 3：并行工具与超时

```ts
import { test, expect } from "vitest";

test("slow tool times out, others succeed", async () => {
  const tool = (ms: number) => async (_: string, s: AbortSignal) => {
    await sleep(ms, s);
    return `done ${ms}`;
  };
  const start = Date.now();
  const res = await runTools(
    [
      { id: "a", name: "fast", args: "{}" },
      { id: "b", name: "mid", args: "{}" },
      { id: "c", name: "slow", args: "{}" },
    ],
    { fast: tool(50), mid: tool(100), slow: tool(5000) },
    { signal: new AbortController().signal, perToolMs: 200 },
  );
  expect(res.map((r) => r.isError)).toEqual([false, false, true]);
  expect(Date.now() - start).toBeLessThan(400);
});
```

能在 200ms 左右结束，前提是 `sleep` 尊重了 `signal`。如果工具不理会 signal，`AbortSignal.timeout` 只能让你“不再等”，工具本身还在后台跑，这就是坑 3。

### 练习 4：服务端感知断开

```ts
import http from "node:http";
import { once } from "node:events";
import type { AddressInfo } from "node:net";

test("client abort closes server stream", async () => {
  let resolveClosed!: () => void;
  const closed = new Promise<void>((r) => (resolveClosed = r));

  const server = http.createServer((req, res) => {
    res.writeHead(200, { "Content-Type": "text/event-stream" });
    let n = 0;
    const timer = setInterval(() => {
      res.write(`data: {"type":"text","delta":"t${n++}"}\n\n`);
    }, 20);
    res.on("close", () => {
      clearInterval(timer);
      resolveClosed();
    });
  });
  server.listen(0);
  await once(server, "listening");
  const { port } = server.address() as AddressInfo; // 这里的 as 是可接受的：类型来自 Node 自己

  const ctrl = new AbortController();
  const res = await fetch(`http://127.0.0.1:${port}`, { signal: ctrl.signal });
  let count = 0;
  try {
    for await (const _ of sseEvents(res, ctrl.signal)) {
      if (++count === 3) ctrl.abort();
    }
  } catch (err) {
    expect((err as Error).name).toBe("AbortError");
  }

  await closed; // 等不到就会被 vitest 超时判失败
  expect(count).toBe(3);
  server.close();
});
```

### 练习 5：类型检查与测试

```bash
npm i zod
npm i -D typescript vitest @types/node
npx tsc --noEmit
npx vitest run
```

两条都通过，回来把输出贴给我，我帮你批改并在 ROADMAP 里勾上 TypeScript。
