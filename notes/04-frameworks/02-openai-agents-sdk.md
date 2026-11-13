# 4.2 OpenAI Agents SDK：Runner 循环、handoff、guardrail 与 RunState

> 第 4 部分 · 第 2 课 ｜ 预计 4–5 小时 ｜ 前置：3.2 Agent Loop、3.3 多智能体、3.4 长任务 Agent、4.1 LangGraph
>
> 配套练习：`projects/25-agents-sdk/`（纯 Go 标准库，18 个测试）。用 Go 重写 OpenAI Agents SDK 运行时的核心：Runner 循环与“什么算最终输出”；工具并发执行和限流，结果按调用顺序写回；handoff 也是工具（`transfer_to_<agent>`）、`on_handoff`、`input_filter`、多个 handoff 只取第一个；agent-as-tool；输入 guardrail 的阻塞和并行两种模式，以及“只看第一个 agent / 只看最后一个 agent”的边界；输出 tripwire 不把被拒绝的输出写进 session；工具 guardrail；`needs_approval`、回调 fail closed、`RunState` 序列化与恢复、按 agent 区分的 sticky 决定、部分决定；`tool_use_behavior`、`max_turns` 与错误处理器、找不到工具的两种行为；Session。

## 读前说明

上一课的 LangGraph 把一切都摊开成图：节点、边、通道、checkpoint，你自己决定控制流。OpenAI Agents SDK 走的是另一条路：**控制流只有一个固定的循环**，你配置的是 agent，以及循环里几个可插拔的点。

两者对照：

| 问题 | LangGraph | OpenAI Agents SDK |
|---|---|---|
| 控制流 | 你画的图 | 固定的 Runner 循环 |
| 换一个“角色”做事 | 条件边跳到另一个节点 | handoff：模型调用 `transfer_to_xxx` 工具 |
| 一次运行的状态 | 通道 + reducer，每步一个 checkpoint | 一串 item（消息、函数调用、调用结果）+ 当前 agent |
| 人工审批 | `interrupt()`，恢复时节点从头重跑 | 工具声明 `needs_approval`，run 停下来，`RunState` 序列化，决定后从状态继续 |
| 对话记忆 | checkpointer + thread_id | Session，或服务端的 `conversation_id` / `previous_response_id` |
| 防护 | 自己写节点 | 输入、输出、工具三种 guardrail |

本课依据 OpenAI Agents SDK 的 Python 官方文档（openai.github.io/openai-agents-python，2026 年 11 月；PyPI 最新版 0.24.0，2026-10-15 发布）。JS 版的概念一致。

先想几个问题：

- 一次 `Runner.run()` 里可能调用模型很多次、换好几个 agent，它对应的是用户的“一轮”还是“多轮”？
- 模型同时发出一个 handoff 和两个工具调用，SDK 怎么处理？
- 输入 guardrail 和模型并行跑，guardrail 判定违规时，模型的 token 还要付钱吗？
- 一个调用被批准了，进程在工具执行完、状态保存前崩溃，恢复后会发生什么？

---

## 1. 四个基本概念

- **Agent**：一份配置。名字、指令（system prompt，可以是函数）、模型、工具、handoff、guardrail、`output_type`（结构化输出）、`tool_use_behavior` 等。`clone()` 可以复制后改几个字段。Agent 不是进程，同一个 agent 可以被很多 run 同时使用。
- **Runner**：执行循环。`Runner.run()`（异步）、`Runner.run_sync()`（同步包装）、`Runner.run_streamed()`（流式）。
- **Item**：Responses API 格式的输入输出项：消息、函数调用、函数调用结果、推理项等。整个 run 的历史就是一串 item。
- **RunResult**：`final_output`、`last_agent`、`new_items`、`interruptions`、`to_input_list()`、`to_state()` 等。

一次 `Runner.run()` 代表聊天里的**一个逻辑轮次**：用户说一句话，可能有多次模型调用、多次工具调用、几次 handoff，最后产生一个输出。用户接着问，就再调一次 `run()`。

---

## 2. Runner 循环

官方文档描述的循环：

1. 用当前 agent 和当前输入调用模型；
2. 模型产生输出：
    - 如果被判定为**最终输出**，循环结束，返回结果；
    - 如果模型请求 **handoff**，更新当前 agent 和输入，重新进入循环；
    - 如果模型产生了**工具调用**，执行工具、追加结果，重新进入循环；
3. 超过 `max_turns` 抛 `MaxTurnsExceeded`。`max_turns=None` 关闭上限。

“最终输出”的规则：**产生了符合期望类型的文本输出，并且没有工具调用**。

`max_turns` 默认是 10（源码里 `DEFAULT_MAX_TURNS = 10`），按模型调用次数计。这就是 3.2 课 Agent Loop 的步数上限。

Go 练习的 `loop` 就是这个循环，每一圈做三件事：

```go
for {
	if !st.Processed {                  // 1. 处理上一次模型响应
		final, paused, err := r.processTurn(...)
		...                             // 暂停 → 返回 interruptions；最终输出 → 输出 guardrail、写 session、返回
		continue                        // 执行了工具或 handoff → 再调模型
	}
	if maxTurns > 0 && st.Turn >= maxTurns { ... }   // 2. 轮数上限
	items, err = r.Model.Respond(ctx, req)           // 3. 调模型，把响应追加到 Items
	st.Turn++; st.Processed = false
}
```

把“处理响应”和“调模型”拆开，并用 `Processed` 标记，是为了审批恢复：恢复时上一次响应还没处理完，直接从处理那一步继续，**不会重新调用模型**。

### 2.1 一轮里的处理顺序

`processTurn` 对一次模型响应按这个顺序处理：

1. 逐个看函数调用：找不到工具？需要审批但没有决定？被拒绝？可以执行？
2. 可以执行的工具**并发**执行，结果**按调用顺序**写回（`TestToolLoopConcurrencyAndOrder`：四个工具耗时不同、最多两个并发，完成顺序和调用顺序不一样，写回顺序仍是 t1–t4）；
3. 有未决定的审批 → 暂停；
4. `tool_use_behavior` 要求工具结果直接当最终输出 → 结束；
5. 有 handoff → 只采用第一个，切换 agent；
6. 有工具调用 → 再调一次模型；
7. 什么调用都没有 → 最后一条 assistant 消息就是最终输出。

### 2.2 工具执行的配置

- `ModelSettings.parallel_tool_calls`：模型是否**允许**在一次响应里发出多个工具调用（提供商侧）；
- `RunConfig.tool_execution.max_function_tool_concurrency`：模型发出之后，SDK **同时执行**几个本地函数工具。默认 `None` 表示全部同时开始；
- `tool_not_found_behavior`：模型调用了当前 agent 没有的工具，默认抛 `ModelBehaviorError`；设成 `"return_error_to_model"` 则把错误作为工具结果交给模型，让它重新选；
- 工具函数自己抛异常：默认把错误信息作为工具结果交给模型（练习 `TestToolErrorBecomesOutput`）；
- `tool_error_formatter`：自定义交给模型的错误文字，比如审批被拒、找不到工具。

### 2.3 tool_use_behavior

- `"run_llm_again"`（默认）：执行工具，把结果交给模型，由模型给出最终回答；
- `"stop_on_first_tool"`：第一个工具调用的结果直接当最终输出，不再调模型；
- `StopAtTools(stop_at_tool_names=[...])`：调用了其中任何一个工具就停，用它的结果当最终输出；
- 也可以传一个函数自己决定。

另外 `reset_tool_choice` 默认 `True`：强制模型调用工具（`tool_choice`）之后会重置，避免每轮都被迫调用工具、陷入死循环。

### 2.4 错误处理器

`error_handlers` 按错误类型返回一个受控的最终输出，而不是让 run 以异常结束。支持三个键：`"max_turns"`、`"model_refusal"`、`"invalid_final_output"`。`RunErrorHandlerResult.include_in_history` 默认 `True`，会把兜底输出写进历史和 session；不想写就设 `False`。处理器**不会重试模型调用，也不会重放工具副作用**。

练习的 `OnMaxTurns` 对应 `"max_turns"` 处理器（`TestMaxTurnsAndHandler`）。

---

## 3. 多 agent：handoff 与 agent-as-tool

### 3.1 handoff 就是工具

官方文档原话：**handoff 对 LLM 来说就是工具**。交给 `Refund Agent` 的 handoff，工具名是 `transfer_to_refund_agent`。

```python
from agents import Agent, handoff

billing_agent = Agent(name="Billing agent")
refund_agent = Agent(name="Refund agent")
triage_agent = Agent(name="Triage agent", handoffs=[billing_agent, handoff(refund_agent)])
```

`handoff()` 可以定制：

| 参数 | 作用 |
|---|---|
| `tool_name_override` / `tool_description_override` | 改工具名和描述 |
| `on_handoff` | 转交时执行的回调，可以收到模型生成的参数 |
| `input_type` | handoff 工具调用的参数 schema（比如 `reason`、`priority`），解析后传给 `on_handoff` |
| `input_filter` | 改写下一个 agent 能看到的历史 |
| `is_enabled` | 布尔值或函数，运行时开关这个 handoff |

几个容易误解的点：

- `input_type` 是**模型在转交时决定的元数据**，不替换下一个 agent 的主输入，也不能用来在多个目的地之间选择。多个专家就注册多个 handoff，让模型选；
- `is_enabled` 在准备工具列表时求值，那时还没有参数，所以它**不能**基于参数做授权。授权要放在 `on_handoff` 开头、任何副作用之前，失败就抛异常，SDK 就不会转交（练习 `TestOnHandoffErrorBlocksTransfer`）；
- 工具 guardrail 对 handoff 不生效，handoff 走自己的管道。

练习里，模型在同一次响应里发了两个 handoff，只有第一个生效，第二个得到“忽略”的结果（`TestHandoffSwitchesAgentAndFilters`）。

### 3.2 input_filter：新 agent 看到什么

默认情况下，handoff 后新 agent **看到之前的全部对话历史**。`input_filter` 接收 `HandoffInputData`，返回新的 `HandoffInputData`：

- `input_history`：`Runner.run()` 开始前的输入历史；
- `pre_handoff_items`：发生 handoff 的那一轮之前产生的项；
- `new_items`：这一轮产生的项，包括 handoff 调用和结果；
- `input_items`：可选，代替 `new_items` 交给下一个 agent，这样可以过滤模型输入，同时让 session 历史保持完整；
- `run_context`。

常用的过滤器在 `agents.extensions.handoff_filters` 里，比如 `remove_all_tools` 去掉所有工具相关的项。注意它只去掉**结构化的工具项**，已经被抄进普通消息里的工具参数和结果不会被脱敏。

还有一个可选的 beta 功能 `nest_handoff_history`（默认关闭），把可以摘要的历史压成有序的摘要段。它改变的是历史的表示方式，**不做脱敏**。使用服务端会话（`conversation_id`、`previous_response_id`）时不支持 handoff 的 input filter。

练习里 `doHandoff` 用 `Prefix` 和 `ModelFrom` 实现：过滤后的历史成为新的前缀，模型以后只看到前缀加上之后的项；`Items` 本身完整保留，结果和 session 不受影响。这对应 SDK 里“过滤模型输入，同时保持 session 历史完整”的设计。

### 3.3 agent-as-tool：控制权不交出去

另一种多 agent 方式是把 agent 包成工具：

```python
orchestrator = Agent(
    name="Orchestrator",
    tools=[
        booking_agent.as_tool(tool_name="booking_expert", tool_description="Handles booking questions."),
        refund_agent.as_tool(tool_name="refund_expert", tool_description="Handles refund questions."),
    ],
)
```

和 handoff 的区别：

| | handoff | `Agent.as_tool()` |
|---|---|---|
| 控制权 | 交给新 agent，由它产出最终输出 | 留在调用方，嵌套 agent 的输出只是工具结果 |
| 新 agent 看到的历史 | 默认全部对话 | 只有工具参数 |
| 适合 | 分诊后由专家直接面对用户 | 管理者统一和用户对话，专家只提供能力 |

这正是 3.3 课的“交接 vs 委派”。练习 `TestAgentAsToolKeepsControl` 验证嵌套 agent 只看到 1 条输入，`LastAgent` 仍是外层的“经理”。

---

## 4. guardrail

### 4.1 三种 guardrail，三个边界

| 种类 | 什么时候跑 | 只对谁生效 |
|---|---|---|
| 输入 guardrail | run 开始时，检查用户输入 | **run 的第一个 agent** |
| 输出 guardrail | 产生最终输出后 | **产生最终输出的那个 agent** |
| 工具 guardrail | 每次调用被保护的函数工具前后 | 那个工具，不管哪个 agent 调用 |

所以在有 handoff、有管理者和专家的工作流里，handoff 后面那个 agent 的输入 guardrail **不会跑**，前面那个 agent 的输出 guardrail 也不会跑（练习 `TestGuardrailBoundaries`）。需要每次工具调用前后都检查，就用工具 guardrail。

guardrail 函数返回 `GuardrailFunctionOutput`，`tripwire_triggered=True` 就抛 `InputGuardrailTripwireTriggered` / `OutputGuardrailTripwireTriggered`，运行停止。guardrail 可以内部跑一个便宜的 agent 做分类，也可以直接调外部分类器。

### 4.2 输入 guardrail 的两种执行模式

- **并行**（默认，`run_in_parallel=True`）：guardrail 和 agent 同时开始，延迟最低。但 tripwire 触发时，agent **可能已经消耗了 token、执行了工具**；
- **阻塞**（`run_in_parallel=False`）：guardrail 先跑完，agent 才开始。tripwire 触发时 agent 根本不执行，不花 token、没有工具副作用。

练习 `TestInputGuardrailBlockingVsParallel`：同样是违规输入，阻塞模式下模型调用 0 次，并行模式下 1 次。练习做了一个简化：并行模式下等 guardrail 结束再执行工具。真实 SDK 不保证这一点，有副作用的工具要用阻塞模式。

输出 guardrail 总是在 agent 完成之后跑，没有并行选项。

### 4.3 输出 tripwire 和 session

输出 guardrail 判定违规时，SDK 让 session **保存已经完成的工具调用和结果，但不保存被拒绝的候选输出**。guardrail 函数自己抛异常时，结论算“未知”，这一轮完成的项照常保存再抛异常。

如果被拒绝的是一个“工具结果直接当最终输出”的情况（`stop_on_first_tool`），工具已经执行过了，SDK 会把保存下来的工具结果替换成占位文字 `"Output withheld by an output guardrail."`，原始内容不进 session、`RunState` 和流式结果状态。可以用 `RunConfig.output_guardrail_blocked_message` 改占位文字，但它会被保存和重放，不要放敏感信息。

练习的 `finish` 实现了前一条：tripwire 时把最后那条 assistant 消息去掉再写 session。

### 4.4 工具 guardrail

工具 guardrail 挂在 `FunctionTool` 上，每次调用都跑：

- **输入**：执行前运行，可以 `allow`、`reject_content`（不执行工具，用一段话代替结果交给模型）、或触发 tripwire；
- **输出**：执行后运行，可以替换输出或触发 tripwire；
- 工具需要审批时，输入 guardrail **默认在审批通过后、执行前**跑。设 `ToolExecutionConfig(pre_approval_tool_input_guardrails=True)` 可以在发出审批请求前先跑一次，通过的调用批准后还会再检查一次（时效性检查需要重新验证）；
- 只适用于函数工具。托管工具（web search、file search、hosted MCP、code interpreter 等）、内置执行工具（computer、shell、apply_patch）和 handoff 都不走这条管道；本地 MCP server 可以给它暴露的所有工具统一配置。

练习 `TestToolGuardrails` 覆盖了三种结果，并验证审批通过之前输入 guardrail 不运行。

---

## 5. 人工审批与 RunState

这是本课和上一课最值得对照的部分。

### 5.1 用法

```python
@tool(needs_approval=True)
async def cancel_order(order_id: int) -> str:
    return f"Cancelled order {order_id}"

async def requires_review(_ctx, params, _call_id) -> bool:
    return "refund" in params.get("subject", "").lower()

@tool(needs_approval=requires_review)
async def send_email(subject: str, body: str) -> str: ...

result = await Runner.run(agent, "取消订单 7")
while result.interruptions:
    state = result.to_state()
    store.save(state.to_string())                 # 可以存库，换进程恢复
    state = await RunState.from_json(agent, json.loads(store.load()))
    for item in result.interruptions:
        if ask_human(item.name, item.arguments):
            state.approve(item)
        else:
            state.reject(item, rejection_message="主管拒绝：订单已发货")
    result = await Runner.run(agent, state)       # 传原来的顶层 agent
```

`needs_approval` 可以用在 `function_tool`、`Agent.as_tool`、`ShellTool`、`ApplyPatchTool` 上；本地 MCP server 用 `require_approval`，托管 MCP 用 `tool_config={"require_approval": "always"}`。shell、apply_patch 和托管 MCP 还可以给一个回调，在代码里直接批准或拒绝，不暂停。

### 5.2 流程

1. 模型发出工具调用，runner 评估审批规则；
2. 这个调用已经有决定（存在 `RunContextWrapper` 里）→ 直接继续；
3. 需要审批但没有决定 → 暂停，`result.interruptions` 里是 `ToolApprovalItem`（agent 名、工具名、参数）。**handoff 之后的 agent、嵌套的 agent-as-tool 里发出的审批，也都冒到外层 run 上**；
4. `result.to_state()` → `state.approve(...)` / `state.reject(...)` → `Runner.run(原顶层 agent, state)`；
5. 恢复后从停下的地方继续，遇到新的审批再进入这个流程。

和 LangGraph 的根本区别：LangGraph 的 interrupt 在**节点内部**暂停，恢复时节点从头重跑，所以有“之前的副作用要幂等”“调用顺序不能变”这些规则。Agents SDK 在**工具执行之前**暂停，暂停的单位是“一个还没执行的工具调用”，恢复时直接决定执行或不执行这个调用，**不重新调用模型**（练习 `TestApprovalPauseSerializeResume`：模型只调用了 2 次，被批准的工具只执行了 1 次）。代价是灵活性：它只能在工具调用这个位置暂停，不能在任意代码行暂停。

### 5.3 几个细节

- **拒绝不是报错**：被拒绝的调用得到一段文字作为工具结果交给模型，模型可以换个方案。默认文字可以用 `tool_error_formatter` 全局改，`state.reject(..., rejection_message=...)` 单次改，单次优先（`TestRejectReturnsMessageToModel`）；
- **部分决定**：不必一次决定所有审批。已经决定的先继续，没决定的留在 `interruptions` 里再次暂停（`TestPartialDecisions`）；
- **sticky 决定**：`always_approve=True` / `always_reject=True` 让同一个工具以后自动按这个决定处理，并随 `RunState` 序列化保存。对函数工具，**按请求审批的 agent 区分**，另一个 agent 的同名工具不继承（`TestStickyApprovalScopedToAgent`）。但本地 `ShellTool` 和 `ApplyPatchTool` 例外：sticky 决定按工具类型和名字匹配，**不区分 agent**，handoff 后可能作用到权限不同的另一个 agent 的 shell 上。权限不同时用逐次决定；
- **审批回调 fail closed**：`needs_approval` 是函数时，如果参数缺失、为空、不是合法 JSON、是合法 JSON 但不是对象（如 `null` 或列表）、或含 `NaN` / `Infinity`，**不调用回调，直接要求人工审批**（`TestApprovalCallableFailsClosed`）。

### 5.4 长时间审批：状态放在服务端

`RunState` 用 `to_json()` / `to_string()` 存进数据库或队列，`from_json()` / `from_string()` 恢复。官方文档对安全讲得很重：

- `RunState` 里有审批决定、待执行的调用和参数。**反序列化不认证快照，也不认证提交者**。只能从受信任的存储加载；
- 给浏览器或手机做审批界面时，**完整快照留在服务端**，只把审批人有权看的工具信息和不透明的 ID 发给客户端。工具名和参数是不可信的展示内容，渲染 HTML 要转义；
- 决定到达服务端时：用应用的会话认证审批人（不信请求体里的身份）；授权他能处理这个 run 的这些调用（拿到 run ID 不等于有权限）；只接受 ID 和布尔值，从服务端快照里用 `state.get_interruptions()` 取待审批项，不接受客户端提交的调用、参数或状态；用存储层保证每个待审批请求**只被消费一次**（原子的、检查归属的状态转换），防止并发或重放提交把同一个快照恢复两次；
- 应用上下文（`RunContextWrapper.context`）会随状态一起序列化，**别放密钥**；
- 审批可能挂很久，同时存一个 agent 定义或 SDK 的版本号，恢复时按版本走对应的代码路径。

练习 2 就是实现这一套。

### 5.5 批准不等于恰好执行一次

官方文档专门有一节：**审批允许一个调用执行，但不保证只执行一次**。工具执行了外部动作，进程在保存新状态之前停了，从旧的“已批准”快照恢复就会再执行一次，比如付款已经完成，但快照里没有付款结果。

做法和 3.4 课一样：给每个逻辑动作一个稳定的幂等键，交给能原子去重的目的系统；重试时用同样的键和参数。拿不到目的系统的幂等保证时，重试前先和目的系统**权威地对账**：普通查询查不到不代表没执行过（读可能滞后，之前的请求可能还会完成）。结果不确定就记为未知，不要自动重放。练习 3 实现幂等键。

### 5.6 持久执行集成

需要跨越长时间等待、重试、进程重启的 run，SDK 文档列了几个集成：Dapr、Temporal、Restate、DBOS。它们把 run 放进持久工作流引擎里。Temporal 在模块 08 详细讲。

---

## 6. 对话状态

### 6.1 四种记忆策略

| 策略 | 状态在哪 | 下一轮传什么 |
|---|---|---|
| `result.to_input_list()` | 你的内存 | 这个列表 + 新的用户消息 |
| `session` | 你的存储，SDK 管理 | 同一个 session |
| `conversation_id` | OpenAI Conversations API | 同一个 ID + 新的用户消息 |
| `previous_response_id` | OpenAI Responses API | `result.last_response_id` + 新的用户消息 |

前两种是客户端管理，后两种是 OpenAI 服务端管理，只适用于 Responses API。**一个会话选一种**，混用会重复上下文。session 不能和服务端会话设置在同一次 run 里同时用。`conversation_id` 和 `previous_response_id` 互斥。审批暂停后从 `RunState` 恢复，SDK 会保留服务端会话设置，恢复的这一轮继续在同一个服务端会话里。

### 6.2 Session

```python
session = SQLiteSession("conversation_123")
await Runner.run(agent, "金门大桥在哪个城市？", session=session)
await Runner.run(agent, "在哪个州？", session=session)   # 自动带上历史
```

Session 在每次 run 之前取历史、之后存新项，不同 session ID 互相隔离。接口：`get_items(limit)`、`add_items(items)`、`pop_item()`（撤回最后一项，适合用户修改上一条消息）、`clear_session()`。现成实现有 SQLite、SQLAlchemy、Redis、MongoDB、Dapr、加密 session 等。`RunConfig.session_input_callback` 自定义历史和新输入怎么合并，`session_settings` 控制取多少条。

从 `RunState` 恢复时，传同一个 session（或指向同一存储、同一 ID 的 session），恢复的这一轮会追加到同一段历史。

练习的 `MemSession` 和 `TestSessionAccumulates`。练习做了一个简化：session 在 run 结束（或输出 tripwire）时一次写入，暂停期间的项保存在 `RunState` 里。

### 6.3 call_model_input_filter

`RunConfig.call_model_input_filter` 在每次模型调用之前拿到准备好的完整输入（指令 + 输入项，已经合并了 session 历史），返回新的 `ModelInputData`。用来裁剪历史、脱敏、注入额外指令。它收到的是副本，改了不影响原列表。这是 1.6 课上下文工程在框架里的挂钩点。

---

## 7. 其他能力（本课不展开）

- **Tracing**：默认开启，记录 agent、模型调用（generation）、工具（function）、handoff、guardrail 的 span，发到 OpenAI 的 trace 后台，可以接自己的处理器。`workflow_name`、`group_id`（把多次 run 串成一个会话）、`trace_include_sensitive_data`。练习的 `Result.Trace` 是一个极简版本。模块 07 详讲。
- **Streaming**：`run_streamed()` + `stream_events()`。审批在流式下一样工作：先把事件流读完，再看 `interruptions`。
- **Sandbox agents**：在沙箱里给 agent 文件系统、shell、记忆等能力（2.5 课的内容在 SDK 里的形态）。
- **Realtime / Voice agents**：语音。
- **MCP**：本地 stdio / SSE / Streamable HTTP server 和托管 MCP。
- **模型无关**：可以通过 LiteLLM、Any-LLM 适配器用其他提供商的模型。

---

## 8. 练习代码导读

| 文件 | 内容 | 对应 SDK |
|---|---|---|
| `types.go` | `Item`、`Model`、`Agent`、`FunctionTool`、`Handoff`（`transfer_to_<name>`）、`RemoveAllTools`、`AsTool`、三种 guardrail、错误类型、`parseArgs` | `Agent`、`function_tool`、`handoff()`、`handoff_filters`、`Agent.as_tool()`、`InputGuardrail` 等 |
| `state.go` | `Interruption`、`Decision`、`RunState`（`Approve`/`Reject`/`ToJSON`/`StateFromJSON`）、`Session`、`MemSession` | `ToolApprovalItem`、`RunState`、`Session`、`SQLiteSession` |
| `runner.go` | `Runner.Run`/`Resume`、`loop`、`processTurn`、`runTools`、`runOne`、`doHandoff`、`finish` | `Runner.run()`、`RunConfig`、`error_handlers` |
| `agents_test.go` | 18 个测试，用一个按调用次数返回脚本的假模型 | |

和 Python 版的差异：

- 恢复用单独的 `Resume` 方法，Python 是把 `RunState` 当输入传给同一个 `Runner.run()`；
- `RunState` 只存 agent 的**名字**，`StateFromJSON` 沿 handoff 图把 agent 找回来。agent 定义改了（改名、删掉）就恢复失败，这就是 5.4 节说要存版本号的原因；
- 没有实现流式、tracing 后台、结构化输出、嵌套 agent-as-tool 的审批冒泡、服务端会话。

---

## 9. 常见的坑

- **以为一次 `run()` 是一次模型调用**：它是一个逻辑轮次，可能有很多次调用。
- **`max_turns` 太小**：默认 10，工具多的任务会提前抛错；用 `error_handlers` 给兜底输出。
- **把 handoff 和 agent-as-tool 搞混**：前者交出控制权和完整历史，后者只交出一个子任务。
- **用 `is_enabled` 做基于参数的授权**：那时还没有参数，授权放在 `on_handoff` 开头。
- **以为 handoff 后专家 agent 的输入 guardrail 会跑**：只有第一个 agent 的会跑。
- **有副作用的工具配并行输入 guardrail**：tripwire 触发前工具可能已经执行了，用阻塞模式。
- **sticky 批准 shell**：shell 和 apply_patch 的 sticky 决定不区分 agent。
- **把 `RunState` 发给浏览器再收回来**：快照不认证，必须留在服务端。
- **把密钥放进 run context**：会随 `RunState` 一起序列化。
- **以为批准后工具恰好执行一次**：崩溃恢复可能重复，要幂等键或对账。
- **session 和 `previous_response_id` 混用**：重复上下文，SDK 也不允许同一次 run 里同时用。
- **以为 `remove_all_tools` 会脱敏**：只去掉结构化工具项。

---

## 10. 练习

### 练习 1：读懂并跑通（必做）

```bash
cd projects/25-agents-sdk && make test
```

回答：

1. `TestToolLoopConcurrencyAndOrder` 第二次模型调用看到的输入为什么是 9 条？如果把结果按完成顺序写回，会出什么问题？
2. `TestApprovalPauseSerializeResume` 里，恢复之后为什么模型没有被重新调用？在 `runner.go` 里指出是哪个字段保证的。如果没有这个字段，会发生什么？
3. `TestStickyApprovalScopedToAgent` 里，如果 sticky 的键只用工具名、不带 agent 名，测试结果会怎样？为什么 SDK 对函数工具要这样区分？
4. `TestGuardrailBoundaries` 里，B 的输入 guardrail 设成了“总是触发”，为什么 run 没有在 handoff 时被它拦下？如果确实需要在专家 agent 开始前做检查，有哪几种办法？

### 练习 2：服务端审批存储（必做）

实现 `ApprovalStore`，按 5.4 节的要求处理来自客户端的审批：

```go
type ApprovalStore interface {
	// Save 保存暂停的 run，返回 runID。快照、所属用户、agent 版本号都存在服务端。
	Save(owner string, version string, st *RunState) (runID string, err error)
	// Pending 返回给审批界面看的内容：只有不透明的决定 ID、工具名和参数，不含快照。
	Pending(user, runID string) ([]PendingView, error)
	// Decide 应用决定并“消费”这个快照：同一个快照只能被成功消费一次。
	Decide(user, runID string, decisions map[string]bool) (*RunState, error)
}
```

要求：

- `user` 由调用方从会话里认证得到，不是请求体里的字段；不是 owner 返回 `ErrForbidden`；
- `decisions` 的键必须正好覆盖所有待审批的决定 ID（和官方示例一样，要求一批全部决定），多了、少了、未知 ID 都拒绝；
- 版本号和当前代码的版本不一致时拒绝（`ErrVersionMismatch`），不要尝试恢复；
- 两个 goroutine 同时 `Decide` 同一个 run，恰好一个成功，另一个返回 `ErrAlreadyConsumed`。用 `-race` 跑；
- 测试：完整走一遍 暂停 → Save → Pending → Decide → Resume；越权、重放、并发、版本不符各一个测试。

### 练习 3：有副作用的工具和幂等键（选做）

给 `FunctionTool` 加 `Idempotent bool`。对这样的工具，runner 在执行前生成幂等键 `runID/callID`（`runID` 存在 `RunState` 里，第一次 `Run` 时生成），通过 `RunContext.IdempotencyKey()` 交给工具。写一个假的支付服务：同一个键只扣一次款，第二次返回第一次的结果。

测试：批准支付 → 工具执行成功 → **模拟崩溃**：丢掉恢复后得到的新状态，用批准后、执行前的旧快照再恢复一次。断言支付服务只扣了一次款，两次恢复交给模型的工具结果相同。

---

## 11. 自测题

1. Runner 循环有哪几步？什么算“最终输出”？`max_turns` 默认多少，按什么计数？
2. handoff 对模型来说是什么？`input_type`、`is_enabled`、`on_handoff` 分别做什么，授权应该放在哪？
3. handoff 和 agent-as-tool 有什么区别？各适合什么场景？
4. 输入、输出、工具三种 guardrail 分别在什么时候跑、对谁生效？
5. 输入 guardrail 并行和阻塞两种模式各有什么代价？
6. 输出 tripwire 触发时，session 里保存什么、不保存什么？
7. Agents SDK 的审批和 LangGraph 的 interrupt 在暂停位置和恢复方式上有什么不同？各自的代价是什么？
8. 什么是 sticky 决定？函数工具和 shell 工具在作用范围上有什么不同？
9. 为什么 `RunState` 不能交给客户端保管？服务端处理审批决定要做哪几件事？
10. 为什么“批准”不保证“恰好执行一次”？怎么办？

## 12. 自测题答案

1. 用当前 agent 和输入调用模型；如果是最终输出就结束；如果是 handoff 就换 agent 和输入再循环；如果有工具调用就执行、追加结果再循环；超过 `max_turns` 抛 `MaxTurnsExceeded`。最终输出是“产生了期望类型的文本输出，并且没有工具调用”。默认 10，按模型调用（循环轮次）计，`None` 关闭。
2. 一个叫 `transfer_to_<agent 名>` 的工具。`input_type` 定义这个工具调用的参数 schema，是模型在转交时给出的元数据，解析后交给 `on_handoff`，不替换下一个 agent 的输入也不选目的地；`is_enabled` 在准备工具列表时求值，控制这个 handoff 是否出现；`on_handoff` 在转交时执行。基于参数的授权放在 `on_handoff` 开头、任何副作用之前，失败就抛异常，SDK 不会转交。
3. handoff 把控制权和（默认）完整历史交给新 agent，由它产出最终输出；agent-as-tool 跑一个嵌套 run，嵌套 agent 只看到工具参数，结果作为工具输出交回，控制权留在调用方。前者适合分诊后由专家直接面对用户，后者适合一个管理者统一对话、调用多个专家能力。
4. 输入 guardrail 在 run 开始时检查用户输入，只对 run 的第一个 agent 生效；输出 guardrail 在产生最终输出后检查，只对产生最终输出的 agent 生效；工具 guardrail 在每次调用被保护的函数工具前后运行，有审批时输入检查默认在批准后、执行前。
5. 并行：延迟最低，但触发时 agent 可能已经消耗 token、执行了工具；阻塞：多了 guardrail 的延迟，但触发时 agent 根本不执行，没有成本和副作用。
6. 保存已经完成的工具调用和结果（以及重放它们所需的推理上下文），不保存被拒绝的候选最终输出。如果被拒绝的是直接当最终输出的工具结果，保存的工具结果被替换成占位文字。guardrail 自己抛异常时算结论未知，这一轮完成的项照常保存。
7. LangGraph 在节点内部任意位置暂停，恢复时节点从头重跑，按序号把恢复值交给 `interrupt()`，所以要求之前的副作用幂等、调用顺序不变；灵活，但约束多。Agents SDK 只在工具执行之前暂停，暂停的单位是还没执行的工具调用，恢复时直接执行或拒绝这个调用，不重新调用模型；简单，但只能在工具调用处暂停。
8. `always_approve=True` / `always_reject=True` 让同一工具以后的调用自动按这个决定处理，保存在 `RunContextWrapper` 和 `RunState` 里。函数工具的 sticky 决定按请求审批的 agent 区分；本地 `ShellTool` 和 `ApplyPatchTool` 只按工具类型和名字匹配，不区分 agent，handoff 后可能作用到权限不同的 agent。
9. `RunState` 里有审批决定、待执行的调用和参数，反序列化不认证快照也不认证提交者，客户端可以篡改或重放。服务端要：用会话认证审批人；授权他处理这个 run 的这些调用；只接受决定 ID 和布尔值，从服务端快照取待审批项；原子地消费每个待审批请求，防止并发或重放导致恢复两次。
10. 工具执行了外部动作，进程在保存新状态前停了，从旧的已批准快照恢复会再执行一次。给每个逻辑动作一个稳定的幂等键，交给能原子去重的目的系统，重试时用同样的键和参数；没有这种保证时，先和目的系统权威对账再决定是否重试，结果不确定就记为未知，不自动重放。

---

## 13. 参考答案

### 练习 1

1. 1 条用户输入 + 第一次响应里的 4 个函数调用 + 4 个结果 = 9 条。按完成顺序写回的话：同样的输入每次产生不同的历史顺序，测试和重放都不稳定；Responses API 要求调用和结果能对上，乱序本身问题不大，但结果的顺序变成了调度的副产品，日志、session、缓存前缀（1.5 课）都会随机变化，prompt cache 命中率下降。
2. `RunState.Processed`。模型响应追加到 `Items` 时它被设成 `false`，`processTurn` 处理完（包括暂停）之前保持 `false`。恢复时 `loop` 看到 `Processed == false`，直接再处理一次上一次响应：已经有结果的调用跳过，被批准的执行，然后才调模型。没有这个字段，恢复时会把“暂停”当成“该调模型了”，模型会在缺少工具结果的情况下被调用（Responses API 会因为调用没有对应结果而报错），或者重新生成一批新的调用，审批的那个调用就丢了。
3. 第三个调用（handoff 后“另一个” agent 的 `cancel_order`）会被自动批准，不会暂停，`runs` 变成 3。SDK 区分 agent 是因为同名不等于同权限：两个 agent 的 `cancel_order` 可能连着不同的后端、有不同的参数约束，批准了一个 agent 不应该让另一个也自动通过。
4. 输入 guardrail 只对 run 的第一个 agent 生效，B 是 handoff 进来的，它的输入 guardrail 不跑。办法：
    - 在 handoff 的 `on_handoff` 里做检查，失败就抛异常阻止转交；
    - 把检查写成 B 的工具的工具 guardrail；
    - 把检查放在第一个 agent 的输入 guardrail 里（它看到的是同一份用户输入）；
    - 把 B 作为 agent-as-tool 用，嵌套 run 里 B 是第一个 agent，它的输入 guardrail 会跑。

### 练习 2

```go
var (
	ErrForbidden       = errors.New("无权处理这个 run")
	ErrAlreadyConsumed = errors.New("这个审批已经处理过了")
	ErrVersionMismatch = errors.New("agent 版本已变化")
	ErrBadDecisions    = errors.New("决定必须正好覆盖所有待审批项")
)

type record struct {
	owner, version string
	snapshot       []byte            // RunState JSON，只在服务端
	ids            map[string]string // 不透明决定 ID → call_id
	consumed       bool
}

type MemApprovalStore struct {
	mu      sync.Mutex
	version string // 当前代码的 agent 版本
	root    *Agent
	runs    map[string]*record
}

func (s *MemApprovalStore) Decide(user, runID string, d map[string]bool) (*RunState, error) {
	s.mu.Lock()
	rec, ok := s.runs[runID]
	if !ok || rec.owner != user {
		s.mu.Unlock()
		return nil, ErrForbidden // 不区分“不存在”和“不是你的”，避免泄露 run 是否存在
	}
	if rec.consumed {
		s.mu.Unlock()
		return nil, ErrAlreadyConsumed
	}
	if rec.version != s.version {
		s.mu.Unlock()
		return nil, ErrVersionMismatch
	}
	if len(d) != len(rec.ids) {
		s.mu.Unlock()
		return nil, ErrBadDecisions
	}
	for id := range d {
		if _, ok := rec.ids[id]; !ok {
			s.mu.Unlock()
			return nil, ErrBadDecisions
		}
	}
	rec.consumed = true // 先消费，再反序列化和恢复：失败或取消也算消费掉了
	snap := rec.snapshot
	s.mu.Unlock()

	st, err := StateFromJSON(s.root, snap)
	if err != nil {
		return nil, err
	}
	for id, ok := range d {
		in := Interruption{CallID: rec.ids[id]}
		if ok {
			err = st.Approve(in, false)
		} else {
			err = st.Reject(in, "", false)
		}
		if err != nil {
			return nil, err
		}
	}
	return st, nil
}
```

要点：

- 不透明 ID 用随机串（`crypto/rand`），不要直接用 `call_id`，客户端不需要知道内部 ID；
- “检查未消费 → 标记已消费”必须在**同一把锁或同一个原子操作**里。数据库里是 `UPDATE ... SET consumed = true WHERE run_id = $1 AND owner = $2 AND consumed = false`，看影响行数是不是 1；
- 和官方示例一样，**在反序列化和恢复之前**就消费。代价是恢复失败后这个审批不能再提交一次，需要人工处理；好处是不会因为“恢复到一半失败、再提交一次”而执行两次；
- `Pending` 返回的 `PendingView` 只有不透明 ID、工具名和参数，展示时转义；
- 只决定一部分的情况，官方示例选择拒绝。如果要支持，就要在消费后把剩余的待审批项连同新快照重新 `Save` 成一条新记录。

### 练习 3

```go
// RunState 加一个字段：第一次 Run 时生成，序列化后不变
RunID string `json:"run_id"`

// RunContext 加一个字段，runOne 执行前设置（注意并发：每个 job 用自己的 RunContext 副本，或者通过参数传）
func (rc *RunContext) IdempotencyKey() string { return rc.key }

// 假的支付服务
type Payments struct {
	mu      sync.Mutex
	charged map[string]string // key → 第一次的结果
	count   int
}

func (p *Payments) Charge(key string, amount int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r, ok := p.charged[key]; ok {
		return r // 同一个键：返回第一次的结果，不再扣款
	}
	p.count++
	r := fmt.Sprintf("已扣款 %d 元，流水号 %d", amount, p.count)
	p.charged[key] = r
	return r
}
```

测试步骤：

1. `Run` → 暂停 → `st.Approve` → `old, _ := st.ToJSON()`（批准后、执行前的快照）；
2. `Resume(st)` → 工具扣款；
3. 丢掉新状态，`StateFromJSON(old)` 再 `Resume` 一次 → 工具再执行一次，但用的是同一个 `runID/callID`；
4. 断言 `payments.count == 1`，两次 `FinalOutput` 相同。

要点：

- 键必须**稳定**：用 `RunID + callID`，不能用时间戳或每次重新生成的随机数，否则重试就是一个新动作；
- `RunID` 存在 `RunState` 里，所以从旧快照恢复也拿到同一个；
- 幂等是在**目的系统**（支付服务）里保证的，不是在 runner 里。runner 无法让“外部动作”和“保存状态”成为一个原子操作，这正是 5.5 节的结论；
- 真实支付服务的幂等键有保留期（比如 24 小时），超过保留期再重试就不受保护了，这时要先对账。

---

## 完成标准

- `make test` 全部通过（18 个测试），`go test -race` 通过；
- 练习 1 的四个问题能讲清楚，尤其是第 2 题 `Processed` 的作用；
- 练习 2 写完，并发测试在 `-race` 下稳定通过；
- 能画出 Runner 循环，说出一次响应里工具、审批、handoff、最终输出的处理顺序；
- 能说清 handoff 和 agent-as-tool、三种 guardrail 的边界；
- 能对比 Agents SDK 的审批和 LangGraph 的 interrupt：暂停在哪、恢复时重跑什么、各自要求你保证什么；
- 自测题 10 道能不看答案讲出来，尤其是第 4、7、9、10 题。

下一课是 4.3 Claude Agent SDK：把 Claude Code 的 agent 循环做成库，工具、权限、hooks、子 agent 和会话又是另一种取舍，可以和 3.6 课的 Coding Agent 架构对照着看。
