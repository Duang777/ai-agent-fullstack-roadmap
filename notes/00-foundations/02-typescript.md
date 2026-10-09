# 0.2 TypeScript：给 Agent 套上类型的安全带（Go 程序员版）

> 第 0 部分 · 第 2 课 ｜ 预计 3–4 小时 ｜ 前置：学完 0.1 Go 并发，Node 22+
>
> 本课默认你熟悉 Go、不熟悉 TS。每个新语法第一次出现都会对照 Go 讲一遍；每段代码后面都有“逐行解读”。

## 为什么学这个

Go 那一课解决的是“并发不出错”，这一课解决的是“数据不出错”。Agent 里最危险的数据有三种：

```
LLM 返回的 tool_call 参数（模型随时可能编错 JSON）
流式事件（text / tool_call / done / error 混在一个流里）
外部取消（用户点了“停止”、超时、上游断开）
```

| 问题 | Go 里你怎么做 | TS 里怎么做 |
|---|---|---|
| 一个流里有多种事件 | `interface` + `switch v := ev.(type)` | **可辨识联合** + `switch (ev.type)` |
| 校验外部 JSON | `json.Unmarshal` + 手写 `Validate()` | **Zod** |
| 流式读取 | goroutine 往 channel 里塞 | **AsyncGenerator** + `for await` |
| 取消 | `context.Context` | **AbortController / AbortSignal** |
| 并行工具 | `errgroup` | `Promise.all` + worker |

本课结束时，你能写出 TS 版的“流式解析 + 工具参数校验 + 可取消的并行工具执行”，和 0.1 的 Go 代码一一对应。

---

## 0. Go 程序员的 TS 语法速成

先把后面会用到的语法一次讲清楚。看不懂后面的代码时，回到这一节查。

### 0.1 变量

```ts
const name = "search";   // 不能重新赋值，类似 Go 的 name := "search" 但之后不能改
let count = 0;           // 可以重新赋值
count = 1;
const limit: number = 4; // 显式写类型：变量名: 类型（Go 是 var limit int = 4，类型在后面，这点一样）
```

- 默认用 `const`，需要改才用 `let`。**不要用 `var`**（老语法，作用域有坑）。
- TS 会自动推导类型，`const name = "search"` 里 `name` 就是 `string`，不用写。

### 0.2 基本类型

| TS | Go | 说明 |
|---|---|---|
| `string` | `string` | |
| `number` | `int` / `float64` | TS 只有一种数字，没有 int/float 之分 |
| `boolean` | `bool` | |
| `string[]` | `[]string` | 数组 |
| `Record<string, number>` | `map[string]int` | 键值对象 |
| `undefined` / `null` | `nil` | 两种“空”，`undefined` 更常见 |
| `unknown` | `any`（`interface{}`） | **不知道是什么**，用之前必须先检查 |
| `any` | 没有对应 | 关掉类型检查，尽量别用 |
| `never` | 没有对应 | “不可能出现的值”，后面讲 |
| `void` | 函数无返回值 | |

### 0.3 对象类型：`type`

```ts
type ToolCall = {
  id: string;
  name: string;
  args: string;
};
```

等价于 Go：

```go
type ToolCall struct {
    ID   string
    Name string
    Args string
}
```

区别：TS 的对象不需要 `ToolCall{...}` 这种构造，直接写字面量就行：

```ts
const c: ToolCall = { id: "1", name: "search", args: "{}" };
console.log(c.name); // "search"
```

字段后面加 `?` 表示可选（可以不传）：

```ts
type Options = { limit?: number };   // limit 的类型是 number | undefined
```

> 你也会看到 `interface ToolCall { ... }`，作用和 `type` 基本一样。本课统一用 `type`。

### 0.4 联合类型 `|`：“要么是 A，要么是 B”

```ts
let x: string | number;  // x 可以是 string，也可以是 number
x = "hi";
x = 42;
```

Go 没有这个语法，最接近的是“用一个 interface 装多种类型”。

**字面量类型**：类型可以是一个具体的值。

```ts
type Role = "user" | "assistant" | "tool"; // Role 只能是这三个字符串之一
const r: Role = "user";      // ✅
const r2: Role = "admin";    // ❌ 编译报错
```

相当于 Go 里的 `type Role string` + 三个 `const`，但 TS 会在编译期拦住非法值，Go 不会。

### 0.5 函数

```ts
// 普通函数：参数名: 类型，返回值类型写在括号后面
function add(a: number, b: number): number {
  return a + b;
}

// 箭头函数：另一种写法，常用于回调和短函数
const add2 = (a: number, b: number): number => a + b;
//          ^^^^^^^^^^^^^^^^^^^^^^^^  ^^^^^^   ^^^^^
//          参数                      返回类型 函数体（只有一个表达式时可省略 return 和 {}）

// 函数类型：描述“一个函数长什么样”
type Tool = (args: string, signal: AbortSignal) => Promise<string>;
// 读作：Tool 是一个函数，接收 args 和 signal 两个参数，返回 Promise<string>
```

对照 Go：

```go
func add(a, b int) int { return a + b }
add2 := func(a, b int) int { return a + b }
type Tool func(args string, ctx context.Context) (string, error)
```

注意：TS 函数**只有一个返回值**，错误靠 `throw` 抛出，而不是 `(string, error)`。

### 0.6 错误处理：throw / try / catch / finally

```ts
function mustPositive(n: number): number {
  if (n < 0) throw new Error("负数");  // 相当于 Go 的 return 0, errors.New("负数")
  return n;
}

try {
  mustPositive(-1);
  console.log("不会执行到这里");      // throw 之后的代码直接跳过
} catch (err) {
  console.log("出错了：", err);        // 相当于 if err != nil { ... }
} finally {
  console.log("无论成功失败都执行");  // 相当于 defer
}
```

- `throw` 会一路往上冒，直到被某个 `catch` 接住；没人接，程序崩（类似 panic）。
- `catch (err)` 里 `err` 的类型是 `unknown`，因为 JS 里什么都能被 throw。

### 0.7 Promise 与 async/await：TS 的“异步”

这是 Go 程序员最需要转换思路的地方。

- **Go**：你开 goroutine，多个真正并行运行，用 channel 等结果。
- **JS/TS**：只有**一个线程**。遇到 I/O（网络、定时器）时不阻塞线程，而是登记一个“等结果的凭证”，这个凭证就叫 **Promise**。

```ts
// fetch 返回 Promise<Response>：一个“将来会给你 Response”的凭证
const p: Promise<Response> = fetch("https://example.com");

// await：在这里暂停当前函数，等凭证兑现，拿到真正的值
const res: Response = await fetch("https://example.com");
```

`await` 只能在 `async` 函数里用：

```ts
async function getText(url: string): Promise<string> {
  const res = await fetch(url);   // 暂停，等网络返回；此时线程去处理别的事
  return await res.text();        // 再暂停，等 body 读完
}
// async 函数的返回值会自动包一层 Promise：写 return "abc"，调用方拿到 Promise<string>
```

对照 Go 心智模型：

| TS | Go 大致对应 |
|---|---|
| `const p = doWork()`（不 await） | `go doWork()`，开始跑了，但你没等它 |
| `await p` | `<-done`，阻塞等结果 |
| `await Promise.all([p1, p2, p3])` | `wg.Wait()`，等三个都完成 |
| Promise reject（失败） | 返回了 error |

关键区别：**`await` 只暂停当前函数，不暂停整个程序**。所以同一时间可以有很多个函数“停在 await 上”，看起来像并发。但同一时刻真正在执行 JS 代码的只有一个，所以**普通变量读写不会有数据竞争，不需要锁**。

### 0.8 泛型 `<T>`

```ts
type Box<T> = { value: T };
const b: Box<string> = { value: "hi" };

Promise<string>   // 一个最终会给出 string 的 Promise
Array<number>     // 等同于 number[]
```

和 Go 1.18 的泛型一样，`Box[T any]` 写成 `Box<T>`，用尖括号。

### 0.9 几个小语法（后面代码里都会出现）

```ts
// 解构：一次从对象里取多个字段，可带默认值
const opts = { signal: s, limit: 8 };
const { signal, limit = 4, perToolMs = 20_000 } = opts;
// 等价于：
// const signal = opts.signal;
// const limit = opts.limit ?? 4;          // opts 里有 limit=8，所以是 8
// const perToolMs = opts.perToolMs ?? 20000; // 没有，所以用默认值 20000
// 20_000 就是 20000，下划线只是方便阅读（Go 也支持）

// 可选链 ?.：左边是 undefined/null 时不报错，直接得到 undefined
signal?.aborted   // signal 为空 → undefined；否则 → signal.aborted

// 非空断言 !：告诉编译器“我保证这里不是 undefined”
signal!.reason

// 模板字符串：反引号 + ${}，相当于 fmt.Sprintf
const msg = `未知工具 ${name}`;   // Go: fmt.Sprintf("未知工具 %s", name)

// 类型断言 as：告诉编译器“把它当成这个类型”，运行时不做任何检查
const x = JSON.parse(raw) as ToolCall; // 危险！后面专门讲为什么

// typeof（在类型位置）：取一个变量的类型
const cfg = { a: 1 };
type Cfg = typeof cfg;   // { a: number }

// import / export：模块，相当于 Go 的 package 导入导出
import { z } from "zod";           // 从 zod 包里导入名字 z
export function parseEvent() {}    // 让别的文件能 import 它（Go 里是首字母大写）
```

好，有了这些，下面的代码都能读懂了。

---

## 1. 严格模式：先把编译器调到最凶

TS 编译器的检查强度是可调的。`tsconfig.json` 是项目配置文件（类似 `go.mod` + `go vet` 规则），最少这样开：

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

逐项解释：

| 选项 | 作用 |
|---|---|
| `target: ES2022` | 编译输出的 JS 版本，ES2022 Node 22 全支持 |
| `module / moduleResolution: NodeNext` | 按 Node 的规则解析 `import` |
| `strict: true` | 打开一整套严格检查，最重要的是：不许隐式 `any`、`undefined` 必须处理 |
| `noUncheckedIndexedAccess` | `arr[0]` 的类型变成 `T \| undefined` |
| `exactOptionalPropertyTypes` | 可选字段不许显式传 `undefined` |
| `noImplicitOverride` | 子类覆盖方法必须写 `override` |
| `verbatimModuleSyntax` | 只导入类型时必须写 `import type`，避免打包问题 |

重点看 `noUncheckedIndexedAccess`：

```ts
const choices: string[] = [];
const first = choices[0];   // 开启后，first 的类型是 string | undefined
first.toUpperCase();        // ❌ 编译报错：first 可能是 undefined
if (first) first.toUpperCase(); // ✅ 检查过了，可以用
```

Go 里 `choices[0]` 越界会运行时 panic；TS 默认会让你得到 `undefined` 然后在别处崩。开了这个选项，**编译期就逼你处理“模型返回空 choices”的情况**。

两条规范：
- 外部数据（模型输出、HTTP 请求体、第三方 API）一律先当 `unknown`，校验后才变成具体类型。**禁止用 `as` 强转外部数据。**
- 不用 `any`。真要用，旁边写注释说明为什么。

## 2. 可辨识联合：一个流里的多种事件

LLM 流式返回时，同一个流里会混着几种事件：一段文字、一次工具调用、结束、出错。怎么用类型描述“这几种之一”？

```ts
type StreamEvent =
  | { type: "text"; delta: string }
  | { type: "tool_call"; id: string; name: string; args: string }
  | { type: "done"; usage: { input: number; output: number } }
  | { type: "error"; message: string };
```

### 逐行解读

- `type StreamEvent =`：定义一个类型叫 `StreamEvent`。
- 后面四行用 `|` 连起来，意思是：**一个 StreamEvent 是这四种对象之一**。开头那个 `|` 只是为了排版整齐，可写可不写。
- 每种对象都有一个 `type` 字段，而且值是**字面量**（`"text"`、`"tool_call"`…），这个字段就叫“标签”（discriminant）。四种对象的其他字段各不相同。

这样的值都是合法的 `StreamEvent`：

```ts
const a: StreamEvent = { type: "text", delta: "你好" };
const b: StreamEvent = { type: "done", usage: { input: 10, output: 20 } };
const c: StreamEvent = { type: "text", message: "x" }; // ❌ text 类型没有 message 字段
```

对照 Go，你大概会这么写：

```go
type StreamEvent interface{ isEvent() }
type TextEvent struct{ Delta string }
type ToolCallEvent struct{ ID, Name, Args string }
// ...每个 struct 实现 isEvent()

switch e := ev.(type) {
case TextEvent:     fmt.Print(e.Delta)
case ToolCallEvent: fmt.Println(e.Name)
}
```

TS 版本的用法：

```ts
function handle(ev: StreamEvent): void {
  switch (ev.type) {
    case "text":
      process.stdout.write(ev.delta);
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
      assertNever(ev);
  }
}

function assertNever(x: never): never {
  throw new Error(`unhandled: ${JSON.stringify(x)}`);
}
```

### 逐行解读

- `switch (ev.type)`：根据标签分支。注意 TS 的 `switch` 每个 `case` 结尾要写 `break`，否则会“掉”到下一个 case（Go 默认不掉，要写 `fallthrough` 才掉，**这里正好相反**）。
- `case "text":` 之后，编译器知道 `ev` 一定是 `{ type: "text"; delta: string }`，所以 `ev.delta` 能用，而 `ev.name` 会报错。这叫**类型收窄**，效果等同于 Go 的 `e := ev.(type)`，但不需要新变量。
- `process.stdout.write(...)`：往标准输出写，不换行。相当于 `fmt.Print`。`console.log` 相当于 `fmt.Println`。
- `throw new Error(ev.message)`：抛错，`throw` 之后不用 `break`，因为不会再往下走。
- `default: assertNever(ev);`：四种都处理完了，走到 `default` 时 `ev` 的类型被收窄成 `never`（“不可能有值”）。
- `function assertNever(x: never): never`：参数只接受 `never`。**如果以后有人往 `StreamEvent` 里加了第五种 `{ type: "thinking" }`，却忘了加 `case`**，那 `default` 里的 `ev` 就是 `{ type: "thinking" }` 而不是 `never`，传给 `assertNever` 就会**编译报错**。这是 Go 的 type switch 做不到的：Go 漏了一个 case 只会静默跳过。
- 返回类型 `never` 表示“这个函数永远不会正常返回”（总是 throw）。
- `JSON.stringify(x)`：把对象转成 JSON 字符串，相当于 `json.Marshal`。

OpenAI Responses、Anthropic Messages 的流式事件本质上都是这种结构，你自己 Agent 内部的事件也应该这么定义。

## 3. Zod：运行时校验 + 类型推导 + JSON Schema 一次写完

### 问题在哪

TS 的类型**只在编译期存在**，编译成 JS 后全部消失。而 LLM 的输出**只在运行时出现**。所以：

```ts
const args = JSON.parse(modelOutput) as SearchArgs;  // 编译器信了你
args.query.trim();  // 运行时：模型没给 query → 崩溃 "Cannot read properties of undefined"
```

`as` 只是让编译器闭嘴，运行时什么也不检查。Go 的 `json.Unmarshal` 至少会把缺失字段填零值，TS 连这个都没有。

**Zod** 是一个库：你用它的 API 描述数据形状，它同时给你三样东西：
1. **运行时校验函数**（真的去检查每个字段）
2. **TS 类型**（自动推导，不用再手写一份 `type`）
3. **JSON Schema**（直接发给模型当工具定义）

### 定义一个工具参数

```ts
import { z } from "zod"; // zod v4

const SearchArgs = z.object({
  query: z.string().min(1).describe("搜索关键词"),
  topK: z.number().int().min(1).max(20).default(5),
});

type SearchArgs = z.infer<typeof SearchArgs>;
```

逐行解读：

- `import { z } from "zod"`：`z` 是 Zod 的入口对象，所有构造函数都挂在它上面。
- `z.object({...})`：描述“一个对象，有这些字段”。类似 Go 的 struct 定义，但它是一个**运行时的值**（存在变量 `SearchArgs` 里），不只是类型。
- `query: z.string().min(1).describe("搜索关键词")`：
  - `z.string()`：必须是字符串
  - `.min(1)`：长度至少 1（不能是空串）
  - `.describe(...)`：附一段说明，会出现在生成的 JSON Schema 里，**模型能看到**，相当于工具参数的文档
  - 这种 `.a().b().c()` 连着调的写法叫链式调用，每一步返回一个新的 schema
- `topK: z.number().int().min(1).max(20).default(5)`：必须是整数，1 到 20 之间，**没传就默认 5**。
- `type SearchArgs = z.infer<typeof SearchArgs>;`：这行最绕，拆开看：
  - `typeof SearchArgs`：取变量 `SearchArgs`（Zod schema 对象）的类型
  - `z.infer<...>`：Zod 提供的泛型工具，从 schema 的类型里“推导出它校验通过后的数据类型”
  - 结果就是 `{ query: string; topK: number }`
  - 变量和类型可以同名（都叫 `SearchArgs`），TS 根据位置区分：写在 `:` 后面是类型，写在表达式里是变量。

**一份定义，同时得到校验器和类型**，两者永远不会不一致。Go 里你得写一个 struct + 一个 `Validate()` 方法 + 手写一份 JSON Schema，三处改一处忘一处是常事。

### 用它生成工具定义（发给模型）

```ts
const toolDef = {
  name: "search",
  description: "搜索内部文档",
  parameters: z.toJSONSchema(SearchArgs),
};
```

`z.toJSONSchema(SearchArgs)` 生成大致这样的对象：

```json
{
  "type": "object",
  "properties": {
    "query": { "type": "string", "minLength": 1, "description": "搜索关键词" },
    "topK":  { "type": "integer", "minimum": 1, "maximum": 20, "default": 5 }
  },
  "required": ["query"]
}
```

这正是 OpenAI / Anthropic 工具调用 API 要的 `parameters` / `input_schema` 格式。

### 校验模型返回的参数

```ts
type ParseResult =
  | { ok: true; data: SearchArgs }
  | { ok: false; error: string };

function parseArgs(raw: string): ParseResult {
  let json: unknown;
  try {
    json = JSON.parse(raw);
  } catch {
    return { ok: false, error: "参数不是合法 JSON" };
  }

  const r = SearchArgs.safeParse(json);
  if (r.success) {
    return { ok: true, data: r.data };
  }
  return { ok: false, error: z.prettifyError(r.error) };
}
```

逐行解读：

- `type ParseResult = | {ok: true; ...} | {ok: false; ...}`：又是一个可辨识联合，标签是 `ok`。这是 TS 里模仿 Go `(value, error)` 的常见写法：成功时有 `data`，失败时有 `error`，调用方 `if (r.ok)` 之后就能安全访问 `r.data`。
- `let json: unknown;`：先声明变量，类型是 `unknown`。用 `let` 因为下一行要赋值。
- `try { json = JSON.parse(raw); } catch { ... }`：`JSON.parse` 遇到非法 JSON 会 throw（Go 的 `json.Unmarshal` 是返回 err）。`catch` 后面不写 `(err)` 表示不关心错误内容。
- `SearchArgs.safeParse(json)`：真正的运行时校验。**不会 throw**，返回一个结果对象 `r`：
  - 成功：`{ success: true, data: {query: "...", topK: 5} }`，注意 `default(5)` 已经填进去了
  - 失败：`{ success: false, error: ZodError }`
- `z.prettifyError(r.error)`：把错误转成人能读的文字，比如：
  ```
  ✖ Too big: expected number to be <=20
    → at topK
  ```

调用方：

```ts
const r = parseArgs(toolCall.args);
if (!r.ok) {
  // 把错误文本作为工具结果回传给模型，让它修正参数重试
  return { id: toolCall.id, output: r.error, isError: true };
}
await search(r.data.query, r.data.topK); // 这里 r.data 类型确定，而且真的校验过
```

**校验失败不抛异常，而是把错误文本回传给模型**，让它下一轮自己修正。这和 Go 课里“工具失败作为观察回传，`return nil`”是同一个思想。

`parse` vs `safeParse`：
- `SearchArgs.parse(x)`：失败直接 throw。用于**内部配置**，启动时错了就该崩。
- `SearchArgs.safeParse(x)`：失败返回结果对象。用于**信任边界外**的数据（模型、用户、第三方 API）。

## 4. AsyncGenerator：把 SSE 变成 `for await`

### 先理解两个概念

**普通生成器（generator）**：一个可以“暂停-继续”的函数，每次产出一个值。

```ts
function* count() {     // 注意 function 后面的 *，表示这是生成器
  yield 1;              // 产出 1，然后暂停在这里
  yield 2;              // 下次继续，产出 2，再暂停
  yield 3;
}

for (const n of count()) {
  console.log(n);       // 1, 2, 3
}
```

对照 Go：这就像一个 goroutine 往 channel 里发 1、2、3，然后 `close`；调用方 `for n := range ch`。区别是：**生成器是“拉”模式**，调用方要下一个值时它才执行到下一个 `yield`，不会提前跑；没有额外的 goroutine，也就不存在泄漏。

**异步生成器（async generator）**：生成器里面可以 `await`。

```ts
async function* ticks() {   // async + function*
  for (let i = 0; i < 3; i++) {
    await sleep(100);       // 等 100ms
    yield i;
  }
}

for await (const t of ticks()) {   // for await：每拿一个值都要等
  console.log(t);                  // 0, 1, 2，每隔 100ms 一个
}
```

这正好适合流式读取：网络数据一点点到达，读到一条完整事件就 `yield` 一条。

### 解析 SSE

先回忆 SSE 格式（0.1 讲过）：

```
data: {"type":"text","delta":"你"}

data: {"type":"text","delta":"好"}

data: [DONE]

```

每条事件是一行 `data: ...`，事件之间空一行，最后以 `[DONE]` 结束。

```ts
async function* sseEvents(res: Response): AsyncGenerator<string> {
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
```

### 逐行解读

**函数签名**

- `async function* sseEvents(res: Response): AsyncGenerator<string>`：异步生成器，接收一个 `fetch` 返回的 `Response`，每次产出一个 `string`（`data:` 后面的内容）。

**拿到读取器**

- `if (!res.body) throw ...`：`res.body` 是响应体流，类型是 `ReadableStream | null`，可能为空，严格模式逼你先检查。`!res.body` 意思是“body 为空”。
- `res.body.pipeThrough(new TextDecoderStream())`：
  - `res.body` 是**字节流**（类似 Go 的 `io.Reader` 读出 `[]byte`）
  - `TextDecoderStream` 把字节流转成**字符串流**，按 UTF-8 解码
  - `pipeThrough` 就是“接一根管子”，相当于 Go 里 `bufio.NewReader(transform.NewReader(body, utf8Decoder))`
  - **为什么不能每块单独解码**：网络按字节到达，“你”这个字 UTF-8 是 3 个字节，可能前 2 个字节在第一块、第 3 个在第二块。单独解码就会出现乱码 `�`。`TextDecoderStream` 会把不完整的字节留到下一块。
- `.getReader()`：拿到一个读取器，之后用 `reader.read()` 一块一块读。

**主循环**

- `let buf = "";`：缓冲区，攒还没凑成完整一行的文本。
- `while (true)`：等同于 Go 的 `for {}`。
- `const { value, done } = await reader.read();`：
  - `reader.read()` 返回 `Promise<{ value, done }>`，`await` 等它
  - 用解构一次取出两个字段：`value` 是这次读到的字符串块，`done` 表示流是否结束
  - 相当于 Go 的 `n, err := r.Read(buf); if err == io.EOF {...}`
- `if (done) return;`：流结束了，生成器结束（相当于 `close(ch)`）。
- `buf += value;`：把新数据接到缓冲区末尾。

**按行切**

- `let idx: number;`：声明一个变量存“换行符位置”。
- `while ((idx = buf.indexOf("\n")) >= 0)`：这一行做了两件事：
  1. `idx = buf.indexOf("\n")`：找第一个换行符的位置，找不到返回 `-1`（同 Go 的 `strings.Index`）
  2. 赋值表达式本身的值就是 `idx`，判断 `>= 0`
  
  意思是：**只要缓冲区里还有完整的一行，就一直处理**。一块数据可能包含好几行，也可能只有半行（那就留在 `buf` 里等下一块）。
- `const line = buf.slice(0, idx).trim();`：取出这一行（`slice(0, idx)` = Go 的 `buf[:idx]`），`trim()` 去掉首尾空白（含 `\r`）。
- `buf = buf.slice(idx + 1);`：把这一行连同换行符从缓冲区删掉（= `buf[idx+1:]`）。
- `if (!line.startsWith("data:")) continue;`：不是 `data:` 开头的（空行、`event:`、`: 注释`）跳过。
- `const data = line.slice(5).trim();`：去掉前 5 个字符 `data:`，再去空格。
- `if (data === "[DONE]") return;`：结束标记。注意 TS 比较用 `===`（三个等号），**永远不要用 `==`**，后者会做奇怪的类型转换。
- `yield data;`：产出这条数据，**函数在这里暂停**，直到调用方的 `for await` 要下一条。

**清理**

- `finally { reader.releaseLock(); }`：无论是正常结束、调用方 `break`、还是出错，都会执行。这就是 TS 版的 `defer`。`releaseLock()` 释放读取器，流可以被关闭/回收。

### 怎么用，以及怎么取消

```ts
const ctrl = new AbortController();

const res = await fetch("https://api.example.com/chat", {
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ stream: true, messages: [] }),
  signal: ctrl.signal,
});

let count = 0;
for await (const data of sseEvents(res)) {
  const json: unknown = JSON.parse(data);
  // 下一步交给 Zod 校验成 StreamEvent（见练习 1）
  count++;
  if (count >= 100) break;
}

// 别的地方（比如用户点了停止按钮）：
ctrl.abort();
```

逐行解读：

- `new AbortController()`：创建一个“取消器”，相当于 Go 的 `ctx, cancel := context.WithCancel(...)`。`ctrl.signal` 相当于 `ctx`，`ctrl.abort()` 相当于 `cancel()`。
- `fetch(url, { ... })`：第二个参数是选项对象：
  - `method`、`headers`、`body`：和 Go 的 `http.NewRequest` 一样
  - `JSON.stringify(...)`：对象转 JSON 字符串，= `json.Marshal`
  - `signal: ctrl.signal`：**把取消信号绑定到这次请求**，= `http.NewRequestWithContext(ctx, ...)`
- `for await (const data of sseEvents(res))`：每次循环拿一条 `data`。
- `break`：提前退出循环。这时 JS 会自动通知生成器“不要了”，生成器里的 `finally` 执行，`releaseLock()` 被调用。
- `ctrl.abort()`：取消后，正在等待的 `reader.read()` 会立刻 reject（抛出 `AbortError`），这个错误从生成器里冒出来，经过 `finally`，再从 `for await` 那一行抛给你；同时底层 TCP 连接被断开，**上游停止生成、停止计费**。

## 5. AbortController：TS 版的 context

| Go | TS | 说明 |
|---|---|---|
| `ctx, cancel := context.WithCancel(parent)` | `const ctrl = new AbortController()` | 手动取消 |
| `ctx` | `ctrl.signal`（类型 `AbortSignal`） | 往下传的就是它 |
| `cancel()` | `ctrl.abort()` | 触发取消 |
| `context.WithTimeout(ctx, 5*time.Second)` | `AbortSignal.timeout(5000)` | 超时自动取消，单位毫秒 |
| 子 ctx 继承父 ctx | `AbortSignal.any([a, b])` | 任一取消就取消 |
| `<-ctx.Done()` | `signal.addEventListener("abort", fn)` | 取消时回调 |
| `ctx.Err() != nil` | `signal.aborted` | 是否已取消（布尔值） |
| `ctx.Err()` | `signal.reason` | 取消原因 |

一个和 Go 的关键不同：Go 的 `ctx` 是一棵树，子 ctx 自动继承父 ctx。TS 没有父子关系，要用 `AbortSignal.any` **手动合并**。

```ts
async function runTurn(userSignal: AbortSignal): Promise<void> {
  const signal = AbortSignal.any([userSignal, AbortSignal.timeout(60_000)]);
  const res = await fetch(LLM_URL, { method: "POST", body: "...", signal });
  // ... 再把 signal 一路往下传给每个工具
}
```

逐行解读：

- `userSignal: AbortSignal`：调用方传进来的信号，比如用户点“停止”时会触发。
- `AbortSignal.timeout(60_000)`：一个 60 秒后自动触发的信号。
- `AbortSignal.any([a, b])`：合成一个新信号，`a` 或 `b` 任一触发它就触发。整体效果等同于 Go：
  ```go
  ctx, cancel := context.WithTimeout(userCtx, 60*time.Second)
  defer cancel()
  ```
- TS 这边**不需要 `defer cancel()`**：`AbortSignal.timeout` 的定时器由运行时自己管理。

区分“被取消”和“真出错”：

```ts
try {
  await runTurn(signal);
} catch (err) {
  if (err instanceof Error && err.name === "AbortError") {
    console.log("用户取消了");        // 手动 abort()
  } else if (err instanceof Error && err.name === "TimeoutError") {
    console.log("超时了");            // AbortSignal.timeout 触发
  } else {
    throw err;                        // 真正的错误，继续往上抛
  }
}
```

- `err instanceof Error`：检查 `err` 是不是 `Error` 对象（前面说过 `catch` 里的 `err` 是 `unknown`，要先检查才能访问 `.name`）。等价于 Go 的 `errors.As`。
- `err.name === "AbortError"`：等价于 Go 的 `errors.Is(err, context.Canceled)`。

规范（和 Go 一致）：
- `signal` 作为参数显式往下传。
- 自己写的长循环里要检查：`signal.throwIfAborted();`，已取消就立刻抛错（= Go 里 `if ctx.Err() != nil { return ctx.Err() }`）。

## 6. 并行工具调用：带并发上限 + 单工具超时

这是 0.1 里 `runTools`（errgroup 版）的 TS 翻译。先看完整代码，再逐行拆。

```ts
type ToolCall = { id: string; name: string; args: string };
type ToolResult = { id: string; output: string; isError: boolean };
type Tool = (args: string, signal: AbortSignal) => Promise<string>;

type RunOptions = {
  signal: AbortSignal;
  limit?: number;
  perToolMs?: number;
};

async function runTools(
  calls: ToolCall[],
  tools: Record<string, Tool>,
  opts: RunOptions,
): Promise<ToolResult[]> {
  const { signal, limit = 4, perToolMs = 20_000 } = opts;
  const results: ToolResult[] = new Array(calls.length);
  let next = 0;

  async function worker(): Promise<void> {
    while (true) {
      const i = next++;
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
        if (signal.aborted) throw err;
        results[i] = { id: c.id, output: String(err), isError: true };
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
```

### 逐行解读

**类型定义**

- `ToolCall`：模型要求调用的工具，`args` 是原始 JSON 字符串。
- `ToolResult`：执行结果，`isError` 标记是否失败（失败的结果也要回传给模型）。
- `Tool`：一个工具就是一个异步函数：收参数和取消信号，返回 `Promise<string>`。对应 Go 的 `func(ctx, args) (string, error)`，错误走 throw。
- `RunOptions`：选项对象，`limit` 和 `perToolMs` 带 `?`，可以不传。TS 没有 Go 那种 functional options，通常就用一个选项对象。

**函数签名**

- `tools: Record<string, Tool>`：工具注册表，= Go 的 `map[string]Tool`。
- 返回 `Promise<ToolResult[]>`：因为是 `async` 函数，返回值自动包一层 Promise。

**准备**

- `const { signal, limit = 4, perToolMs = 20_000 } = opts;`：解构 + 默认值（见 0.9）。
- `const results: ToolResult[] = new Array(calls.length);`：预先开一个长度为 N 的数组，= Go 的 `make([]ToolResult, len(calls))`。
- `let next = 0;`：下一个待执行任务的下标。**所有 worker 共享这个变量**。

**worker：一个“工人”，不停领任务直到领完**

- `async function worker()`：在 `runTools` 里面定义一个函数。它能直接访问外层的 `calls`、`next`、`results`（闭包，和 Go 的匿名函数捕获外部变量一样）。
- `const i = next++;`：领一个任务号。`next++` 的意思是“先取 next 的当前值给 i，再把 next 加 1”。
  - **为什么不用加锁**：JS 只有一个线程，这一行是同步执行的，不可能两个 worker 同时执行它。worker 之间只会在 `await` 那一行切换。
  - 在 Go 里这样写是数据竞争，`go test -race` 会报错，必须用 `atomic.AddInt64` 或 mutex。
- `const c = calls[i]; if (!c) return;`：开了 `noUncheckedIndexedAccess`，`calls[i]` 类型是 `ToolCall | undefined`。`i` 超出范围时得到 `undefined`，说明任务领完了，这个 worker 结束。
- `const tool = tools[c.name]; if (!tool) {...}`：按名字查工具；查不到（模型编了一个不存在的工具名）就记一条错误结果，`continue` 领下一个任务。

**执行一个工具**

- `const s = AbortSignal.any([signal, AbortSignal.timeout(perToolMs)]);`：这个工具的取消信号 = 整体取消 **或** 单工具超时。= Go 的 `tctx, cancel := context.WithTimeout(ctx, perTool)`。
- `const output = await tool(c.args, s);`：调用工具并等待结果。**这里是让出点**：这个 worker 暂停，其他 worker 可以继续执行。
- `results[i] = { id: c.id, output, isError: false };`：`output` 是 `output: output` 的简写（字段名和变量名相同时可省略）。

**失败处理：分两种**

- `catch (err)`：工具抛错了，原因可能是工具本身出错、单工具超时、或者整体被取消。
- `if (signal.aborted) throw err;`：**整体被取消**（用户点了停止 / 整轮超时），没必要继续了，往上抛，整个 `runTools` 失败。= Go 里的 `return err`。
- 否则：**只是这个工具失败**，把错误转成文字记进结果，回传给模型。`String(err)` 把任意值转成字符串。= Go 里的 `results[i].Err = err; return nil`。

**启动 worker 并等待**

- `const n = Math.min(limit, calls.length);`：worker 数量 = 并发上限和任务数取小。3 个任务就没必要开 4 个 worker。
- `workers.push(worker());`：**调用 `worker()` 但不 `await`**，它立刻开始执行，直到第一个 `await` 让出，然后返回一个 Promise。`push` 把这个 Promise 放进数组（= Go 的 `append`）。这一步效果等同于开了 n 个 goroutine。
- `await Promise.all(workers);`：等所有 worker 完成。= Go 的 `g.Wait()`。任一 worker 抛错（只会是整体取消），`Promise.all` 立刻 reject。

**和 Go 版对照**

| Go（0.1） | TS |
|---|---|
| `g.SetLimit(4)` | 开 `limit` 个 worker |
| `g.Go(func() error {...})` | `workers.push(worker())` |
| `g.Wait()` | `await Promise.all(workers)` |
| 每个 goroutine 写 `results[i]` | 每个 worker 写 `results[i]` |
| 工具失败 `return nil` | `catch` 里记结果，不抛 |
| 整体取消 `return err` | `if (signal.aborted) throw err` |

> 你也会看到别人写 `Promise.all(Array.from({ length: n }, worker))`，意思和上面的 `for` 循环一样：`Array.from({ length: n }, fn)` 会调用 `fn` n 次，把返回值组成数组。本课用 `for` 循环，更好读。

**`Promise.all` vs `Promise.allSettled`**：
- `Promise.all`：任一失败就立刻失败。
- `Promise.allSettled`：等全部完成，每个结果标记成功/失败，自己永远不会失败。

这里故意用 `all`：单个工具失败已经在 `try/catch` 里变成了结果，只有整体取消才会抛到这一层，这时就应该立刻停。

## 7. 三个高频坑

### 坑 1：忘了 `await`

```ts
async function handle() {
  saveToDb(result);   // ❌ 没 await：函数开始执行，但你没等它
  return "ok";        // 立刻返回；如果 saveToDb 后来失败了，没人接这个错误
}
```

没被接住的 Promise 失败叫 unhandled rejection，Node 默认会**直接退出进程**。类似 Go 里 `go saveToDb()` 然后 goroutine 里 panic。

解决：开 ESLint 规则 `@typescript-eslint/no-floating-promises`，没 await 的 Promise 直接报错。真想“发出去不管”就显式写 `void saveToDb(result).catch(log)`。

### 坑 2：用 `as` 骗编译器

```ts
const call = JSON.parse(raw) as ToolCall; // 编译通过
call.name.toUpperCase();                  // 运行时：模型没给 name → 崩
```

`as` 不做任何检查。**外部数据只能 Zod 校验后再用。** `as` 只在你比编译器更清楚、而且数据来自你自己的代码时才用（练习 4 里有一个例子）。

### 坑 3：abort 了，但资源没释放

```ts
const badTool: Tool = async (args, signal) => {
  await new Promise((r) => setTimeout(r, 30_000)); // 完全没理会 signal
  return "done";
};
```

`runTools` 那边 `AbortSignal.timeout` 到点了，`await` 因此提前结束吗？**不会**。`AbortSignal` 只是一个“通知”，**工具自己不检查，就没人能打断它**。这和 Go 一样：goroutine 不读 `ctx.Done()`，cancel 了也照样跑。

正确写法见练习 2 的 `sleep(ms, signal)`：监听 `abort` 事件，清掉定时器，立刻 reject。`fetch` 自带这个能力，所以把 `signal` 传给 `fetch` 就够了；你自己用 `setTimeout`、子进程、数据库连接时，要自己处理。

---

## 练习（今天做完）

### 环境准备

```bash
mkdir -p projects/01-ts-agent-core/src && cd projects/01-ts-agent-core
npm init -y                       # 生成 package.json，相当于 go mod init
npm pkg set type=module           # 用 ES 模块（import/export）
npm i zod                         # 运行时依赖，相当于 go get
npm i -D typescript vitest @types/node   # -D：只在开发时用（编译器、测试框架、Node 类型定义）
npx tsc --init                    # 生成 tsconfig.json，再按第 1 节改
```

- `npx xxx`：运行项目里安装的命令行工具。
- `vitest` 是测试框架，相当于 `go test`。测试文件命名为 `xxx.test.ts`。
- 目录建议：`src/events.ts`、`src/sleep.ts`、`src/runTools.ts`、`src/sse.ts`，测试放 `src/*.test.ts`。

### 题目

1. 用 Zod 定义 `StreamEvent` 可辨识联合（`z.discriminatedUnion`），写 `parseEvent(data: string)`，非法输入返回错误而不是抛异常
2. 写可取消的 `sleep(ms, signal)`，abort 时立刻 reject 并清掉定时器
3. 用 `runTools` 跑三个假工具（50ms / 100ms / 5s），`perToolMs = 200`，断言前两个成功、第三个 `isError: true`、总耗时 < 400ms
4. 用 `node:http` 起一个假 SSE 服务，客户端读到 3 个事件后 `abort()`，断言服务端收到 `close` 事件
5. `npx tsc --noEmit` 零错误，`npx vitest run` 全绿

建议先自己写，卡住了再看下面的参考实现。

## 自测题

1. 为什么 `switch` 的 `default` 里要调 `assertNever`？
2. 工具参数 Zod 校验失败，为什么不抛异常而是回传给模型？
3. `for await` 里 `break`，generator 里的 `finally` 会执行吗？为什么？
4. `AbortSignal.any` 解决了什么问题？对应 Go 的什么？
5. 为什么 `runTools` 里 `next++` 不需要锁，而 Go 里同样写法会被 `-race` 报出来？

---

# 参考答案与详解

## 自测题答案

### 1. `assertNever` 的作用
`default` 分支里 `ev` 的类型应该是 `never`，因为所有情况都处理完了。以后有人往 `StreamEvent` 里加了 `{ type: "thinking" }` 却忘了加 `case`，`default` 里的 `ev` 就变成了 `{ type: "thinking" }`，传给只接受 `never` 的 `assertNever` 会**编译报错**。这把“漏处理一种事件”从运行时 bug 变成了编译错误。Go 的 type switch 漏写 case 只会静默跳过。

### 2. 校验失败为什么回传给模型
模型编错参数是**常态**，不是异常。回传类似 `topK: Too big, expected <=20` 的错误文本，模型下一轮通常能自己修正参数重试。抛异常会让整轮对话失败，用户什么也拿不到。只有“系统坏了”（网络断、配置错、被取消）才应该抛。和 0.1 里“工具失败 `return nil`，把错误写进结果”是同一个原则。

### 3. `break` 后 `finally` 会执行吗
会。`for await...of` 提前退出（`break`、`return`、循环体里抛错）时，JS 会自动调用生成器的 `return()` 方法，生成器从暂停的 `yield` 处“被结束”，执行 `finally`。所以把 `releaseLock()`、关连接放在 `finally` 里是安全的，相当于 Go 的 `defer`。

例外：如果你不用 `for await`，而是手动调用 `gen.next()`，中途把生成器丢掉不管，`finally` **不会**执行。所以总是用 `for await` 消费生成器。

### 4. `AbortSignal.any`
把多个取消源合成一个：用户停止 OR 整轮超时 OR 单工具超时，任一触发就取消。Go 的 ctx 天然是树形继承的（`WithTimeout(parent, d)` 自动带上父 ctx 的取消），TS 的 `AbortSignal` 没有父子关系，`AbortSignal.any` 就是用来补这个的。

### 5. 为什么 `next++` 不需要锁
JS 运行在单线程的事件循环上。`const i = next++` 是一段同步代码，执行过程中不可能被别的 worker 插进来；worker 之间只在 `await` 处切换。所以不存在两个 worker 同时读写 `next`。

Go 的 goroutine 可以跑在多个 CPU 核上**真正同时执行**，两个 goroutine 同时 `next++`（读、加一、写回三步）就可能都读到同一个值，这就是数据竞争，`-race` 会报。Go 版要么用 `atomic.AddInt64(&next, 1)`，要么像 0.1 那样每个 goroutine 预先分到自己的下标 `i`。

## 练习参考实现

### 练习 1：`parseEvent`（`src/events.ts`）

```ts
import { z } from "zod";

export const StreamEventSchema = z.discriminatedUnion("type", [
  z.object({ type: z.literal("text"), delta: z.string() }),
  z.object({ type: z.literal("tool_call"), id: z.string(), name: z.string(), args: z.string() }),
  z.object({ type: z.literal("done"), usage: z.object({ input: z.number(), output: z.number() }) }),
  z.object({ type: z.literal("error"), message: z.string() }),
]);

export type StreamEvent = z.infer<typeof StreamEventSchema>;

export type ParseEventResult =
  | { ok: true; event: StreamEvent }
  | { ok: false; error: string };

export function parseEvent(data: string): ParseEventResult {
  let json: unknown;
  try {
    json = JSON.parse(data);
  } catch {
    return { ok: false, error: "invalid json" };
  }
  const r = StreamEventSchema.safeParse(json);
  if (r.success) return { ok: true, event: r.data };
  return { ok: false, error: z.prettifyError(r.error) };
}
```

逐行解读：

- `z.discriminatedUnion("type", [...])`：告诉 Zod “这是一个可辨识联合，标签字段叫 `type`”。Zod 会先看 `type` 的值，再用对应的那个 schema 校验，报错信息也更精确（“type 是 text 但缺 delta”，而不是“四种都不匹配”）。
- `z.literal("text")`：值必须正好是字符串 `"text"`，对应第 2 节的字面量类型。
- `export type StreamEvent = z.infer<typeof StreamEventSchema>;`：推导出的类型和第 2 节手写的那个 `StreamEvent` **完全一样**。以后就用这个，不再手写。
- 其余和第 3 节的 `parseArgs` 相同。

测试（`src/events.test.ts`）：

```ts
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
```

- `test("名字", () => {...})`：定义一个测试用例，= Go 的 `func TestXxx(t *testing.T)`。第二个参数是箭头函数，测试体写在里面。
- `expect(a).toBe(b)`：断言 `a === b`，= `if a != b { t.Fatal() }`。
- `expect(a).toEqual(b)`：断言两个对象**内容相同**（深比较），= `reflect.DeepEqual`。
- `import ... from "./events.js"`：注意写 `.js` 而不是 `.ts`，这是 `NodeNext` 模块规则的要求（编译后文件是 `.js`）。新手最常踩的坑之一。
- `if (r.ok) expect(r.event)...`：必须先判断 `r.ok`，否则编译器不让你访问 `r.event`。

### 练习 2：可取消的 `sleep`（`src/sleep.ts`）

```ts
export function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    if (signal?.aborted) {
      reject(signal.reason);
      return;
    }

    const onAbort = () => {
      clearTimeout(timer);
      reject(signal!.reason);
    };

    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);

    signal?.addEventListener("abort", onAbort, { once: true });
  });
}
```

逐行解读：

- `signal?: AbortSignal`：参数后面带 `?`，可以不传。
- `new Promise<void>((resolve, reject) => {...})`：**手动创建一个 Promise**。你传进去一个函数，它会立即执行，并拿到两个“开关”：
  - `resolve()`：调用它 = Promise 成功（`await` 那边拿到结果，继续往下走）
  - `reject(err)`：调用它 = Promise 失败（`await` 那边抛出 `err`）
  
  当你要把“回调风格”的 API（比如 `setTimeout`）包装成可以 `await` 的东西时，就用这个写法。Go 里大致相当于：
  ```go
  done := make(chan error, 1)
  // ... 在某处 done <- nil 或 done <- err
  return <-done
  ```
- `if (signal?.aborted) { reject(...); return; }`：如果传进来时已经取消了，立刻失败，不开定时器。
- `const onAbort = () => {...}`：取消时要执行的函数：清掉定时器（不然它到点还会触发）、让 Promise 失败。
  - `signal!.reason`：这里用 `!`，因为 `onAbort` 只会在 signal 存在时被注册，我们比编译器更清楚它不为空。
- `const timer = setTimeout(fn, ms)`：`ms` 毫秒后执行 `fn`，返回一个定时器句柄。= Go 的 `time.AfterFunc(d, fn)`。
  - 定时器到点：先移除 abort 监听器（不再需要了，避免泄漏），再 `resolve()`。
- `signal?.addEventListener("abort", onAbort, { once: true })`：signal 被取消时调用 `onAbort`。`{ once: true }` 表示触发一次后自动移除。
- `onAbort` 里引用了 `timer`，但 `timer` 定义在 `onAbort` 后面。这没问题：`onAbort` 只会在之后被调用，那时 `timer` 已经赋值了。

测试：

```ts
test("abort 立刻打断 sleep", async () => {
  const ctrl = new AbortController();
  const start = Date.now();
  setTimeout(() => ctrl.abort(), 20);              // 20ms 后取消
  await expect(sleep(1000, ctrl.signal)).rejects.toThrow();
  expect(Date.now() - start).toBeLessThan(100);    // 远小于 1000ms
});
```

- `async () => {...}`：测试函数里要 `await`，所以写成 async 箭头函数。
- `Date.now()`：当前毫秒时间戳，= `time.Now().UnixMilli()`。
- `expect(promise).rejects.toThrow()`：断言这个 Promise 会失败。前面要加 `await`。

### 练习 3：并行工具与超时（`src/runTools.test.ts`）

```ts
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
```

逐行解读：

- `import { runTools, type Tool }`：`type Tool` 表示只导入类型（`verbatimModuleSyntax` 要求的写法）。`runTools.ts` 里要写 `export type Tool = ...` 和 `export async function runTools`。
- `const fakeTool = (ms: number): Tool => { return async (_args, signal) => {...} }`：**一个返回函数的函数**（工厂）。`fakeTool(50)` 返回一个“睡 50ms 的工具”。
  - 内层函数的参数没写类型，因为返回类型标了 `Tool`，TS 会自动推导出 `_args: string, signal: AbortSignal`。
  - `_args` 前面的下划线是约定：这个参数用不到（= Go 里的 `_`）。
- `runTools([...], {...}, {...})`：三个参数分别是调用列表、工具表、选项。
- `res.map((r) => r.isError)`：把结果数组的每一项映射成 `isError`，得到 `[false, false, true]`。`map` 相当于 Go 里写一个 for 循环构造新切片。
- 为什么能在约 200ms 结束：`slow` 工具 200ms 时被 `AbortSignal.timeout` 取消，因为 `sleep` 正确响应了 signal，`await` 立刻抛错，被 `runTools` 的 `catch` 记成 `isError: true`。**如果工具不理 signal（坑 3），这个测试会等 5 秒。**

### 练习 4：服务端感知断开（`src/sse.test.ts`）

```ts
import { test, expect } from "vitest";
import http from "node:http";
import { once } from "node:events";
import type { AddressInfo } from "node:net";
import { sseEvents } from "./sse.js";

test("客户端 abort 后服务端收到 close", async () => {
  // 1. 准备一个“等服务端关闭”的 Promise
  let markClosed!: () => void;
  const closed = new Promise<void>((resolve) => {
    markClosed = resolve;
  });

  // 2. 起一个假 SSE 服务：每 20ms 发一条
  const server = http.createServer((req, res) => {
    res.writeHead(200, { "Content-Type": "text/event-stream" });
    let n = 0;
    const timer = setInterval(() => {
      res.write(`data: {"type":"text","delta":"t${n++}"}\n\n`);
    }, 20);
    res.on("close", () => {
      clearInterval(timer);
      markClosed();
    });
  });
  server.listen(0);
  await once(server, "listening");
  const { port } = server.address() as AddressInfo;

  // 3. 客户端读 3 条后取消
  const ctrl = new AbortController();
  const res = await fetch(`http://127.0.0.1:${port}`, { signal: ctrl.signal });
  let count = 0;
  try {
    for await (const _ of sseEvents(res)) {
      count++;
      if (count === 3) ctrl.abort();
    }
  } catch (err) {
    expect((err as Error).name).toBe("AbortError");
  }

  // 4. 断言服务端真的感知到了断开
  await closed;
  expect(count).toBe(3);
  server.close();
});
```

逐行解读：

**第 1 步：把 `resolve` 拿到外面**

- `let markClosed!: () => void;`：声明一个变量，类型是“无参无返回的函数”。`!` 告诉编译器“我保证用之前会赋值”。
- `new Promise<void>((resolve) => { markClosed = resolve; })`：创建 Promise 时把它的 `resolve` 开关存到外面。之后任何地方调用 `markClosed()`，`await closed` 就会继续。
- 这是 TS 里“一次性信号”的常用写法，= Go 的：
  ```go
  closed := make(chan struct{})
  // ... 某处 close(closed)
  <-closed
  ```

**第 2 步：假服务端**

- `http.createServer((req, res) => {...})`：= Go 的 `httptest.NewServer(http.HandlerFunc(func(w, r) {...}))`。`res` 相当于 `w`。
- `res.writeHead(200, {...})`：写状态码和响应头。
- `setInterval(fn, 20)`：每 20ms 执行一次 `fn`，= Go 的 `time.NewTicker`。
- `res.write(...)`：写一条 SSE 事件，注意结尾两个 `\n`。
- `res.on("close", () => {...})`：**连接关闭时**的回调，= Go 里 `<-r.Context().Done()`。客户端 abort 后这里会被触发：停掉定时器，并调用 `markClosed()`。
- `server.listen(0)`：端口 0 表示让系统随机分配一个空闲端口。
- `await once(server, "listening")`：等服务器真正开始监听。`once` 把“等某个事件触发一次”包装成 Promise。
- `server.address() as AddressInfo`：拿到实际端口。`address()` 的返回类型比较宽（可能是 string 或 null），我们知道 TCP 服务一定返回 `AddressInfo`，所以用 `as`。**这是 `as` 的合理用法**：数据来自 Node 自己，不是外部输入。

**第 3 步：客户端**

- `for await (const _ of sseEvents(res))`：用不到每条的内容，变量名写 `_`。
- `if (count === 3) ctrl.abort();`：读到第 3 条时取消。之后生成器里的 `reader.read()` 抛出 `AbortError`，经过 `finally`，从 `for await` 抛出来。
- `catch (err) { expect((err as Error).name).toBe("AbortError"); }`：确认抛出来的确实是取消错误。`(err as Error)` 是因为 `err` 是 `unknown`。

**第 4 步：断言**

- `await closed;`：等服务端的 `close` 回调触发。如果取消链断了（连接没真正关闭），这里会一直等，最后被 vitest 的默认 5 秒超时判失败。**这正是我们要验证的：用户取消 → 上游连接真的断了 → 不再计费。**
- `server.close()`：关掉服务器，= `defer srv.Close()`。

### 练习 5：类型检查与测试

```bash
npx tsc --noEmit   # 只做类型检查，不输出文件，= go vet + go build 的类型检查部分
npx vitest run     # 跑一遍所有测试后退出，= go test ./...
```

两条都通过后，把输出贴给我。我会批改，并在 ROADMAP 里勾上 TypeScript。0.1 Go 的练习和 `go test -race` 结果也一起发来，一并勾。
