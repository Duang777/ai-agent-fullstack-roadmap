# 4.5 TS 生态：Vercel AI SDK、Mastra 与 OpenAI Agents SDK (JS)

> 第 4 部分 · 第 5 课 ｜ 预计 4–5 小时 ｜ 前置：0.2 TypeScript、2.1 Function Calling、3.2 Agent Loop、3.4 长任务 Agent、4.2 OpenAI Agents SDK、4.4 Go 生态
>
> 配套练习：`projects/28-ts-ecosystem/`（25 个测试）。和 4.4 一样直接用真库：Vercel AI SDK `ai` 7.0.183、Mastra `@mastra/core` 1.86.0、OpenAI Agents SDK `@openai/agents` 0.22.0。测试里的模型都是按脚本回复的假模型，不需要 API key。内容包括：AI SDK 的 `ToolLoopAgent`、`generateText` 和 agent 的默认步数差异、`isStepCount`、三类工具错误怎么交给模型、并发执行工具、流式事件；`toolApproval` 的审批流程，以及“历史由客户端保存”时审批被篡改的风险和 HMAC 签名；Mastra 的 Agent（直接用 AI SDK 的模型对象）、默认 `maxSteps`、`requireApproval` 和工具内 `suspend()`、跨实例恢复、workflow 的 `branch` 与步骤级 suspend / resume；OpenAI Agents SDK JS 的 `run`、`maxTurns`、工具错误的默认文案、`toolNotFoundBehavior`、`needsApproval` 与 `RunState` 序列化。最后用同一个脚本对比三家的步数上限。

## 读前说明

4.2 讲 OpenAI Agents SDK 时用的是 Python 版，4.4 讲的是 Go 库。这一课换到 TypeScript：做 Web 产品的团队，模型调用很可能就写在 Next.js 或 Node 服务里，TS 这边的选择主要就是这三个。

|   |  Vercel AI SDK |  Mastra |  OpenAI Agents SDK (JS) |
|---|---|---|---|
|  npm 包 |  `ai` |  `@mastra/core` |  `@openai/agents` |
|  本课版本 |  7.0.183 |  1.86.0 |  0.22.0 |
|  定位 |  统一的模型调用层 + 工具循环 + 前端 UI hooks |  基于 AI SDK 模型接口的应用框架：agent、workflow、memory、storage、服务端 |  OpenAI 官方的 agent 运行时，和 Python 版概念一一对应 |
|  模型接入 |  provider 包（`@ai-sdk/openai`、`@ai-sdk/anthropic`……）或字符串 `'openai/xxx'`（走 Vercel AI Gateway） |  字符串 `'provider/model'`（Mastra 的模型路由），或任何 AI SDK 规范的模型对象 |  默认 OpenAI Responses；其他模型通过 `Model` 接口或扩展包接入 |
|  agent 循环 |  `ToolLoopAgent`，`stopWhen` 默认 `isStepCount(20)` |  `Agent`，`maxSteps` 默认 5 |  `run()`，`maxTurns` 默认 10 |
|  人工审批 |  `toolApproval` → `tool-approval-request`，状态是消息历史 |  `requireApproval` / `suspend()`，状态是 storage 里的快照 |  `needsApproval` → `interruptions`，状态是 `RunState` |

版本号来自 npm registry（2026-11-26 查询，三个都是当天的最新版）。几点版本提醒：

- AI SDK 7 是 2026 年的大版本，相对 6 改了很多名字：`stepCountIs` → `isStepCount`，`system` → `instructions`，`onFinish` → `onEnd`，`fullStream` → `stream`，工具上的 `needsApproval` 废弃、改成调用或 agent 上的 `toolApproval`，`experimental_context` 拆成 `runtimeContext` 和按工具分的 `toolsContext`，结果上的 `usage`、`toolCalls` 等改成累计所有步骤（只看最后一步用 `finalStep`）。网上大量示例还是 v5 / v6 的写法，抄之前先看版本。7.0 只支持 ESM，最低 Node 22。
- Mastra 1.x 发版很快（本课写作时一天能发好几个版本），它在 `package.json` 里同时依赖 AI SDK v5、v6、v7 三代 provider 规范，所以三代的模型对象都能用。
- OpenAI Agents SDK JS 还在 0.x，minor 版本之间可能有破坏性变更。

本课依据：

- AI SDK：`ai` 包里自带的文档（`node_modules/ai/docs/`，和 ai-sdk.dev 同源）：Building Agents、Loop Control、Tool Approvals、Tools and Tool Calling、Testing、Migration Guide 7.0。
- Mastra：`@mastra/core` 包里自带的文档（`dist/docs/references/`，开头写着“Mastra docs are the canonical, current reference”）：Agents、Tools、Human-in-the-loop、Suspend and resume、`Agent.generate()` 参考。
- OpenAI Agents SDK JS：0.22.0 的类型声明（`run.d.ts`、`model.d.ts`、`tool.d.ts`、`toolGuardrail.d.ts`）和源码常量（`runner/constants`：`DEFAULT_MAX_TURNS = 10`）。

文档和实际行为不一致的地方，本课以测试为准，并在正文里注明。比如 AI SDK 的 Tools 文档说 `generateText` 遇到 `NoSuchToolError`、`InvalidToolInputError` 会抛出，但 7.0.183 实测是把它们作为错误结果交给模型、循环继续。

先想几个问题：

- `generateText` 不传 `stopWhen`、模型要调工具，工具会执行吗？模型会被调用几次？
- 三个框架都把上限设成 2，模型一直要工具，各自是抛错还是正常返回？
- 模型调用了一个不存在的工具名，三个框架默认各怎么处理？
- AI SDK 的审批状态放在消息历史里，消息历史由前端保存。前端能不能改掉审批人看到的参数？

## 1. 三个库的关系

```
           ┌────────────── 你的 Node / Next.js 服务 ──────────────┐
           │                                                      │
           │   Mastra Agent / Workflow      OpenAI Agents run()   │
           │        │  （storage 快照）           │（RunState）    │
           │        ▼                             ▼               │
           │   AI SDK 模型接口（LanguageModelV2/V3/V4）  Model 接口 │
           │        │                             │               │
           │   ToolLoopAgent / generateText       │               │
           │        │                             │               │
           └────────┼─────────────────────────────┼───────────────┘
                    ▼                             ▼
         @ai-sdk/openai、@ai-sdk/anthropic……    OpenAI Responses（默认）
```

- AI SDK 是最底下那一层：它定义了模型接口规范（provider 实现 `doGenerate` / `doStream`），在上面提供 `generateText`、`streamText`、`ToolLoopAgent`，以及 React / Vue / Svelte 的 `useChat` 等前端工具。
- Mastra 用的是 AI SDK 的模型接口，所以 AI SDK 的任何 provider 模型、包括测试用的 `MockLanguageModelV4`，都能直接交给 Mastra 的 `Agent`。练习里两个框架共用同一个 `ScriptedModel`，测试确认了模型收到的 prompt 结构完全一样（`system`、`user`、`assistant`、`tool` 四种消息）。Mastra 在这之上加了 workflow、memory、storage、HTTP 服务、Studio 调试界面。
- OpenAI Agents SDK 是独立的一套：自己的 `Model` 接口、自己的消息格式（Responses API 的 item：`message`、`function_call`、`function_call_result`），和 Python 版的概念一一对应（agent、handoff、guardrail、session、`RunState`）。

所以“AI SDK 还是 Mastra”不完全是二选一：Mastra 本身就站在 AI SDK 上。真正的选择是：只要模型层和工具循环（AI SDK），还是要一个带存储和流程编排的框架（Mastra），还是要 OpenAI 那套 agent 抽象（Agents SDK）。

## 2. 用 TS 写这些代码要先知道的（对照 Go）

练习代码用到的 TS 写法，对照 4.4 课的 Go 版本逐条说明。

**schema 是值，不是标签。** Go 里参数说明写在结构体标签上，框架用反射推断 JSON Schema：

```go
type DocQuery struct {
    Query string `json:"query" jsonschema:"要搜索的关键词"`
}
```

TS 的类型在运行时不存在，没法反射，所以三个框架都要求你传一个运行时的 schema 对象，通常是 zod：

```ts
const input = z.object({ orderId: z.string().describe('订单号，例如 A1') });
type Input = z.infer<typeof input>; // { orderId: string }
```

`z.object(...)` 是一个普通的值，既能在运行时校验，又能用 `z.infer` 推出静态类型。框架拿到它，一边生成给模型看的 JSON Schema，一边在执行工具前校验参数。所以 TS 里不会出现 4.4 课“两个库标签语法不同”的问题：三家都收 zod（Mastra 还收 Valibot、JSON Schema；OpenAI Agents SDK 要求 zod 4）。

**工具是对象字面量，不是实现接口的类型。** Go 的 Eino 工具要实现 `Info` 和 `InvokableRun` 两个方法；TS 里直接写一个对象：

```ts
tool({
  description: '按订单号查询订单金额',
  inputSchema: z.object({ orderId: z.string() }),
  execute: async ({ orderId }) => shop.lookup(orderId),
});
```

`execute` 的参数类型由 `inputSchema` 推断出来，`({ orderId })` 是解构，相当于 Go 里先 `json.Unmarshal` 到结构体再取字段，只是这一步由框架做了。返回值不用自己序列化成字符串：AI SDK 和 Mastra 把对象作为 `{ type: 'json', value }` 交给模型，OpenAI Agents SDK 把它 `JSON.stringify` 成文本。

**`async` / `await` 和并发。** Go 里并发执行几个工具要自己开 goroutine、用 `sync.WaitGroup` 等；TS 的 `async` 函数返回 Promise，框架用 `Promise.all` 一类的方式同时等多个工具。单线程事件循环下“并发”的意思是：一个工具在 `await` 网络或定时器时，另一个工具可以开始执行。练习的 `Shop.lookup` 里有一个 `await setTimeout`，测试用 `maxInFlight` 记录同时在执行的个数，结果是 2。

**可选属性和展开。** 练习的 tsconfig 开了 `exactOptionalPropertyTypes`：可选属性不能显式赋 `undefined`。所以代码里常见这种写法：

```ts
...(opts.stopWhen ? { stopWhen: opts.stopWhen } : {}),
```

意思是“有值才加这个字段”，相当于 Go 里 `if opts.StopWhen != nil { cfg.StopWhen = opts.StopWhen }`。

**错误。** Go 返回 `error`；TS 抛异常，`await` 一个被 reject 的 Promise 就会在调用处抛出。测试里用 `await expect(promise).rejects.toBeInstanceOf(MaxTurnsExceededError)` 断言抛出的错误类型，相当于 Go 的 `errors.As`。

## 3. Vercel AI SDK

### 3.1 generateText 和 ToolLoopAgent

AI SDK 有两个入口跑工具循环：函数 `generateText` / `streamText`，和类 `ToolLoopAgent`。后者把模型、指令、工具、循环设置打包成一个可复用的对象，`generate()` 和 `stream()` 接受的参数和前者基本一样。

```ts
const agent = new ToolLoopAgent({
  model,
  instructions: '你是客服 agent，需要时调用工具。',
  tools: { lookup_order, refund },
});
const r = await agent.generate({ prompt: '查一下 A1' });
r.text;       // 最终回答
r.steps;      // 每一步的模型输出、工具调用、工具结果
r.stoppedBy;  // 'model' | 'stop-condition' | 'tool-without-execute' | 'approval'
```

测试检查了第二次调用模型时的 prompt：`instructions` 变成最前面的 `system` 消息，然后是 `user`、带 `tool-call` 的 `assistant`、带 `tool-result` 的 `tool`。工具返回的对象原样作为 `{ type: 'json', value: { orderId: 'A1', amount: 120 } }` 交给模型。（测试验证过）

**两个入口的默认步数不一样，这是最容易踩的坑。**

- `ToolLoopAgent` 默认 `stopWhen: isStepCount(20)`。用默认值撞到上限时会打一条警告，提示你显式设置。
- `generateText` / `streamText` 默认 `isStepCount(1)`：只调一次模型。如果模型要工具，工具**会执行**，但不会再调模型去看工具结果，所以 `text` 是空字符串，`stoppedBy` 是 `'stop-condition'`。（测试验证过）

很多人第一次用 `generateText` 加工具，发现“工具执行了但模型什么也没说”，原因就在这里。要循环，显式传 `stopWhen`。

### 3.2 一步是什么，stopWhen 什么时候检查

文档的定义：一步是一次模型生成，加上执行这次生成要求的本地工具。`stopWhen` 只在最后一步有工具结果时检查。所以 `isStepCount(2)` 的行为是：

1. 第 1 步：模型要 `lookup_order(A1)`，执行。
2. 第 2 步：模型要 `lookup_order(A2)`，**执行**。
3. 检查 `stopWhen`：已经 2 步，停。

模型调用 2 次，工具执行 2 次，第 2 步工具的结果没有交给模型。结果不抛错：`stoppedBy: 'stop-condition'`，`finishReason: 'tool-calls'`（`finishReason` 只描述最后一次模型调用），`text` 为空。（测试验证过）

这和 4.4 课 Eino 的 `MaxIterations` 数法一样（都是模型调用次数），区别是 Eino 撞到上限报错，AI SDK 正常返回。所以用 AI SDK 时要看 `stoppedBy`，不能只看有没有异常。

`stopWhen` 可以是数组（任意一个满足就停），内置的还有 `hasToolCall('done')`（调到某个工具就停）和 `isLoopFinished()`（不设上限）。自定义条件是一个函数，参数里有所有步骤，可以按 token 用量估算成本来停。

### 3.3 工具错误：三种都交给模型

| 情况 | 7.0.183 实测行为 |
|---|---|
| `execute` 抛错 | 变成 `{ type: 'error-text', value: 'Error: 订单 Z9 不存在' }` 交给模型，循环继续 |
| 工具名不存在 | 变成 `error-text`，内容是 `AI_NoSuchToolError: Model tried to call unavailable tool 'cancel_order'. Available tools: lookup_order, refund.`，循环继续 |
| 参数不符合 schema | 变成 `error-text`，内容以 `AI_InvalidToolInputError` 开头，带上 zod 的校验错误，循环继续 |

（测试验证过）

和 4.4 课对照：Eino 默认“工具不存在”让整次运行失败，Genkit 参数不合 schema 时整个 `Generate` 失败；AI SDK 三种都交给模型，让它自己纠正。工具名不存在的提示里还列出了可用的工具，模型很容易改对。

如前所述，Tools 文档的 “Handling Errors” 一节写的是 `generateText` 会抛出这两种错误，和实测不符。写错误处理代码时，用 `onToolExecutionEnd` 回调或检查 `steps[].content` 里 `type === 'tool-error'` 的部分，不要指望 `try/catch` 能捕获到。

### 3.4 并发和流式

同一步里模型要了多个工具，默认并发执行；`toolCallConcurrency: 1` 改成按模型给出的顺序逐个执行（后一个要看到前一个的副作用时用）。练习的测试让两个 `lookup_order` 各等 20 毫秒，`maxInFlight` 是 2。（测试验证过）

`agent.stream()` 返回的结果上有两个流：`stream`（7.0 之前叫 `fullStream`）是所有事件，`textStream` 只有文字。一次“查订单再回答”的事件序列：

```
start
start-step  tool-call  tool-result  finish-step
start-step  text-start  text-delta × 5  text-end  finish-step
finish
```

（测试验证过）每个 `start-step` / `finish-step` 包住一步。前端用 `useChat` 时，这些事件会被转换成 UI 消息流，工具调用变成消息的 `parts`。

### 3.5 toolApproval：审批

7.0 起，审批策略写在调用或 agent 上：

```ts
new ToolLoopAgent({
  model,
  tools: { lookup_order, refund },
  toolApproval: {
    refund: async ({ amount }) => (amount > 500 ? 'user-approval' : undefined),
  },
});
```

策略返回四种状态之一：`'not-applicable'`（照常执行，`undefined` 等同于它）、`'approved'`（自动批准并记录）、`'denied'`（自动拒绝，可以带 `reason`）、`'user-approval'`（停下等人）。也可以传一个函数统一处理所有工具，参数里有完整的工具调用。

流程是两次调用：

```ts
const messages: ModelMessage[] = [{ role: 'user', content: '给 A2 退 800' }];
const r1 = await agent.generate({ messages });
// r1.stoppedBy === 'approval'，r1.content 里有 tool-approval-request
messages.push(...r1.responseMessages);
messages.push({
  role: 'tool',
  content: [{ type: 'tool-approval-response', approvalId, approved: true }],
});
const r2 = await agent.generate({ messages }); // 先执行工具，再调模型
```

要点：

- **状态就是消息历史。** 没有 checkpoint store，也没有 runId。`responseMessages` 里有带 `tool-call` 和 `tool-approval-request` 的 assistant 消息，你把它和人的决定一起追加到历史里，下次调用时 SDK 发现有已批准的调用，先执行工具，再调模型。练习的测试把 `messages` 经过 `JSON.stringify` / `JSON.parse` 往返一次，交给另一个 agent 实例、另一个模型对象，第二个模型只被调用一次。（测试验证过）
- **拒绝时**模型看到 `{ type: 'execution-denied', reason: '超过单笔额度' }`，工具不执行。文档建议在指令里写上“工具没被批准时不要重试”，否则模型可能反复申请。（测试验证过）
- **不满足条件的调用不受影响**：60 元的退款直接执行，`stoppedBy` 是 `'model'`。（测试验证过）
- 恢复时 SDK 会重新按 schema 校验参数，并**重新执行一次审批策略**；策略在服务端判定为拒绝的调用，即使客户端送来 `approved: true` 也不会执行。

### 3.6 历史由客户端保存时：审批可以被篡改

`useChat` 的标准用法里，服务端不存对话，每次请求由前端把完整的消息历史发回来。这意味着审批相关的消息也来自客户端。文档的 Security Considerations 一节专门讲了这个问题，练习把它写成了测试：

1. 模型要给 A2 退 800，停在审批。审批人看到的是 A2。
2. 客户端在发回的历史里把 `tool-call` 的 `orderId` 改成 B9，附上 `approved: true`。
3. 服务端重新校验：参数仍符合 schema，策略重新执行，800 仍然需要审批，而客户端说批准了。结果：**按 B9 退了 800**。（测试验证过）

解决办法是 `experimental_toolApprovalSecret`：配上之后，SDK 签发审批请求时用 HMAC 对工具名、调用 ID、参数签名，签名放在 `tool-approval-request` 里；恢复时校验，对不上就抛 `AI_InvalidToolApprovalSignatureError`，工具不执行。测试里同样的篡改在签名开启后被拒绝，未篡改的历史正常执行。（测试验证过）

部署在多个实例上时，所有实例要用同一个密钥。不想依赖客户端保存历史，就在服务端存（数据库按会话 ID 存消息），前端只发新消息，这也是 3.5 课状态管理的结论。

### 3.7 其他值得知道的

- `prepareStep`：每一步开始前调用，可以换模型、改 `temperature`、限制可用工具（`activeTools`）、改写消息做上下文压缩。返回的消息覆盖会作为之后各步的基础。
- `runtimeContext` 和 `toolsContext`：前者是整个循环共享的状态，`prepareStep` 和回调里都能读；后者按工具名分配，工具用 `contextSchema` 声明自己需要什么，只能看到自己那一份。API key 这类东西放 `toolsContext`，不要放进 prompt。
- 生命周期回调：`onStart`、`onStepStart`、`onToolExecutionStart`、`onToolExecutionEnd`、`onStepEnd`、`onEnd`。回调里抛出的错误会被吞掉，不影响生成。
- 测试工具：`ai/test` 导出 `MockLanguageModelV4`，可以给一个响应数组，按顺序返回；`doGenerateCalls` / `doStreamCalls` 记录每次调用收到的参数。练习的 `ScriptedModel` 就是继承它，加了从脚本生成流式分片的逻辑。注意要把 `doGenerate` / `doStream` 传给父类构造函数，父类在包装函数里记录调用；构造之后再给 `this.doGenerate` 赋值会绕过记录，`doGenerateCalls` 永远是空的。

## 4. Mastra

### 4.1 Agent 和工具

```ts
const lookup_order = createTool({
  id: 'lookup_order',
  description: '按订单号查询订单金额',
  inputSchema: z.object({ orderId: z.string() }),
  execute: async ({ orderId }) => shop.lookup(orderId),
});

const support = new Agent({
  id: 'support',
  name: 'support',
  instructions: '你是客服 agent，需要时调用工具。',
  model: 'openai/gpt-6.1-sol',     // 模型路由字符串；也可以传 AI SDK 的模型对象
  tools: { lookup_order },
});
const mastra = new Mastra({ agents: { support }, storage });
const r = await mastra.getAgent('support').generate('查一下 A1');
```

- 工具必须用 `createTool` 创建，`execute(inputData, context)` 只有这一种签名，第一个参数已经按 `inputSchema` 校验过。文档特意强调普通对象当工具会静默失败。
- 模型可以是字符串 `'provider/model'`，Mastra 自己的模型路由会按前缀找对应的环境变量（`OPENAI_API_KEY` 等）。练习里传的是 AI SDK 的 `MockLanguageModelV4` 子类，和 AI SDK 部分共用。
- 审批、`suspend()` 要靠 storage 里的快照恢复，所以 agent 要注册到带 `storage` 的 `Mastra` 实例上，再用 `mastra.getAgent()` 取出来用。没有配 storage 时会报 “snapshot not found”。练习用 `@mastra/core/storage` 的 `InMemoryStore`；生产换成 Postgres、LibSQL 等持久化的 storage。

### 4.2 默认 maxSteps 是 5

`Agent.generate()` 的参考文档写着：`maxSteps` 控制顺序调用模型的最大次数，默认 5。实测：模型一直要工具，第 5 次调用后停下，5 个工具都执行了，`finishReason: 'tool-calls'`，`text` 为空，不抛错。（测试验证过）

这和 AI SDK 的 `isStepCount` 数法一样（Mastra 底层的循环就是照 AI SDK 的步骤模型做的），但默认值小得多：`ToolLoopAgent` 是 20，Mastra 是 5。一个要查好几个订单、再调用退款的任务，默认设置下很可能做到一半就停了，而且不报错。要改，在 `generate` 的选项里传 `maxSteps`，或者在 agent 的 `defaultOptions` 里设。

### 4.3 工具错误

| 情况 | 1.86.0 实测行为 |
|---|---|
| `execute` 抛错 | `{ type: 'error-text', value: '订单 Z9 不存在' }`，只有错误消息，没有 `Error:` 前缀 |
| 工具名不存在 | `error-text`：`Tool "cancel_order" not found. Available tools: lookup_order, refund, refund_checked. Call tools by their exact name only — never add prefixes, namespaces, or colons.` |
| 参数不符合 schema | `{ type: 'json', value: { error: true, message: 'Tool input validation failed for lookup_order. Please fix the following errors and try again:\n- orderId: Invalid input: expected string, received number ...', validationErrors } }` |

（测试验证过）三种都交给模型、循环继续，和 AI SDK 一致。参数错误的提示写得很“面向模型”：逐个字段列出问题，末尾附上模型给的原始参数。工具名不存在时还专门提醒模型不要加前缀和冒号，这是在处理模型给 MCP 工具名加命名空间的常见错误。

另一个细节：工具定义了 `outputSchema` 而返回值不符合时，Mastra 把校验错误交给模型，模型会以为调用失败而重试。对已经产生副作用的工具（下单、退款），文档建议设 `outputValidation: 'warn'`，只记日志、原样返回结果。

### 4.4 人工审批：两种机制

**执行前审批**：工具上设 `requireApproval: true`，或者调用时传 `requireToolApproval: true`（对所有工具生效，也可以是一个按工具名和参数判断的函数）。`generate()` 碰到需要审批的调用就返回：

```ts
const r1 = await agent.generate('给 A2 退 800');
r1.finishReason;    // 'suspended'
r1.suspendPayload;  // { toolCallId: 'c1', toolName: 'refund', args: { orderId: 'A2', amount: 800 }, ... }
r1.runId;

await agent.approveToolCallGenerate({ runId: r1.runId, toolCallId: 'c1' });
// 或者
await agent.declineToolCallGenerate({ runId: r1.runId, toolCallId: 'c1', reason: '超过单笔额度' });
```

流式版本对应 `stream()` 的 `tool-call-approval` 事件和 `approveToolCall` / `declineToolCall`。

**执行中暂停**：工具在 `execute` 里调用 `suspend()`，带上自己定义的数据（`suspendSchema`），恢复时从 `context.agent.resumeData` 拿到答复（`resumeSchema`）：

```ts
execute: async ({ orderId, amount }, ctx) => {
  const decision = ctx?.agent?.resumeData as { approved: boolean } | undefined;
  if (amount > 500 && decision === undefined) {
    return await ctx!.agent!.suspend({ question: `批准给 ${orderId} 退 ${amount} 吗？` });
  }
  if (decision && !decision.approved) return '退款被拒绝';
  return shop.refund(orderId, amount);
}
```

恢复用 `agent.resumeGenerate({ approved: true }, { runId })`。文档提醒：`suspend()` 不会抛出，一定要 `return`，否则后面的代码会接着跑。和 4.4 课 Eino、Genkit 一样，恢复时 `execute` 从头再执行一次，所以暂停之前不能有副作用。

测试验证的行为：

- 第一个 `Mastra` 实例跑到 `suspended`；另一个 `Mastra` 实例（新的模型对象）只要用同一个 storage，就能用 `runId` 批准，工具执行，模型只被调用一次。拒绝时模型看到的工具结果是拒绝原因。（测试验证过）
- `suspend()` 的数据在 `suspendPayload.suspendPayload` 里（外层是工具调用的信息），`resumeGenerate` 之后工具拿到答复完成退款。（测试验证过）
- 参数不合 schema 的调用不会进入审批，直接把校验错误交给模型。

进程重启以后不知道 runId 时，用 `agent.listSuspendedRuns({ threadId, resourceId })` 从 storage 里找回挂起的运行；处理恢复的进程中途崩溃，可以用 `recoverResume({ runId })` 在任意进程里按已记录的决定继续。这些都依赖持久化的 storage。

和 AI SDK 对照：AI SDK 的审批状态在消息历史里，谁拿着历史谁就能恢复；Mastra 的状态在服务端 storage 里，客户端只拿 runId，3.6 节那种篡改参数的问题不存在，代价是你必须配 storage。

### 4.5 workflow

Mastra 的另一半是 workflow：用类型化的步骤拼出确定的流程，和 4.4 课 Eino 的 `compose.Graph` 是同一类东西。练习的工单分流：

```ts
const classify = createStep({ id: 'classify', inputSchema: Ticket, outputSchema: Classified, execute: ... });
const approve = createStep({
  id: 'approve',
  inputSchema: Classified,
  outputSchema: TriageOutput,
  suspendSchema: z.object({ question: z.string() }),
  resumeSchema: z.object({ approved: z.boolean() }),
  execute: async ({ inputData, resumeData, suspend }) => {
    if (inputData.amount > 500 && resumeData === undefined) {
      return await suspend({ question: `批准给 ${inputData.orderId} 退 ${inputData.amount} 吗？` });
    }
    ...
  },
});

createWorkflow({ id: 'triage', inputSchema: Ticket, outputSchema: TriageOutput })
  .then(classify)
  .branch([
    [async ({ inputData }) => inputData.kind === 'refund', approve],
    [async ({ inputData }) => inputData.kind !== 'refund', human],
  ])
  .commit();
```

行为细节：

- 相邻步骤的 `outputSchema` 和 `inputSchema` 要对得上，类型错误在编译期由 TS 报出（Eino 是在 `AddEdge` 时运行期报）。
- `branch` 的条件按顺序判断，只执行匹配的分支。结果以步骤 id 为键：走了人工分支，`result` 是 `{ human: { result: '已转人工：T1' } }`。（测试验证过）
- 运行用 `workflow.createRun()` 拿到 run，再 `run.start({ inputData })`。步骤 suspend 时返回 `status: 'suspended'`，`suspended` 是 `[['approve']]`（每个挂起步骤一条从顶层到它的路径，嵌套 workflow 时路径更长），`suspendPayload` 是 `{ approve: { question: '批准给 A2 退 800 吗？' } }`。（测试验证过）
- 恢复：另一个 Mastra 实例用 `createRun({ runId })` 拿回同一个 run，`run.resume({ step: 'approve', resumeData: { approved: true } })`。挂起的步骤从头再执行一次（测试里 `approve` 的执行计数是 2），已经完成的 `classify` 不重跑（`lookup` 只调用了一次）。（测试验证过）

workflow 还有 `.parallel()`、`.foreach()`、`.dowhile()` / `.dountil()`、`.sleep()`、`.map()`，步骤也可以直接是一个 agent 或工具。Mastra 的 agent 和 workflow 共用同一套快照存储，这也是 agent 的审批能跨实例恢复的原因。

## 5. OpenAI Agents SDK (JS)

### 5.1 和 Python 版一样的结构

```ts
const agent = new Agent({
  name: 'support',
  instructions: '你是客服 agent，需要时调用工具。',
  model,
  tools: [lookup_order, refund],
});
const r = await run(agent, '查一下 A1');
r.finalOutput;
```

4.2 课讲的 Runner 循环、handoff、guardrail、session 在 JS 版里都有，名字从 snake_case 改成 camelCase（`max_turns` → `maxTurns`，`needs_approval` → `needsApproval`）。几处值得看的地方：

- **模型接口是 Responses 风格的。** 自己实现 `Model` 只需要 `getResponse(request)` 和 `getStreamedResponse(request)`。`request.systemInstructions` 单独放指令，`request.input` 是 item 列表：`message`、`function_call`、`function_call_result`。测试检查了第二次调用时 input 的三项和工具结果的格式：对象被 `JSON.stringify` 成文本，`{ type: 'text', text: '{"orderId":"A1","amount":120}' }`。（测试验证过）
- 0.22.0 自带测试工具 `@openai/agents/testing`，里面有一个 `ScriptedModel`。练习自己写了一个，是为了让你看清 `Model` 接口，正式项目可以直接用自带的。
- **trace 默认上传。** 测试里调了 `setTracingDisabled(true)`。生产环境默认会把 trace 发到 OpenAI 后台，用别家模型或有数据合规要求时注意关掉或换 trace 处理器。

### 5.2 maxTurns：默认 10，超过抛错

`maxTurns` 默认 10（源码 `DEFAULT_MAX_TURNS = 10`），数的是模型调用次数。设成 2：第 1、2 次调用要的工具都执行了，第 3 次调用前检查到超限，抛 `MaxTurnsExceededError`。模型调用 2 次，工具执行 2 次。（测试验证过）

和 AI SDK、Mastra 数法相同，区别在于**抛错**。4.2 课讲过可以用 `errorHandlers` 按错误类型返回兜底输出，不让 run 以异常结束。

### 5.3 工具错误

| 情况 | 0.22.0 实测行为 |
|---|---|
| `execute` 抛错 | 模型看到固定文案 `An error occurred while running the tool.`，**没有原因** |
| 参数不符合 schema | 同上，固定文案 |
| 工具名不存在 | 默认抛 `ModelBehaviorError: Tool cancel_order not found in agent support.`；`toolNotFoundBehavior: 'return_error_to_model'` 时模型看到 `Tool 'cancel_order' not found.` |

（测试验证过）

默认文案不带原因，好处是不会把内部错误信息泄露给模型（进而泄露给用户），坏处是模型不知道哪里错了，只能瞎试或者放弃。需要让模型知道原因，给工具设 `errorFunction: (ctx, error) => '工具出错：' + error.message`；设成 `null` 则不兜底，工具抛错时整个 run 抛 `ToolCallError`。写课时在沙箱里试过这两种设置。

### 5.4 needsApproval 和 RunState

```ts
tool({
  name: 'refund',
  parameters: z.object({ orderId: z.string(), amount: z.number() }),
  needsApproval: async (_ctx, { amount }) => amount > 500,
  execute: async ({ orderId, amount }) => shop.refund(orderId, amount),
});

const r1 = await run(agent, '给 A2 退 800');
r1.interruptions;           // [{ type: 'tool_approval_item', name: 'refund', arguments: '{"orderId":"A2","amount":800}' }]
const saved = r1.state.toString();   // 存进数据库

// 另一个进程
const state = await RunState.fromString(agent, saved);
state.approve(state.getInterruptions()[0]);   // 或 state.reject(...)
const r2 = await run(agent, state);
```

- 状态是 `RunState` 序列化成的 JSON 字符串（测试里约 3 KB），包含对话、当前 agent、待审批的调用。恢复时要传一个**同样定义的** agent：工具的实现不在字符串里，只有名字。测试用新的 agent 实例和新的模型对象恢复，模型只被调用一次。（测试验证过）
- 拒绝时模型看到固定文案 `Tool execution was not approved.`，工具不执行。（测试验证过）
- 小额退款 `needsApproval` 返回 false，直接执行，`interruptions` 为空。（测试验证过）

和前两家对照：AI SDK 的状态是消息历史（你存），Mastra 的状态是 storage 快照（框架存，你只拿 runId），OpenAI 是一个不透明的字符串（你存，但内容由框架定义）。这三种和 4.4 课 Genkit（`History()`）、Eino（`CheckPointStore`）、MCP（`RequestState`）正好又是一组对照。

## 6. 同一件事，三个框架怎么写

| 问题 | Vercel AI SDK 7 | Mastra 1.86 | OpenAI Agents JS 0.22 |
|---|---|---|---|
| 循环上限 | `stopWhen: isStepCount(n)` | `maxSteps` | `maxTurns` |
| 默认上限 | `ToolLoopAgent` 20；`generateText` 1 | 5 | 10 |
| 上限数的是 | 模型调用次数 | 模型调用次数 | 模型调用次数 |
| 撞到上限 | 正常返回，`stoppedBy: 'stop-condition'` | 正常返回，`finishReason: 'tool-calls'` | 抛 `MaxTurnsExceededError` |
| 最后一步要的工具 | 执行 | 执行 | 执行 |
| 工具抛错 | 错误消息交给模型 | 错误消息交给模型 | 固定文案交给模型，`errorFunction` 可改 |
| 工具不存在 | 交给模型，附可用工具列表 | 交给模型，附可用工具列表 | 默认抛 `ModelBehaviorError`，可改 |
| 参数不合 schema | 交给模型，附 zod 错误 | 交给模型，逐字段列出 | 固定文案交给模型 |
| 同一步多个工具 | 默认并发，`toolCallConcurrency` 限制 | 并发 | 并发 |
| 审批声明在哪 | 调用 / agent 的 `toolApproval` | 工具 `requireApproval`，或调用的 `requireToolApproval` | 工具 `needsApproval` |
| 审批期间的状态 | 消息历史（你存） | storage 快照（框架存），对外是 runId | `RunState` 字符串（你存） |
| 自动拒绝并告诉模型原因 | 策略返回 `{ type: 'denied', reason }` | `hooks.beforeToolCall` 返回 `{ proceed: false, output }` | 工具输入 guardrail 返回 `rejectContent` |
| 恢复时工具 | 执行一次 | `requireApproval`：执行一次；`suspend()`：从头再执行 | 执行一次 |

练习的 `compare.test.ts` 用同一个“模型一直要工具”的脚本跑三家：上限都设 2 时，三家都是模型调用 2 次、工具执行 2 次，AI SDK 和 Mastra 的 `text` 是空字符串，OpenAI 抛错；不设上限时，三家分别在第 20、5、10 次调用后停下。（测试验证过）

**同一个“上限”，三种结束方式。** 写服务时最危险的是 AI SDK 和 Mastra 的静默结束：接口返回 200、回答是空的。上线前给每个 agent 显式设上限，并在 `stoppedBy` / `finishReason` 是“被上限截断”时走单独的处理（重试、提示用户、记告警）。

## 7. 怎么选

- **Next.js / React 前端 + 一两个模型调用 + 几个工具**：Vercel AI SDK。`useChat`、UI 消息流、各家 provider 都现成，`ToolLoopAgent` 足够应付大部分 agent 场景。注意 3.6 节的审批签名，以及历史放在客户端的信任问题。
- **需要持久化的 agent（审批隔天才来、多实例部署）、确定流程和 agent 混着用、想要现成的 memory 和服务端**：Mastra。它站在 AI SDK 的模型层上，换模型不用改代码；workflow 的 suspend / resume 和 agent 的审批共用存储。代价是框架大、更新快、默认 `maxSteps` 只有 5。
- **主要用 OpenAI 模型、团队里同时有 Python 和 TS、想用 handoff / guardrail / Responses 的服务端能力**：OpenAI Agents SDK。Python 和 JS 两版概念一致，4.2 课的东西可以直接搬过来。
- **工具要给多个框架、多种语言共用**：和 4.4 课一样，放进 MCP 服务端。AI SDK 有 `@ai-sdk/mcp`，Mastra 有 `@mastra/mcp`，OpenAI Agents SDK 有 `MCPServerStdio` / `MCPServerStreamableHttp`。
- **只调一个模型、一两个工具**：可能三个都不需要，这是 4.6 课的话题。

## 8. 练习代码导读

`projects/28-ts-ecosystem/` 里的文件：

| 文件 | 内容 |
|---|---|
| `src/script.ts` | `ScriptedModel`：继承 AI SDK 的 `MockLanguageModelV4`，按脚本返回文字或工具调用，支持流式；`callCount`、`promptAt`、`lastMessageAt` 方便断言。AI SDK 和 Mastra 共用 |
| `src/shop.ts` | 三家共用的业务：订单库、退款账本、`maxInFlight` 并发计数；审批阈值 500 |
| `src/aisdk.ts` | `shopTools`、`supportAgent`（`toolApproval` 策略、可选的签名密钥）、`pendingApprovals`、`approvalMessage` |
| `src/mastra.ts` | `mastraTools`（`requireApproval` 的 `refund`、自己 `suspend()` 的 `refund_checked`）、`buildMastra`、工单分流 workflow |
| `src/openai.ts` | `ScriptedOpenAIModel`（实现 `Model` 接口）、`openaiTools`（`needsApproval`）、`supportAgent`、`restore` |
| `src/*.test.ts` | 25 个测试：`aisdk.test.ts` 9 个、`mastra.test.ts` 8 个、`openai.test.ts` 6 个、`compare.test.ts` 2 个 |

运行：

```
cd projects/28-ts-ecosystem
npm install
npm run typecheck
npm test
```

需要 Node 22 以上（AI SDK 7 的要求，Mastra 要求 22.13 以上）。跑到“默认上限”那个测试时，AI SDK 会打一条 `isStepCount(20)` 的警告，这是预期的。

## 9. 常见的坑

- **`generateText` 加了工具却没有回答。** 默认只走一步，要传 `stopWhen`。
- **以为撞到上限会报错。** AI SDK 和 Mastra 都是正常返回，要检查 `stoppedBy` / `finishReason`。
- **Mastra 的 `maxSteps` 默认只有 5。** 多工具任务会静默截断。
- **照着 v5 / v6 的示例写 AI SDK 7。** `stepCountIs`、`system`、`fullStream`、工具上的 `needsApproval`、`experimental_context` 都改了名或废弃了。
- **相信文档说 `generateText` 会抛 `NoSuchToolError`。** 7.0.183 实测是交给模型，循环继续。
- **审批历史由客户端保存却没签名。** 客户端可以改参数再附上批准，见 3.6 节。配 `experimental_toolApprovalSecret`，或者把历史存在服务端。
- **Mastra 没配 storage 就用审批。** 恢复时报 “snapshot not found”。`InMemoryStore` 只适合测试，进程退出就丢。
- **Mastra 的 `suspend()` 后面没写 `return`。** `suspend()` 不抛出，后面的代码会继续执行。
- **Mastra 在 `generate()` 里传 `hooks`，以为审批恢复后还在。** 调用级的 `hooks` 不会存进快照，`approveToolCallGenerate` 恢复时不带上它们，拦截不会生效。要么把 hooks 配在 agent 上，要么恢复时再传一次。练习 3 的参考答案里有这个测试。
- **Mastra 工具返回值不符合 `outputSchema`。** 模型会以为调用失败而重试，有副作用的工具设 `outputValidation: 'warn'`。
- **OpenAI Agents SDK 的工具错误模型看不到原因。** 默认是固定文案，需要时设 `errorFunction`。
- **OpenAI Agents SDK 恢复 `RunState` 时传了不同的 agent。** 工具实现不在序列化的字符串里，按名字找；工具改名后旧状态恢复不了。
- **自己写的 AI SDK 假模型在构造后给 `this.doGenerate` 赋值。** 会绕过 `MockLanguageModelV4` 的调用记录。
- **三家的“步数”按同一个意思理解。** 三家都数模型调用次数，但默认值是 20 / 5 / 10，撞到之后的行为也不同。

## 10. 练习

### 练习 1：读懂并跑通（必做）

- `npm install && npm run typecheck && npm test`，25 个测试全部通过。
- 读 `aisdk.test.ts` 里“历史由客户端保存时可能被篡改”那个测试，回答：签名绑定了哪三样东西？为什么服务端重新执行审批策略也挡不住这种篡改？
- 把 `compare.test.ts` 第一个测试的上限从 2 改成 3，先预测三家的模型调用次数、工具执行次数、是否抛错，再改断言跑一遍。
- 给 AI SDK 写一个测试：同一步里两个 `lookup_order`，分别在默认设置和 `toolCallConcurrency: 1` 下运行，断言 `maxInFlight` 分别是 2 和 1，后者耗时不少于两次延迟之和。

### 练习 2：统一的审批接口（必做）

三个框架的审批状态放在三个不同的地方。写一个统一接口，让上层代码不关心用的是哪家：

```ts
type Pending = { id: string; tool: string; args: unknown };
type Step = { done: true; text: string } | { done: false; pending: Pending[]; token: string };

interface HumanApproval {
  start(prompt: string): Promise<Step>;
  decide(token: string, id: string, approved: boolean): Promise<Step>;
}
```

- `token` 是要存进数据库的全部状态：AI SDK 是消息历史的 JSON，Mastra 是 runId（真正的状态在 storage 里），OpenAI 是 `RunState.toString()`。
- `decide` 每次都**新建** agent 和模型对象，模拟请求落在另一台机器上。
- AI SDK 版打开签名。
- 用 `describe.each` 对三个实现跑同一组测试：批准时退款执行、拒绝时不执行，两种情况下最终回答都来自第二段脚本。

### 练习 3：硬性拒绝（选做）

单笔超过 5000 的退款不问人，直接拒绝，并把原因告诉模型。三家各用自己的机制实现：

- AI SDK：`toolApproval` 返回 `{ type: 'denied', reason }`。
- Mastra：给 `buildMastra` 加一个 `hooks` 选项，在 agent 上配 `beforeToolCall`，超限时返回 `{ proceed: false, output: 原因 }`。注意 `refund` 设了 `requireApproval: true`，看看拦截发生在审批之前还是之后。再写一个测试，把 hooks 只传给 `generate()` 而不是 agent，然后批准，观察会发生什么。
- OpenAI：给工具加 `inputGuardrails`，用 `defineToolInputGuardrail` 返回 `{ behavior: { type: 'rejectContent', message: 原因 } }`。

每家断言：退款没执行，模型下一轮看到的工具结果是原因文字。

## 11. 自测题

1. `generateText` 不传 `stopWhen`、模型要调工具时，工具会执行吗？模型调用几次？`text` 是什么？
2. `ToolLoopAgent` 的 `stopWhen: isStepCount(2)`，模型一直要工具，模型调用几次？工具执行几次？会抛错吗？怎么知道是被上限截断的？
3. AI SDK 7.0.183 里，工具抛错、工具名不存在、参数不合 schema，模型分别看到什么？
4. AI SDK 的审批流程需要几次调用？审批期间的状态存在哪？
5. 为什么审批历史由客户端保存时，服务端重新执行审批策略也不够？`experimental_toolApprovalSecret` 怎么解决？
6. Mastra 为什么能直接用 AI SDK 的模型对象？
7. Mastra 的 `maxSteps` 默认多少？撞到上限时 `generate()` 返回什么？
8. Mastra 的 `requireApproval` 和工具里调 `suspend()` 有什么区别？各自用什么方法恢复？
9. Mastra 的审批为什么必须配 storage？另一个实例怎么恢复？
10. Mastra workflow 恢复时，挂起的步骤和已完成的步骤分别怎样？
11. OpenAI Agents SDK JS 的 `maxTurns` 默认多少？撞到上限时怎样？
12. OpenAI Agents SDK JS 的工具抛错时，模型默认看到什么？怎么改？
13. `RunState.fromString` 为什么要传 agent？
14. 三个框架都把上限设成 2，结束方式有什么不同？上线前该做什么？

## 12. 自测题答案

1. 会执行。`generateText` 默认 `isStepCount(1)`，只调一次模型；这一步里模型要的工具会执行，但不会再调模型看结果，所以 `text` 是空字符串，`stoppedBy` 是 `'stop-condition'`。
2. 模型调用 2 次，工具执行 2 次（第 2 步要的工具也执行），不抛错。看 `stoppedBy === 'stop-condition'`；`finishReason` 是 `'tool-calls'`，它只描述最后一次模型调用。
3. 三种都作为 `error-text` 交给模型，循环继续。工具抛错是 `Error: <消息>`；工具名不存在是 `AI_NoSuchToolError: ... Available tools: ...`，附可用工具列表；参数不合 schema 是 `AI_InvalidToolInputError` 加 zod 的校验错误。
4. 两次。第一次 `generate` 以 `stoppedBy: 'approval'` 结束，`content` 里有 `tool-approval-request`；把 `responseMessages` 和一条含 `tool-approval-response` 的 tool 消息追加到历史，第二次 `generate` 先执行（或不执行）工具再调模型。状态就是消息历史，没有 runId 或 checkpoint。
5. 篡改后的参数（比如把订单号改掉）仍然符合 schema，策略重新执行的结论也还是“需要审批”，而客户端附上了 `approved: true`，服务端没法知道人批准的是不是这组参数。签名把工具名、工具调用 ID、参数绑在一起，用服务端的密钥做 HMAC；恢复时重新计算，对不上就抛 `AI_InvalidToolApprovalSignatureError`，工具不执行。自动拒绝的调用不签名，客户端也没法把它改成批准。
6. Mastra 的 agent 底层就是按 AI SDK 的模型接口规范调用模型的，`@mastra/core` 同时依赖 AI SDK v5、v6、v7 三代 provider 规范，所以任何 AI SDK provider 的模型对象（包括 `MockLanguageModelV4`）都能直接传给 `Agent` 的 `model`。
7. 5。撞到上限时正常返回，`finishReason` 是 `'tool-calls'`，`text` 为空，不抛错；第 5 步要的工具也执行了。
8. `requireApproval` 在 `execute` 之前停，框架给出工具名和参数，恢复用 `approveToolCallGenerate` / `declineToolCallGenerate`（流式用 `approveToolCall` / `declineToolCall`），批准后 `execute` 执行一次。`suspend()` 在 `execute` 里面停，带自定义数据，恢复用 `resumeGenerate(resumeData, { runId })`（流式用 `resumeStream`），`execute` 从头再执行一次，从 `context.agent.resumeData` 读答复。
9. 挂起的运行以快照形式存在 storage 里，恢复要从 storage 读回来；没配 storage 会报 “snapshot not found”。另一个实例只要用同一个 storage（生产里是同一个数据库），拿 runId 调审批或恢复方法即可，模型和工具都可以是新对象。
10. 挂起的步骤从头再执行一次，这次 `resumeData` 有值；已完成的步骤不重跑，它们的输出在快照里。
11. 10，数模型调用次数。超过时抛 `MaxTurnsExceededError`；最后一次调用要的工具已经执行了。可以用 `errorHandlers` 返回兜底输出。
12. 固定文案 `An error occurred while running the tool.`，不带原因。给工具设 `errorFunction: (ctx, error) => ...` 自定义交给模型的文字；设成 `null` 时不兜底，run 抛 `ToolCallError`。
13. 序列化的字符串里只有工具名、对话和待审批的调用，没有工具的实现（函数没法序列化）。恢复时要靠 agent 上同名的工具来执行，所以传进去的 agent 要和中断时定义一致。
14. 三家都是模型调用 2 次、工具执行 2 次。AI SDK 和 Mastra 正常返回，回答为空；OpenAI 抛 `MaxTurnsExceededError`。上线前给每个 agent 显式设上限，并在被截断时单独处理（检查 `stoppedBy` / `finishReason`，或捕获异常），不要把空回答直接返回给用户。

## 13. 参考答案

### 练习 1

- 签名绑定工具名、工具调用 ID 和参数。服务端重新执行的审批策略只能判断“这组参数需不需要审批”，判断不了“人批准的是不是这组参数”；客户端改了参数又附上批准，策略的结论仍是“需要审批且已批准”。
- 上限 3：三家都是模型调用 3 次、工具执行 3 次（脚本里正好有 3 个工具调用）；AI SDK 和 Mastra 正常返回、`text` 为空，OpenAI 抛 `MaxTurnsExceededError`。
- 并发测试：

```ts
it.each([[undefined, 2], [1, 1]] as const)('toolCallConcurrency=%s → 峰值 %i', async (limit, peak) => {
  const shop = new Shop(undefined, 20);
  const model = new ScriptedModel([
    calls(
      { id: 'c1', name: 'lookup_order', input: { orderId: 'A1' } },
      { id: 'c2', name: 'lookup_order', input: { orderId: 'A3' } },
    ),
    say('ok'),
  ]);
  const agent = new ToolLoopAgent({
    model,
    tools: shopTools(shop),
    ...(limit ? { toolCallConcurrency: limit } : {}),
  });
  const t0 = Date.now();
  await agent.generate({ prompt: 'q' });
  expect(shop.maxInFlight).toBe(peak);
  expect(shop.lookups).toEqual(['A1', 'A3']);
  if (limit === 1) expect(Date.now() - t0).toBeGreaterThanOrEqual(40);
});
```

`toolCallConcurrency: 1` 时按模型给出的顺序逐个执行，所以 `lookups` 的顺序固定；默认并发时两个调用几乎同时开始，顺序在这个例子里也是 A1、A3，但不要依赖它。时间阈值写得宽松（40 毫秒，而不是 40 毫秒以上的精确值），避免 CI 机器的定时器误差。

### 练习 2

`src/approval.ts`：

```ts
import type { ModelMessage } from 'ai';
import type { MastraCompositeStore } from '@mastra/core/storage';
import type { RunResult } from '@openai/agents';
import type { ScriptedModel } from './script.js';
import { approvalMessage, pendingApprovals, supportAgent as aiAgent } from './aisdk.js';
import { buildMastra } from './mastra.js';
import { restore, run, supportAgent as oaAgent, type ScriptedOpenAIModel } from './openai.js';
import type { Shop } from './shop.js';

export type Pending = { id: string; tool: string; args: unknown };
export type Step = { done: true; text: string } | { done: false; pending: Pending[]; token: string };

export interface HumanApproval {
  start(prompt: string): Promise<Step>;
  decide(token: string, id: string, approved: boolean): Promise<Step>;
}

// AI SDK：状态就是消息历史
export function aiSdkApproval(models: () => ScriptedModel, shop: Shop, secret: string): HumanApproval {
  const agent = () => aiAgent(models(), shop, { approvalSecret: secret });
  const step = (
    messages: ModelMessage[],
    r: { text: string; content: readonly { type: string }[]; responseMessages: ModelMessage[] },
  ): Step => {
    const pending = pendingApprovals(r.content);
    if (pending.length === 0) return { done: true, text: r.text };
    return {
      done: false,
      pending: pending.map((p) => ({ id: p.approvalId, tool: p.toolName, args: p.input })),
      token: JSON.stringify([...messages, ...r.responseMessages]),
    };
  };
  return {
    async start(prompt) {
      const messages: ModelMessage[] = [{ role: 'user', content: prompt }];
      return step(messages, await agent().generate({ messages }));
    },
    async decide(token, id, approved) {
      const messages: ModelMessage[] = [...JSON.parse(token), approvalMessage([{ approvalId: id, approved }])];
      return step(messages, await agent().generate({ messages }));
    },
  };
}

// Mastra：状态在 storage 里，token 只是 runId
export function mastraApproval(models: () => ScriptedModel, shop: Shop, storage: MastraCompositeStore): HumanApproval {
  const agent = () => buildMastra(models(), shop, storage).getAgent('support');
  const step = (r: { text: string; finishReason?: string | undefined; runId?: string | undefined; suspendPayload?: unknown }): Step => {
    if (r.finishReason !== 'suspended') return { done: true, text: r.text };
    const p = r.suspendPayload as { toolCallId: string; toolName: string; args: unknown };
    return { done: false, pending: [{ id: p.toolCallId, tool: p.toolName, args: p.args }], token: r.runId! };
  };
  return {
    async start(prompt) {
      return step(await agent().generate(prompt));
    },
    async decide(runId, id, approved) {
      const a = agent();
      return step(
        approved
          ? await a.approveToolCallGenerate({ runId, toolCallId: id })
          : await a.declineToolCallGenerate({ runId, toolCallId: id }),
      );
    },
  };
}

// OpenAI Agents：状态是 RunState 序列化后的字符串
export function openaiApproval(models: () => ScriptedOpenAIModel, shop: Shop): HumanApproval {
  const step = (r: RunResult<any, any>): Step => {
    if (r.interruptions.length === 0) return { done: true, text: String(r.finalOutput) };
    return {
      done: false,
      pending: r.interruptions.map((i) => ({
        id: (i.rawItem as { callId: string }).callId,
        tool: i.name ?? '',
        args: JSON.parse(i.arguments ?? '{}'),
      })),
      token: r.state.toString(),
    };
  };
  return {
    async start(prompt) {
      return step(await run(oaAgent(models(), shop), prompt));
    },
    async decide(token, id, approved) {
      const agent = oaAgent(models(), shop);
      const state = await restore(agent, token);
      const item = state.getInterruptions().find((i) => (i.rawItem as { callId: string }).callId === id)!;
      if (approved) state.approve(item);
      else state.reject(item);
      return step(await run(agent, state));
    },
  };
}
```

`src/approval.test.ts`：

```ts
import { InMemoryStore } from '@mastra/core/storage';
import { describe, expect, it } from 'vitest';
import { aiSdkApproval, mastraApproval, openaiApproval, type HumanApproval } from './approval.js';
import { ScriptedOpenAIModel, fnCall, reply } from './openai.js';
import { ScriptedModel, call, say } from './script.js';
import { Shop } from './shop.js';

// 每家两段脚本：第一次请求要退款，第二次请求给最终回答
function adapter(name: string, shop: Shop): HumanApproval {
  const queue = <T>(xs: T[]) => () => xs.shift()!;
  const refund = () => call('c1', 'refund', { orderId: 'A2', amount: 800 });
  if (name === 'ai-sdk') {
    return aiSdkApproval(queue([new ScriptedModel([refund()]), new ScriptedModel([say('好了')])]), shop, 's'.repeat(32));
  }
  if (name === 'mastra') {
    return mastraApproval(queue([new ScriptedModel([refund()]), new ScriptedModel([say('好了')])]), shop, new InMemoryStore());
  }
  return openaiApproval(
    queue([
      new ScriptedOpenAIModel([fnCall('c1', 'refund', { orderId: 'A2', amount: 800 })]),
      new ScriptedOpenAIModel([reply('好了')]),
    ]),
    shop,
  );
}

describe.each(['ai-sdk', 'mastra', 'openai'])('%s', (name) => {
  it.each([true, false])('approved=%s', async (approved) => {
    const shop = new Shop();
    const a = adapter(name, shop);
    const s1 = await a.start('给 A2 退 800');
    expect(s1.done).toBe(false);
    if (s1.done) return;
    expect(s1.pending).toEqual([{ id: expect.any(String), tool: 'refund', args: { orderId: 'A2', amount: 800 } }]);
    const s2 = await a.decide(s1.token, s1.pending[0]!.id, approved);
    expect(s2).toEqual({ done: true, text: '好了' });
    expect(shop.refunds).toEqual(approved ? ['A2:800'] : []);
  });
});
```

几个细节：

- AI SDK 的 `token` 要包含**发起请求的 user 消息**和 `responseMessages`，只存后者恢复时会缺上下文。
- Mastra 的 `pending.id` 用的是 `toolCallId`，因为审批方法按它定位；AI SDK 用的是 `approvalId`（签名绑定的是它对应的调用）；OpenAI 用 `rawItem.callId` 在 `getInterruptions()` 里找回对应的项。三家的 id 含义不同，统一接口把它们都当成不透明字符串。
- `token` 的大小差别很大：Mastra 只是一个 UUID，另外两家是整段对话。对话长了以后，存储和传输成本也要考虑进去。

这段代码和测试在写课时的沙箱里跑通了，6 个测试全部通过。

### 练习 3

`src/mastra.ts` 的改动：

```ts
export type MastraAgentOptions = {
  maxSteps?: number;
  hooks?: ConstructorParameters<typeof Agent>[0]['hooks'];
};

// buildMastra 里 new Agent({...}) 加一行
    ...(opts.hooks ? { hooks: opts.hooks } : {}),
```

测试：

```ts
import { InMemoryStore } from '@mastra/core/storage';
import { Agent as OAAgent, defineToolInputGuardrail, tool as oaTool } from '@openai/agents';
import { ToolLoopAgent } from 'ai';
import { describe, expect, it } from 'vitest';
import { z } from 'zod';
import { shopTools } from './aisdk.js';
import { buildMastra } from './mastra.js';
import { ScriptedOpenAIModel, fnCall, reply, run } from './openai.js';
import { ScriptedModel, call, say } from './script.js';
import { Shop } from './shop.js';

const LIMIT = 5000;
const REASON = `单笔退款超过 ${LIMIT}，按规定不能退`;

describe('硬性拒绝', () => {
  it('AI SDK：toolApproval 返回 denied', async () => {
    const shop = new Shop();
    const model = new ScriptedModel([call('c1', 'refund', { orderId: 'A2', amount: 9000 }), say('退不了')]);
    const agent = new ToolLoopAgent({
      model,
      tools: shopTools(shop),
      toolApproval: {
        refund: ({ amount }) =>
          amount > LIMIT ? { type: 'denied', reason: REASON } : amount > 500 ? 'user-approval' : undefined,
      },
    });
    const r = await agent.generate({ prompt: '退 9000' });
    expect(r.text).toBe('退不了');
    expect(shop.refunds).toEqual([]);
    expect(model.lastMessageAt(1)).toMatchObject({ content: [{ output: { type: 'execution-denied', reason: REASON } }] });
  });

  const hooks = {
    beforeToolCall: ({ input }: { input: unknown }) =>
      (input as { amount: number }).amount > LIMIT ? { proceed: false as const, output: REASON } : undefined,
  };

  it('Mastra：先停在审批，批准后 hook 才拦', async () => {
    const shop = new Shop();
    const m = new ScriptedModel([call('c1', 'refund', { orderId: 'A2', amount: 9000 }), say('退不了')]);
    const agent = buildMastra(m, shop, new InMemoryStore(), { hooks }).getAgent('support');
    const r1 = await agent.generate('退 9000');
    expect(r1.finishReason).toBe('suspended');
    const r2 = await agent.approveToolCallGenerate({ runId: r1.runId!, toolCallId: 'c1' });
    expect(r2.text).toBe('退不了');
    expect(shop.refunds).toEqual([]);
    expect(m.lastMessageAt(1)).toMatchObject({ content: [{ output: { type: 'text', value: REASON } }] });
  });

  it('Mastra 的坑：hooks 只传给 generate()，审批恢复时不会带上', async () => {
    const shop = new Shop();
    const m = new ScriptedModel([call('c1', 'refund', { orderId: 'A2', amount: 9000 }), say('?')]);
    const agent = buildMastra(m, shop, new InMemoryStore()).getAgent('support');
    const r1 = await agent.generate('退 9000', { hooks });
    await agent.approveToolCallGenerate({ runId: r1.runId!, toolCallId: 'c1' });
    expect(shop.refunds).toEqual(['A2:9000']); // 拦截没有生效
  });

  it('OpenAI：工具输入 guardrail 返回 rejectContent', async () => {
    const shop = new Shop();
    const limit = defineToolInputGuardrail({
      name: 'refund_limit',
      run: async ({ toolCall }) => {
        const { amount } = JSON.parse(toolCall.arguments) as { amount: number };
        return { behavior: amount > LIMIT ? { type: 'rejectContent', message: REASON } : { type: 'allow' } };
      },
    });
    const refund = oaTool({
      name: 'refund',
      description: '退款',
      parameters: z.object({ orderId: z.string(), amount: z.number() }),
      inputGuardrails: [limit],
      execute: async ({ orderId, amount }) => shop.refund(orderId, amount),
    });
    const model = new ScriptedOpenAIModel([fnCall('c1', 'refund', { orderId: 'A2', amount: 9000 }), reply('退不了')]);
    const r = await run(new OAAgent({ name: 's', model, tools: [refund] }), '退 9000');
    expect(r.finalOutput).toBe('退不了');
    expect(shop.refunds).toEqual([]);
    expect(model.lastInputAt(1)).toMatchObject({ output: { type: 'text', text: REASON } });
  });
});
```

结论：

- AI SDK 把“拒绝”做成了审批策略的一种结果，同一个函数里写允许、问人、拒绝三种情况，最清楚。
- Mastra 的 `requireApproval` 在 hook 之前生效：9000 的退款先问了人，人批准之后 hook 才把它拦下。也就是说审批人可能批准一笔最终不会执行的退款。想先拦后问，用调用级的 `requireToolApproval` 函数对超限的调用返回 false，或者干脆不给 `refund` 设 `requireApproval`，在 hook 里判断。
- Mastra 调用级的 `hooks` 不会存进快照：第二个 Mastra 测试里，hooks 只传给了 `generate()`，`approveToolCallGenerate` 恢复时没有它，9000 的退款真的执行了。安全相关的拦截要配在 agent 上，或者每次恢复时都显式传入。
- OpenAI 用工具 guardrail 实现，`rejectContent` 的消息作为工具结果交给模型；`throwException` 则让整个 run 抛错。

这段代码和测试在写课时的沙箱里跑通了（4 个测试）。

## 完成标准

- `npm run typecheck` 零错误，`npm test` 25 个测试全部通过。
- 能说出 `generateText` 和 `ToolLoopAgent` 的默认步数，以及三家撞到上限时的行为差别。
- 能说清楚三家对工具抛错、工具不存在、参数错误的默认处理，以及各自怎么改。
- 能写出三家的人工审批流程，说明审批期间的状态在哪、由谁保存、怎么在另一个实例上恢复。
- 能解释审批历史由客户端保存时的篡改风险，以及签名如何防住它。
- 练习 2 完成：同一组测试在三个实现上通过。
- 能根据项目情况在 AI SDK、Mastra、OpenAI Agents SDK 之间做出选择，并说出理由。
