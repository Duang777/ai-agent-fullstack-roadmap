# 3.3 多智能体：Supervisor、Handoff、Swarm

> 第 3 部分 · 第 3 课 ｜ 预计 3–4 小时 ｜ 前置：3.1 ReAct、3.2 Agent Loop、1.6 上下文工程
>
> 配套练习：`projects/19-multiagent/`（纯 Go 标准库，12 个测试）。一个可以跑多个 agent 的运行时，支持两种基本编排方式：**Supervisor**（把 agent 包成工具，主管全程掌控，子 agent 在隔离的上下文里运行，可以并行）和 **Handoff**（用 `transfer_to_<名字>` 工具把对话交给另一个 agent，接手的 agent 直接回答用户）；**Swarm** 是 Handoff 加上“活跃 agent 跨轮保留”。另外还有：所有 agent 共享的预算和按 agent 记账、转交时的历史过滤、一条消息只认第一次转交、防止来回转交的上限、子 agent 的嵌套深度上限。

## 读前说明

到 3.2 为止，我们只有一个 agent：一套系统提示、一组工具、一段历史。任务一复杂，这个 agent 会遇到三个问题：

- 工具太多，选错工具的概率上升；
- 不同领域的知识和规则都塞进同一个提示，互相干扰；
- 子任务明明可以同时做，却只能一个一个串行。

多智能体是解决这些问题的一种办法，但不是唯一的办法，代价也不小。先看几个问题：

- “主管调用专家”和“把对话转给专家”有什么区别？分别适合什么场景？
- 子 agent 应该看到主管的完整历史，还是只看到一句任务描述？
- 用户问完退款又问“那运费呢”，第二句话该发给谁？
- 两个 agent 互相转交，停不下来怎么办？
- 多智能体比单 agent 贵多少？什么任务值得用？

本课结束时你能：

1. 说清 Supervisor、Handoff、Swarm 三种模式的控制流，以及各自适合的场景；
2. 实现 agents-as-tools：上下文隔离、并行执行、嵌套深度限制；
3. 实现 Handoff：转交工具、历史过滤、多次转交和来回转交的处理；
4. 给多智能体系统加共享预算和按 agent 记账，并判断一个任务该不该拆成多个 agent。

**关于来源**：OpenAI Agents SDK 文档的 *Handoffs*、*Agent orchestration*；LangChain 文档的 *Multi-agent*；Anthropic 工程博客 *How we built our multi-agent research system*；Anthropic *Building effective agents*（3.1 课已引用）。均为 2026-10 版本。

---

## 1. 先问：真的需要多个 agent 吗

LangChain 的多智能体文档开头就提醒：很多场景下，一个配好工具和提示的单 agent 就能达到类似效果。它把人们说“我需要多智能体”时真正想要的东西归成三类：

- **上下文管理**：专门的知识不必全塞进一个上下文窗口；
- **分开开发**：不同团队各自维护一个能力，边界清楚；
- **并行**：子任务交给多个 worker 同时做。

如果你要的只是第一条，Skills（单个 agent 按需加载专门的提示和知识）往往就够了，不需要多个 agent。

**代价**。Anthropic 在多智能体研究系统的文章里给了数字：agent 一般比普通聊天多用约 4 倍 token，多智能体系统约 15 倍。他们的结论是，多智能体只适合价值高到能覆盖成本的任务；需要所有 agent 共享同一个上下文、或者 agent 之间依赖很多的任务（他们举的例子是大多数编码任务）目前并不适合。

**收益**。同一篇文章里，以 Claude Opus 4 为主 agent、Claude Sonnet 4 为子 agent 的多智能体系统，在他们内部的研究评测上比单 agent 的 Claude Opus 4 高 90.2%。他们在 BrowseComp 上的分析发现，token 用量本身就能解释 80% 的表现差异，工具调用次数和模型选择是另外两个因素。也就是说，多智能体之所以有效，很大程度上是因为它能**花掉足够多的 token**：每个子 agent 有自己的上下文窗口，相当于把总容量扩大了几倍，还能并行。

所以判断标准是：任务能不能拆成**相互独立、可以并行**的部分，每部分需要大量上下文，而且结果值这么多钱。研究、调研、多源信息收集符合；修改一个代码库里互相依赖的几处，通常不符合。

---

## 2. 两种基本编排

OpenAI Agents SDK 的编排文档把最常用的模式归成两种：

| 模式 | 怎么工作 | 适合 |
|---|---|---|
| Agents as tools | 主管 agent 掌控对话，通过 `Agent.as_tool()` 调用专家 agent | 需要一个 agent 负责最终回答、综合多个专家的输出，或者在一处统一做护栏 |
| Handoffs | 分诊 agent 把对话转给专家，专家成为本轮剩下部分的活跃 agent | 希望专家直接回答用户、各自的提示保持专注 |

LangChain 文档的分类更细：Subagents（对应 agents as tools）、Handoffs、Skills、Router（先分类，再分发给一个或多个专家，最后综合）和自定义工作流。它给了一个具体对比：对“买咖啡”这种一次性请求，Handoffs、Skills、Router 都是 3 次模型调用，Subagents 是 4 次，因为子 agent 的结果要多经过一次主管。

本课把 agents as tools 叫 **Supervisor**（主管模式，Anthropic 文章里叫 orchestrator-worker），把在 Handoff 之上让活跃 agent 跨轮保留的做法叫 **Swarm**。Swarm 这个名字来自 OpenAI 早期的实验库 Swarm（后来演进成 Agents SDK）和 LangGraph 的 langgraph-swarm。

画成控制流：

```text
Supervisor                         Handoff / Swarm
用户 ⇄ 主管                         用户 ⇄ 分诊 ──转交──▶ 退款 ⇄ 用户
        ├─▶ 专家 A（隔离上下文）              （下一轮仍然是退款）
        ├─▶ 专家 B（可并行）
        └── 主管综合，回答用户
```

---

## 3. Supervisor：把 agent 包成工具

### 3.1 实现

在主管眼里，子 agent 就是一个普通工具，参数是一段任务描述，返回值是子 agent 的最终回答：

```go
func (rt *Runtime) AsTool(name string) Tool {
	return func(ctx context.Context, args string) (string, error) {
		var in struct {
			Task string `json:"task"`
		}
		if err := json.Unmarshal([]byte(args), &in); err != nil || in.Task == "" {
			return "", fmt.Errorf("参数要是 {\"task\": \"...\"}")
		}
		d := depth(ctx) + 1
		if rt.MaxDepth > 0 && d > rt.MaxDepth {
			return "", ErrTooDeep
		}
		ctx = context.WithValue(ctx, depthKey{}, d)

		sub := *rt.Agents[name]
		sub.Handoffs = nil
		h := []Message{{Role: User, Content: in.Task}}
		// …… 3.1 的 ReAct 循环，跑到不再调用工具，返回最后一条消息的内容
	}
}
```

几个设计决定：

**上下文隔离**（开头第二个问题）。子 agent 的历史只有一条：主管写的任务描述。它看不到主管和用户的对话，也看不到其他子 agent 在做什么（`TestSupervisorIsolatesContext`）。这正是多智能体能“扩大上下文容量”的原因：每个子 agent 用自己的窗口专心处理一部分，结束后只把压缩过的结论交回去，中间几十次搜索的原始结果不会进入主管的上下文。

代价是：子 agent 只知道任务描述里写了的东西。Anthropic 的经验是，主 agent 最初只给“研究一下半导体短缺”这种一句话指令，子 agent 就会重复工作、留下空白、或者理解成别的任务。他们后来要求每个子任务描述都包含：**目标、输出格式、该用哪些工具和来源、明确的任务边界**。所以任务描述是这个模式里最重要的“接口”，主管的提示要专门教它怎么写。

**子 agent 不能转交**。`sub.Handoffs = nil`：作为工具运行的 agent 只能把结果交回给调用它的主管，不能把对话转走，否则主管会失去控制。测试检查子 agent 看到的工具列表里没有 `transfer_to_*`。

**嵌套深度**。子 agent 也可以有自己的 `ask_*` 工具，再派子 agent。不加限制，模型可能一层套一层（甚至递归调用自己）。深度用 `context.Value` 传递，超过 `MaxDepth` 返回 `ErrTooDeep`。这个错误和普通工具错误一样，变成上层看到的观察结果，由上层决定怎么办（`TestSupervisorDepthAndBadArgs`）。

### 3.2 并行

主管在一条消息里发出多个 `ask_researcher` 调用，运行时就并行执行它们，结果按调用顺序放回历史（3.1 练习 2 的并行工具执行在这里直接复用）：

```go
sem := make(chan struct{}, limit)
for i, c := range calls {
	wg.Add(1)
	sem <- struct{}{}
	go func(i int, c ToolCall) {
		defer wg.Done()
		defer func() { <-sem }()
		defer func() {
			if r := recover(); r != nil {
				out[i] = toolMsg(c, a, "工具崩溃", true)
			}
		}()
		...
		out[i] = toolMsg(c, a, s, false)
	}(i, c)
}
wg.Wait()
```

`TestSupervisorParallelSubagents` 让 4 个子 agent 各睡 30 毫秒，总耗时要在 100 毫秒以内，同时运行的峰值至少是 2，结果按 1、2、3、4 的顺序拼接。

Anthropic 的研究系统做了两层并行：主 agent 同时启动 3–5 个子 agent，每个子 agent 同时调用 3 个以上工具。他们报告这让复杂查询的研究时间最多缩短 90%。

他们也指出了同步执行的瓶颈：主 agent 要等这一批子 agent **全部**完成才能继续，一个慢的子 agent 拖住所有人，主 agent 也没法在中途调整方向。异步执行能解决，但结果协调、状态一致和错误传播都会更复杂。练习里用的是同步版本。

### 3.3 别玩“传话游戏”

子 agent 的结论要经过主管转述才能到用户，每转述一次都可能丢细节或走样。Anthropic 的做法是让子 agent 把成果直接写进外部存储（文件系统之类），只把一个轻量的引用交回给主管。这样既省主管的上下文，也保证原文不被改写。这是练习 3。

---

## 4. Handoff：把对话交出去

### 4.1 转交就是一个工具调用

OpenAI Agents SDK 把转交表示成给模型的工具：转交给名为 `Refund Agent` 的 agent，工具名就是 `transfer_to_refund_agent`。

```go
func HandoffTool(name string) string {
	return handoffPrefix + strings.ReplaceAll(strings.ToLower(name), " ", "_")
}
```

模型调用这个工具时，运行时做三件事：

1. 给这次调用补一条工具结果“已转交给 X”，保持调用和结果配对（2.1 课：缺了结果，下一次请求会被拒）；
2. 把活跃 agent 换成 X；
3. 用 X 的系统提示、X 的工具继续循环。历史默认原样交给 X（`TestHandoffSwitchesActiveAgent` 检查 Refund 看到了用户原话和“已转交给 Refund”）。

每个 agent 只看到自己的工具和自己能转交的目标：Refund 看到的是 `lookup_order` 和 `transfer_to_triage`，看不到 FAQ。这本身就缓解了“工具太多选错”的问题。

SDK 里每个转交还能配置：`on_handoff` 回调（一转交就开始预取数据）、`input_type`（让模型在转交时附带一小段结构化元数据，比如原因、语言、优先级、摘要）、`input_filter`（过滤下一个 agent 看到的历史）、`is_enabled`（运行时动态开关）。SDK 文档还建议在 agent 的提示里说明转交机制，并提供了推荐的提示前缀。

### 4.2 历史过滤

接手的 agent 默认看到完整对话。有时这不合适：分诊阶段的内部工具结果对 FAQ agent 没用，还可能泄露不该给它看的数据。SDK 提供了 `handoff_filters.remove_all_tools`，转交时去掉历史里所有工具相关的条目：

```go
func RemoveTools(h []Message) []Message {
	var out []Message
	for _, m := range h {
		if m.Role == ToolRole {
			continue
		}
		if len(m.Calls) > 0 {
			if m.Content == "" {
				continue
			}
			m.Calls = nil
		}
		out = append(out, m)
	}
	return out
}
```

注意第二个分支：一条 assistant 消息既有文字又有工具调用时，保留文字、去掉调用；只有调用没有文字时整条去掉（`TestHandoffFilterRemovesTools`）。SDK 文档特别提醒：这个过滤器只去掉**结构化**的工具条目，如果工具参数或结果已经被复制进普通消息里（比如 assistant 在文字里转述了查询结果），它不会处理，要脱敏就得自己写过滤器。

过滤是 1.6 课上下文工程在多智能体里的直接应用。LangChain 文档的说法是，多智能体设计的核心就是上下文工程：决定每个 agent 看到什么。

### 4.3 一条消息里多次转交

模型可能在同一条消息里发出多个调用：转给 Refund、转给 FAQ、再调一个普通工具。运行时的规则：

- **只认第一个转交**，后面的转交返回错误结果“本轮已经转交给 Refund”；
- 转交目标不在列表里的，返回错误结果（模型编造了一个 `transfer_to_billing`）；
- 同一条消息里的**普通工具一律不执行**，返回“未执行：本轮已转交”。

最后一条值得多想一下：模型一边转交、一边调用 `delete_all`，如果照样执行，就是一个已经决定放手的 agent 在做有副作用的操作，而接手的 agent 对此一无所知（`TestHandoffSkipsOtherTools`）。不管执行还是不执行，**每个调用都要有结果**（`TestOnlyFirstHandoffWins` 检查三个调用对应三条结果）。

### 4.4 来回转交

开头第四个问题。分诊觉得是退款问题，退款觉得应该先分诊，两个 agent 互相 `transfer_to_*`，每次都是一次模型调用。`MaxHandoffs` 限制每次 `Run` 的转交次数，超过返回 `ErrTooManyHandoffs`（`TestPingPongLimited`）。`MaxSteps` 也会兜底，但单独的转交上限能给出更明确的错误，方便排查是哪两个 agent 在踢皮球（看 `Result.Path`）。

根本的修法在提示：每个 agent 的转交描述要写清楚什么情况转、什么情况不转。OpenAI 的 SDK 允许给 agent 写 `handoff_description`，会附加在转交工具的描述里，帮助模型判断什么时候选它。

---

## 5. Swarm：活跃 agent 跨轮保留

开头第三个问题。用户问完退款，又问“那运费呢”。

- **每轮都从分诊开始**：分诊再判断一次，多一次模型调用，而且可能判断错；
- **从上次接手的 agent 开始**：退款 agent 直接回答，必要时再转交出去。

Swarm 选后者。实现上只是把活跃 agent 存在会话状态里：

```go
type Session struct {
	Active  string
	History []Message
}
```

`Run` 从 `s.Active` 开始，转交时更新它。`TestSwarmKeepsActiveAcrossTurns`：第一轮 Triage 转给 FAQ，第二轮直接由 FAQ 回答，`Path` 只有一个元素，Triage 一共只被调用一次。

这就是去中心化：没有一个主管一直坐镇，每个 agent 自己决定继续还是交出去。好处是专家可以直接、连续地和用户对话（LangChain 的对比表里，Handoffs 和 Skills 在“直接和用户交互”这一项上是五星，Subagents 是一星）；坏处是没有一个地方能看全局，护栏和最终质量把关也分散在各个 agent 里。

会话状态要持久化（模块 05 的状态与记忆）：服务重启后，用户下一句话还应该发给同一个 agent。

---

## 6. 共享预算和记账

开头最后一个问题已经回答了“贵多少”。工程上要做的是：

- **所有 agent 共用一份预算**。子 agent 各自有预算的话，主管并行派出 10 个子 agent，总花费就是 10 倍，而且没有一个地方能喊停；
- **按 agent 记账**。不知道钱花在谁身上，就没法优化：是主管反复派任务，还是某个子 agent 在一个问题上搜了五十次？

```go
func (m *Meter) Add(agent string, u Usage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	...
	m.total += n
	m.perAgent[agent] += n
	if m.Max > 0 && m.total > m.Max {
		return fmt.Errorf("%w：已用 %d，上限 %d", ErrBudget, m.total, m.Max)
	}
	return nil
}
```

`Meter` 被并行的子 agent 同时写，必须加锁（`-race` 会抓到）。`TestSharedBudgetStopsEveryone`：上限 1200，主管第一次调用 500，两个并行的子 agent 各 500，第二个子 agent 记账时超限，它的错误变成主管看到的工具结果；主管下一次调用记账后，整个运行停止。和 3.2 一样，这是软上限，要做硬上限就用 3.2 练习 2 的预留。

Anthropic 还提到了一个常见的失败：早期版本的主 agent 会为简单问题过度投入，派出一大堆子 agent。他们的解法是在提示里写明投入规则：简单的事实查询只要 1 个 agent、3–10 次工具调用；直接比较需要 2–4 个子 agent、每个 10–15 次调用；复杂研究可能要 10 个以上职责清楚的子 agent。预算是硬兜底，提示里的规则才是让钱花在刀刃上的办法。

---

## 7. 选哪个

| 你的需求 | 选 |
|---|---|
| 任务能拆成独立的几块、可以并行，最后要综合成一个回答 | Supervisor |
| 不同领域的专家要直接和用户对话，各自的提示和工具差别很大 | Handoff |
| 在 Handoff 基础上，用户会围绕同一个领域连续追问 | Swarm |
| 只是知识太多，一个 agent 的工具和流程其实够用 | 不用多智能体，用 Skills 按需加载 |
| 步骤固定、可以预先写死 | 不用 agent，用工作流（3.1 课 Anthropic 的 Workflow / Agent 区分） |

这些模式可以组合：Supervisor 的某个子 agent 内部可以是一个 Router，Handoff 接手的专家也可以把子任务派给自己的 worker。

---

## 本课小结

1. 先问需不需要：多智能体约 15 倍于聊天的 token，只适合可并行、上下文需求大、价值高的任务。它有效主要是因为能花掉足够多的 token。
2. Supervisor（agents as tools）：主管全程掌控，子 agent 在隔离上下文里运行，只交回结论；任务描述要写清目标、输出格式、工具和来源、边界。
3. 子 agent 不能转交，嵌套深度要限制；同一条消息里的多个子 agent 并行执行，结果按调用顺序放回。
4. Handoff：转交是 `transfer_to_<名字>` 工具，补上工具结果后切换活跃 agent；接手的 agent 默认看到完整历史，可以用过滤器去掉工具条目。
5. 一条消息只认第一个转交，同一条消息里的其他工具不执行，但每个调用都要有结果；来回转交要有上限。
6. Swarm：活跃 agent 存在会话里，跨轮保留，下一句话直接给上次接手的 agent。
7. 所有 agent 共享一份预算并按 agent 记账；Meter 要并发安全；在提示里写明按任务难度投入多少 agent。

---

## 练习（今天做完）

### 练习 1：跑通并读懂

```bash
cd projects/19-multiagent
make test
```

回答：

1. 把 `AsTool` 里的 `sub.Handoffs = nil` 删掉，哪个测试会失败？在生产里会出现什么问题？
2. `TestHandoffSkipsOtherTools` 里，如果同一条消息中的 `delete_all` 照常执行，会有什么风险？为什么还要给它返回一条结果？
3. 对“帮我比较 A、B、C 三家公司的融资历史”和“我要退款”这两个请求，你分别会选哪种模式？为什么？

### 练习 2：带元数据的转交

参考 Agents SDK 的 `input_type` 和 `on_handoff`。

要求：给 `Runtime` 加 `OnHandoff func(from, to string, m HandoffMeta)`，`HandoffMeta` 有 `Reason`、`Summary` 两个字段。设置了回调时，转交调用的参数必须是 `{"reason": "...", "summary": "..."}`，`reason` 不能为空；不合法的转交不生效，返回错误结果让模型重试。合法时先调用回调，再切换。

再写一个过滤器工厂 `SummaryOnly(summary func() string) Filter`：转交后，接手的 agent 只看到用户最后一句话和一条“前情摘要”，用来在长对话里省 token。

测试：第一次转交参数是 `{}`，被拒绝，活跃 agent 不变；第二次带上原因后成功，回调收到原因，`Path` 只有两个元素。

### 练习 3：用外部存储代替传话

要求：实现一个并发安全的 `ArtifactStore`（`Put(agent, content string) string` 返回 `artifact://<id>`，`Get(ref string) (string, bool)`）。给子 agent 一个 `save_artifact` 工具；给主管一个 `read_artifact` 工具；最终回答里出现的 `artifact://` 引用，在 `Result` 里展开成原文（加一个 `Artifacts map[string]string` 字段）。

测试：两个并行的子 agent 各保存一份 2000 字的报告，只把引用交回；主管的历史里没有报告原文（只有引用），最终回答引用了两份报告，`Result.Artifacts` 里能拿到完整原文。

---

## 自测题

1. 人们说“需要多智能体”时，通常想要的是哪三种能力？哪一种往往不需要多个 agent 就能解决？
2. Anthropic 的研究系统里，多智能体比单 agent 好多少？他们认为主要原因是什么？token 用量大约是聊天的多少倍？
3. Agents as tools 和 Handoffs 的控制流有什么区别？各适合什么场景？
4. Supervisor 模式里，子 agent 为什么只看任务描述？代价是什么，怎么弥补？
5. 为什么作为工具运行的子 agent 不能转交？嵌套深度为什么要限制？
6. 转交工具调用之后，运行时要做哪三件事？
7. `remove_all_tools` 过滤了什么、没过滤什么？
8. 一条消息里有多个转交、或者转交加普通工具时，应该怎么处理？为什么？
9. Swarm 和普通 Handoff 的区别是什么？有什么好处和坏处？
10. 为什么要共享预算、按 agent 记账？除了预算，还有什么办法防止主 agent 过度投入？

---

# 参考答案与详解

## 自测题答案

**1.** 上下文管理（专门的知识不必全塞进一个窗口）、分开开发（不同团队维护不同能力）、并行（多个 worker 同时做子任务）。只为上下文管理的话，单 agent 加按需加载的 Skills 往往就够了，LangChain 文档也提醒单 agent 配好工具和提示常能达到类似效果。

**2.** 以 Claude Opus 4 为主 agent、Sonnet 4 为子 agent 的系统，在内部研究评测上比单 agent Opus 4 高 90.2%。主要原因是能花掉足够多的 token：在 BrowseComp 上 token 用量单独解释了 80% 的表现差异，工具调用次数和模型选择是另外两个因素；多个子 agent 各有自己的上下文窗口，相当于扩大容量并且并行。agent 约是聊天的 4 倍 token，多智能体约 15 倍。

**3.** Agents as tools：主管始终是活跃 agent，调用专家拿回结果，自己综合后回答，适合需要一个地方负责最终回答、综合多方输出、统一护栏的场景。Handoffs：分诊把对话交出去，专家成为活跃 agent，直接回答用户，适合专家需要直接、连续地和用户对话，各自提示保持专注的场景。

**4.** 隔离让每个子 agent 用自己的窗口专注处理一部分，中间的大量搜索结果不进入主管的上下文，只交回压缩后的结论，这是多智能体扩大容量的来源。代价是子 agent 只知道任务描述里写了的东西，描述太短就会重复工作、留下空白或理解错。弥补办法是要求任务描述包含目标、输出格式、可用的工具和来源、明确的任务边界，并在主管提示里专门教它怎么写。

**5.** 子 agent 是作为主管的一个工具调用运行的，结果必须交回给主管；如果它能转交，对话就被转走，主管失去控制，主管的那次工具调用也拿不到结果。嵌套不限制，模型可能层层派子 agent 甚至递归调用自己，成本和延迟失控，所以用 `context` 传递深度，超过上限返回错误。

**6.** 给转交调用补一条工具结果（“已转交给 X”），保持调用和结果配对；把活跃 agent 切换成 X；用 X 的提示和工具继续循环，历史默认原样交给 X（如果配了过滤器就先过滤）。

**7.** 它去掉历史里结构化的工具条目：工具调用和工具结果。它不处理已经被复制进普通消息里的工具参数或结果（比如 assistant 在文字里转述了查询结果），也不处理嵌套历史摘要里的内容；需要脱敏就得自己写过滤器。

**8.** 只认第一个转交，后续转交返回错误结果；目标不在可转交列表里的返回错误结果；同一条消息里的普通工具不执行，返回“未执行”。原因：活跃 agent 只能有一个；一个已经决定交出对话的 agent 不应该再执行有副作用的操作，而接手的 agent 对此一无所知。每个调用都要有结果，否则调用和结果不配对，下一次请求会被拒绝。

**9.** Swarm 把活跃 agent 存在会话状态里，跨轮保留：下一句话直接发给上次接手的 agent，而不是每轮都从分诊开始。好处是省掉重复分诊的模型调用和误判，专家可以连续和用户对话。坏处是没有中心，看不到全局，护栏和质量把关分散在各个 agent；会话状态还要持久化。

**10.** 各自独立的预算无法限制总花费：并行派出 10 个子 agent 就是 10 倍，而且没有一个地方能喊停。按 agent 记账才能知道钱花在哪里、该优化谁。除了预算这个硬兜底，还要在主 agent 的提示里写明投入规则（Anthropic：简单事实查询 1 个 agent、3–10 次工具调用；直接比较 2–4 个子 agent、每个 10–15 次；复杂研究 10 个以上职责清楚的子 agent），防止它为简单问题派出一大堆子 agent。

## 练习参考答案

### 练习 1

1. `TestSupervisorIsolatesContext` 会失败：子 agent 看到的工具列表里出现了 `transfer_to_lead`。生产里，子 agent 可能调用转交，把对话从主管手里转走；而子 agent 是在一个隔离的历史里跑的，转交没有意义的对象，主管的那次 `ask_*` 调用也拿不到结果。要么出错，要么行为完全不可预期。
2. 分诊 agent 已经决定把对话交给 FAQ，却同时执行了一个有副作用的危险操作。接手的 FAQ 不知道发生过这件事，用户也没被问过，事后很难追查是谁做的。给它返回一条结果是因为每个工具调用都要有对应的结果：缺了的话，历史里的调用和结果不配对，下一次请求会被 API 拒绝，而且模型（或者之后切回来的分诊）会以为它还在执行中。
3. 比较融资历史：Supervisor。三家公司可以并行调研，每家都需要大量搜索结果（上下文需求大），最后要综合成一个对比，适合主管派三个子 agent 再汇总。退款：Handoff（或 Swarm）。分诊判断出是退款后，退款 agent 需要直接和用户来回沟通（核对订单、确认金额），后续追问也大概率还是退款相关，没必要每句话都经过一个主管转述。

### 练习 2

```go
type HandoffMeta struct{ Reason, Summary string }

// Runtime 增加：
OnHandoff func(from, to string, m HandoffMeta)

// Run 里处理转交的 default 分支：
default:
	if rt.OnHandoff != nil {
		var meta HandoffMeta
		if err := json.Unmarshal([]byte(c.Args), &meta); err != nil || meta.Reason == "" {
			results[c.ID] = toolMsg(c, active, `转交参数要是 {"reason": "...", "summary": "..."}`, true)
			continue
		}
		rt.OnHandoff(active.Name, target, meta)
	}
	next = target
	results[c.ID] = toolMsg(c, active, "已转交给 "+target, false)
```

注意 `continue` 让这次不合法的转交不占用“第一个转交”的位置，同一条消息后面合法的转交仍然可以生效。

```go
func SummaryOnly(summary func() string) Filter {
	return func(h []Message) []Message {
		var lastUser Message
		for i := len(h) - 1; i >= 0; i-- {
			if h[i].Role == User {
				lastUser = h[i]
				break
			}
		}
		return []Message{
			{Role: User, Content: "前情摘要：" + summary()},
			lastUser,
		}
	}
}
```

摘要从哪里来：最直接的是练习 2 前半部分的 `HandoffMeta.Summary`，在 `OnHandoff` 里存下来，`summary` 函数读出来。把摘要当作 user 消息而不是 system 消息，是为了不改动接手 agent 自己的系统提示。两条连续的 user 消息有的 API 不接受，适配层可以合并成一条。这种过滤会丢信息，适合长对话、话题明确切换的转交；如果接手的 agent 需要引用之前的具体内容，就不要用。

### 练习 3

```go
type ArtifactStore struct {
	mu   sync.Mutex
	n    int
	data map[string]string
}

func (s *ArtifactStore) Put(agent, content string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = map[string]string{}
	}
	s.n++
	ref := fmt.Sprintf("artifact://%s-%d", agent, s.n)
	s.data[ref] = content
	return ref
}

func (s *ArtifactStore) Get(ref string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[ref]
	return v, ok
}
```

工具：`save_artifact` 的参数是 `{"content": "..."}`，返回引用；子 agent 的提示要求它保存完整报告后，只回答“报告已保存：artifact://…，要点：……”。`read_artifact` 参数是引用，返回原文，主管只在需要细节时才读。`Run` 返回前用正则 `artifact://[\w-]+` 找出最终回答里的引用，从存储里取出原文放进 `Result.Artifacts`。

测试要点：检查主管历史里所有消息的总长度远小于两份报告的长度（证明原文没进主管上下文）；检查 `Result.Artifacts` 两个引用都能拿到 2000 字原文；用 `-race` 跑，两个子 agent 并发 `Put`。

生产里存储可以是对象存储或数据库（模块 05），引用要带权限检查：子 agent 只能读写自己任务相关的产物，用户只能看到自己会话里的产物。

---

## 完成标准

- `make test` 全部通过（12 个测试）；
- 练习 1 的三个问题能讲清楚；
- 练习 2 的 `OnHandoff`、参数校验和 `SummaryOnly` 写完，拒绝和成功两个用例都有测试；
- 练习 3 的 `ArtifactStore` 和两个工具写完，测试证明原文没有进入主管的上下文，并且用 `-race` 通过；
- 自测题 10 道能不看答案讲出来，尤其是第 2、4、8、10 题。
