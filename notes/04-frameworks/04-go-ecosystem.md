# 4.4 Go 生态：Eino、Genkit Go 与官方 MCP Go SDK

> 第 4 部分 · 第 4 课 ｜ 预计 4–5 小时 ｜ 前置：2.1 Function Calling、2.2 MCP、3.2 Agent Loop、3.4 长任务 Agent、4.1 LangGraph
>
> 配套练习：`projects/27-go-ecosystem/`（22 个测试）。这一课不再重写运行时，而是直接用真实的库：Eino v0.9.23、Genkit Go v1.14.0、官方 MCP Go SDK v1.8.0。测试里的模型是按脚本回复的假模型，不需要 API key。内容包括：用 `compose.Graph` 写一个带局部状态和分支的确定性流程，再包装成工具；用 ADK 的 `ChatModelAgent` + `Runner` 跑 ReAct 循环（流式事件、`MaxIterations`、调用不存在的工具）；Eino 的工具中断、checkpoint 和跨实例恢复；Genkit 的工具循环、`tool.Fail` 和普通错误的区别、`maxTurns`、可恢复工具和 flow；用官方 SDK 写 MCP 服务端（schema 推断、调用 handler 前先校验参数、错误结果、MRTR 多轮请求）；最后把同一组 MCP 工具分别接进 Eino 和 Genkit。

## 读前说明

4.1 到 4.3 讲的三个框架都是 Python / TypeScript 生态的，所以那几课用 Go 把它们的运行时重写了一遍，目的是看清楚机制。Go 这边有自己能直接用的库，这一课就用真库写代码，看它们的 API 实际长什么样、行为在哪里和你预想的不一样。

三个库管的事情不同：

| | Eino | Genkit Go | 官方 MCP Go SDK |
|---|---|---|---|
| 出身 | 字节跳动 CloudWeGo | Google Firebase | MCP 官方组织（modelcontextprotocol） |
| 定位 | 组件接口 + 编排引擎（compose）+ agent 开发包（ADK） | 统一的模型调用接口 + 工具 + flow + 开发工具（Dev UI、trace） | MCP 协议本身：客户端、服务端、传输层、OAuth |
| 模型接入 | `eino-ext` 里的组件（OpenAI、Claude、Gemini、Ark、Ollama……） | 插件（googlegenai、anthropic、compat_oai、ollama……） | 不涉及模型 |
| 控制流 | 你画图（Graph / Chain / Workflow），或用 ADK 的固定循环 | `Generate` 内置的工具循环，外面用 flow 包起来 | 不涉及 |
| 人工审批 | `tool.StatefulInterrupt` + `CheckPointStore` + `Runner.ResumeWithParams` | 可恢复工具 + `tool.Interrupt` + `ai.WithResume` | 服务端返回 `InputRequests`，客户端带 `InputResponses` 重试（MRTR） |
| 当前版本 | v0.9.23（2026-11-09 发布） | v1.14.0（2026-10-19 UTC 发布） | v1.8.0（2026-09-04 发布），最新协议版本 2026-07-28 |

版本号来自 Go module proxy（proxy.golang.org）。Eino 还没到 v1，minor 版本之间有破坏性变更（v0.7 重构了中断恢复，v0.8 改了 ADK 中间件，v0.9 加了 agentic 运行时），升级前看它的迁移文档。Genkit Go 的 agent API（`genkit/exp`）标着预览，minor 版本也可能改。MCP SDK 已经是 v1，承诺不破坏兼容。

本课依据：

- Eino：仓库 README 和 `llms.txt`，cloudwego.io 上的 “Agent or Graph?”、“Stream Programming Essentials”、“Human-in-the-Loop Framework” 三篇文档，以及 v0.9.23 源码里的注释（`adk/chatmodel.go`、`compose/tool_node.go`、`components/tool/interrupt.go`）。
- Genkit Go：`go/` 目录下的 README，v1.14.0 源码里 `genkit.DefineResumableTool`、`ai/tool` 包、`ai.WithMaxTurns` 的注释。
- MCP Go SDK：仓库 README（版本兼容表）、`docs/protocol.md`（生命周期、`server/discover`）、`docs/server.md`（工具、MRTR）、`docs/rough_edges.md`，以及 `mcp.AddTool` / `ToolHandlerFor` 的注释。

下文凡是写了“测试验证过”的行为，练习里都有对应的测试。

先想几个问题：

- Eino 的 `MaxIterations` 设成 2 和 Genkit 的 `WithMaxTurns(2)`，模型各会被调用几次？
- 模型调用了一个不存在的工具名，Eino 默认怎么处理？
- 一个 MCP 工具的 handler 返回了 Go 的 `error`，客户端拿到的是协议错误还是别的？
- 审批要等到明天，第二天处理恢复请求的是另一台机器，Eino 需要你保存什么？

## 1. 三个库在一张图里

```
            ┌──────────────── 你的服务 ────────────────┐
            │                                           │
  用户 ───► │  Eino ADK Runner          Genkit flow     │
            │   └ ChatModelAgent         └ Generate     │
            │       ├ ChatModel (eino-ext)   ├ 模型插件 │
            │       └ Tools ──┐              └ Tools ─┐ │
            │                 │                       │ │
            │        compose.Graph（包成工具）         │ │
            │                 │                       │ │
            │                 └──── MCP 适配器 ◄───────┘ │
            └───────────────────────┬───────────────────┘
                                    │ MCP（stdio / Streamable HTTP）
                           MCP Go SDK 写的服务端
```

Eino 和 Genkit 是同一层的东西，二选一就够了，很少有项目同时用。MCP SDK 在另一层：不管你用哪个框架，工具都可以放在 MCP 服务端里，框架通过适配器调用。练习里把同一个 MCP 服务端同时接进两个框架，就是为了对照这一点。

## 2. Eino

### 2.1 组件接口

Eino 先定义组件接口，具体实现放在单独的 `eino-ext` 仓库。和 agent 关系最大的两个：

```go
// components/model
type BaseChatModel interface {
    Generate(ctx context.Context, input []*schema.Message, opts ...Option) (*schema.Message, error)
    Stream(ctx context.Context, input []*schema.Message, opts ...Option) (*schema.StreamReader[*schema.Message], error)
}

type ToolCallingChatModel interface {
    BaseChatModel
    WithTools(tools []*schema.ToolInfo) (ToolCallingChatModel, error)
}

// components/tool
type BaseTool interface {
    Info(ctx context.Context) (*schema.ToolInfo, error)
}
type InvokableTool interface {
    BaseTool
    InvokableRun(ctx context.Context, argumentsInJSON string, opts ...Option) (string, error)
}
```

几点值得注意：

- 还有一个旧接口 `ChatModel`，它的 `BindTools` 会修改模型自己，多个 goroutine 共用一个模型实例、各自绑不同工具时会互相覆盖。新接口 `ToolCallingChatModel.WithTools` 返回一个新实例，新代码用这个。
- `ChatModelAgent` 实际上不调用 `WithTools`，而是每次调用模型时通过 `model.WithTools(...)` 这个调用选项把工具传进去。练习里的假模型一开始只在 `WithTools` 方法里记录工具，测试读到的是空列表，改成从 `model.GetCommonOptions(nil, opts...).Tools` 读之后才对。你自己写模型适配器时，两个入口都要处理。（测试验证过）
- 工具参数用的是字符串形式的 JSON，返回值也是字符串。要用 Go 结构体，用 `utils.InferTool`，它从输入类型推断 schema，自动做反序列化和序列化。字段描述写在 `jsonschema:"description=..."` 标签里。
- `schema.Message` 是各家模型消息格式的公共子集：`Role`、`Content`、`ToolCalls`、`ToolCallID` 等字段。v0.9 还加了一套 `AgenticMessage`（beta），用来表示推理过程、服务端工具这类新内容块。本课只用 `schema.Message`。

### 2.2 compose：把确定的流程画成图

`compose` 包提供三种编排方式：`Chain` 是线性的，`Graph` 是带条件分支的有向图，`Workflow` 在 `Graph` 上加了字段级别的数据映射。练习里的工单分流图：

```go
g := compose.NewGraph[Ticket, Triage](compose.WithGenLocalState(func(context.Context) *triageState {
    return &triageState{}
}))
_ = g.AddLambdaNode("classify", compose.InvokableLambda(classifyFn))
_ = g.AddLambdaNode("refund", route("billing", "P2"), visit("refund"))
_ = g.AddLambdaNode("bug", route("engineering", "P1"), visit("bug"))
_ = g.AddLambdaNode("human", route("frontline", "P3"), visit("human"))

_ = g.AddEdge(compose.START, "classify")
_ = g.AddBranch("classify", compose.NewGraphBranch(pickFn,
    map[string]bool{"refund": true, "bug": true, "human": true}))
for _, n := range []string{"refund", "bug", "human"} {
    _ = g.AddEdge(n, compose.END)
}
r, err := g.Compile(ctx, compose.WithGraphName("triage"))
out, err := r.Invoke(ctx, Ticket{ID: "T1", Text: "我要退款"})
```

和 4.1 的 LangGraph 对照：

- **类型是泛型参数**。`NewGraph[Ticket, Triage]` 规定了整张图的输入输出类型，每个节点的输入输出类型在连边时检查。练习里有个测试故意把输出 `int` 的节点连到输入 `string` 的节点，`AddEdge` 当场返回 `start node's output type[int] and end node's input type[string] mismatch`。（测试验证过）
- **错误会粘在图上**。构图时第一个出错的调用会把错误记在图里，之后所有 `AddXxx` 和 `Compile` 都返回同一个错误，所以示例代码里每一步都可以写 `_ =`，只检查 `Compile`。上面那个测试也验证了 `Compile` 返回的错误和 `AddEdge` 的一样。
- **状态是局部的、显式的**。LangGraph 的状态是全图共享的 channel，节点返回部分更新，由 reducer 合并。Eino 的节点之间传的是普通的 Go 值；需要跨节点共享的东西放在 `WithGenLocalState` 生成的状态对象里，用 `WithStatePreHandler` / `WithStatePostHandler` 在节点前后读写，或者在节点里调 `compose.ProcessState` 读写。框架保证这些访问是串行的。
- **分支是一个函数**。`NewGraphBranch` 接收上游输出，返回下一个节点的名字，第二个参数列出所有可能的目标。还有 `NewStreamGraphBranch`，它接收的是流，可以只看第一个分片就决定走哪条路（典型用途：看模型输出的第一片有没有工具调用）。

### 2.3 流：四种范式和自动转换

Eino 文档把组件对流的支持分成四种：

| 方法 | 输入 | 输出 | Lambda 构造函数 |
|---|---|---|---|
| Invoke | 非流 | 非流 | `compose.InvokableLambda` |
| Stream | 非流 | 流 | `compose.StreamableLambda` |
| Collect | 流 | 非流 | `compose.CollectableLambda` |
| Transform | 流 | 流 | `compose.TransformableLambda` |

组件只需要实现对它有意义的那几种：ChatModel 和 Tool 实现 Invoke 和 Stream，Retriever、Embedder 这些只实现 Invoke。在图里上下游对不上时，框架自动转换：上游给的是流而下游只收完整值，框架把流拼起来（concat）；反过来，框架把一个值包成只有一个分片的流（文档管它叫“假流”）。

文档里有一条规则要记住：**整张图用 Invoke 调用时，里面每个组件都按 Invoke 调用；用 Stream / Collect / Transform 调用时，里面每个组件都按 Transform 调用。** 也就是说，你在图外面用 `Invoke`，里面的 ChatModel 也不会流式输出；想要首字延迟低，整张图要用 `Stream` 调。文档承认这不是在所有情况下最优，但规则简单、好预测。

自定义类型的流要拼接时，框架可能需要你注册一个 concat 函数，否则会报错。

### 2.4 Graph 还是 Agent

Eino 的文档专门写了一篇 “Agent or Graph?”，结论很明确：

- Graph 适合输入是结构化数据、流程固定、只关心最终结果的任务，比如“录音 → 总结 → 保存”这种按钮背后的功能。
- Agent 适合输入是自然语言、步骤不确定、过程和结果都要展示给用户的任务。
- 把整个 agent 塞进 graph 当一个节点通常不合适：agent 依赖自己的对话历史，输出是一串异步事件，下游节点不好用。graph 里需要智能的地方，放一个普通的 LLM 节点就够了。
- **最好的结合方式是把 graph 包装成 agent 的工具。** graph 的输入是结构化的、执行一次就结束、对调用方来说是同步的，这些正好都是工具的特征。

练习里的 `TriageTool` 就是这么做的：

```go
func TriageTool(r compose.Runnable[Ticket, Triage]) (tool.InvokableTool, error) {
    return utils.InferTool("triage_ticket", "把工单分到对应队列并给出优先级",
        func(ctx context.Context, t Ticket) (Triage, error) {
            return r.Invoke(ctx, t)
        })
}
```

工具的参数 schema 从 `Ticket` 推断，`jsonschema:"description=用户的原话"` 会出现在模型看到的参数说明里（测试验证过）。eino-examples 里有一个更完整的 `graphtool`，还处理了图内部中断的透传。

### 2.5 ADK：ChatModelAgent 和 Runner

ADK 是 v0.5 加进来的 agent 开发包，层级在 compose 之上。最常用的是 `ChatModelAgent`，它就是 3.2 课的 ReAct 循环：

```go
agent, _ := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
    Name:          "support",
    Description:   "处理客服工单的 agent",
    Instruction:   "你是客服 agent，需要时调用工具。",
    Model:         m,
    MaxIterations: 0, // 0 = 默认 20
    ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
        Tools:               tools,
        UnknownToolsHandler: nil,   // nil = 调用不存在的工具时整次运行报错
        ExecuteSequentially: false, // false = 同一轮的多个工具调用并行执行
    }},
})
runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent, EnableStreaming: true, CheckPointStore: store})
iter := runner.Query(ctx, "工单 T9：申请退款", adk.WithCheckPointID("s1"))
for {
    ev, ok := iter.Next()
    if !ok { break }
    // ev.Err / ev.Output.MessageOutput / ev.Action.Interrupted
}
```

行为细节：

- **`Instruction` 变成 system 消息**放在最前面。测试里检查了第二次调用模型时收到 4 条消息：system、user、带工具调用的 assistant、tool。（测试验证过）
- **`MaxIterations` 数的是模型调用次数**，默认 20。设成 2，模型被调用 2 次后还想调工具，运行以错误结束。（测试验证过）
- **同一轮的多个工具调用默认并行执行**，设 `ExecuteSequentially: true` 才按顺序。这和 4.3 课 Claude Agent SDK “只读工具才并发”不同，Eino 不区分只读和有副作用，你要自己判断。
- **调用不存在的工具默认让整次运行失败。** 模型偶尔会编造工具名，生产环境建议配 `UnknownToolsHandler`，返回一段“没有这个工具，可用的有……”让模型自己纠正。练习里两种情况都测了。（测试验证过）
- **事件是迭代器**，每个事件可能带一条消息输出、一个动作（中断、转交、退出），或者一个错误。`MessageOutput.Role` 区分是模型输出（`schema.Assistant`）还是工具结果（`schema.Tool`），工具结果事件的 `ToolName` 是工具名。流式模式下 `IsStreaming` 为 true，消息在 `MessageStream` 里，必须读完并关闭；`adk.GetMessage(ev)` 会帮你把流拼成一条完整消息。练习里的 `Collect` 函数就是这样处理的。
- **`Runner` 不只是个壳。** 它管 checkpoint 的保存和读取，`Resume` / `ResumeWithParams` 都在它上面。直接调 `agent.Run` 拿不到这些。

ADK 还有：`deep.New` 创建的 DeepAgent（规划、子 agent、文件系统，类似 4.3 课的 Claude Code）；`ChatModelAgentMiddleware`（文件系统、技能、上下文摘要、工具搜索等现成的中间件）；Plan-Execute、Supervisor 这些预置的多 agent 模式；`adk.NewAgentTool` 把一个 agent 包成另一个 agent 的工具。源码注释里对“agent 转交、共享全部上下文”的方式（`Exit`、`OutputKey`、`TransferToAgent` 等）都标了 NOT RECOMMENDED，原话是这种方式“没有被经验证明更有效”，建议用 AgentTool 或 DeepAgent。这和 3.3 课、4.2 课的结论一致：子 agent 拿干净的上下文、只回传结果，比整段对话交出去更稳。

### 2.6 中断与恢复

v0.7 重构之后，Eino 的中断机制基于“地址”：每个可能中断的位置（agent、图节点、某一次工具调用）都有一个稳定唯一的 ID，比如 `agent:support;tool:refund:c1` 这种形式（具体格式是框架内部的，不要自己拼）。恢复时你按 ID 把数据交给对应的位置。

在工具里用的四个函数（`components/tool` 包，和 `compose` 包里同名的函数对称）：

| 函数 | 作用 |
|---|---|
| `tool.Interrupt(ctx, info)` | 中断，`info` 是给人看的信息，不保存 |
| `tool.StatefulInterrupt(ctx, info, state)` | 中断并把 `state` 存进 checkpoint |
| `tool.GetInterruptState[T](ctx)` | 恢复时问：我之前中断过吗？存的状态是什么？ |
| `tool.GetResumeContext[T](ctx)` | 恢复时问：这次恢复是冲着我来的吗？人给了什么数据？ |

练习里的退款工具：

```go
func (t *RefundTool) InvokableRun(ctx context.Context, argsJSON string, _ ...tool.Option) (string, error) {
    var args RefundArgs
    _ = json.Unmarshal([]byte(argsJSON), &args)

    wasInterrupted, _, pending := tool.GetInterruptState[*RefundPending](ctx)
    if !wasInterrupted {
        if args.Amount <= t.Threshold {
            return t.execute(args), nil
        }
        p := &RefundPending{OrderID: args.OrderID, Amount: args.Amount}
        return "", tool.StatefulInterrupt(ctx, p, p)
    }

    isTarget, hasData, decision := tool.GetResumeContext[*RefundApproval](ctx)
    if !isTarget {
        return "", tool.StatefulInterrupt(ctx, pending, pending) // 不是我：原样再中断
    }
    if !hasData || decision == nil {
        return "", errors.New("refund: resumed without a decision")
    }
    if !decision.Approved {
        return "退款被拒绝：" + decision.Note, nil
    }
    return t.execute(RefundArgs{OrderID: pending.OrderID, Amount: pending.Amount}), nil
}
```

调用方：

```go
res, _ := Collect(runner.Query(ctx, "给 A100 退 800", adk.WithCheckPointID("s1")))
ic := res.Interrupts[0]               // IsRootCause 为 true 的那个
// ……把 ic.Info 展示给审批人，拿到决定……
iter, _ := runner.ResumeWithParams(ctx, "s1", &adk.ResumeParams{
    Targets: map[string]any{ic.ID: &RefundApproval{Approved: true}},
})
```

要点：

- **恢复时工具会被重新调用**，和 4.1 课 LangGraph 的“节点重跑”一样。所以中断之前不能有副作用，或者副作用必须幂等。
- **“不是我”的时候必须再中断一次。** 同一轮可能有多个工具并行中断，你这次只恢复了其中一个，其余的工具也会被重新调用，它们发现自己不是目标，就要带着原来的状态再中断，否则状态就丢了。Eino 的测试里有专门的例子：两个工具并行中断，先恢复 A，B 再中断一次，第二次恢复 B。
- **checkpoint 用 gob 序列化**，状态里放自定义类型要先注册：`schema.RegisterName[*RefundPending]("goeco_refund_pending")`。名字要全局唯一而且以后不能改，否则旧的 checkpoint 读不出来。
- **`CheckPointStore` 只有两个方法**：`Get(ctx, id) ([]byte, bool, error)` 和 `Set(ctx, id, []byte) error`，Redis、数据库都很好实现。
- **跨实例恢复只需要共享 store 和 checkpoint ID。** 练习里的第二个测试用另一个 Runner、另一个模型实例、另一个工具实例去恢复，只共享 store，结果正确。新模型只需要回答恢复之后的那一轮，因为对话历史在 checkpoint 里。（测试验证过）
- 人给的恢复数据（`Targets` 的值）不需要 gob 注册，它是恢复时从内存直接传进去的。
- 中断事件里，`InterruptContexts` 是一条链：最内层真正中断的那个 `IsRootCause` 为 true，外面包着它的 agent 也各有一个上下文。展示给人看时取 root cause。

### 2.7 和 4.1 LangGraph 的中断对照

| | LangGraph | Eino |
|---|---|---|
| 中断 | 节点里调 `interrupt(value)` | 工具 / 节点里返回 `tool.Interrupt` 或 `StatefulInterrupt` 的 error |
| 恢复时重跑 | 整个节点从头重跑，`interrupt()` 第 N 次调用返回第 N 个恢复值 | 被中断的工具或节点重跑，自己查 `GetInterruptState` / `GetResumeContext` |
| 状态 | 图状态由 checkpointer 保存，节点内的局部变量不保存 | 可以用 `StatefulInterrupt` 显式保存一份局部状态 |
| 定位 | 按中断 ID 或按顺序 | 按地址 ID |
| 存储 | checkpointer（每个 super-step 都写） | `CheckPointStore`，中断时写 |

Eino 的方式更啰嗦，但每个中断点自己决定存什么，不靠“第几次调用”这种隐式顺序。

## 3. Genkit Go

### 3.1 一切都是注册过的 action

Genkit 的核心概念是 action：模型、工具、flow、prompt、检索器都注册在一个 `*genkit.Genkit` 实例上，有名字、有输入输出 schema，可以被 Dev UI 列出来单独调用，每次调用都自动产生 trace。

```go
g := genkit.Init(ctx, genkit.WithPlugins(&googlegenai.GoogleAI{}))
answer, err := genkit.GenerateText(ctx, g,
    ai.WithModelName("googleai/gemini-flash-latest"),
    ai.WithPrompt("Why is Go a great language for AI applications?"),
)
```

插件负责注册模型。练习里不加任何插件，用 `genkit.DefineModel` 自己注册一个脚本模型：

```go
m := genkit.DefineModel(g, "test/loop", &ai.ModelOptions{
    Supports: &ai.ModelSupports{Multiturn: true, Tools: true, SystemRole: true},
}, func(ctx context.Context, req *ai.ModelRequest, cb ai.ModelStreamCallback) (*ai.ModelResponse, error) {
    // req.Messages、req.Tools 是 Genkit 交给模型的全部内容
    return &ai.ModelResponse{Request: req, Message: next(), FinishReason: ai.FinishReasonStop}, nil
})
```

`Supports` 描述模型能做什么，要如实填，Genkit 和 Dev UI 会读它。写真实模型的适配器就是这个函数：把 `ModelRequest` 转成厂商的请求格式，把响应转回 `ModelResponse`，流式时每个分片调一次 `cb`。

### 3.2 Generate 和工具循环

```go
stock := genkit.DefineTool(g, "check_stock", "查询商品库存",
    func(ctx *ai.ToolContext, in StockQuery) (int, error) { ... })

resp, err := genkit.Generate(ctx, g,
    ai.WithModel(m),
    ai.WithPrompt("A1 还有货吗？"),
    ai.WithTools(stock),
    ai.WithMaxTurns(5),
)
```

`Generate` 自己跑工具循环：模型要工具就执行，把结果追加进消息，再调模型，直到模型不再要工具。

- **`maxTurns` 数的是工具轮数，不是模型调用次数**，默认 50。注释原文：每一轮往返（包括同一轮里并行的多个工具）算一轮。设成 2：模型第 1、2 次要工具都执行了，第 3 次还要工具时报 `ai.ErrMaxTurnsExceeded`，所以模型一共被调用 3 次。对比 Eino 的 `MaxIterations=2` 是模型调用 2 次。同一个数字，两个框架差一次。（测试验证过）
- **工具错误分两种。** 用 `tool.Fail(ctx, err)` 包起来的错误会作为 `{"error": "..."}` 交给模型，循环继续，模型可以换个参数再试；其他错误直接让 `Generate` 失败。练习里查不到商品用 `tool.Fail`，库存服务挂了直接返回 `ErrInventoryDown`，测试分别验证了“模型第二轮看到了错误”和“`errors.Is(err, ErrInventoryDown)` 成立、模型只被调用一次”。（测试验证过）
- **模型给的工具参数不符合 schema 时，`Generate` 直接失败**，错误是 `invalid input to action ... data did not match expected schema`，不会作为 `tool.Fail` 交还模型。写课时用一个把字符串字段传成数字的工具调用试过。想让模型自己纠正参数，就把输入类型放宽，在工具里自己校验，校验失败时返回 `tool.Fail`。
- 不想让 `Generate` 自动执行工具，用 `ai.WithReturnToolRequests(true)`，它会把工具请求原样返回给你，循环由你自己写。
- 结构化输出用 `genkit.GenerateData[T]`，schema 从 `T` 推断，结果直接解码成 `*T`。

和 Eino 对照：Eino 默认把“工具不存在”当致命错误，工具返回的普通 error 也会让运行失败；想让模型看到错误，要在工具里自己把错误变成字符串返回。Genkit 反过来提供了明确的标记 `tool.Fail`。两种设计都要求你在写工具时想清楚：这个错误模型能处理吗？

### 3.3 可恢复工具

Genkit 1.14 用 `DefineResumableTool` 定义能暂停的工具。工具函数多一个参数，第一次调用时为 nil，恢复时是调用方给的答复：

```go
transfer := genkit.DefineResumableTool(g, "transfer", "转账",
    func(ctx context.Context, in TransferInput, approval *TransferApproval) (string, error) {
        if approval == nil && in.Amount > limit {
            return "", tool.Interrupt(ctx, TransferHold{Reason: "over_limit", Amount: in.Amount})
        }
        if approval != nil && !approval.Approved {
            return "转账已取消", nil
        }
        log.add(in)
        return fmt.Sprintf("已向 %s 转账 %.0f", in.To, in.Amount), nil
    })
```

调用方：

```go
resp, _ := genkit.Generate(ctx, g, ai.WithModel(m), ai.WithPrompt("给 alice 转 5000"), ai.WithTools(transfer))
// resp.FinishReason == ai.FinishReasonInterrupted
part := resp.Interrupts()[0]
call, ok := transfer.Interrupted(part)        // call.Input 是 TransferInput
hold, _ := ai.InterruptAs[TransferHold](part)  // 读中断数据

resp, _ = genkit.Generate(ctx, g,
    ai.WithModel(m),
    ai.WithMessages(resp.History()...),        // 把之前的对话原样带回去
    ai.WithTools(transfer),
    ai.WithResume(call.Restart(TransferApproval{Approved: true})),
)
```

- `call.Restart(res)` 重新执行工具，`res` 作为第三个参数传进去。`call.Respond(out)` 不执行工具，直接替它给出输出。练习里有一个测试用 `Respond` 拒绝转账，检查了工具没有执行、模型在下一轮看到了这个输出。（测试验证过）
- **`Res` 和中断数据都必须能序列化成 JSON 对象**（结构体或 key 为字符串的 map）。`tool.Interrupt(ctx, "请确认")` 传一个字符串不会中断，而是返回一个普通错误让工具调用失败。这是因为它们要能通过 HTTP 发给远端调用方，进程内和过网络时的形状要一致。
- **没有 checkpoint store。** 状态就是 `resp.History()` 返回的消息列表，中断的工具请求在最后一条消息里，带着元数据。要跨进程恢复，你自己把这些消息存起来，恢复时用 `WithMessages` 传回去。对比 Eino，Genkit 的 `Generate` 是无状态的，存储完全交给你；Eino 的 Runner 帮你存。
- 中断之前同样不能有副作用：`Restart` 会重新执行整个工具函数。

`ToolApproval` 中间件可以不改工具代码，给任意工具加上“先暂停等批准”，原理一样。

### 3.4 flow

flow 是 Genkit 的部署单元：

```go
flow := genkit.DefineFlow(g, "ask", func(ctx context.Context, question string) (string, error) {
    q, err := genkit.Run(ctx, "normalize", func() (string, error) {
        return strings.TrimSpace(question), nil
    })
    if err != nil {
        return "", err
    }
    resp, err := genkit.Generate(ctx, g, ai.WithModel(model), ai.WithSystem("你是库存助手"),
        ai.WithPrompt(q), ai.WithTools(tools...))
    if err != nil {
        return "", err
    }
    return resp.Text(), nil
})
out, err := flow.Run(ctx, "  A1 还有货吗？ ")
```

- flow 有类型化的输入输出，自动产生一条 trace，里面的 `genkit.Run` 子步骤和 `Generate` 调用都是 trace 的子节点。
- `genkit start -- go run .` 启动 Dev UI（设置环境变量 `GENKIT_ENV=dev` 后会开一个本地反射服务），可以在浏览器里单独跑某个 flow、某个模型、某个工具，看每一步的输入输出。不设这个变量时 Genkit 以 prod 模式运行，不开反射服务，测试日志里能看到 `Genkit initialized env=prod`。
- `genkit.Handler(flow)` 把 flow 变成 `http.HandlerFunc`，配合 `plugins/server` 可以直接部署。

### 3.5 Agents（预览）

Genkit Go 在 `genkit/exp` 和 `ai/exp` 里有一套预览中的 agent API，要 `genkit.WithExperimental()` 才能用：`genkitx.DefineAgent` 定义有会话存储的 agent，每一轮写一个快照，可以从任意快照继续或分叉；`Detach` 把一轮交给后台运行；`middlewarex.Agents` 给 agent 生成 `delegate_to_<name>` 工具来委派子 agent；`AllAgentRoutes` 把 agent 挂成 HTTP 接口，还能用 `DefineRemoteAgent` 调另一个进程（Go、JS 或 Dart 写的）里的 agent。概念上和 4.3 课 Claude Agent SDK 的会话 / fork 很接近。README 写着这套 API 在 minor 版本里可能有破坏性变更，本课的练习不用它。

## 4. 官方 MCP Go SDK

### 4.1 版本和协议

SDK 的 README 有一张兼容表：

| SDK 版本 | 最新 MCP 规范 | 支持的规范 |
|---|---|---|
| v1.7.0+ | 2026-07-28 | 2026-07-28、2025-11-25、2025-06-18、2025-03-26、2024-11-05 |
| v1.4.0 – v1.6.1 | 2025-11-25 | 2025-11-25、2025-06-18、2025-03-26、2024-11-05 |
| v1.0.0 – v1.1.0 | 2025-06-18 | 2025-06-18、2025-03-26、2024-11-05 |

2.2 课讲过 2026-07-28 规范的变化：没有 `initialize` 握手，每个请求在 `_meta` 里带协议版本和客户端能力；新增 `server/discover`；服务端向客户端要信息改用 MRTR（多轮请求）。SDK 对上层隐藏了两种模式的差别：

- `Client.Connect` 先发 `server/discover`，用结果协商版本；发现失败或服务端不支持最新版本时，退回旧的 `initialize` 握手。
- `Server.Connect` 自动注册 `server/discover`。
- 不管协商出哪个版本，你用的 API 都一样。

练习里客户端和服务端都是 v1.8.0，测试检查了协商结果是 `2026-07-28`。（测试验证过）

另外，2026-07-28 规范通过 SEP-2577 把 roots、sampling、logging 三个功能标为废弃，SDK 在废弃期（至少十二个月）内继续支持。新代码不要再依赖它们。

### 4.2 服务端：类型化工具

```go
type DocQuery struct {
    Query string `json:"query" jsonschema:"要搜索的关键词"`
    Limit int    `json:"limit,omitempty" jsonschema:"最多返回几条"`
}
type DocHits struct {
    Titles []string `json:"titles"`
}

srv := mcp.NewServer(&mcp.Implementation{Name: "docs", Version: "v0.1.0"}, nil)
mcp.AddTool(srv, &mcp.Tool{Name: "search_docs", Description: "搜索内部文档"},
    func(ctx context.Context, req *mcp.CallToolRequest, in DocQuery) (*mcp.CallToolResult, DocHits, error) {
        return nil, DocHits{Titles: search(in)}, nil
    })
```

泛型函数 `mcp.AddTool` 会自动做很多事（`ToolHandlerFor` 的注释里逐条列了）：

- 从 `In` 推断输入 schema，从 `Out` 推断输出 schema。没有 `omitempty` 的字段是必填。`jsonschema` 标签的**整段内容就是字段描述**，不是 `key=value` 格式。这里和 Eino 的 `jsonschema:"description=..."` 写法不同：两个库用的是不同的 JSON Schema 包（MCP SDK 用 `google/jsonschema-go`，Eino 用 `eino-contrib/jsonschema`），同一个结构体在两边要写不同的标签。
- **调用 handler 之前先按 schema 校验参数。** 不合法的输入根本到不了 handler。测试里漏传必填的 `query`，handler 调用次数是 0。（测试验证过）
- `Out` 的值放进 `StructuredContent`；没设 `Content` 时，SDK 把输出的 JSON 也放进一个文本内容块，兼容只读文本的客户端。
- **handler 返回的 error 是工具错误，不是协议错误。** 它被放进 `Content`，`IsError` 设为 true，客户端的 `CallTool` 返回 `err == nil`。参数校验失败也是这样。只有像“工具不存在”这种情况才是协议错误，`CallTool` 返回非 nil 的 err。练习里这三种情况都测了。（测试验证过）
- `In` 必须是结构体或 map（推断出的 schema 类型必须是 object）；不需要参数时用 `struct{}`。

这个区分对写 agent 很重要：工具错误应该交给模型，让它决定重试或换办法；协议错误说明连接或配置有问题，应该让这次运行失败。第 5 节的适配器就按这个规则来。

### 4.3 客户端和传输

```go
client := mcp.NewClient(&mcp.Implementation{Name: "bridge", Version: "v0.1.0"}, nil)
cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: exec.Command("docs-server")}, nil)
defer cs.Close()

for t, err := range cs.Tools(ctx, nil) { ... }   // 自动翻页的迭代器
res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "search_docs", Arguments: map[string]any{"query": "eino"}})
```

传输层有 `StdioTransport` / `CommandTransport`（子进程）、`StreamableClientTransport` / `StreamableHTTPHandler`（HTTP）、旧的 SSE 传输，以及测试用的 `NewInMemoryTransports()`。练习全部用内存传输，换成别的传输，上层代码不用改。

### 4.4 MRTR：服务端向用户要确认

2026-07-28 规范里，服务端不再在请求进行中主动给客户端发请求，而是在结果里返回 `InputRequests`，客户端拿到答复后带着 `InputResponses` 重新发起原来的调用。SDK 的写法：

```go
mcp.AddTool(srv, &mcp.Tool{Name: "delete_doc"}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
    Title string `json:"title"`
}) (*mcp.CallToolResult, any, error) {
    if len(req.Params.InputResponses) == 0 {
        return &mcp.CallToolResult{
            InputRequests: mcp.InputRequestMap{"confirm": &mcp.ElicitParams{
                Message:         "确认删除《" + in.Title + "》？",
                RequestedSchema: &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{"ok": {Type: "boolean"}}},
            }},
            RequestState: "title=" + in.Title,
        }, nil, nil
    }
    res, _ := req.Params.InputResponses["confirm"].(*mcp.ElicitResult)
    ...
})
```

- handler 会被调用两次：第一次返回输入请求，第二次读答复。`InputRequests` 的 key 是你自己起的名字，客户端会原样带回。`RequestState` 是一个不透明的字符串，客户端也原样带回，服务端可以用它恢复进度，不需要按会话存东西。
- 同时返回 `InputRequests` 和内容是服务端的 bug，SDK 返回 `-32603`。
- **客户端默认自动处理**：`ClientOptions.ElicitationHandler` 配好后，SDK 的 MRTR 中间件拿到输入请求就调你的 handler，自动重试，你的 `CallTool` 只看到最终结果。测试验证了这一点。（测试验证过）
- 设 `ClientOptions.MultiRoundTrip = &mcp.MultiRoundTripOptions{Disabled: true}` 关掉自动处理，`CallTool` 就会直接返回带 `InputRequests` 的结果，`res.NeedsInput()` 为 true，重试由你来做。练习 2 要用这个。
- 对旧协议版本的客户端，服务端的中间件会代替你调用旧的 `ServerSession.Elicit` 等接口，再带着答复重新调用一次 handler。所以按 MRTR 方式写的 handler 对新旧客户端都能用。
- 自动重试最多 10 次；服务端返回空的输入请求（相当于“稍后再试”）累计 3 次就放弃。

### 4.5 Go 里的另一个 MCP 库

在官方 SDK 之前，Go 社区用得最多的是 `mark3labs/mcp-go`，现在仍在维护（v1.5.0，2026-10-30 发布），官方 SDK 的 README 也把它列为可选的替代品。框架的 MCP 集成用的库不一样：

- Genkit Go v1.14.0 的 `plugins/mcp` 依赖 `mark3labs/mcp-go` v0.32.0（看它的 go.mod）。
- `eino-ext` 的 `components/tool/mcp` 依赖 `mark3labs/mcp-go`；另有一个子模块 `components/tool/mcp/officialmcp` 依赖官方 SDK（v0.1.1 用的是 v1.6.1）。

这意味着用框架自带的 MCP 集成时，你实际用的 MCP 实现和协议版本由框架的依赖决定，不一定支持 2026-07-28。需要新协议的特性（无状态、MRTR）时，自己用官方 SDK 写一个适配器，就是下一节的内容。

## 5. 把 MCP 工具接进 Eino 和 Genkit

适配器要做三件事：把工具列表转过去，把 schema 转过去，把调用结果和错误转过去。

### 5.1 schema：用 JSON 当桥

三个库用了三种 schema 表示：MCP SDK 是 `google/jsonschema-go` 的 `*jsonschema.Schema`（客户端拿到的 `Tool.InputSchema` 是 `any`），Eino 是 `eino-contrib/jsonschema` 的 `*jsonschema.Schema`，Genkit 是 `map[string]any`。最稳的转换方式是序列化成 JSON 再反序列化：

```go
func einoToolInfo(t *mcp.Tool) (*schema.ToolInfo, error) {
    raw, err := json.Marshal(t.InputSchema)
    if err != nil {
        return nil, err
    }
    var js einojs.Schema
    if err := json.Unmarshal(raw, &js); err != nil {
        return nil, fmt.Errorf("tool %s: input schema: %w", t.Name, err)
    }
    return &schema.ToolInfo{Name: t.Name, Desc: t.Description, ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&js)}, nil
}
```

Genkit 那边更简单：反序列化成 `map[string]any`，传给 `ai.WithInputSchema(in)`，工具的输入类型因此是 `any`。测试检查了转换之后 `required` 和字段描述都还在。（测试验证过）

### 5.2 错误：两个框架的约定不同

| MCP 结果 | Eino 适配器 | Genkit 适配器 |
|---|---|---|
| 成功，有 `StructuredContent` | 返回它的 JSON 字符串 | 返回 `StructuredContent` 本身 |
| 成功，只有文本 | 返回拼接的文本 | 返回拼接的文本 |
| `IsError = true`（工具错误） | 返回 `"工具出错：" + 文本`，error 为 nil，模型能看到 | 返回 `tool.Fail(ctx, err)`，模型看到 `{"error": ...}` |
| `CallTool` 返回 err（协议错误） | 返回 err，运行失败 | 返回 err，`Generate` 失败 |

Eino 没有 `tool.Fail` 这样的标记，所以想让模型看到错误，只能作为正常输出返回。练习里两个框架的测试都让模型先调一个总是失败的 `broken` 工具，再调 `search_docs`，验证了模型在下一轮看到了错误信息，循环没有中断。（测试验证过）

### 5.3 生命周期

- 适配器持有 `*mcp.ClientSession`，它要比 agent 活得久。服务关闭时 `cs.Close()`。
- 工具列表在构造 agent 时取一次。服务端的工具可能变化（`notifications/tools/list_changed`），长期运行的服务需要处理变化通知，或者每次运行前重新取。2026-07-28 规范还加了可缓存的列表结果，SDK 的 `docs/server.md` 有一节专门讲。
- Genkit 的工具注册在 `*genkit.Genkit` 上，同一个名字注册两次会 panic。练习给 MCP 工具加了 `mcp_` 前缀，避免和本地工具重名。

## 6. 同一件事，三个库怎么写

| 问题 | Eino ADK | Genkit Go | MCP Go SDK |
|---|---|---|---|
| 循环上限 | `MaxIterations`，数模型调用，默认 20 | `WithMaxTurns`，数工具轮，默认 50 | 不涉及；MRTR 自动重试最多 10 次 |
| 上限设成 2 时模型调用次数 | 2 | 3 | |
| 并行工具 | 默认并行，`ExecuteSequentially` 改为顺序 | 同一轮的多个工具算一轮 | |
| 工具不存在 | 默认运行失败，`UnknownToolsHandler` 兜底 | （工具定义来自注册表） | 协议错误 |
| 工具出错让模型看到 | 作为正常输出返回 | `tool.Fail` | handler 返回 error 即可，自动变成 `IsError` |
| 工具出错让运行失败 | 返回 error | 返回普通 error | 不涉及 |
| 参数校验 | 由你在工具里做（`InferTool` 只做反序列化） | 按输入 schema 校验，不合法时整个 `Generate` 失败 | 调 handler 前按 schema 校验，不合法时返回 `IsError` 结果 |
| 人工审批 | `StatefulInterrupt` + `ResumeWithParams` | 可恢复工具 + `WithResume` | `InputRequests` + `InputResponses` |
| 审批期间的状态存在哪 | `CheckPointStore`（框架写） | `resp.History()`（你自己存） | `RequestState`（客户端带回） |
| 恢复时 | 工具重新执行 | `Restart` 重新执行或 `Respond` 直接给结果 | handler 重新执行 |

“恢复时重新执行”这一行三个库都一样。这也是 3.4 课的结论：能暂停的地方前面不要有副作用。

## 7. 怎么选

- **团队主力语言是 Go，要做的是 agent 产品（对话、多步、工具很多）**：Eino ADK。它的 agent 运行时最完整（中断恢复、checkpoint、中间件、DeepAgent），组件生态大，在字节内部有大规模使用。代价是 v0，API 还在变；文档站点由飞书文档同步生成，细节以源码注释为准。
- **做的是“AI 功能”（总结、分类、抽取、带几个工具的问答），想要好用的开发工具和 trace**：Genkit。`Generate` + flow 很顺手，Dev UI 调试方便，多家模型接口统一。agent 相关的 API 还在预览。
- **只需要调一两个模型、几个工具**：可能两个都不需要，直接用厂商的 Go SDK 自己写循环（3.2 课那个循环几十行就够了）。这是 4.6 课要讨论的。
- **工具要给多个 agent、多种语言的客户端共用**：放进 MCP 服务端，用官方 SDK 写。框架选什么都可以接。
- 两个框架都想要的部分（比如 Eino 的编排 + Genkit 的 Dev UI）不要硬凑在一个服务里。它们各有一套模型、工具、消息类型，混用就要写双向转换。

## 8. 练习代码导读

`projects/27-go-ecosystem/` 里的文件：

| 文件 | 内容 |
|---|---|
| `eino_model.go` | `ScriptedModel`：按脚本回复，记录每次的输入和工具列表；`ChunkRunes` 控制流式分片大小。`CallTool` / `Say` 构造脚本消息 |
| `eino_graph.go` | `NewTriageGraph`：classify → 分支 → refund / bug / human，局部状态记录路径；`TriageTool` 把图包成工具 |
| `eino_agent.go` | `MemoryStore`；`NewEinoRunner`；`Collect` 把事件收集成 `RunResult`；`RefundTool` 中断恢复示例 |
| `genkit_part.go` | `DefineScriptedModel`；`DefineStockTool`（`tool.Fail` 与普通错误）；`DefineTransferTool`（可恢复工具）；`DefineAskFlow` |
| `mcp_bridge.go` | `NewDocsServer`（search_docs / broken / delete_doc）；`ConnectInMemory`；`MCPToEino`；`MCPToGenkit` |

测试按文件分组：`eino_test.go`（图、图工具、agent、流式、上限、未知工具、中断恢复，10 个）、`genkit_test.go`（flow、`tool.Fail`、普通错误、`maxTurns`、`Restart`、`Respond`，6 个）、`mcp_test.go`（协议版本、schema 推断、参数校验与错误、MRTR、接入 Eino、接入 Genkit，6 个）。

运行：

```
cd projects/27-go-ecosystem
make test
```

如果你的 Go 版本比 Eino 依赖的 sonic 支持的版本新，测试开头会打一行 `WARNING: sonic/ast only supports ...`，sonic 会退回 `encoding/json`，不影响结果。

## 9. 常见的坑

1. **假模型只实现了 `WithTools` 方法。** `ChatModelAgent` 通过调用选项传工具，你的模型适配器要从 `opts` 里读，否则模型看不到工具。
2. **流式事件没读完。** Eino 的 `MessageStream` 必须读到 `io.EOF` 并 `Close`，不读完会泄漏 goroutine。用 `adk.GetMessage` 省事。
3. **以为图外面用 `Invoke` 里面也能流式。** 不能。要流式就整张图用 `Stream`。
4. **中断状态里的类型没注册 gob。** 恢复时报反序列化错误。`schema.RegisterName` 的名字以后也不能改。
5. **恢复时“不是我”的工具没有再中断。** 并行中断时其余的状态会丢。
6. **中断前做了副作用。** 三个库恢复时都会重新执行，退款、转账、删除要放在审批通过之后。
7. **把 Genkit 中断数据写成字符串。** `tool.Interrupt(ctx, "请确认")` 不会中断，会变成工具调用失败。用结构体或 map。
8. **以为 Genkit 帮你存了中断状态。** 没有，`resp.History()` 要你自己存。
9. **两个框架的 `maxTurns` / `MaxIterations` 按同一个意思理解。** 一个数工具轮，一个数模型调用。
10. **MCP 工具错误当协议错误处理。** handler 返回 error 时 `CallTool` 的 err 是 nil，要看 `res.IsError`。反过来，适配器把 `IsError` 当成运行失败，模型就失去了纠正的机会。
11. **同一个结构体同时给 MCP SDK 和 Eino 推断 schema。** 两边的 `jsonschema` 标签语法不同，一边的描述会丢或者解析错。各写各的类型，或者像练习那样只在 MCP 一侧定义、用 JSON 转换。
12. **以为框架自带的 MCP 集成支持最新协议。** 先看它依赖哪个 MCP 库、哪个版本。
13. **以为 Genkit 会把参数错误交给模型。** 参数不符合 schema 时整个 `Generate` 失败。
14. **在 Genkit 上重复注册同名工具。** 会 panic（`action "/tool.v2/check_stock" is already registered`），每次请求都 `DefineTool` 的写法不行，工具要在启动时注册一次。

## 10. 练习

### 练习 1：读懂并跑通（必做）

1. `make test`，22 个测试全部通过。
2. 读 `TestRefundResumeInNewRunnerAndReject`，回答：第二个 Runner 用的模型脚本只有一条消息，为什么够用？
3. 把 `TestMaxIterationsStopsLoop` 的 `MaxIterations` 改成 3，`TestGenkitMaxTurns` 的 `WithMaxTurns` 改成 3，先预测两边模型各被调用几次，再改断言跑一遍。
4. 在 `NewEinoRunner` 里把 `ExecuteSequentially` 设成 true，写一个测试：模型一次要求两个工具调用，用两个会 `time.Sleep(50 * time.Millisecond)` 的工具，比较并行和顺序时 `Collect` 的耗时。

### 练习 2：把 MCP 的输入请求变成 Eino 中断（必做）

现在 MRTR 由 MCP 客户端的 `ElicitationHandler` 当场回答，这要求回答的人此刻就在线。改成走 Eino 的中断恢复：

- 客户端关闭自动 MRTR：`&mcp.ClientOptions{MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}}`。
- 写 `MCPToEinoInterrupting(ctx, cs)`，返回的工具在 `res.NeedsInput()` 为 true 时调用 `tool.StatefulInterrupt`，保存工具名、原始参数、`RequestState`，以及每个输入请求要问的话（ElicitParams 的 `Message`）。
- 恢复时，`ResumeWithParams` 传进来的数据类型是 `map[string]*mcp.ElicitResult`（key 是输入请求的 key）。工具用保存的参数和 `RequestState`，带上 `InputResponses` 重新调用 `CallTool`。
- 不是恢复目标时原样再中断。
- 测试：模型调用 `delete_doc`，运行中断，中断信息里能看到“确认删除《旧周报》？”；用 `{"confirm": {Action: "accept", Content: {"ok": true}}}` 恢复，工具输出是“已删除《旧周报》”，最终回答来自脚本。

提示：中断状态是自定义类型，记得注册 gob。

### 练习 3：Genkit 版（选做）

同样的事情用 Genkit 做：写 `MCPToGenkitResumable(ctx, g, cs)`，把每个 MCP 工具注册成 `DefineResumableTool`，输入类型 `any`、输出类型 `any`、恢复类型自己定义。需要输入时用 `tool.Interrupt` 暂停，中断数据里放要问的话和 `RequestState`。调用方用 `Interrupted` 认领，读中断数据，`Restart` 时把答复和 `RequestState` 一起传回去。

想一想：Eino 版的 `RequestState` 存在 checkpoint 里，Genkit 版存在哪里？哪种更适合“审批人明天才回复”？

## 11. 自测题

1. Eino 的 `ChatModel.BindTools` 和 `ToolCallingChatModel.WithTools` 有什么区别？为什么推荐后者？
2. 一张 Eino 图用 `Invoke` 调用，里面的 ChatModel 节点是流式调用的吗？
3. Eino 文档认为 Graph 和 Agent 最好的结合方式是什么？为什么不推荐把 agent 当成 graph 的一个节点？
4. `ChatModelAgent` 的 `MaxIterations` 默认是多少？数的是什么？
5. 模型调用了一个不存在的工具，`ChatModelAgent` 默认怎么处理？怎么改？
6. Eino 工具恢复时，`GetInterruptState` 和 `GetResumeContext` 分别回答什么问题？“不是恢复目标”时为什么要再中断一次？
7. 跨机器恢复一个中断的 Eino agent，需要两台机器共享什么？
8. Genkit 的 `tool.Fail` 返回的错误和普通错误有什么不同？
9. `ai.WithMaxTurns(2)` 时模型最多被调用几次？
10. Genkit 可恢复工具的 `Restart` 和 `Respond` 有什么区别？
11. 为什么 `tool.Interrupt(ctx, "请确认")` 不会中断？
12. MCP Go SDK 的 `mcp.AddTool` 里，handler 返回 error 时客户端看到什么？参数缺少必填字段呢？工具名不存在呢？
13. MCP Go SDK 的 `jsonschema` 标签和 Eino 的有什么不同？
14. MRTR 里 `RequestState` 的作用是什么？
15. 用 Genkit 的 `plugins/mcp` 能拿到 2026-07-28 协议的特性吗？怎么判断？

## 12. 自测题答案

1. `BindTools` 修改模型实例自己，多个 goroutine 共用一个实例、各自绑不同工具时会互相覆盖；`WithTools` 返回新实例，原实例不变，并发安全。
2. 不是。整张图用 Invoke 调用时，所有组件都按 Invoke 调用；要流式，整张图用 Stream 调用，这时内部组件都按 Transform 调用。
3. 把 graph 包成 agent 的工具。graph 的输入是结构化的、执行一次就结束、对调用方是同步的，和工具的特征一致。agent 依赖自己的对话历史，输出是一串异步的过程事件，放进 graph 当节点时下游很难使用它的输出。graph 里需要智能时放 LLM 节点就够了。
4. 默认 20，数的是模型生成的次数。超过后运行以错误结束。
5. 默认整次运行失败。配 `ToolsNodeConfig.UnknownToolsHandler`，返回一段文字（比如可用的工具列表）作为工具结果交给模型。
6. `GetInterruptState`：我之前中断过吗，存的状态是什么。`GetResumeContext`：这次恢复是冲着我来的吗，人给了什么数据。同一轮里多个工具并行中断时，恢复其中一个，其余的也会被重新执行；它们如果不再中断，原来保存的状态就没了，等到真正恢复它们时拿不到。
7. 同一个 `CheckPointStore`（比如同一个 Redis），以及 checkpoint ID 和中断 ID。模型、工具都可以是新实例，对话历史在 checkpoint 里。中断状态的类型要在两边用同样的名字注册 gob。
8. `tool.Fail` 的错误作为 `{"error": "..."}` 交给模型，工具循环继续；普通错误让 `Generate` 直接返回错误。
9. 3 次。前两次的工具请求都执行，第三次模型还要工具时报 `ErrMaxTurnsExceeded`。
10. `Restart` 重新执行工具函数，把恢复数据作为第三个参数传进去；`Respond` 不执行工具，直接用你给的值作为工具输出。
11. 中断数据必须能序列化成 JSON 对象（结构体或字符串 key 的 map），字符串是 JSON 标量。这时 `tool.Interrupt` 返回一个普通错误，工具调用失败。这样规定是为了让中断数据在进程内和通过网络传给远端调用方时形状一致。
12. handler 返回 error：`CallTool` 的 err 为 nil，结果的 `IsError` 为 true，错误信息在 `Content` 里。缺少必填字段：同样是 `IsError` 结果，而且 handler 根本不会被调用。工具名不存在：协议错误，`CallTool` 返回非 nil 的 err。
13. MCP SDK（`google/jsonschema-go`）的标签整段就是描述：`jsonschema:"要搜索的关键词"`。Eino（`eino-contrib/jsonschema`）用 `key=value`：`jsonschema:"description=用户的原话"`。
14. 服务端返回输入请求时附带的不透明字符串，客户端重试时原样带回。服务端可以把进度编码进去，第二次调用时据此继续，不需要按会话保存状态，这符合 2026-07-28 规范的无状态设计。
15. 不一定。Genkit Go v1.14.0 的 `plugins/mcp` 依赖 `mark3labs/mcp-go` v0.32.0，要看这个库的这个版本支持哪个协议版本。判断方法是看框架的 go.mod 里 MCP 库的名字和版本，再查那个版本支持的规范。

## 13. 参考答案

### 练习 1

2. 对话历史（user 消息、带工具调用的 assistant 消息）都在 checkpoint 里，恢复后工具执行完，Runner 只需要再调一次模型拿最终回答。新模型的脚本只要覆盖恢复之后的调用。
3. `MaxIterations=3`：模型被调用 3 次后报错。`WithMaxTurns(3)`：模型被调用 4 次，第 4 次还要工具时报错。
4. 一个思路：

```go
type sleepTool struct{ name string }

func (s sleepTool) Info(context.Context) (*schema.ToolInfo, error) {
    return &schema.ToolInfo{Name: s.name, Desc: "sleep"}, nil
}
func (s sleepTool) InvokableRun(context.Context, string, ...tool.Option) (string, error) {
    time.Sleep(50 * time.Millisecond)
    return "ok", nil
}

// 模型脚本：一条消息里两个工具调用，然后 Say("done")
msg := schema.AssistantMessage("", []schema.ToolCall{
    {ID: "1", Function: schema.FunctionCall{Name: "a", Arguments: "{}"}},
    {ID: "2", Function: schema.FunctionCall{Name: "b", Arguments: "{}"}},
})
// 并行时 Collect 耗时约 50ms，ExecuteSequentially=true 时约 100ms。
// 断言用宽松的阈值（比如并行 < 90ms、顺序 >= 100ms），避免 CI 机器慢导致误报。
```

`AgentOptions` 需要加一个 `Sequential bool` 字段，传给 `ToolsNodeConfig.ExecuteSequentially`。

### 练习 2

```go
// MCPPending 是中断时保存的状态，也是交给人看的信息。
type MCPPending struct {
    Tool         string
    Args         string
    RequestState string
    Questions    map[string]string // 输入请求的 key → 要问用户的话
}

func init() { schema.RegisterName[*MCPPending]("goeco_mcp_pending") }

type interruptingMCPTool struct{ einoMCPTool }

func MCPToEinoInterrupting(ctx context.Context, cs *mcp.ClientSession) ([]tool.BaseTool, error) {
    base, err := MCPToEino(ctx, cs)
    if err != nil {
        return nil, err
    }
    out := make([]tool.BaseTool, len(base))
    for i, b := range base {
        out[i] = &interruptingMCPTool{*b.(*einoMCPTool)}
    }
    return out, nil
}

func (t *interruptingMCPTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
    params := &mcp.CallToolParams{Name: t.info.Name, Arguments: json.RawMessage(args)}

    if was, _, p := tool.GetInterruptState[*MCPPending](ctx); was {
        isTarget, hasData, answers := tool.GetResumeContext[map[string]*mcp.ElicitResult](ctx)
        if !isTarget || !hasData {
            return "", tool.StatefulInterrupt(ctx, p, p)
        }
        params.Arguments = json.RawMessage(p.Args)
        params.RequestState = p.RequestState
        params.InputResponses = mcp.InputResponseMap{}
        for k, v := range answers {
            params.InputResponses[k] = v
        }
    }

    res, err := t.cs.CallTool(ctx, params)
    if err != nil {
        return "", err
    }
    if res.NeedsInput() {
        p := &MCPPending{Tool: t.info.Name, Args: args, RequestState: res.RequestState, Questions: map[string]string{}}
        for k, r := range res.InputRequests {
            if e, ok := r.(*mcp.ElicitParams); ok {
                p.Questions[k] = e.Message
            }
        }
        return "", tool.StatefulInterrupt(ctx, p, p)
    }
    if res.IsError {
        return "工具出错：" + resultText(res), nil
    }
    return resultText(res), nil
}
```

测试：

```go
func TestMCPInputBecomesEinoInterrupt(t *testing.T) {
    ctx := context.Background()
    _, cs := connect(t, &mcp.ClientOptions{MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}})
    tools, err := MCPToEinoInterrupting(ctx, cs)
    if err != nil {
        t.Fatal(err)
    }
    store := NewMemoryStore()
    m := NewScriptedModel(CallTool("c1", "delete_doc", `{"title":"旧周报"}`), Say("删好了"))
    r, _ := NewEinoRunner(ctx, m, store, AgentOptions{}, tools...)

    res, err := Collect(r.Query(ctx, "删掉旧周报", adk.WithCheckPointID("d1")))
    if err != nil || len(res.Interrupts) != 1 {
        t.Fatalf("err %v interrupts %d", err, len(res.Interrupts))
    }
    p := res.Interrupts[0].Info.(*MCPPending)
    if p.Questions["confirm"] != "确认删除《旧周报》？" {
        t.Fatalf("%+v", p)
    }

    iter, err := r.ResumeWithParams(ctx, "d1", &adk.ResumeParams{Targets: map[string]any{
        res.Interrupts[0].ID: map[string]*mcp.ElicitResult{
            "confirm": {Action: "accept", Content: map[string]any{"ok": true}},
        },
    }})
    if err != nil {
        t.Fatal(err)
    }
    res, err = Collect(iter)
    if err != nil {
        t.Fatal(err)
    }
    if res.ToolOutputs["delete_doc"] != "已删除《旧周报》" || res.Final != "删好了" {
        t.Fatalf("%v %q", res.ToolOutputs, res.Final)
    }
}
```

两个细节：恢复时用的是保存的 `p.Args`，不是这次传进来的 `args`（通常相同，但以 checkpoint 为准更稳）；`MCPPending` 里只放能被 gob 序列化的字段，`ElicitParams` 里的 schema 不要整个存进去，只存要问的话。这段代码在写课时的沙箱里跑通过。

### 练习 3

```go
type MCPAnswer struct {
    RequestState string                       `json:"requestState"`
    Answers      map[string]*mcp.ElicitResult `json:"answers"`
}

func MCPToGenkitResumable(ctx context.Context, g *genkit.Genkit, cs *mcp.ClientSession) ([]*ai.ResumableToolAction[any, any, MCPAnswer], error) {
    var out []*ai.ResumableToolAction[any, any, MCPAnswer]
    for t, err := range cs.Tools(ctx, nil) {
        if err != nil {
            return nil, err
        }
        var in map[string]any
        raw, _ := json.Marshal(t.InputSchema)
        _ = json.Unmarshal(raw, &in)
        name := t.Name
        out = append(out, genkit.DefineResumableTool(g, "mcp_"+name, t.Description,
            func(ctx context.Context, input any, ans *MCPAnswer) (any, error) {
                params := &mcp.CallToolParams{Name: name, Arguments: input}
                if ans != nil {
                    params.RequestState = ans.RequestState
                    params.InputResponses = mcp.InputResponseMap{}
                    for k, v := range ans.Answers {
                        params.InputResponses[k] = v
                    }
                }
                res, err := cs.CallTool(ctx, params)
                if err != nil {
                    return nil, err
                }
                if res.NeedsInput() {
                    q := map[string]any{"requestState": res.RequestState}
                    for k, r := range res.InputRequests {
                        if e, ok := r.(*mcp.ElicitParams); ok {
                            q[k] = e.Message
                        }
                    }
                    return nil, gktool.Interrupt(ctx, q)
                }
                if res.IsError {
                    return nil, gktool.Fail(ctx, errors.New(resultText(res)))
                }
                return resultText(res), nil
            }, ai.WithInputSchema(in)))
    }
    return out, nil
}
```

调用方：

```go
resp, _ := genkit.Generate(ctx, g, ai.WithModel(m), ai.WithPrompt("删"), ai.WithTools(refs...))
part := resp.Interrupts()[0]
call, _ := del.Interrupted(part)                 // del 是 mcp_delete_doc 那个工具
q, _ := ai.InterruptAs[map[string]any](part)     // q["confirm"] 是要问的话
rs, _ := q["requestState"].(string)
resp, _ = genkit.Generate(ctx, g, ai.WithModel(m), ai.WithMessages(resp.History()...), ai.WithTools(refs...),
    ai.WithResume(call.Restart(MCPAnswer{RequestState: rs, Answers: map[string]*mcp.ElicitResult{
        "confirm": {Action: "accept", Content: map[string]any{"ok": true}},
    }})))
```

Genkit 版的 `RequestState` 在中断数据里，中断数据在 `resp.History()` 的最后一条消息上，所以它和对话一起由你保存。审批人明天才回复的话，两种都行，区别在于谁负责存：Eino 由 Runner 写进 `CheckPointStore`，你只需要提供一个持久化的 store；Genkit 要你自己把 `History()` 存进数据库，第二天读出来。Eino 省事，Genkit 更透明，存储格式完全在你手里。这段代码也在写课时的沙箱里跑通过。

## 完成标准

- `make test` 22 个测试全部通过，`go vet` 干净。
- 能说清楚 Eino 的 `MaxIterations` 和 Genkit 的 `maxTurns` 各数的是什么，设成同一个数时模型调用次数差多少。
- 能写一个 Eino 工具，正确处理中断、恢复、“不是恢复目标时再中断”，并说明跨实例恢复需要共享什么。
- 能说清楚 MCP 工具错误和协议错误的区别，以及在 Eino、Genkit 里分别怎么交给模型。
- 练习 2 完成：MCP 的输入请求变成 Eino 中断，恢复后工具拿到答复并完成调用。
- 能根据项目情况在 Eino、Genkit、不用框架之间做出选择，并说出理由。
