# 3.1 ReAct、Plan-and-Execute、Reflexion

> 第 3 部分 · 第 1 课 ｜ 预计 3–4 小时 ｜ 前置：2.1 Function Calling、1.4 推理模型、1.6 上下文工程
>
> 配套练习：`projects/17-patterns/`（纯 Go 标准库，12 个测试，模型用脚本化的假实现）。三种模式各写一个能跑的最小版本：ReAct 循环（工具错误变观察、步数上限、取消）和论文原版文本格式的解析；Plan-and-Execute（`#E1` 引用、计划校验、按依赖分波并行、失败后带着已完成结果重新规划）；Reflexion（外部评估、反思记忆窗口、如实返回失败）。

## 读前说明

模块 02 讲的是“模型怎么调用一个工具”。从这一课开始讲“怎么把很多次调用组织成一个能完成任务的 Agent”。最基础的三种组织方式，都来自 2023 年前后的几篇论文：

- **ReAct**：想一步、做一步、看结果，再想下一步；
- **Plan-and-Execute**：先把整个计划写出来，再照着执行，出问题时重新规划；
- **Reflexion**：做完拿到反馈，失败了写一段反思，带着反思再做一遍。

现在主流 Agent（Claude Code、Codex、各家的 Deep Research）都不是“纯”的某一种，而是组合。但不懂这三种基本形状，就看不懂那些系统为什么那样设计。

先看几个问题：

- 模型已经原生支持工具调用了，还需要 ReAct 吗？“ReAct”现在指的是什么？
- 模型一直调用工具、就是不给最终答案，怎么办？
- 先规划再执行，省的是什么？丢掉的又是什么？
- 两个互不依赖的搜索，为什么要串行等两轮？
- 计划是模型写的，执行之前要检查什么？
- Reflexion 的“反思”靠什么判断自己错了？没有测试的时候还能用吗？

本课结束时你能：

1. 区分 Workflow 和 Agent，知道什么时候不需要 Agent；
2. 写一个正确的 ReAct 循环，处理工具错误、步数上限和取消；
3. 写一个 Plan-and-Execute：计划校验、依赖分析、并行执行、失败重规划；
4. 写一个 Reflexion 循环，知道它依赖什么样的反馈信号；
5. 根据任务特点选择或组合这三种模式。

**关于来源**：论文数据来自各论文 arXiv 摘要页：ReAct（2210.03629）、Plan-and-Solve（2305.04091）、ReWOO（2305.18323）、LLMCompiler（2312.04511）、Reflexion（2303.11366）；Workflow 与 Agent 的区分来自 Anthropic 的 *Building effective agents*。

---

## 1. 先问：要不要 Agent

Anthropic 的 *Building effective agents* 把这类系统分成两种：

- **Workflow**：LLM 和工具按**预先写好的代码路径**编排。比如“先分类，再按类别走不同的提示词”，“先生成，再让另一个调用评审”；
- **Agent**：LLM **自己决定**下一步做什么、调用哪个工具，控制整个过程。

Workflow 可预测、便宜、好调试；Agent 灵活，但步数、成本、结果都不确定。文章的建议是先找最简单的方案，只有在步骤真的没法事先写死时才用 Agent。

本课的三种模式正好落在这条线的不同位置：ReAct 是最“Agent”的，每一步都由模型决定；Plan-and-Execute 把决策集中到规划阶段，执行阶段接近 Workflow；Reflexion 是一个外层循环，可以套在任何一种外面。文章里的 evaluator-optimizer（一个调用生成、另一个评估并反馈，循环改进）就是 Reflexion 的 Workflow 版本。

---

## 2. ReAct

### 2.1 论文说了什么

ReAct（Reasoning + Acting）之前，“推理”（思维链）和“行动”（生成动作计划）是分开研究的。ReAct 让模型**交替**生成推理和动作：推理帮助模型制定、跟踪、更新计划和处理异常；动作让它从外部（知识库、环境）获取信息。

论文的结果：在 HotpotQA 和 Fever 上，通过一个简单的 Wikipedia API 交互，缓解了思维链常见的幻觉和错误传播；在 ALFWorld 和 WebShop 两个交互决策任务上，只用一两个示例，成功率分别比模仿学习和强化学习方法**绝对**高出 34% 和 10%。

原版是纯文本格式，模型每一步写：

```text
Thought 1: 我需要先查阿里巴巴的总部在哪。
Action 1: search[阿里巴巴 总部]
Observation 1: （由你的程序填入搜索结果）
Thought 2: 总部在杭州，可以回答了。
Action 2: finish[杭州]
```

### 2.2 现在的 ReAct

现在三家的模型都原生支持工具调用（2.1 课），推理模型还会在调用之间思考（1.4 课）。所以今天说“ReAct”，指的是这个**循环的形状**，而不是那个文本格式：

```go
func ReAct(ctx context.Context, m Model, tools map[string]Tool, task string, maxSteps int) (Trace, error) {
	h := []Message{{Role: User, Content: task}}
	for step := 1; step <= maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return Trace{Steps: step - 1, History: h}, err
		}
		msg, err := m.Next(ctx, h)
		...
		h = append(h, msg)
		if len(msg.Calls) == 0 {
			return Trace{Steps: step, Answer: msg.Content, History: h}, nil
		}
		for _, c := range msg.Calls {
			h = append(h, runTool(ctx, tools, c))
		}
	}
	return Trace{Steps: maxSteps, History: h}, ErrMaxSteps
}
```

逐段看：

- **每一步先检查 `ctx`**：用户取消、请求超时时立即停（`TestReActCancel`）。
- **没有工具调用就是最终回答**：这是循环唯一的正常出口。Thought 现在要么在模型的推理内容里，要么在调用前的文字里，不需要你解析。
- **每个调用都要有一个结果**，按顺序放回历史，`CallID` 对上。少一个，下一次请求会被 API 拒绝（2.1 课）。`TestReActLoop` 检查第二次请求时模型看到的历史是 4 条：任务、调用、两个结果。
- **步数上限**：开头第二个问题。模型可能一直查、一直改，永远不收尾。`maxSteps` 是最后一道防线，到了就返回 `ErrMaxSteps` 和已有历史（`TestReActMaxSteps`）。下一课 3.2 会把它扩展成完整的停止条件和预算。

### 2.3 工具错误是观察，不是异常

```go
func runTool(ctx context.Context, tools map[string]Tool, c ToolCall) Message {
	t, ok := tools[c.Name]
	if !ok {
		return Message{..., IsError: true, Content: "没有这个工具：" + c.Name + "，可用的有：" + names(tools)}
	}
	out, err := t(ctx, c.Args)
	if err != nil {
		return Message{..., IsError: true, Content: err.Error()}
	}
	...
}
```

ReAct 论文强调推理能“处理异常”，前提是模型**看得到**异常。工具报错时，如果你的循环直接 `return err`，模型就没有机会换个参数重试。`TestReActToolErrorIsObservation` 里模型先把“北京”写成“北平”，再把工具名拼错，两次都从错误信息里学到了正确做法，第三次成功。工具名错误时顺便列出可用工具，模型下一步几乎都能改对。

真正要中断循环的只有：`ctx` 取消、模型 API 本身出错、步数或预算用完。

### 2.4 文本格式为什么被淘汰

练习里保留了一个 `ParseText`，解析论文的文本格式，用来体会它的问题：

- 模型经常**顺手把 Observation 也编出来**，后面还接着写下一步。解析时必须在 `\nObservation` 处截断，否则你会把模型编的结果当成真的（`TestParseText` 第一个用例）。原版实现靠 `stop` 参数在这里截断；
- 少写一个括号、`Action` 写成 `Act`、一步写两个 Action，都要你兜底，再把格式错误回给模型；
- 参数是一个字符串，复杂参数要自己设计格式。

原生工具调用把这些问题交给了模型训练和 API 的 schema（1.3 课的约束解码）。如果你用的模型不支持工具调用（一些小的开源模型），还得回到文本格式，这时 `ParseText` 的几个坑都要处理。

### 2.5 ReAct 的代价

- **串行**：每次工具调用都要等一轮模型推理。三个互不依赖的搜索，如果模型一次只调一个，就是三轮。现在的模型可以在一条消息里发多个调用（并行工具调用），能缓解一部分；
- **上下文越来越长**：每一步都把全部历史发回去（1.5 课的缓存能省钱，但窗口还是会满，1.6 课）；
- **容易绕圈**：同一个工具、同样的参数反复调。练习 3 处理这个问题；
- **短视**：每次只想下一步，长任务容易偏离目标。

---

## 3. Plan-and-Execute

### 3.1 思路的来源

几篇论文从不同角度走到了“先规划”：

- **Plan-and-Solve Prompting**：针对零样本思维链的“漏步骤”错误，让模型先制定计划把任务拆成子任务，再按计划逐个解决。它是纯提示词方法，没有工具；
- **ReWOO**（Reasoning WithOut Observation）：指出 ReAct 交替推理和观察，导致提示词大量重复、计算冗余。它把推理和观察**解耦**：Planner 一次写出完整计划，步骤之间用 `#E1`、`#E2` 引用前面的结果；Worker 执行；Solver 根据所有结果写答案。在 HotpotQA 上做到 5 倍的 token 效率，准确率还提升了 4%，工具出错时也更稳；
- **LLMCompiler**：借鉴编译器，Planner 产出带依赖的任务图，Task Fetching Unit 分发，Executor **并行**执行。和 ReAct 相比，延迟最多快 3.7 倍，成本最多省 6.7 倍，准确率最多提升约 9%。

开头第三个问题：省的是**模型调用次数和重复的上下文**（整个计划只推理一次），以及**等待时间**（互不依赖的步骤可以并行）。丢掉的是**中途调整的能力**：计划写好时还没看到任何结果，第一步的结果如果出乎意料，后面的步骤可能全错。所以需要重新规划。

### 3.2 计划的表示

练习用 ReWOO 的写法：

```go
p := Plan{
	{"E1", "search", "北京 人口"},
	{"E2", "search", "上海 人口"},
	{"E3", "llm", "比较 #E1 和 #E2"},
}
```

依赖不用单独写，从 `#En` 引用里推出来（`Step.Deps`）。E3 依赖 E1、E2；E1 和 E2 互不依赖。

### 3.3 执行之前先校验

开头第五个问题。计划是模型写的，和 2.1 课的工具参数一样不可信。`Validate` 检查四件事（`TestPlanValidate`）：

| 问题 | 不检查的后果 |
|---|---|
| ID 重复 | 结果互相覆盖，后面的引用拿到错的值 |
| 引用不存在的步骤 | 替换成空字符串，工具拿着残缺参数执行 |
| 循环依赖 | 执行器死等或死循环 |
| 不存在的工具 | 执行到一半才失败，前面的步骤白做（如果有副作用，还收不回来） |

校验失败不是直接报错给用户，而是作为失败原因交回规划器，让它修计划（`PlanAndExecute` 里的 `continue`）。

### 3.4 按依赖分波，同一波并行

```go
func (p Plan) Waves() ([][]Step, error) {
	...
	for len(left) > 0 {
		// 依赖都已完成的步骤放进这一波
		...
		if len(wave) == 0 {
			return nil, errf("计划里有循环依赖")
		}
		...
	}
}
```

这是拓扑排序的分层版本：每一波里的步骤互不依赖。一波里找不到任何就绪的步骤，说明剩下的步骤互相等待，也就是有环。

`Execute` 一波一波执行，同一波里每步一个 goroutine，用 `sync.Mutex` 保护结果表，执行前把 `#En` 替换成结果。`TestPlanWavesAndRefs` 让每次搜索耗时 150 毫秒，要求总时间小于 280 毫秒，证明 E1、E2 是并行的。这就是 LLMCompiler 延迟优势的来源。

失败处理：一波里有步骤失败，这一波等全部结束后停下，不再执行后面的波（依赖失败结果的步骤没意义），**已完成的结果保留**（`TestExecuteStopsOnFailure`）。同一波里多个失败时取 ID 最小的那个，让结果可复现。

### 3.5 重新规划

```go
for i := 1; i <= maxPlans; i++ {
	p, err := plan(ctx, task, done, failure)
	...
	res, err := Execute(ctx, p, tools)
	for k, v := range res {
		done[k] = v
	}
	if err != nil {
		failure = err
		continue
	}
	ans, err := solve(ctx, task, done)
	return ...
}
```

重新规划时把两样东西交给规划器：**已完成的结果**和**失败原因**。规划器只需要补上没做完的部分，已经做完的步骤不重跑（`TestReplanKeepsDoneWork` 检查搜索总共只调用了 3 次）。这在步骤有副作用时尤其重要：发过的邮件、下过的单，不能因为后面一步失败就再来一遍。

重新规划也要有上限（`TestReplanGivesUp`）。规划器一直写出同样错误的计划时，到次数就停，并把最后的失败原因带出来。

---

## 4. Reflexion

### 4.1 论文说了什么

传统强化学习需要大量样本和微调。Reflexion 不更新权重，而是用**语言反馈**强化：Agent 对任务反馈信号做文字反思，把反思存进一个情景记忆缓冲区，下一次尝试时带上它。反馈可以是标量或自由文本，可以来自外部或内部模拟。它在 HumanEval 编程基准上 pass@1 达到 91%，超过当时 GPT-4 的 80%。

### 4.2 循环

```go
for t := 1; t <= maxTrials; t++ {
	attempt, err := act(ctx, task, mem)     // 带着反思做
	fb, err := eval(ctx, attempt)           // 外部评估
	if fb.Pass {
		return ...
	}
	if t == maxTrials {
		break                                // 最后一次失败后不用再反思
	}
	r, err := reflect(ctx, task, attempt, fb)
	mem = append(mem, r)
	if len(mem) > memSize {
		mem = mem[len(mem)-memSize:]         // 只留最近几条
	}
}
return ReflexionRun{Answer: last, Trials: maxTrials, Reflections: mem}, nil
```

三个设计点：

- **反思是一段具体的文字**，不是“再试一次”。“上次 `Abs(-3)` 返回了 -3，负数分支没处理”比“上次错了”有用得多。反思器拿到的是尝试本身和评估的细节；
- **记忆窗口**：论文里只保留 1 到 3 条。全留的话提示词越来越长，而且早期的反思可能已经被修正过，留着反而误导。`TestReflexion` 用 `memSize=1`，第三次尝试只看到第二次的反思；
- **如实失败**：次数用完仍未通过，返回最后一次结果和 `Pass=false`，不要假装成功（`TestReflexionHonestFailure`）。

### 4.3 关键是评估信号

开头最后一个问题。Reflexion 在编程任务上效果好，是因为**单元测试是可靠的外部信号**：通过就是通过，失败的测试名和报错就是现成的反馈细节。

没有可靠信号时，让模型自己评估自己，效果会大打折扣：它很可能认为自己第一次就对了，或者被“反思”带着把对的改成错的。Anthropic 对 evaluator-optimizer 的建议也是同一个意思：适合有清晰评估标准、迭代改进能带来可衡量提升的场景。

常见的可靠信号：单元测试和编译器、类型检查和 lint、JSON Schema 校验（1.3 课）、SQL 能否执行、和已知答案比对、工具返回的明确错误。模块 06 讲的 LLM-as-Judge 也可以当信号，但要先校准。

---

## 5. 怎么选，怎么组合

| | ReAct | Plan-and-Execute | Reflexion |
|---|---|---|---|
| 决策时机 | 每一步 | 主要在开头，失败时重排 | 每次尝试之后 |
| 模型调用次数 | 多（每步一次） | 少（规划 + 求解） | 尝试次数 × 每次的成本 |
| 并行 | 只有同一条消息里的多个调用 | 天然支持 | 不涉及 |
| 适应意外 | 强 | 弱，靠重新规划 | 靠下一次尝试 |
| 适合 | 探索型、下一步取决于上一步结果 | 步骤可以预见、有大量独立子任务 | 有可靠评估信号、允许重试 |
| 主要风险 | 绕圈、偏题、上下文膨胀 | 计划和现实脱节 | 没有好信号时越改越错 |

实际系统几乎都是组合：

- **Coding Agent**：外层 ReAct（读代码、改代码、跑命令），内部用测试结果做 Reflexion 式的反馈，长任务里先写一份待办清单（轻量的规划），做完一项勾一项；
- **Deep Research**：先规划研究提纲和查询（Plan），并行搜索（LLMCompiler 式），每个子问题内部用 ReAct 追问，最后汇总；
- **规划作为工具**：在 ReAct 里给模型一个“更新计划”的工具，计划写进上下文，模型随时修改。兼顾了全局视野和灵活性。

3.6 和 3.7 课会拆解 Coding Agent 和 Deep Research Agent 的具体架构。

---

## 本课小结

1. 先判断要不要 Agent：步骤能写死就用 Workflow，更便宜、更可控。
2. ReAct 今天指循环的形状：模型决定调用工具或给出最终回答，没有调用就是唯一的正常出口。
3. 循环要做对：每步检查取消，每个调用都有结果，工具错误变成观察回给模型，步数上限兜底。
4. 文本格式的 ReAct 要截断模型编造的 Observation，原生工具调用免掉了这些麻烦。
5. Plan-and-Execute 省调用、省重复上下文、能并行，代价是中途适应能力；ReWOO 用 `#En` 引用，LLMCompiler 按依赖并行。
6. 计划执行前必须校验：重复 ID、悬空引用、循环、未知工具；失败时带着已完成结果和失败原因重新规划，不重跑做完的步骤。
7. Reflexion 靠具体的语言反思和有限的记忆窗口，次数用完要如实失败；效果取决于评估信号是否可靠。
8. 实际系统是组合：ReAct 做外层，规划做全局，测试做反馈。

---

## 练习（今天做完）

### 练习 1：跑通并读懂

```bash
cd projects/17-patterns
make test
```

回答：

1. 把 `runTool` 改成工具出错就让 `ReAct` 直接返回错误，`TestReActToolErrorIsObservation` 会怎样？在真实使用里，用户会看到什么？
2. `Execute` 里如果不等整波结束、某一步一失败就立即返回，会有什么问题？（提示：其他 goroutine 还在写 `results`。）
3. `TestReflexion` 里如果 `memSize` 改成 5，第三次尝试会看到什么？什么情况下这样反而更好？

### 练习 2：ReAct 并行执行工具

现在同一条消息里的多个调用是串行执行的。

要求：改成并行执行，但放回历史的顺序必须和调用顺序一致；给并发数一个上限（例如 4）；某个调用 panic 时不能拖垮整个循环，要变成一条 `IsError` 的结果。

测试：一条消息里 3 个调用，每个耗时 100 毫秒，总时间小于 200 毫秒，结果顺序与调用顺序一致；其中一个工具 panic，另外两个正常返回，循环继续。

### 练习 3：绕圈检测

模型有时会用同样的参数反复调用同一个工具。

要求：在 `ReAct` 里记录每个（工具名，参数）出现的次数。第 2 次重复时，在结果后面附加一句提示：“你已经用相同参数调用过这个工具，结果不会变，请换个做法或给出回答”；第 3 次重复时不再执行，直接返回 `ErrLoop`。

测试：脚本让模型连续 3 次 `weather(北京)`，第二次的结果里能看到提示，第三次返回 `ErrLoop`，工具实际只执行了 2 次。

---

## 自测题

1. Workflow 和 Agent 的区别是什么？什么时候不该用 Agent？
2. ReAct 论文的核心思想是什么？在哪些任务上取得了什么效果？
3. 有了原生工具调用，ReAct 循环还剩下哪些需要你写对的地方？
4. 为什么工具错误要变成观察结果？哪些情况下应该真的中断循环？
5. 文本格式的 ReAct 有哪些坑？为什么要在 Observation 处截断？
6. Plan-and-Execute 相比 ReAct 省了什么、丢了什么？ReWOO 和 LLMCompiler 各自的贡献是什么？
7. 模型写的计划执行前要检查哪些问题？各自不检查会怎样？
8. 怎么从计划里找出可以并行的步骤？怎么发现循环依赖？
9. 重新规划时要给规划器哪些信息？为什么不能简单地从头再来？
10. Reflexion 依赖什么？为什么记忆只保留几条？没有可靠评估信号时会怎样？

---

# 参考答案与详解

## 自测题答案

**1.** Workflow 是 LLM 和工具按预先写好的代码路径编排；Agent 是 LLM 自己决定下一步和用哪个工具。步骤可以事先写死、对成本和延迟敏感、需要高度可预测时不该用 Agent，比如固定的“抽取 → 校验 → 入库”。

**2.** 让模型交替生成推理和动作：推理帮助制定和更新计划、处理异常，动作从外部获取信息、纠正幻觉。在 HotpotQA、Fever 上缓解了思维链的幻觉和错误传播；在 ALFWorld、WebShop 上只用一两个示例，成功率比模仿学习和强化学习方法绝对高出 34% 和 10%。

**3.** 每步检查取消；判断有没有工具调用来决定是否结束；每个调用都要有对应 ID 的结果并按顺序放回；工具错误转成观察；未知工具时告诉模型可用的有哪些；步数（以及下一课的预算）上限；返回完整历史便于调试。

**4.** 模型只有看到错误才能调整参数或换工具，这正是 ReAct “处理异常”的能力所在；直接中断等于浪费了前面所有步骤。应该中断的是：用户取消或超时（`ctx`）、模型 API 本身失败、步数或预算耗尽，以及安全策略拒绝的操作（模块 10）。

**5.** 模型会编造 Observation 并继续往下写；格式错误（缺括号、拼错关键字、一步多个 Action）要兜底并回给模型；参数只能是字符串。截断是因为 Observation 必须来自真实工具执行，模型编的内容当真会直接产生幻觉答案。

**6.** 省了模型调用次数和每步重复发送的上下文，互不依赖的步骤还能并行；丢了根据中间结果随时调整的能力，需要重新规划来弥补。ReWOO 把推理和观察解耦，用 `#En` 引用串起步骤，在 HotpotQA 上 5 倍 token 效率、准确率提升 4%；LLMCompiler 把计划看成依赖图并行执行，相比 ReAct 延迟最多快 3.7 倍、成本最多省 6.7 倍、准确率最多提升约 9%。

**7.** ID 重复（结果互相覆盖）、引用不存在的步骤（参数残缺）、循环依赖（死等或死循环）、未知工具（执行到一半才失败，前面有副作用的步骤收不回来）。还可以加上参数 schema 校验（2.1 课）和权限检查。

**8.** 从引用推出依赖，然后分层拓扑排序：每一轮把依赖都已完成的步骤放进同一波，同一波内可以并行。某一轮剩下的步骤里一个就绪的都没有，就说明它们互相等待，存在环。

**9.** 已完成步骤的结果和失败原因（哪一步、什么错误）。从头再来会重复执行已经成功的步骤，浪费时间和费用；如果那些步骤有副作用（发邮件、写数据库、下单），重复执行会造成真实的错误。

**10.** 依赖可靠的反馈信号（测试、编译器、校验器、已知答案）和具体的语言反思。记忆只留几条，是因为提示词会越来越长，而且旧反思可能已经过时或被修正，留着会误导。没有可靠信号时，模型自评容易认为自己是对的，或者把正确答案改错，循环只会多花钱。

## 练习参考答案

### 练习 1

1. 测试会失败：第一次 `weather(北平)` 报错后 `ReAct` 直接返回，模型没机会改成“北京”。真实使用中，用户看到的是“出错了”，而模型本来下一步就能自己修好。
2. 其他 goroutine 还在运行并写 `results`，函数已经返回、调用方开始读这个 map，就是数据竞争（`-race` 会报）；而且那些还在跑的工具调用没人等，结果丢失，副作用却已经发生。正确做法是等整波结束，或者用 `ctx` 取消其他步骤后仍然等它们退出。
3. 会看到两条反思：第一次的和第二次的。当早期反思仍然有效、而且问题需要同时注意几个方面时（比如“负数分支”和“注释要和实现一致”都要记住），多保留几条更好；代价是提示词变长，以及过时反思的干扰。

### 练习 2

```go
func runAll(ctx context.Context, tools map[string]Tool, calls []ToolCall, limit int) []Message {
	out := make([]Message, len(calls)) // 按下标写，顺序天然一致
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func(i int, c ToolCall) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					out[i] = Message{Role: ToolRole, CallID: c.ID, IsError: true, Content: fmt.Sprintf("工具崩溃：%v", r)}
				}
			}()
			out[i] = runTool(ctx, tools, c)
		}(i, c)
	}
	wg.Wait()
	return out
}
```

`ReAct` 里把逐个执行换成 `h = append(h, runAll(ctx, tools, msg.Calls, 4)...)`。每个 goroutine 只写自己下标的元素，不需要锁；`recover` 必须在同一个 goroutine 里 defer 才能接住 panic。注意：并行只适合互不影响的调用，有副作用、有先后顺序要求的工具（先建目录再写文件）要标记为串行，或者交给模型分两步调用。

### 练习 3

```go
var ErrLoop = errors.New("模型在用相同参数重复调用同一个工具")

// 在 ReAct 里，循环开始前：
seen := map[string]int{}

// 执行每个调用时：
key := c.Name + "\x00" + c.Args
seen[key]++
switch n := seen[key]; {
case n >= 3:
	return Trace{Steps: step, History: h}, ErrLoop
case n == 2:
	r := runTool(ctx, tools, c)
	r.Content += "\n（你已经用相同参数调用过这个工具，结果不会变，请换个做法或给出回答）"
	h = append(h, r)
default:
	h = append(h, runTool(ctx, tools, c))
}
```

用 `\x00` 分隔是为了避免“工具名+参数”拼接后撞车。返回 `ErrLoop` 之前，这条消息里已经发起、还没有结果的调用在历史里是不完整的；如果之后还要继续对话，要先给它补一条错误结果。对于结果会随时间变化的工具（查询任务状态、轮询），重复调用是正常的，要按工具配置是否参与检测。

---

## 完成标准

- `make test` 全部通过（12 个测试）；
- 练习 1 的三个问题能讲清楚；
- 练习 2 的并行执行写完，`-race` 下通过，顺序和 panic 两个用例都覆盖；
- 练习 3 的绕圈检测写完并有测试；
- 自测题 10 道能不看答案讲出来，尤其是第 3、6、7、10 题。
