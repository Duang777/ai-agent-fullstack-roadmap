# 4.3 Claude Agent SDK：agent 循环、六步权限、hooks、子 agent 与会话

> 第 4 部分 · 第 3 课 ｜ 预计 4–5 小时 ｜ 前置：2.2 MCP、3.2 Agent Loop、3.3 多智能体、3.6 Coding Agent 架构、4.2 OpenAI Agents SDK
>
> 配套练习：`projects/26-claude-agent-sdk/`（纯 Go 标准库，22 个测试）。用 Go 重写 Claude Agent SDK 运行时的核心：agent 循环与消息流（每个内容块一条 assistant 消息、`max_turns` 把最后一轮也算进去、预算在调模型前检查所以会超出一次响应）；只读工具并发、其他工具顺序执行；六步权限判定（hooks → deny 规则 → ask 规则 → 权限模式 → allow 规则 → `canUseTool`）和规则语法的几个陷阱；`PreToolUse` 多个 hook 的聚合、`updatedInput`、`onFailure`；`PostToolUse` 改写结果；`canUseTool` 改参数与“批准并记住”；`defer` 暂停与恢复；子 agent 的上下文隔离、深度 / 并发 / 预算上限、权限模式继承；会话的 resume / fork / continue。

## 读前说明

4.2 课的 OpenAI Agents SDK 是“一个循环 + 几个可插拔的点”，运行时是你进程里的一个 Python 库。Claude Agent SDK 的出发点不一样：**它把 Claude Code 整个当成库来用**。你在 Python 或 TypeScript 里调 `query()`，SDK 启动一个 `claude` CLI 子进程，通过 stdio 和它通信。这个子进程自带内置工具（Read、Edit、Bash、Grep……）、权限系统、hooks、子 agent、会话文件、上下文压缩。你得到的是一个现成的 coding agent，你做的事情主要是**收紧它**。

三个框架对照：

| 问题 | LangGraph（4.1） | OpenAI Agents SDK（4.2） | Claude Agent SDK（本课） |
|---|---|---|---|
| 运行时在哪 | 你的进程 | 你的进程 | 一个 `claude` 子进程，一个会话一个 |
| 自带工具 | 没有 | 没有（托管工具除外） | Read / Edit / Write / Bash / Grep / Glob / WebFetch / Agent 等 |
| 控制流 | 你画的图 | 固定的 Runner 循环 | 固定的 agent 循环 |
| 多 agent | 子图、Send | handoff（交出控制权）、agent-as-tool | 子 agent（`Agent` 工具），只回传最终消息 |
| 人工审批 | `interrupt()`，节点重跑 | `needs_approval`，run 停下，`RunState` 序列化 | `canUseTool` 回调在循环内阻塞等待；要跨进程就用 hook 返回 `defer`，之后 resume 会话 |
| 防护 | 自己写节点 | 输入 / 输出 / 工具 guardrail | 权限规则 + 权限模式 + hooks |
| 状态持久化 | checkpointer | Session / 服务端会话 | 会话 JSONL 文件，可镜像到 `SessionStore` |

本课依据 code.claude.com 上的 Agent SDK 文档（2026 年 11 月）：agent-loop、permissions、user-input、hooks、subagents、sessions、custom-tools、file-checkpointing、hosting 等页面，以及 Claude Code 的 hooks 参考里 `defer` 一节。写课时 PyPI 上 `claude-agent-sdk` 最新是 0.2.182，npm 上 `@anthropic-ai/claude-agent-sdk` 最新是 0.3.325，都在 2026-11-13（UTC）发布。这个 SDK 更新很快，文档里不少行为都标了“需要 Claude Code v2.1.xxx 以上”，用之前先看你的版本。

先想几个问题：

- `allowed_tools=["Read"]` 加上 `permission_mode="bypassPermissions"`，Bash 还能跑吗？
- 你在 `canUseTool` 里写了一套检查，为什么有的调用根本没进来？
- 一个 `PreToolUse` hook 返回 `allow`，一个返回 `ask`，最后听谁的？hook 自己抛了异常呢？
- 审批人可能明天才回复，你的进程不能挂一整天，怎么办？
- 子 agent 能看到主 agent 之前读过的文件吗？主 agent 能看到子 agent 读过的文件吗？

---

## 1. 它是什么，不是什么

文档把四个东西放在一起比较：

| 你想要 | 用 | 得到什么 |
|---|---|---|
| 在自己的 Python / TS 应用里嵌入 Claude Code 的 agent，跑在你运维的进程里 | **Agent SDK** | 一个驱动 Claude Code 二进制的库，带内置工具、权限、会话、hooks |
| 在终端里交互开发 | Claude Code CLI | 终端界面 |
| 直接调 Claude API | Client SDK | 直接访问 API，工具循环自己写（或用 beta 的 tool runner） |
| 让 Anthropic 托管 agent | Managed Agents | 托管的 agent harness，沙箱在 Anthropic 云上或你自己的基础设施上 |

其他语言想用同一个循环，就把 CLI 当子进程跑：`claude -p ... --output-format json`。Go 没有官方 Agent SDK，本课练习用 Go 重写的是**运行时的语义**，不是去调用这个二进制。

托管时要记住的事实（hosting 页）：

- 一个 agent 会话就是一个子进程，N 个并发会话就是 N 个子进程，各自有进程树和会话文件；
- 子进程拥有 shell、工作目录、本地磁盘上的 JSONL 会话记录。会话记录、CLAUDE.md、工作目录里的产物默认都在容器磁盘上，容器重启就没了；
- 会话要跨主机恢复，用 `SessionStore` 适配器把记录镜像到你自己的存储（第 8 节）。

所以它不是一个“无状态 API 包装”。部署形态更像 3.6 课说的 coding agent：一个任务一个容器，或者一个长期运行的容器里跑多个会话。

另一个约束：除非事先获批，第三方产品不能用 claude.ai 登录或它的额度，要用 API key。

---

## 2. agent 循环

### 2.1 五步

官方文档描述的循环：

1. **收到 prompt**：Claude 收到你的 prompt、system prompt、工具定义和历史。SDK 先吐一条 `SystemMessage`，subtype 是 `"init"`，里面有会话信息；
2. **评估并响应**：Claude 输出文本、请求一个或多个工具调用，或者两者都有。SDK 为**每个内容块**吐一条 `AssistantMessage`，同一次响应的这些消息共享一个消息 ID；
3. **执行工具**：SDK 执行每个请求的工具，收集结果，结果作为 `UserMessage` 吐出来并交回 Claude。hooks 可以在执行前拦截、修改、阻止；
4. **重复**：2、3 循环，直到 Claude 给出一个**没有工具调用**的响应；
5. **返回结果**：最后一条 `AssistantMessage` 是纯文本回答，然后是 `ResultMessage`：最终文本、token 用量、花费、会话 ID。

一个 turn 就是走一遍 2 和 3。这一切发生在一次 `query()` 里，中间不把控制权交回你的代码。

练习 `TestLoopTurnsAndEvents` 验证事件顺序是 `system, assistant, assistant, user, assistant, result`，前两个 assistant（同一次响应里的一个文本块和一个工具调用）消息 ID 相同。

### 2.2 消息类型

| 类型 | 什么时候出现 |
|---|---|
| `SystemMessage` | 会话事件：`init`、`compact_boundary`（压缩之后）、`informational` 等 |
| `AssistantMessage` | Claude 响应里的每个内容块，包括最后那个纯文本回答 |
| `UserMessage` | 每次工具执行后的结果；你在循环中途流式发进去的用户输入 |
| `StreamEvent` | 打开 partial messages 之后才有，是原始的流式事件（文本增量、工具参数片段） |
| `ResultMessage` | 循环结束。之后还可能有少量 system 事件，所以要把流读完，别一见到 result 就 break |

TypeScript 里 `AssistantMessage` 和 `UserMessage` 把原始 API 消息包在 `.message` 字段里，内容块在 `message.message.content`，不是 `message.content`。

### 2.3 max_turns 和预算

| 选项 | 控制什么 | 默认 |
|---|---|---|
| `max_turns` / `maxTurns` | 最多多少个 turn | 不限 |
| `max_budget_usd` / `maxBudgetUsd` | 估算花费到多少就停 | 不限 |

两个容易算错的地方：

- **`max_turns` 要把最后那个纯文本回答也算进去**。一个任务用了 3 轮工具调用加 1 轮回答，`max_turns=3` 会以 `error_max_turns` 结束，没有最终回答（`TestMaxTurnsCountsFinalTurn`）；
- **预算会超出一次响应的花费**。每次响应的花费要等 API 返回用量才知道，所以 SDK 是在响应回来之后再比较。把花费推过上限的那次响应会照常完成并计入总花费，此时还在跑的子 agent 停下前花的钱也算。设上限要留余量（`TestBudgetLetsCrossingResponseFinish`：每次 0.4 美元，上限 1.0，最后花了 1.2）。

预算覆盖子 agent：它们的花费计入总数；达到上限后，再派子 agent 会失败并返回 `Budget limit reached`，后台还在跑的子 agent 会被停掉（文档注明需要 Claude Code v2.1.217 以上）。

练习在**每次调模型之前**检查轮数和预算，这样“越线的那次响应”自然会完整执行完，包括它请求的工具。真实 SDK 的检查时机是“响应回来之后”，效果相同的部分是：越线那次响应的花费一定会计入。

没有限制时循环会一直跑到 Claude 自己结束，对“改进一下这个代码库”这种开放任务可能跑很久。文档的建议是：**生产环境默认设预算**。

### 2.4 结果 subtype

| subtype | 含义 | 有 `result` 吗 |
|---|---|---|
| `success` | 正常完成 | 有 |
| `error_max_turns` | 到了 `maxTurns` | 没有 |
| `error_max_budget_usd` | 到了预算 | 没有 |
| `error_during_execution` | 执行中出错，比如请求被取消、会话崩溃 | 没有 |
| `error_max_structured_output_retries` | 结构化输出在重试次数内都没通过校验 | 没有 |

所有 subtype 都带 `total_cost_usd`、`usage`、`num_turns`、`session_id`，所以出错之后也能记账、也能 resume。单次 `query()` 在吐出错误结果之后会**再抛一个异常**，这是有意的，要用 try 包住。`usage` 只覆盖主循环，整棵树的用量看 `modelUsage` / `model_usage`。

`stop_reason` 是模型最后一轮停下的原因：`end_turn`、`max_tokens`、`refusal`。检测拒答就看 `stop_reason == "refusal"`。

### 2.5 effort

`effort` 控制每次响应的推理深度：`low`、`medium`、`high`、`xhigh`、`max`。可以在整个会话上设，也可以在某个子 agent 的定义上单独设。读文件、列目录这类子任务用 `low` 能省不少 token。这是 1.4 课推理模型在框架里的开关。

---

## 3. 工具

### 3.1 内置工具

| 类别 | 工具 |
|---|---|
| 文件 | `Read`、`Edit`、`Write` |
| 搜索 | `Glob`、`Grep` |
| 执行 | `Bash` |
| 网络 | `WebSearch`、`WebFetch` |
| 发现 | `ToolSearch`：按需加载工具，而不是一开始全部塞进上下文 |
| 编排 | `Agent`（子 agent）、`Skill`、`AskUserQuestion`、`TaskCreate`、`TaskUpdate` |

这就是 3.6 课拆过的 Claude Code 工具集，现在你可以直接拿来用。

### 3.2 并发规则

一轮里有多个工具调用时：**只读工具**（Read、Glob、Grep、标了只读的 MCP 工具）可以并发；**会改状态的工具**（Edit、Write、Bash）顺序执行，避免冲突。自定义工具默认顺序执行，在注解里设 `readOnlyHint: true` 才能并发。这个字段名来自 MCP SDK。

对比 4.2：OpenAI 的 SDK 用一个并发上限 `max_function_tool_concurrency` 控制所有函数工具；Claude 的 SDK 按工具**有没有副作用**区分。后者更安全，但要求你对每个自定义工具如实标注。

练习的 `handleCalls` 先按调用顺序逐个做权限判定，再执行：相邻的已批准只读调用一起并发，其他调用单独执行；结果按调用顺序写回。`TestReadOnlyConcurrentMutatingSequential` 发了 `R1 R2 W R3 R4`，断言 R1、R2 真的同时在跑，W 开始时没有别的调用在跑，W 结束后 R3、R4 才开始。

### 3.3 自定义工具：进程内 MCP server

自定义工具不是直接传函数，而是包成一个**跑在你进程里的 MCP server**：

```python
from claude_agent_sdk import tool, create_sdk_mcp_server, ClaudeAgentOptions

@tool("get_temperature", "查询某地当前气温", {"latitude": float, "longitude": float})
async def get_temperature(args):
    t = await fetch_temp(args["latitude"], args["longitude"])
    return {"content": [{"type": "text", "text": f"{t}°C"}]}

weather = create_sdk_mcp_server(name="weather", version="1.0.0", tools=[get_temperature])

options = ClaudeAgentOptions(
    mcp_servers={"weather": weather},
    allowed_tools=["mcp__weather__get_temperature"],
)
```

要点：

- 工具的完整名字是 `mcp__{server}__{tool}`，`server` 是你在 `mcp_servers` 里给的键；权限规则、hook 的 matcher 都用这个完整名字；
- handler 返回 `content` 数组（text / image / audio / resource / resource_link），可选 `structuredContent`（机器可读的 JSON），可选 `isError: true`；
- 想让 Claude 读到一段你组织好的错误信息，就返回 `isError: true` 加文字，而不是让原始异常冒出去；
- 工具多了用 tool search：默认对 SDK MCP 工具开启，Claude 先看到一个紧凑的名字列表，需要时再加载完整 schema。这是 1.6 课上下文工程的一个具体手段。

TypeScript 版还能给自定义工具设 `permission_policy: "always_ask"`：这样的调用**任何 allow 规则都批不了**，连 `allowedTools` 里写了也不行。

---

## 4. 权限：六步判定

这是本课最重要的一节。官方文档给的顺序：

1. **hooks**：`PreToolUse` hook 可以直接拒绝，或放行到下一步。hook 返回 `allow` **不会跳过**第 2、3 步；
2. **deny 规则**（`disallowed_tools` 和 SDK 加载的 settings 文件）：命中就拒绝，`canUseTool` 不会被调用。**`bypassPermissions` 模式下、hook 返回了 `allow` 也一样被拒**；
3. **ask 规则**（settings.json 里的 `ask`）：命中就交给 `canUseTool` 确认，**`bypassPermissions` 下也一样**。需要用户交互的工具（`AskUserQuestion`、服务端标了 `anthropic/requiresUserInteraction` 的 MCP 工具）同理；
4. **权限模式**：`bypassPermissions` 在这里批准；`acceptEdits` 批准工作目录内的文件编辑；`plan` 把编辑交给回调；`default` 和 `dontAsk` 继续往下；
5. **allow 规则**（`allowed_tools` 和 settings.json）：命中就批准。工具“自己就能批准”的调用也在这一步通过，比如工作目录内的读文件、只读的 Bash 命令；
6. **`canUseTool` 回调**：前面都没定下来，就问你的回调。`dontAsk` 模式跳过这一步直接拒绝；没传回调也是拒绝。

练习的 `decide` 一步一步对应：

```go
// 1. hooks
dec, reason, upd := aggregatePre(runHooks(ctx, o.Hooks, "PreToolUse", t.Name, hin))
// deny → 直接拒绝；defer → 暂停；ask → 记下 forceAsk；allow → 记下 hookAllow（先不批）
// 2. deny 规则
if anyRule(o.DisallowedTools, t, in, o.Cwd, false) { return deny(...) }
// 3. ask 规则
if forceAsk || anyRule(o.AskRules, t, in, o.Cwd, false) { return r.callback(...) }
// 4. 权限模式
switch r.mode { case ModeBypass: ...; case ModeAcceptEdits: ...; case ModePlan: ... }
if hookAllow { return allow }       // hook 的 allow 在模式之后才生效
// 5. allow 规则 + 工具自批
// 6. canUseTool
```

`hookAllow` 放在第 4 步之后，是为了表达文档里 plan 模式的规则：规划期间的编辑**即使 allow 规则命中、即使 `PreToolUse` 返回了 `allow`**，也要交给回调（`TestPlanModeEditsReachCallback`）。

被拒绝的调用不会让循环报错：Claude 收到一条拒绝信息作为工具结果，通常会换个办法，或者说明做不了。

### 4.1 权限模式

| 模式 | 行为 | 适合 |
|---|---|---|
| `default` | 没有基于模式的自动批准；需要批准、又没有 allow 规则命中的调用交给 `canUseTool`；没有回调就拒绝 | 有审批界面的交互应用 |
| `acceptEdits` | 自动批准工作目录内的文件编辑和常见文件系统命令（`mkdir`、`touch`、`mv`、`cp` 等） | 信任编辑、想快速迭代 |
| `plan` | Claude 只探索和规划；文件工具的编辑交给回调，allow 规则也挡不住 | 先出方案再动手 |
| `dontAsk` | 从不提示：本来会提示的调用一律拒绝 | 没人审批的无头 agent |
| `auto` | 用一个模型分类器审查 shell 命令、网络请求等，逐个放行或拦截 | 想自主运行又要护栏 |
| `bypassPermissions` | 不提示直接执行，**包括 `allowedTools` 里没列的工具** | 只在隔离环境（CI、容器）里用 |

几个细节：

- Claude Code v2.1.302 起，没设模式的应用从 `default` 开始；
- `bypassPermissions` 在 Linux / macOS 上以 root 或 sudo 运行（且不在被识别的沙箱里）时拒绝启动；TS SDK 还要求 `allowDangerouslySkipPermissions: true`；
- `acceptEdits` 不自动批准工作目录之外的路径、受保护路径、对关键路径的 `rm`。练习里 `/work/../etc/x` 先规范化成 `/etc/x`，不在工作目录内，交给回调（`TestAcceptEditsScopedToCwd`）；
- 模式可以在流式会话中途用 `set_permission_mode()` 切换，比如先看过方案再切到 `acceptEdits`。

### 4.2 allow / deny 规则的语法和陷阱

| 写法 | 效果 |
|---|---|
| `allowed_tools=["Read", "Grep"]` | 自动批准这两个工具；其他工具**仍然可用**，需要批准的调用落到权限模式和 `canUseTool` |
| `disallowed_tools=["Bash"]` | 把 Bash 的工具定义从请求里**拿掉**，模型根本看不到 |
| `disallowed_tools=["Bash(rm *)"]` | Bash 还在；**按写法**匹配 `rm *` 的调用在所有模式下被拒；`/bin/rm` 不匹配，落到权限模式 |
| `disallowed_tools=["*"]`、`["mcp__*"]` | deny 规则支持工具名通配 |
| `allowed_tools=["*"]`、`["mcp__*"]` | **被忽略**，什么都不批准。allow 的通配必须跟在字面的 `mcp__<server>__` 后面，如 `mcp__github__get_*` |
| `Edit(//secrets/**)` | `//` 开头是绝对路径，单个 `/` 开头是工作目录内的路径 |
| `Write(path)` | **不匹配任何调用**。写文件的路径规则一律写成 `Edit(path)`，它管 Write、Edit、NotebookEdit |

这些都在练习里：

- `TestDenyRuleBeatsHookAllowAndBypass`：hook 对所有调用返回 `allow`，模式是 `bypassPermissions`，`Bash(rm *)` 照样拦下 `rm -rf build`，但 `/bin/rm -rf build` 漏过去了；
- `TestToolNameGlobs`：`"*"` 和 `"mcp__*"` 放在 allow 里不起作用，`mcp__github__get_*` 只批准 `get_issue`；deny 里的 `mcp__*` 和 `Bash` 把这些工具从请求里拿掉；
- `TestPathRules`：`Write(//etc/**)` 不匹配，`Edit(//etc/**)` 匹配 Write 调用。

这些规则是**字符串匹配**，不是安全边界。文档自己说：`Edit(//secrets/**)` 能拦住文件工具和 `tee`、`>` 重定向这类 Bash 写法，但 Claude 跑的脚本自己打开文件写，照样能写进去。要操作系统级的保证，用沙箱化的 Bash（2.5 课）。

### 4.3 两个警告

文档用了两个警告框，值得原样记住：

- **自动批准的调用永远到不了 `canUseTool`**。被 `acceptEdits`、`bypassPermissions`、allow 规则批准的调用跳过你的回调，你写在回调里的检查对它们**静默失效**（`TestAllowedToolsSkipCallbackAndNoCallbackDenies`）。要检查所有调用，写在 `PreToolUse` hook 里，它对这些调用也会运行。TS SDK 检测到这种配置会发一个代码为 `CLAUDE_SDK_CAN_USE_TOOL_SHADOWED` 的进程警告；
- **`allowed_tools` 约束不了 `bypassPermissions`**。`allowed_tools=["Read"]` 加 `bypassPermissions`，Bash、Write、Edit 照样被批准。在这个模式下要禁掉某些工具，用 `disallowed_tools`。

想要一个锁死的 agent：`allowedTools` 加 `permissionMode: "dontAsk"`。列出的工具放行，其他本来会提示的调用全部拒绝。注意“默认就不需要批准的调用”仍然会跑：工作目录内的读文件、只读 Bash 命令、`Agent`。想让模型连看都看不到某个内置工具，把它的裸名字放进 `disallowedTools`。

### 4.4 有些调用任何 allow 规则都批不了

- `AskUserQuestion`，以及标了需要用户交互的 MCP 工具：要在回调里回答；
- 组织设置为 `ask` 的连接器工具；
- TS 里 `permission_policy: "always_ask"` 的自定义工具；
- 被识别出的关键路径删除（如 `rm -rf ~`），`PreToolUse` 返回 `allow` 也批不了；
- 从网络路径读文件。

在 `dontAsk` 模式下这些调用直接被拒绝。

---

## 5. canUseTool：运行时审批

### 5.1 回调收到什么、返回什么

回调在两种情况下被调用：工具需要批准且前面五步都没定下来；Claude 调用 `AskUserQuestion` 问澄清问题。参数是工具名、输入，以及一个上下文（含 `suggestions` 和取消信号）。

返回两种之一：

| | Python | TypeScript |
|---|---|---|
| 允许 | `PermissionResultAllow(updated_input=...)` | `{ behavior: "allow", updatedInput }` |
| 拒绝 | `PermissionResultDeny(message=...)` | `{ behavior: "deny", message }` |

由此有六种回应方式：

- **批准**：原样放行；
- **改了再批**：返回修改过的输入，比如清理路径、加约束。Claude 看到结果，但**不会被告知你改过**；
- **批准并记住**：把上下文里给的 `suggestions`（现成的 `PermissionUpdate`）原样放进 `updatedPermissions`，以后匹配的调用不再问。目标是 `localSettings` 的建议会写进 `.claude/settings.local.json`；
- **拒绝**：给一段说明，Claude 看到后可能换办法；
- **拒绝并建议**：在说明里写你想要什么；
- **彻底换方向**：用流式输入直接发一条新指令。

练习 `TestCanUseToolUpdatedInputAndRemember`：第一次 `npm test` 被回调改成 `npm test --silent` 并记住 `Bash(npm test)`；第二次直接被 allow 规则批准，没有再问，也就没有被改写。注意这个副作用：**“记住”的规则批准的是原始命令**，改写逻辑不会再跑。如果你的改写是安全必需的（比如加 `--dry-run`），就不要“记住”，或者把改写放到 `PreToolUse` hook 里。

### 5.2 回调可以一直挂着

文档：回调可以无限期挂起，执行一直暂停到回调返回。这和 4.2 很不一样。OpenAI 的 SDK 遇到需要审批的调用会**结束这次 run**，把 `RunState` 交给你；Claude 的 SDK 默认是**在循环里阻塞等待**，子进程一直活着。

对一个网页或手机上的审批界面，阻塞等待意味着：审批人一天后才点，这个进程、这个容器就要活一天。文档给的出路是下一节的 `defer`。

### 5.3 AskUserQuestion

Claude 在有多种合理做法时会调用 `AskUserQuestion`，输入里是一个 `questions` 数组，每个问题有 `question`、`options`、`multiSelect`。你在回调里展示给用户，把选择放回去。两个限制：你不能往这个流程里加自己的问题；如果你给了 `tools` 数组限制工具，要把 `AskUserQuestion` 也放进去，否则 Claude 没法提问。plan 模式下澄清问题特别常见。

---

## 6. Hooks

### 6.1 事件

hooks 是在 agent 生命周期关键点运行的回调，跑在**你的应用进程里**，不占 agent 的上下文。常用的：

| 事件 | 什么时候 | 典型用途 |
|---|---|---|
| `PreToolUse` | 工具执行前（可以阻止或修改） | 拦截危险命令、改写参数、自动批准 |
| `PostToolUse` | 工具返回后 | 审计、改写或补充结果 |
| `PostToolUseFailure` | 工具执行失败 | 记录或处理错误 |
| `UserPromptSubmit` | 提交 prompt 时 | 注入额外上下文 |
| `Stop` | agent 结束时 | 校验结果、保存状态 |
| `SubagentStart` / `SubagentStop` | 子 agent 启动 / 结束 | 跟踪并行任务 |
| `PreCompact` | 上下文压缩前 | 归档完整记录 |
| `PermissionRequest` | 某个调用需要权限决定 | 自定义权限处理、发通知 |
| `Notification` | agent 状态消息 | 转发到 Slack |

TS SDK 的事件多得多（`SessionStart`、`SessionEnd`、`PostToolBatch`、`PostCompact`、`TaskCompleted`、`PermissionDenied` 等），Python 还不全支持。

### 6.2 matcher

hooks 按事件名注册，每个事件下是一组 matcher，每个 matcher 有一个过滤模式和一组回调：

```python
options = ClaudeAgentOptions(hooks={
    "PreToolUse": [
        HookMatcher(matcher="Write|Edit|NotebookEdit", hooks=[file_guard]),  # 精确列表
        HookMatcher(matcher="^mcp__", hooks=[mcp_audit]),                    # 正则
        HookMatcher(hooks=[global_logger]),                                  # 不写：全部
    ],
})
```

matcher 只匹配**工具名**，不匹配路径和参数。要按路径过滤，在回调里看 `tool_input["file_path"]`。还要注意：`Write|Edit` 这个 matcher 拦得住 Write 工具写 `.env`，拦不住 Claude 用 Bash 写 `.env`。

### 6.3 输入和输出

所有 hook 输入都有 `session_id`、`cwd`、`hook_event_name`；在子 agent 里触发时还有 `agent_id`、`agent_type`。工具类 hook 有 `tool_name`、`tool_input`，第二个参数 `tool_use_id` 用来把同一次调用的 Pre 和 Post 对上。

输出分两类：

- 顶层字段：`systemMessage`（给用户看）、`continue`（agent 是否继续）；
- `hookSpecificOutput`：
    - `PreToolUse`：`permissionDecision`（`allow` / `deny` / `ask` / `defer`）、`permissionDecisionReason`（告诉模型为什么）、`updatedInput`；
    - `PostToolUse`：`additionalContext`（追加到工具结果）、`updatedToolOutput`（替换工具结果，内置工具和 MCP 工具都支持）。

返回 `{}` 表示没意见。`updatedInput` 的几种组合：配 `allow` 就是“改了直接批”；配 `ask` 是“改了给用户看”；不写决定，改过的输入照样生效并走正常的权限流程；配 `defer` 时被忽略。总是返回新对象，不要原地改 `tool_input`。

### 6.4 多个 hook：并行执行，最严格的胜出

一个事件触发时，所有匹配的 hook **并行**运行，完成顺序不确定，所以每个 hook 都要能独立工作，不能假设另一个先跑过。权限决定取最严格的：**`deny` > `defer` > `ask` > `allow`**。

练习 `TestHookAggregationAndUpdatedInput`：

- Write：一个 hook 返回 `allow` 加改写后的路径，另一个返回 `{}`，结果是按新路径执行；
- MCP 工具：一个 `deny`、一个 `allow`，结果是拒绝，模型看到的是 deny 的理由；
- Bash：一个 `allow`、一个 `ask`，`ask` 胜出，交给回调。

### 6.5 hook 失败了怎么办

这是一个默认值很“宽松”的地方：大多数事件上，回调**抛异常或返回不合法的输出，动作照常进行**，应用收到一条 `hook_callback_failed` 警告。也就是说，一个崩掉的 `PreToolUse` 守卫会让调用直接进入正常的权限流程。回调**什么都没返回**（某个分支忘了 return）也算失败。

要 fail closed，在 matcher 上设 `onFailure: "block"`（Python 是 `on_failure="block"`）。这时工具结果里会有 `failed; blocking because onFailure is "block"`，Claude 看不到你的错误文字。几个例外：`Stop`、`SubagentStop` 等事件忽略这个选项；`PostToolUse` 时工具已经跑过了，只能把失败作为反馈给 Claude。

练习 `TestHookFailure` 覆盖三种情况：默认放行到后续步骤；`block` 时拒绝；`block` 且回调返回 nil 时也拒绝。

还有超时：每个回调有超时，大多数事件默认 600 秒，`UserPromptSubmit` 30 秒。`PreToolUse` 超时会阻止这次工具调用。如果一个 hook 只是做日志、发 webhook，不需要影响 agent，可以返回 `async: true`，agent 不等它；这种输出不能阻止、修改或注入上下文。

### 6.6 defer：把一个工具调用挂起，之后再继续

`permissionDecision: "defer"` 是给“跑 `claude -p` 子进程并读它的 JSON 输出”的集成用的，Agent SDK 应用正是这种情况。流程（以 `AskUserQuestion` 为例）：

1. Claude 调用工具，`PreToolUse` 触发；
2. hook 返回 `defer`。工具不执行，这一轮以一个 `stop_reason: "tool_deferred"` 的结果结束，待执行的调用留在会话记录里；
3. 你的进程从结果里读 `deferred_tool_use`（调用的 `id`、`name`、`input`），在自己的界面里收集输入。**这时进程可以退出**；
4. 之后用同一个会话 ID resume，同一个工具调用会**再次**触发 `PreToolUse`；
5. hook 这次返回 `allow`，把答案放进 `updatedInput`。工具执行，Claude 继续。

几个限制：只在非交互模式（`-p`）下生效；**一轮里只有一个工具调用时才生效**，多个调用时 `defer` 被忽略并给出警告，工具走正常权限流程；没有超时和重试上限，会话文件保留到清理周期（默认 30 天）；答案还没好时 hook 可以再返回 `defer`。如果 resume 时带了 prompt，Claude 会在没有工具结果的情况下回答它，不执行那个调用，所以要**不带 prompt** 地 resume，直到 hook 返回 `allow` 或 `deny`。

练习 `TestDeferAndResume` 实现了这个往返：第一次 run 停在 `tool_deferred`，模型只调用了 1 次；带 prompt 恢复返回 `ErrDeferredPending`（练习的简化，真实行为见上一段）；不带 prompt 恢复时 hook 返回 `allow` 和答案，工具执行，**模型没有被重新调用来生成那次工具调用**，总共只调了 2 次。同一个测试还验证：一轮两个调用时 `defer` 被忽略。

这和 4.2 的审批是同一个思路：**暂停的单位是一个还没执行的工具调用，恢复时不重新生成它**。区别是状态放在哪：OpenAI 交给你一个 `RunState` 对象自己存；Claude 的状态就是会话记录，你只需要保存会话 ID（会话记录本身要能被 resume 找到，跨主机就要 `SessionStore`）。

4.2 课说过的“批准不等于恰好执行一次”在这里一样成立：resume 后 hook 返回 `allow`、工具执行了外部动作，进程在会话记录落盘前崩了，再 resume 一次就会再执行一次。有副作用的工具照样需要幂等键（练习 2）。

---

## 7. 子 agent

### 7.1 定义和调用

子 agent 是主 agent 可以派生的独立 agent 实例。三种定义方式：代码里通过 `agents` 参数（推荐）；`.claude/agents/` 目录下的 markdown 文件；内置的 `general-purpose`，什么都不定义 Claude 也能用。Claude 通过 `Agent` 工具调用子 agent。

```python
options = ClaudeAgentOptions(
    allowed_tools=["Read", "Grep", "Glob", "Agent"],
    agents={
        "code-reviewer": AgentDefinition(
            description="代码审查专家。审查代码质量、安全问题时使用。",
            prompt="你是代码审查员……",
            tools=["Read", "Grep", "Glob"],
            model="sonnet",
        ),
    },
)
```

`AgentDefinition` 的主要字段：`description`（必填，Claude 靠它决定什么时候用）、`prompt`（必填，子 agent 的 system prompt）、`tools`（不写就继承所有可给子 agent 的工具）、`disallowedTools`、`model`、`skills`、`mcpServers`、`maxTurns`、`background`、`effort`、`permissionMode`。

Claude 根据 `description` 自动决定是否委派；想确保用某个子 agent，就在 prompt 里点名。子 agent 默认在**后台**运行，`Agent` 工具先返回 `status: "async_launched"`，报告在后面的轮次到达；Claude 下一步需要这份报告时会等它。

### 7.2 子 agent 能看到什么

除非是 fork，子 agent 的上下文**从空白开始**，但不是完全空的：

| 子 agent 收到 | 子 agent 收不到 |
|---|---|
| 自己的 system prompt 和 `Agent` 工具的 prompt 字符串 | 父 agent 的对话历史和工具结果 |
| 项目的 CLAUDE.md（除非设了 `omitClaudeMd`） | 预加载的 skill 内容（除非列在 `skills` 里） |
| 工具定义（继承的，或 `tools` 指定的子集） | 父 agent 的 system prompt |

父→子唯一的通道是那个 prompt 字符串，所以文件路径、错误信息、已经做的决定都要写进去。子→父也只回传**最终消息**：子 agent 读了几十个文件，主 agent 的上下文只多一段总结。这就是 3.3 课“上下文隔离”的落地，也是 3.7 课 Deep Research 里多个子 agent 并行检索的基础。

文档还提到：父 agent 读子 agent 的最终消息之前，Claude Code 会扫描里面像指令的模式（模仿 `<system-reminder>` 的标签、`Human:` / `Assistant:` 开头的行），加反斜杠中和掉，不删改原文。这是对 2.1 课“工具结果是数据不是指令”的一层防御。

练习 `TestSubagentContextIsolation`：子 agent 第一次请求的消息只有一条 user 消息“审查 /work/a.go”，工具只有 Read；主 agent 第二次请求里，`Agent` 调用的结果是“发现 2 个问题”，看不到子 agent 读到的文件内容；子 agent 内部的 3 条事件带 `ParentToolUseID`；子 agent 里触发的 hook 带 `AgentType`。

TS 里子 agent 产生的消息带 `parent_tool_use_id` 和 `agent_id`。检测是否发生了委派，就看 `tool_use` 块的名字是不是 `"Agent"`（老版本叫 `"Task"`，两个都要匹配）。

### 7.3 深度、并发、预算

Claude 自己决定何时派、派几个，子 agent 还能再派子 agent，一个 prompt 可能长成一棵树。三个上限：

| 上限 | 怎么设 | 默认 | 到了上限 |
|---|---|---|---|
| 深度 | 环境变量 `CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH` | 主 agent 下面 3 层；设 1 则子 agent 不能再派 | 最底层的子 agent 不能派，自己干 |
| 并发 | 环境变量 `CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS` | 同时 20 个 | 拒绝再派，返回 `Concurrent subagent limit reached` |
| 花费 | `max_budget_usd` | 不限 | 拒绝再派（`Budget limit reached`）、停掉后台子 agent、以 `error_max_budget_usd` 结束 |

练习：`TestSubagentDepth`（深度 1 时子 agent 的工具列表里没有 `Agent`）、`TestSubagentConcurrencyLimit`（上限 2，同时派 3 个，恰好 1 个被拒）、`TestSubagentPartialAndSharedBudget`（子 agent 花掉 1.0 后，同一轮里第二次派生被拒，主循环下一次调模型前停下，总花费 1.0 包括子 agent）。

文档专门提醒：新一代模型更倾向于委派，所以这几个上限在那些模型上最重要。只靠 prompt 里写“别乱派子 agent”是引导，上限才是强制。

### 7.4 子 agent 的权限模式

| 父会话的模式 | 子 agent 用什么 |
|---|---|
| `plan` / `acceptEdits` / `auto` / `bypassPermissions` | 继承父的模式，子 agent 自己写的不算 |
| `default` / `dontAsk` | 用子 agent 定义里的模式，但 `bypassPermissions` **永远不生效** |

为什么这样设计：父会话在 plan 模式，说明你还没看过方案，不能因为把活交给了一个声明 `acceptEdits` 的子 agent 就绕过审查；反过来，父会话在 `bypassPermissions`，子 agent 继承它，就拿到了完整的系统权限，文档的警告是子 agent 的 system prompt 可能约束更少，要清楚这一点。练习 `TestSubagentPermissionMode` 用一张表测了 `childMode`，再跑一个集成：父 plan、子 acceptEdits，子 agent 的 Write 仍然以 `plan` 的理由交给回调，回调里能看到 `AgentType` 是 `editor`。

### 7.5 resume 和 fork

子 agent 结束时，`Agent` 工具的结果里有 `agentId: <id>`。resume 同一个会话、在 prompt 里带上这个 ID，就能让子 agent 带着完整历史继续。到了 `maxTurns` 的子 agent，输出会被标成部分结果，方便之后继续（练习用 `[partial: ...]` 前缀表示）。fork 是复制父对话的子 agent：Claude 可以用 `subagent_type: "fork"` 启动；不想要就加一条 `Agent(fork)` 的 deny 规则。

需要协调几十上百个 agent 时，文档推荐 `Workflow` 工具（TS SDK），把编排挪到运行时执行的脚本里，不放在对话上下文里。

---

## 8. 会话

### 8.1 continue、resume、fork

会话就是 SDK 积累的对话历史：prompt、每个工具调用、每个结果、每个响应，自动写到磁盘。**会话保存的是对话，不是文件系统。**

| 你在做什么 | 用什么 |
|---|---|
| 一次性任务 | 什么都不用，一次 `query()` |
| 一个进程里多轮对话 | Python 用 `ClaudeSDKClient`；TS 用 `continue: true` |
| 进程重启后接着上一次 | `continue`：取当前目录下最近的会话 |
| 回到某个特定的会话 | 保存会话 ID，传给 `resume` |
| 试另一种做法而不丢原来的 | `resume` 加 `fork`：新会话从原历史的副本开始，原会话不变 |
| 不想写磁盘 | TS：`persistSession: false` |

会话 ID 从 `ResultMessage.session_id` 取，成功和失败的结果都有。典型用法：上次以 `error_max_turns` 结束，提高上限后 resume。

练习 `TestSessionsResumeForkContinue`：resume 后模型看到 3 条消息；fork 后看到 5 条，新 ID，原会话仍是 4 条；之后 `continue` 取到的是最近写入的 fork。

### 8.2 跨主机

会话文件在本机 `~/.claude/projects/<编码后的 cwd>/<id>.jsonl`。跨主机恢复有三种办法：

- 配 `SessionStore` 适配器（必须实现 `append` 和 `load`），SDK 把记录镜像到你的对象存储或数据库。键里编码了工作目录，所以要从**相同的 cwd** resume；子进程仍然先写本地磁盘；
- 把会话文件搬过去；
- 不依赖 resume：把需要的结论、决定、diff 存成应用状态，放进新会话的 prompt。文档说这往往更稳。

### 8.3 文件 checkpoint

因为会话不管文件系统，SDK 另有文件 checkpoint：开启 `enable_file_checkpointing` 后，Write / Edit / NotebookEdit 修改文件前先备份，每个开启一轮的用户消息 UUID 就是一个还原点，`rewind_files()` 把文件恢复到那时的状态（新建的删掉，改过的还原）。它**不回退对话**；Bash 改的文件（比如 `sed -i`）不跟踪；子 agent 的编辑不跟踪。

把这三件事放在一起看：**对话**靠会话 resume / fork，**文件**靠 checkpoint，**外部副作用**两者都管不了，只能靠幂等和对账。这和 3.4、3.5 课讲的状态分层是一回事。

---

## 9. 上下文

上下文窗口在一个会话内不会在轮次之间重置，所有东西都在累积：system prompt、工具定义、历史、工具输入输出。不变的部分（system prompt、工具定义、CLAUDE.md）自动走 prompt caching（1.5 课）。

| 来源 | 何时加载 | 影响 |
|---|---|---|
| system prompt | 每次请求 | 固定的小开销 |
| CLAUDE.md | 会话开始，经 `settingSources` | 每次请求都带全文（有缓存） |
| 工具定义 | 核心内置工具每次都带；MCP 工具和其他工具默认由 tool search 延迟加载 | 关掉 tool search 时 schema 全部预加载 |
| 对话历史 | 逐轮累积 | 越来越大 |
| skill 描述 | 会话开始 | 只是摘要，调用时才加载全文 |

接近上限时 SDK **自动压缩**：把较早的历史总结掉，保留最近的交流和关键决定，并在流里吐一条 `compact_boundary`。压缩会丢掉早期 prompt 里的具体指令，所以**长期规则放进 CLAUDE.md**（每次请求都会重新注入），不要只写在第一条 prompt 里。可以在 CLAUDE.md 里写一段“总结时保留什么”，用 `PreCompact` hook 在压缩前归档完整记录，或者发 `/compact` 手动压缩。

保持上下文精简的办法：子任务交给子 agent；用 `tools` 字段给子 agent 最小工具集；注意 MCP server 的工具数量；常规任务用低 effort。练习 3 实现一个简化的自动压缩。

---

## 10. 和 4.2 对照

| | OpenAI Agents SDK | Claude Agent SDK |
|---|---|---|
| 定位 | 搭 agent 的库，工具自己写 | Claude Code 当库，工具现成 |
| 多 agent | handoff 交出控制权和历史；agent-as-tool 只给参数 | 子 agent 只给 prompt 字符串、只回最终消息，接近 agent-as-tool |
| 工具并发 | 一个并发上限 | 只读并发、有副作用顺序 |
| 审批 | 工具声明 `needs_approval`，run 结束，`RunState` 交给你 | 六步判定，`canUseTool` 在循环内阻塞；跨进程用 `defer` + resume |
| 防护 | 三种 guardrail，各有边界（输入只看第一个 agent） | 规则 + 模式 + hooks；`PreToolUse` 对所有调用都跑 |
| 失败默认 | 审批回调参数异常时 fail closed | hook 失败默认放行，要 `onFailure: "block"` 才 fail closed |
| 状态 | `RunState` 对象（反序列化不认证） | 会话 JSONL（可镜像到 `SessionStore`） |
| max_turns | 默认 10 | 默认不限 |

两个都没有替你解决的问题：**外部副作用的恰好一次**，以及**规则只是字符串匹配**。

---

## 11. 练习代码导读

| 文件 | 内容 | 对应 SDK |
|---|---|---|
| `types.go` | `Block`、`Message`、`Model`、`Tool`（`ReadOnly`、`EditsFiles`、`SpecKey`、`SelfApprove`）、`Event`、`Result` 和 subtype | 内容块、消息类型、`ResultMessage` |
| `permissions.go` | 权限模式、规则解析与匹配（工具名通配的锚定、`//` 与 `/` 路径、`Edit(path)` 管所有写文件工具）、hooks 的 matcher 与并发执行、决定聚合、`decide` 六步判定、`canUseTool` | `permission_mode`、`allowed_tools` / `disallowed_tools`、`HookMatcher`、`can_use_tool` |
| `query.go` | `Query`、`loop`、`handleCalls`（权限 → 分组执行 → `PostToolUse`）、`visibleTools`、`Agent` 工具与 `spawn`、`childMode` | `query()`、`AgentDefinition`、子 agent 上限 |
| `session.go` | `SessionStore`：Save / Load / Latest | 会话文件、`SessionStore` |
| `sdk_test.go` | 22 个测试，用按调用次数返回脚本的假模型；`router` 按 system prompt 把请求分给主 agent 和子 agent 的假模型 | |

和真实 SDK 的差异：

- 没有子进程、没有流式、没有 `StreamEvent`、没有上下文压缩、没有 `auto` 模式的分类器、没有 settings 文件加载；
- 子 agent 同步执行（`Agent` 工具标成只读，所以同一轮的多个 `Agent` 调用会并发），没有后台模式和 `agentId` resume；
- 预算在每次调模型前检查；
- defer 挂起时带 prompt 恢复直接报错，真实 SDK 会让 Claude 回答 prompt 而不执行那个调用；
- 规则匹配只实现了 `*` 通配，没有 Bash 命令解析（真实实现会识别复合命令、只读命令等）。

---

## 12. 常见的坑

- **在 `canUseTool` 里写安全检查**：被 allow 规则、`acceptEdits`、`bypassPermissions` 批准的调用根本到不了回调。所有调用都要过的检查写在 `PreToolUse` hook 里。
- **以为 `allowed_tools` 是白名单**：它只是“自动批准”，没列的工具照样可用；在 `bypassPermissions` 下更是全部放行。白名单用 `tools`（子 agent）或 `disallowed_tools`。
- **`allowed_tools=["*"]`**：被忽略，什么都不批准。
- **`Write(//secrets/**)`**：不匹配任何调用，要写 `Edit(...)`。
- **`Bash(rm *)` 当安全边界**：`/bin/rm`、`find -delete`、脚本里的删除都绕得过去。安全边界是沙箱和容器。
- **hook 默认失败放行**：守卫类 hook 要设 `onFailure: "block"`，并且让错误往外抛，别自己 catch 了返回 `{}`。
- **hook 之间互相依赖**：它们并行跑，顺序不确定。
- **matcher 写路径**：matcher 只匹配工具名。
- **`max_turns` 少算一轮**：最后的纯文本回答也是一轮。
- **预算卡死**：会超出一次响应的花费，加上还在跑的子 agent。留余量。
- **没限制子 agent**：深度、并发、预算三个上限都要想一遍。
- **子 agent 的 prompt 写得太短**：父→子只有这一个字符串。
- **以为 fork 会话会隔离文件**：fork 只分叉对话，文件改动是真实的。
- **审批界面用阻塞回调**：审批要等很久就用 `defer`。
- **长期规则写在第一条 prompt 里**：压缩后可能丢，放进 CLAUDE.md。

---

## 13. 练习

### 练习 1：读懂并跑通（必做）

```bash
cd projects/26-claude-agent-sdk && make test
```

回答：

1. `TestBudgetLetsCrossingResponseFinish` 里上限是 1.0，为什么总花费是 1.2？在 `query.go` 里指出检查发生在哪。如果业务要求“绝对不超过 1.0”，SDK 层面能做到吗？你会怎么设上限？
2. `TestDenyRuleBeatsHookAllowAndBypass` 里 `/bin/rm -rf build` 为什么没被拦住？列出至少三种能拦住它的办法，说明各自在哪一层。
3. `decide` 里 `if hookAllow` 为什么放在权限模式那一步之后？如果挪到第 2 步之前，哪个测试会失败？那代表什么安全问题？
4. `TestSubagentConcurrencyLimit` 里，为什么要等“收到拒绝”之后才放行另外两个子 agent？如果改成 `time.Sleep(50 * time.Millisecond)` 后放行，会有什么问题？

### 练习 2：基于 defer 的异步审批（必做）

实现一个 `ApprovalHook`，让审批人可以第二天再批，进程中途可以退出：

```go
type ApprovalStore interface {
    // Request 为 (sessionID, toolUseID) 登记一个待审批请求，重复登记不产生新请求。
    Request(sessionID, toolUseID, tool string, input map[string]any) error
    // Decide 由审批服务调用（审批人已认证）：approve 为 true 时可附带改写后的输入。
    Decide(sessionID, toolUseID string, approve bool, reason string, updated map[string]any) error
    // Consume 原子地取走决定：没有决定返回 ok=false；同一个决定只能被取走一次。
    Consume(sessionID, toolUseID string) (d Decision, ok bool, err error)
}

func ApprovalHook(store ApprovalStore, tools ...string) HookMatcher
```

要求：

- hook 对列出的工具生效：`Consume` 拿到决定就按决定返回 `allow`（带 `UpdatedInput`）或 `deny`（带理由）；拿不到就 `Request` 并返回 `defer`；
- `Decide` 对同一个调用只接受一次，第二次返回错误；
- `Consume` 在 `-race` 下并发调用，恰好一个拿到决定；
- 测试：完整走一遍 defer → 进程“退出”（丢掉所有内存对象，只保留 store 和会话 store）→ Decide → resume → 工具恰好执行 1 次；拒绝路径；在决定之前 resume，仍然 defer、工具不执行、模型不被调用；一轮里两个需要审批的调用时会发生什么（写测试把这个行为固定下来，并在注释里说明对产品的影响）。

### 练习 3：自动压缩与 PreCompact（选做）

给 `Options` 加 `CompactAtChars int` 和 `KeepLast int`。每次调模型之前，如果历史的总字符数超过阈值：

- 触发 `PreCompact` hook（输入里带上将被压缩的消息数）；
- 用一次单独的模型调用（system 为固定的总结指令）把除最后 `KeepLast` 条之外的历史总结成一条 user 消息；
- 吐一条 `system` / `compact_boundary` 事件；
- 注意：不要把一个 `tool_use` 和它的 `tool_result` 切到两边。

测试：压缩后的请求消息数是 `KeepLast + 1`；`System` 里的规则不受影响，而第一条 prompt 里的指令只剩总结里的内容；切分点不会落在工具调用和结果之间。

---

## 14. 自测题

1. agent 循环有哪几步？一次响应里有文本和两个工具调用，SDK 吐出几条 `AssistantMessage`？它们有什么共同点？
2. `max_turns` 怎么计数？为什么预算上限会被超过？超过多少？
3. 哪些工具可以并发执行？自定义工具怎么才能并发？
4. 权限判定的六步是什么？hook 返回 `allow` 能跳过哪些步骤、不能跳过哪些？
5. 为什么写在 `canUseTool` 里的检查可能静默失效？应该写在哪？
6. `allowed_tools=["Read"]` 加 `bypassPermissions`，Bash 能跑吗？想在 bypass 模式下禁掉 Bash 怎么办？
7. 多个 `PreToolUse` hook 返回不同的决定，结果是什么？hook 抛异常时默认会怎样？
8. `defer` 的完整往返是怎样的？它有什么限制？
9. 子 agent 能看到什么、看不到什么？主 agent 能拿到子 agent 的什么？
10. 父会话在 plan 模式、子 agent 声明 `acceptEdits`，子 agent 的编辑会怎样？父会话在 `default`、子 agent 声明 `bypassPermissions` 呢？
11. continue、resume、fork 有什么区别？fork 会隔离文件改动吗？
12. 为什么长期规则要放在 CLAUDE.md 而不是第一条 prompt？

## 15. 自测题答案

1. 收到 prompt（吐 `init`）→ Claude 评估并响应 → 执行工具、结果交回 → 重复直到没有工具调用 → 返回结果（最后的 `AssistantMessage` 和 `ResultMessage`）。三条 `AssistantMessage`，每个内容块一条，共享同一个消息 ID。
2. 每次模型响应算一轮，包括最后那个纯文本回答；少了这一轮会以 `error_max_turns` 结束且没有最终回答。预算是在响应回来、拿到用量之后比较的，越线的那次响应会照常完成并计入，所以最多超出一次响应的花费，再加上此时还在跑的子 agent 停下前的花费。
3. 只读工具（Read、Glob、Grep、标了只读的 MCP 工具）可以并发；Edit、Write、Bash 顺序执行。自定义工具默认顺序执行，在注解里设 `readOnlyHint: true` 才并发。
4. hooks → deny 规则 → ask 规则 → 权限模式 → allow 规则 → `canUseTool`。hook 的 `allow` 跳不过 deny 规则和 ask 规则；plan 模式下的编辑即使 hook 返回 `allow` 也交给回调；对被识别的关键路径删除、`always_ask` 工具、需要用户交互的工具也不起作用。
5. 被前面步骤批准的调用（allow 规则、`acceptEdits`、`bypassPermissions`）不会进入回调。所有调用都要过的检查写在 `PreToolUse` hook 里，它对这些调用也运行，它的 deny 在 bypass 模式下也有效。
6. 能。`allowed_tools` 只是预先批准，bypass 模式会批准没列出的工具。要禁掉就用 `disallowed_tools`：写 `Bash` 把工具从请求里拿掉，或写 `Bash(...)` 拒绝特定调用。
7. 所有匹配的 hook 并行执行，取最严格的：`deny` > `defer` > `ask` > `allow`。hook 抛异常、返回不合法输出或什么都不返回，默认动作照常进行（进入正常权限流程），应用收到一条警告；设 `onFailure: "block"` 才会阻止。
8. Claude 调用工具 → `PreToolUse` 返回 `defer` → 工具不执行，这一轮以 `stop_reason: "tool_deferred"` 结束，结果里有 `deferred_tool_use` → 你的进程收集输入（可以先退出）→ 用同一会话 ID resume，同一个调用再次触发 `PreToolUse` → hook 返回 `allow` 和 `updatedInput`，工具执行，Claude 继续。限制：只在非交互模式；一轮只有一个工具调用时才生效；带 prompt resume 时 Claude 会回答 prompt 而不执行调用；没有超时，会话文件在清理周期后被删。
9. 子 agent 收到自己的 system prompt、`Agent` 工具的 prompt 字符串、项目 CLAUDE.md、工具定义；收不到父 agent 的历史、工具结果、system prompt，以及没列在 `skills` 里的 skill 内容。主 agent 只拿到子 agent 的最终消息（经过指令模式扫描）。
10. 父在 plan，子 agent 继承 plan，它的编辑交给 `canUseTool`。父在 `default`，子 agent 用自己定义的模式，但 `bypassPermissions` 永远不生效，所以仍是 `default`。
11. continue 取当前目录最近的会话继续；resume 按 ID 继续某个会话；fork 从原会话历史的副本开一个新会话，新 ID，原会话不变。fork 只分叉对话，不隔离文件，文件回退要用 file checkpoint。
12. 上下文接近上限时自动压缩会把早期历史总结掉，第一条 prompt 里的具体指令可能丢失；CLAUDE.md 每次请求都会重新注入，压缩不影响它。

---

## 16. 参考答案

### 练习 1

1. 每次响应 0.4，前两次之后是 0.8，没到上限，于是调第三次模型，第三次之后是 1.2，这次响应和它请求的工具照常执行；下一次调模型前检查到 1.2 ≥ 1.0 才停。检查在 `loop` 里 `r.model.Respond` 之前的 `over := o.MaxBudgetUSD > 0 && r.sh.cost >= o.MaxBudgetUSD`。SDK 层面做不到“绝对不超过”，因为花费在响应之后才知道。办法是把上限设成 `硬上限 - 单次响应最大花费 - 并行子 agent 的余量`；单次响应的最大花费可以用 `max_tokens` 和上下文大小估出来；更硬的控制放在 API key 的额度或你自己的代理层。
2. `Bash(rm *)` 是按命令字符串的写法匹配的，`/bin/rm` 不以 `rm ` 开头。能拦住它的办法：
    - `PreToolUse` hook 里自己解析命令（拆分管道、`&&`、取第一个词的 basename），对删除类命令返回 `deny`，并设 `onFailure: "block"`。这在权限层，bypass 模式下也有效；
    - 不用 bypass，改成 `dontAsk` 加精确的 allow 规则，只放行已知安全的命令。这也在权限层，但是白名单思路；
    - 沙箱化的 Bash：限制可写目录，删不掉工作目录外的东西。这在操作系统层；
    - 容器只读挂载源码、只给一个临时可写目录。这在部署层。前两者是字符串和策略，后两者才是真正的边界。
3. 文档规定 hook 的 `allow` 不能跳过 deny 规则、ask 规则，plan 模式下的编辑也要交给回调。挪到第 2 步之前，`TestDenyRuleBeatsHookAllowAndBypass`（`rm -rf build` 会被执行）和 `TestPlanModeEditsReachCallback`（Write 不再交给回调）都会失败。安全问题是：任何一个“宽松”的 hook（比如为了省事对只读工具返回 `allow`，结果 matcher 写宽了）就能绕过管理员在 settings 里写的 deny 规则，deny 规则就不再是“任何情况下都生效”的底线。
4. 三个子 agent 同时开始，只有当两个占着名额、第三个来检查时才会被拒。如果先放行，占名额的子 agent 可能在第三个检查前就结束释放了名额，于是三个都成功，测试失败。用 `Sleep` 放行依赖调度时机：机器慢或者 `-race` 下调度变化，就会时过时不过。等“拒绝已经发生”这个事件再放行，顺序是确定的：被拒绝的那个一定是在另外两个都还占着名额时检查的。

### 练习 2

```go
var (
    ErrAlreadyDecided = errors.New("这个调用已经有决定了")
    ErrUnknown        = errors.New("没有这个待审批请求")
)

type Decision struct {
    Approve bool
    Reason  string
    Updated map[string]any
}

type entry struct {
    tool     string
    input    map[string]any
    decision *Decision
    consumed bool
}

type MemApprovals struct {
    mu sync.Mutex
    m  map[string]*entry // sessionID + "/" + toolUseID
}

func key(s, t string) string { return s + "/" + t }

func (a *MemApprovals) Request(s, t, tool string, in map[string]any) error {
    a.mu.Lock()
    defer a.mu.Unlock()
    if _, ok := a.m[key(s, t)]; !ok {
        a.m[key(s, t)] = &entry{tool: tool, input: in}
    }
    return nil
}

func (a *MemApprovals) Decide(s, t string, ok bool, reason string, upd map[string]any) error {
    a.mu.Lock()
    defer a.mu.Unlock()
    e, found := a.m[key(s, t)]
    if !found {
        return ErrUnknown
    }
    if e.decision != nil {
        return ErrAlreadyDecided
    }
    e.decision = &Decision{Approve: ok, Reason: reason, Updated: upd}
    return nil
}

func (a *MemApprovals) Consume(s, t string) (Decision, bool, error) {
    a.mu.Lock()
    defer a.mu.Unlock()
    e, found := a.m[key(s, t)]
    if !found || e.decision == nil || e.consumed {
        return Decision{}, false, nil
    }
    e.consumed = true // 检查和标记在同一把锁里
    return *e.decision, true, nil
}

func ApprovalHook(store ApprovalStore, tools ...string) HookMatcher {
    return HookMatcher{
        Matcher:   strings.Join(tools, "|"),
        OnFailure: "block", // 审批服务不可用时拒绝，而不是放行
        Hooks: []HookFunc{func(_ context.Context, in HookInput) (*HookOutput, error) {
            d, ok, err := store.Consume(in.SessionID, in.ToolUseID)
            if err != nil {
                return nil, err
            }
            if !ok {
                if err := store.Request(in.SessionID, in.ToolUseID, in.ToolName, in.ToolInput); err != nil {
                    return nil, err
                }
                return &HookOutput{PermissionDecision: "defer"}, nil
            }
            if !d.Approve {
                return &HookOutput{PermissionDecision: "deny", Reason: "审批人拒绝：" + d.Reason}, nil
            }
            return &HookOutput{PermissionDecision: "allow", UpdatedInput: d.Updated}, nil
        }},
    }
}
```

要点：

- 键用 `sessionID + toolUseID`：resume 时同一个调用再次触发 `PreToolUse`，`tool_use_id` 不变，所以能找回同一个请求。这两个值都来自 SDK 传给 hook 的输入，不是客户端提交的；
- `Consume` 的“检查未消费 → 标记已消费”必须是一个原子操作。数据库里就是 `UPDATE ... SET consumed = true WHERE key = $1 AND decision IS NOT NULL AND consumed = false`，看影响行数；
- 先消费再执行：消费之后工具执行失败或进程崩溃，这个决定不会被再次使用，再 resume 会重新 `Request`，要人重新批。代价是人要多批一次；好处是不会因为重放执行两次。外部动作本身的重复仍然要靠工具侧的幂等键（4.2 课练习 3），键可以直接用 `sessionID/toolUseID`；
- `OnFailure: "block"`：审批存储挂了，默认行为是 hook 失败放行，那就等于没有审批；
- `Decide` 由审批服务调用，审批人的身份和权限在那一层用应用的会话认证，和 4.2 课练习 2 的要求一样；
- 两个需要审批的调用在同一轮：`defer` 被忽略，调用走正常权限流程。如果没有 allow 规则和回调，结果是拒绝，Claude 收到两条拒绝信息。对产品的影响是：需要异步审批的工具最好在 system prompt 里要求“每轮只调用一个”，或者把这类工具设计成一次只处理一个动作。测试里把这个行为固定下来，SDK 改行为时你会第一时间知道。

测试骨架：

```go
store, sessions := &MemApprovals{m: map[string]*entry{}}, NewSessionStore()
mk := func() Options { // 每次都新建，模拟进程重启后只剩两个 store
    return Options{Model: model, Tools: tools, Store: sessions,
        Hooks: map[string][]HookMatcher{"PreToolUse": {ApprovalHook(store, "Pay")}}}
}
r1, _ := Query(ctx, "付款", mk())                      // StopReason == "tool_deferred"
o := mk(); o.Resume = r1.SessionID
r2, _ := Query(ctx, "", o)                             // 还没决定：仍然 defer，模型调用次数不变
_ = store.Decide(r1.SessionID, r1.DeferredToolUse.ID, true, "", nil)
r3, _ := Query(ctx, "", o)                             // Pay 执行，r3.Result 是最终回答
// 断言：Pay 恰好执行 1 次；store.Consume 再调一次返回 ok=false（决定已被取走）
```

### 练习 3

```go
func (r *agentRun) maybeCompact(ctx context.Context) {
    o := r.sh.opts
    if o.CompactAtChars == 0 || size(r.msgs) <= o.CompactAtChars || len(r.msgs) <= o.KeepLast+1 {
        return
    }
    cut := len(r.msgs) - o.KeepLast
    // 不能把 tool_use 和它的 tool_result 分开：如果 cut 处是一条带 tool_result 的 user 消息，往前挪一条
    for cut > 0 && hasToolResult(r.msgs[cut]) {
        cut--
    }
    if cut <= 0 {
        return
    }
    runHooks(ctx, o.Hooks, "PreCompact", "auto", HookInput{SessionID: r.sh.sessionID, ToolOutput: fmt.Sprint(cut)})
    sum, err := r.model.Respond(ctx, ModelRequest{
        System:   "把下面的对话总结成要点，保留任务目标、读过和改过的文件、测试结果、做过的决定。",
        Messages: r.msgs[:cut],
    })
    if err != nil {
        return // 压缩失败就不压缩，下一轮再试
    }
    summary := Message{Role: "user", Blocks: []Block{Text("[之前对话的总结]\n" + lastText([]Message{{Role: "assistant", Blocks: sum.Blocks}}))}}
    r.sh.mu.Lock()
    r.sh.cost += sum.CostUSD // 总结也花钱，计入预算
    r.sh.mu.Unlock()
    r.msgs = append([]Message{summary}, r.msgs[cut:]...)
    r.sh.emit(Event{Type: "system", Subtype: "compact_boundary"})
}
```

要点：

- 在 `loop` 里调模型之前调用 `maybeCompact`；
- 切分点要落在“一条 assistant 消息开始”的位置，否则留下来的第一条可能是一个没有对应 `tool_use` 的 `tool_result`，API 会报错；
- 总结调用的花费计入预算；
- `System` 不参与压缩，这就是“规则放 CLAUDE.md”的原因：练习里的 `System` 相当于每次请求都重新注入的那部分；
- 压缩之后会话记录里保存的是压缩后的历史。真实 SDK 会在记录里留下 `compact_boundary`，resume 时从边界之后加上总结继续。

---

## 完成标准

- `make test` 全部通过（22 个测试），`go test -race` 通过；
- 练习 1 的四个问题能讲清楚，尤其是第 2、3 题；
- 练习 2 写完，并发 `Consume` 在 `-race` 下稳定通过，“进程重启”的测试只依赖两个 store；
- 能默写六步权限判定，并说出 hook 的 `allow` 跳不过哪几步；
- 能说清 `allowed_tools`、`disallowed_tools`、`tools`、权限模式各自管什么，以及 `canUseTool` 什么时候会被绕过；
- 能对比三种审批方式：LangGraph 的 `interrupt`、OpenAI 的 `needs_approval` + `RunState`、Claude 的 `canUseTool` / `defer`，说出各自暂停在哪、状态放在哪、恢复时重跑什么；
- 自测题 12 道能不看答案讲出来，尤其是第 4、5、7、8、10 题。

下一课是 4.4 Go 生态：Eino（CloudWeGo）、Genkit Go 和官方 MCP Go SDK。前三课用 Go 重写了三个 Python / TS 框架的核心，下一课看 Go 原生的框架怎么做同样的事。
