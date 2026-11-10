# 4.1 LangGraph：状态图、持久化与 interrupt

> 第 4 部分 · 第 1 课 ｜ 预计 4–5 小时 ｜ 前置：3.2 Agent Loop、3.4 长任务 Agent、3.5 Agent 状态与数据模型
>
> 配套练习：`projects/24-langgraph/`（纯 Go 标准库，15 个测试）。用 Go 重写 LangGraph 运行时的核心：通道和 reducer；节点、静态边、条件边、`Send`、`Command`；编译期结构检查；Pregel 风格的 super-step（同一步并行、看同一份快照、步末统一合并、LastValue 冲突报错）；每个 super-step 一个 checkpoint，按 thread 组织；节点失败时的 pending writes；`interrupt` 的“抛出 + 节点重跑 + 按序号匹配恢复值”，多个 interrupt 按 ID 恢复、拒绝重复回答；重放、`update_state`、递归上限、载荷必须可序列化。

## 读前说明

模块 03 我们一直在手写：循环、状态、检查点、审批、多智能体。从这一课开始看框架。第一个是 LangGraph，因为它把前面几课的东西几乎一一做成了显式的抽象：

| 前面手写的 | LangGraph 里的名字 |
|---|---|
| 3.2 Agent Loop 的循环和停止条件 | 图 + 条件边 + 递归上限 |
| 3.3 多智能体的 Supervisor、并行 worker | 条件边路由、`Send` 扇出 |
| 3.4 检查点、审批、可恢复执行 | checkpointer、`interrupt`、`Command(resume=...)` |
| 3.5 run / step 的存储、完整重放 | checkpoint 历史、time travel |

所以这一课的目标不是背 API，而是**看懂它的运行时为什么这样设计**，以及这些设计在你自己的代码里意味着什么约束。最典型的一条：恢复时节点会从头重跑，所以 `interrupt` 之前的副作用必须幂等。不懂运行时，这条规则就只能死记。

本课依据 LangGraph 官方文档（docs.langchain.com，2026 年 11 月，Python 1.2 系列），练习用 Go 实现一个简化版运行时，只保留和设计有关的部分。

先想几个问题：

- 两个节点并行写同一个状态键，结果应该是谁的？
- 节点里调用了 `interrupt()`，恢复的时候是从那一行继续，还是从头来？
- 一个节点并行跑出三个分支，其中一个失败了，重试时另外两个要不要重跑？
- `thread_id` 相同就一定会“接着上次跑”吗？

---

## 1. 为什么用图

LangGraph 把 agent 工作流建模成三样东西：

- **State**：整个应用的共享快照，通常是一个有类型的 schema；
- **Nodes**：读状态、做事、返回更新的函数；
- **Edges**：固定或条件的转移，决定下一个运行哪个节点。

官方的一句话总结：**节点做事，边决定下一步做什么**。节点和边都是普通函数，可以调模型，也可以是任何代码。

和直接写一个 `for` 循环比，图的好处是控制流变成了数据：可以画出来、可以在编译期检查、可以在任意两步之间存档。代价是你必须按它的执行模型思考，下面几节就是这个执行模型。

---

## 2. 状态：通道和 reducer

### 2.1 通道

状态存在一组**通道**（channel）里，通道就是 schema 里的键。每个通道有一个值，并且定义“更新怎么合并进来”。节点只返回**部分更新**，不返回整个状态。

### 2.2 reducer

reducer 是一个二元函数：`new = reducer(left=当前值, right=节点的更新)`。

- **默认 reducer 是覆盖**：从 `{"foo": 1, "bar": ["hi"]}` 开始，更新 `{"foo": 2}` 得到 `{"foo": 2, "bar": ["hi"]}`；
- **自定义 reducer**：`bar: Annotated[list[str], operator.add]` 表示追加，`{"bar": ["bye"]}` 得到 `["hi", "bye"]`；
- **`add_messages`**：消息列表的专用 reducer，按消息 ID 追加或**原地更新**。只用 `operator.add` 的话，人工审阅时没法修改某条已有消息；
- **`Overwrite`**：绕过 reducer 直接替换（包括清空）；
- **重置的坑**：带合并 reducer 的字段，返回空值不会清空旧数据。错误缓冲区、重试计数这类每次要清零的字段要特别注意。

```python
from typing import Annotated
import operator
from typing_extensions import TypedDict

class State(TypedDict):
    foo: str                                  # 覆盖
    bar: Annotated[list[str], operator.add]   # 追加
```

Go 练习里对应：

```go
New().
	Channel("foo", nil).    // nil = LastValue，覆盖
	Channel("bar", Append)  // 追加
```

练习要求每个键都先声明，节点写未声明的键会报 `ErrUnknownChannel`。这模拟的是“状态有 schema”。

### 2.3 为什么 reducer 不只是语法糖

reducer 真正的作用在并行时才显现：**同一个 super-step 里多个节点写同一个键，必须有规则决定怎么合并**。有 reducer 就按 reducer 合并；没有 reducer（覆盖语义），两个写入谁赢都说不通，LangGraph 会报 `InvalidUpdateError`。练习里是 `ErrInvalidUpdate`，并在错误信息里提示“需要给它一个 reducer”。

---

## 3. 节点、边、Send、Command

### 3.1 节点

节点是同步或异步函数，可以接收 `state`、`config`（含 `thread_id`）、`runtime`（运行时上下文、store、stream writer 等）。运行时上下文用来传依赖（比如模型名、数据库连接），不放进状态。

### 3.2 边

- **普通边**：总是从 A 到 B，`add_edge("a", "b")`；
- **条件边**：调用一个路由函数决定下一个（或几个）节点，`add_conditional_edges("a", route, {...})`。路由函数读当前状态，返回节点名、`END`、名字列表（并行）或 `Send` 列表；
- **入口**：从虚拟节点 `START` 出发的边。

一个节点可以有多条出边，目标会**在下一个 super-step 并行运行**。

官方特别提醒：**一个节点要么用静态边，要么用动态路由（条件边或 `Command`），不要混用**，混用会两条路都走。练习的 `nextTasks` 就是把静态边、条件边和 `Command.Goto` 叠加起来的，所以混用的后果你可以自己试出来。

### 3.3 Send：运行时才知道数量的扇出

普通边在建图时就固定了拓扑，而且每个被选中的节点都拿到同一份共享状态。如果要“对列表里的每一项各跑一次同一个节点，每次输入不同”，就用 `Send`：

```python
from langgraph.types import Send

def continue_to_jokes(state):
    return [Send("generate_joke", {"subject": s}) for s in state["subjects"]]

graph.add_conditional_edges("plan", continue_to_jokes)
```

这就是 map-reduce，也是 3.3 课 orchestrator-worker、3.7 课主研究员派子研究员的图版本。每个 `Send` 实例**只看到自己的输入**，结果通过 reducer 合并回共享状态。

### 3.4 Command：更新和路由一起做

节点返回 `Command(update=..., goto=...)`，一次完成更新状态和指定下一跳。适合“下一跳取决于这个节点刚算出来的东西”的情况；只是路由的话，用条件边更清楚。`Command` 还有两个用途：

- `graph=Command.PARENT`：从子图跳到父图的节点；
- `resume=...`：恢复 interrupt。**这是唯一应该作为 `invoke` 输入的 `Command` 形式**。

一个常见错误：用 `Command(update={"messages": [...]})` 作为输入去继续对话。任何 `Command` 作为输入都会从最新的 checkpoint **恢复**，而不是从 `START` 开始，一个已经结束的 thread 看起来就像卡住了。开始新的一轮对话要传普通的 dict。

---

## 4. 执行模型：Pregel 与 super-step

LangGraph 用**消息传递**的方式执行图，借鉴的是 Google 的 Pregel。执行按离散的 **super-step** 推进：

- 并行运行的节点属于同一个 super-step；顺序运行的节点各占一个 super-step；
- 节点在入边收到消息时被激活；
- 一个 super-step 结束后，没有待处理消息的节点变为不活跃；
- 所有节点都不活跃、也没有在途消息时，运行结束。

练习的 `execute` 就是这个循环，每一步分四件事：

1. **并行运行这一步的所有任务**。每个任务拿到同一份快照的独立拷贝，**看不到同一步里其他任务的写入**；
2. 每个任务的结果立刻作为写入挂到当前 checkpoint 上；
3. 有失败就停在这一步，有 interrupt 就暂停，checkpoint 不前进；
4. 全部成功则在**步末统一合并**（按任务 ID 排序，结果和 goroutine 调度顺序无关），算出下一步的任务，写一个新 checkpoint。

`TestParallelNodesShareOneSuperStep` 验证了两件事：`a` 和 `b` 并行，互相看不到对方的写入（都看到 `nil`）；`c` 被 `a`、`b` 两条边触发，但只在下一步运行一次。

**递归上限**：一次运行最多执行多少个 super-step。LangGraph 从 1.1.4 起默认是 10,007（1.0.6–1.1.3 是 10,000），超过就抛 `GraphRecursionError`；可以用环境变量或顶层 `recursion_limit` 配置改。它就是 3.2 课 Agent Loop 的“最大步数”，在图里以 super-step 计。

---

## 5. 持久化

### 5.1 checkpointer 和 store

LangGraph 有两套互补的持久化：

| | Checkpointer | Store |
|---|---|---|
| 存什么 | 图状态的快照 | 应用自定义的键值数据 |
| 范围 | 单个 thread | 跨 thread |
| 记忆类型 | 短期、thread 内 | 长期、跨 thread |
| 用途 | 对话延续、human-in-the-loop、time travel、容错 | 用户偏好、事实、共享知识 |
| 访问方式 | config 里传 `thread_id` | 在节点或应用代码里读写 |

这一课只讲 checkpointer。store 和长期记忆是模块 05 的内容。

### 5.2 thread 和 checkpoint

用 checkpointer 编译图之后，每次调用都**必须**在 config 里给 `thread_id`：

```python
graph = builder.compile(checkpointer=InMemorySaver())
graph.invoke(inputs, {"configurable": {"thread_id": "1"}})
```

`thread_id` 是 checkpoint 的主键，可以理解成一个持久化的游标：沿用它就接着同一组 checkpoint，换一个就从空状态开始。

LangGraph **在每个 super-step 边界写一个 checkpoint**。官方文档的例子：`START → node_a → node_b → END` 跑完后正好有 4 个 checkpoint：

1. 空的，下一个要执行 `START`（`source="input"`，`step=-1`）；
2. 带用户输入，下一个是 `node_a`（`step=0`）；
3. 带 `node_a` 的输出，下一个是 `node_b`（`step=1`）；
4. 带 `node_b` 的输出，没有下一个（`step=2`）。

`TestCheckpointPerSuperStep` 在 Go 里复现了这 4 个 checkpoint 的步号、来源和下一步。

每个快照（`StateSnapshot`）的主要字段：`values`（通道值）、`next`（下一步要执行的节点，空表示结束）、`config`（含 `thread_id`、`checkpoint_ns`、`checkpoint_id`）、`metadata`（`source` 是 `input`/`loop`/`update`，`writes`，`step`）、`parent_config`、`tasks`（每个任务的 `id`、`name`、`error`、`interrupts`）。

### 5.3 pending writes

checkpoint 只在 super-step 结束时写，但 LangGraph 还会在**任务级别**持久化写入：一个节点完成，它的输出就作为任务记录写进 `checkpoint_writes` 表，挂在当前的 checkpoint 上。

这样，同一个 super-step 里如果有节点失败，**已经成功的节点不用重跑**，恢复时直接用它们存下来的写入。练习的 `TestPendingWritesAfterFailure`：`a`、`b` 并行，`b` 第一次失败，`Continue` 之后 `a` 只运行过 1 次，`b` 运行了 2 次。

这和 3.4 课的“三个崩溃时刻”是同一个问题的框架级答案。

### 5.4 怎么继续一个 thread

同一个 `thread_id` 不等于“接着跑”。输入和 `checkpoint_id` 决定做什么：

| 目标 | 调用 | 行为 |
|---|---|---|
| 提交新输入 | `graph.invoke(input, config)` | 输入经 reducer 合并进已有状态，从入口开始 |
| 出错后继续 | `graph.invoke(None, config)` | 加载最新 checkpoint，重试没完成的工作 |
| 回答 interrupt | `graph.invoke(Command(resume=answer), config)` | 提供 `interrupt()` 的返回值，被中断的节点从头开始 |
| 重放历史 | `graph.invoke(None, snapshot.config)` | 从历史快照开始，重新执行之后的节点 |

新输入**不会清空** thread，而是按 reducer 合并（`TestThreadMemoryAndReducers` 里同一个 thread 的消息越积越多）。想从空状态开始就换一个 `thread_id`。最新 checkpoint 已经没有未完成工作时，“继续”直接返回已存的结果，不运行任何节点。

练习里对应四个方法：`Invoke`、`Continue`、`Resume`、`Replay`。

### 5.5 durability 模式

每次执行可以选持久化的时机，从弱到强：

- **`exit`**：只在执行退出时（成功、出错或 interrupt）持久化。长图性能最好，但中间状态不保存，进程中途崩溃就无法恢复；
- **`async`**：下一步执行的同时异步写。性能和持久性都不错，但进程崩溃时有小概率没写进去；
- **`sync`**：下一步开始之前同步写完。每个 checkpoint 都保证落盘，代价是一些性能开销。

还有一个容易忽略的细节：**新 thread 的第一个 checkpoint**。`exit` 模式下运行结束前没有任何 checkpoint；`sync` 模式会在第一个用户节点执行前提交输入 checkpoint（step -1）和 step 0。但无论哪种模式，在第一个 checkpoint 落盘之前崩溃，这个 thread 和“从没被调用过”没法区分：`get_tuple()` 返回 `None`，`invoke(None, config)` 抛 `EmptyInputError`。如果你需要区分“没接收”和“接收了但丢了”，要在调用 `invoke` 之前先在自己的存储里记一笔（比如提交一行 outbox）。

练习只实现了相当于 `sync` 的行为，练习 2 让你加上 `exit`。

### 5.6 checkpointer 实现

- 接口（`BaseCheckpointSaver`）：`put` 存 checkpoint，`put_writes` 存挂在 checkpoint 上的中间写入，`get_tuple` 取 checkpoint（不给 `checkpoint_id` 取最新，给了取指定的），`list` 列历史，`delete_thread` 删除 thread 的全部 checkpoint 和写入；异步版本加 `a` 前缀；
- 现成的库：`InMemorySaver`（实验用，进程重启就没了）、`SqliteSaver`（本地开发）、`PostgresSaver`（生产，LangSmith 用的就是它）、MongoDB、Azure Cosmos DB；
- 存储设计：一张 checkpoints 表（每个 super-step 一行，主键 `thread_id, checkpoint_ns, checkpoint_id`，`checkpoint_id` 用可按字典序排序的 ULID）和一张 writes 表（每个任务的每个输出一行）。**按 ID 精确查找必须正确**，time travel 和 delta 通道的状态重建都依赖它；
- 序列化：默认 `JsonPlusSerializer`（底层是 ormsgpack 和 JSON），pandas DataFrame 这类不支持的对象可以开 `pickle_fallback`；可以用 `EncryptedSerializer` 加密全部持久化状态；
- 存储增长：默认每个 super-step 写所有通道的完整值，长对话会越存越大。`DeltaChannel`（需要 `langgraph>=1.2`，目前是 beta）只存增量；也可以定期清理旧 checkpoint；
- `PostgresSaver` 里 `thread_id` 要控制在 255 字符以内。

练习的 `MemSaver` 有一个刻意的设计：所有数据进出都做一次 JSON 往返。这既是深拷贝，也强制“状态和 interrupt 载荷必须能序列化”，和真实数据库实现的行为一致。`TestInterruptPayloadMustSerialize` 往 interrupt 载荷里放了一个函数，结果这个任务失败，错误信息里有“序列化”。

---

## 6. interrupt

### 6.1 用法

`interrupt()` 可以放在节点里的任何地方，暂停执行并把一个值交给调用方。恢复时用 `Command(resume=...)` 再调用一次图，这个值就成为节点里 `interrupt()` 的返回值。需要三样东西：

1. checkpointer（生产环境用持久化的）；
2. config 里的 `thread_id`；
3. 载荷必须能 JSON 序列化。

```python
from langgraph.types import interrupt, Command

def approval_node(state):
    approved = interrupt({"question": "批准吗？", "details": state["action_details"]})
    return Command(goto="proceed" if approved else "cancel")

config = {"configurable": {"thread_id": "approval-123"}}
result = graph.invoke({"action_details": "转账 500 元"}, config)
print(result["__interrupt__"])           # 等待中的 interrupt
graph.invoke(Command(resume=True), config)
```

Go 练习：

```go
AddNode("approval", func(rt *Runtime, s State) (any, error) {
	ok := rt.Interrupt(map[string]any{"question": "批准这个操作吗？", "details": s["details"]})
	if ok == true {
		return Command{Goto: []Target{To("proceed")}}, nil
	}
	return Command{Goto: []Target{To("cancel")}}, nil
}, "proceed", "cancel")

res, _ := g.Invoke(cfg, State{"details": "转账 500 元"}) // res.Interrupts 有一个
res, _ = g.Resume(cfg, true)                            // status = approved
```

官方现在推荐用事件流（`graph.stream_events(..., version="v3")`）驱动可能中断的图：`stream.interrupted` 表示是否暂停，`stream.interrupts` 是载荷；默认的 `invoke` 仍然可用，载荷在 `result["__interrupt__"]` 里。从 `langgraph>=1.2.12` 起，`interrupt()` 还可以带 `response_schema`（Pydantic 模型、TypedDict 或 dataclass），给前端渲染表单，并在恢复时校验输入。

### 6.2 运行时怎么做到的：抛出 + 从头重跑

这是整课最重要的一节。`interrupt()` 的实现是**抛出一个特殊异常**，异常沿调用栈上传，被运行时捕获，运行时保存状态并等待输入。恢复时，运行时**从头重跑整个节点**，而不是从 `interrupt()` 那一行继续。节点再次走到 `interrupt()` 时，发现这次有恢复值，就直接返回它。

如果一个节点里有多个 `interrupt()`，LangGraph 为这个任务维护一个恢复值列表，**严格按序号匹配**：第 1 次调用返回第 1 个恢复值，第 2 次调用返回第 2 个，以此类推。

练习的实现几乎就是这段话：

```go
func (rt *Runtime) Interrupt(payload any) any {
	i := rt.idx
	rt.idx++
	if i < len(rt.resumes) {
		return rt.resumes[i] // 恢复时：直接返回第 i 个恢复值
	}
	panic(interruptSignal{...}) // 第一次：抛出，运行时 recover 后保存并暂停
}
```

恢复值作为 `kind=resume` 的写入挂在 checkpoint 上，重跑节点时按顺序交给 `Runtime`。

`TestInterruptAndResume` 里，`interrupt` 之前的计数器最后是 2：第一次运行到 interrupt 抛出，恢复时又从头跑了一遍。`TestMultipleInterruptsMatchByIndex` 里一个节点问两个问题，节点一共运行了 3 次。

### 6.3 由此推出的四条规则

官方文档列出的四条规则，全部是“抛出 + 从头重跑 + 按序号匹配”的直接后果：

1. **不要用裸的 try/except 包住 `interrupt`**：它靠抛异常暂停，你把异常吃掉，interrupt 就传不回运行时。可以只捕获具体的异常类型。（Go 练习用 `panic`，节点里写 `recover()` 会有同样的问题。）
2. **不要在节点里改变 `interrupt` 的调用顺序**：不要条件性地跳过某个 interrupt，不要按会变的数据循环调用 interrupt（包括 `while True` 校验循环）。按序号匹配，顺序一变，恢复值就对错了位置；
3. **不要往 `interrupt` 里传复杂对象**：函数、类实例序列化不了；
4. **`interrupt` 之前的副作用必须幂等**：节点会重跑，之前的 API 调用、插入记录、往列表追加都会再执行一次。办法是用幂等操作（upsert）、把副作用放到 interrupt 之后、或者干脆拆到单独的节点里。

关于第 2 条，校验人工输入的正确做法是：**每次节点调用只 interrupt 一次**，把下一次要问的问题存进状态，用条件边循环回这个节点，直到输入合法。在一个节点里写 `while True` + `interrupt()`，每次恢复都会重放之前所有轮次。

子图也一样：父图里调用子图的那个节点会从头重跑，子图里调用 interrupt 的那个节点也会从头重跑。

### 6.4 多个 interrupt 同时等待

并行分支同时 interrupt 时（比如扇出到多个都调用了 `interrupt()` 的节点），一次恢复要把**每个 interrupt 的 ID 映射到各自的恢复值**：

```python
resume_map = {i.id: f"answer for {i.value}" for i in stream.interrupts}
graph.stream_events(Command(resume=resume_map), config, version="v3")
```

练习用 `ResumeMap`。`TestParallelInterruptsResumeByID` 覆盖了几种情况：两个都在等时给一个不带 ID 的值 → `ErrAmbiguousResume`；ID 不存在 → `ErrUnknownInterrupt`；只回答 `a` → `b` 继续等，`a` 的结果作为 pending write 保存；再回答一次 `a` → `ErrStaleResume`（拒绝过期的答案）；最后只剩 `b`，可以不带 ID。

练习判断“这个 interrupt 还在等”的规则：任务还没有结果，并且 interrupt 的序号等于这个任务已经收到的恢复值个数。序号更小的说明已经回答过了。

### 6.5 静态断点

编译时或运行时可以设 `interrupt_before` / `interrupt_after`，在指定节点前后暂停，用 `invoke(None, config)` 继续。官方说明：静态断点用于调试，**不推荐用来做 human-in-the-loop**，那应该用 `interrupt()` 函数。

---

## 7. time travel

有了 checkpoint 历史，就可以：

- **查看历史**：`graph.get_state_history(config)`，最新的在前。可以按 `next`、`step`、`source` 筛出想要的那个；
- **重放**：用历史快照的 config 调用 `invoke(None, ...)`。之前的节点不重跑（结果已经存了），之后的节点**重新执行**，包括模型调用、API 请求和 interrupt；
- **修改状态**：`update_state(config, values, as_node=...)` 创建一个**新的** checkpoint，不改原来的。更新和节点更新一样经过 reducer；`as_node` 决定这次更新算是哪个节点产生的，从而决定下一步运行谁。

练习的 `TestReplayForksHistory`：从“`b` 之前”的 checkpoint 重放，`b` 再跑一次，新 checkpoint 的父节点是重放的起点，原来的历史一条不少。`TestUpdateStateAsNode`：以 `a` 的身份写入 `"human"`，下一步就是 `a` 的后继 `b`，`log` 变成 `[a, b, human]`，继续执行后变成 `[a, b, human, b]`。

**图的迁移**：已经有 thread 存在 checkpointer 里时改图，规则是：已结束的 thread 可以随意增删改节点和边；**被中断的 thread** 可以改边、加节点，但不能重命名或删除它即将进入的那个节点（恢复时按名字找节点）。状态 schema 增删键前后兼容；重命名键会丢掉旧 thread 里这个键的值；不兼容的类型变化可能让旧状态出错。

---

## 8. 怎么划分节点

官方给了一张很实用的表：节点是 checkpoint 和重试的边界，也是图可以分叉的地方。

| 单元 | 自己的 checkpoint | 恢复或重试时 | 什么时候用 |
|---|---|---|---|
| 节点里的普通函数 | 没有 | 节点重跑它就重跑 | 之后不分叉，和节点其他部分一起重复也可以接受 |
| task（Functional API） | 不产生新的图 checkpoint，结果存在所属的 checkpoint 上 | 节点从头重放，已完成的 task 直接返回存下的结果 | 有副作用或很贵，恢复时不能重复 |
| 子图 | 有（在父 thread 的命名空间下） | 在子图里中断或失败的节点处恢复 | 有自己的路由或状态 schema，或者被多个父图复用 |
| 图节点 | 有，跟随它所在的 super-step | 从这个节点开始，之前的节点不重跑 | 之后可能分叉，或者需要 interrupt、恢复、time travel 的边界 |

判断标准：**这一步是在决定接下来做什么，还是在执行已经决定的事？** 前者是节点，后者通常不需要单独的节点。练习 3 让你实现 task。

---

## 9. 练习代码导读

| 文件 | 内容 | 对应 LangGraph |
|---|---|---|
| `graph.go` | `State`、`Reducer`、`Append`、`Overwrite`、`Target`/`To`/`SendTo`、`Command`、`Builder`、`Compile` 的结构检查 | `StateGraph`、`Annotated[..., reducer]`、`Overwrite`、`Send`、`Command`、`compile()` |
| `checkpoint.go` | `Task`、`Checkpoint`、`Interrupt`、`Write`、`Checkpointer` 接口、`MemSaver` | `StateSnapshot`、`checkpoint_writes`、`BaseCheckpointSaver`、`InMemorySaver` |
| `pregel.go` | `Invoke`/`Continue`/`Resume`/`Replay`/`UpdateState`/`GetState`/`History`，super-step 主循环，`Runtime.Interrupt` | `invoke(input)`、`invoke(None)`、`Command(resume=)`、`update_state`、`get_state(_history)`、`interrupt()` |
| `graph_test.go` | 15 个测试 | |

几个和 Python 版的差异，读代码时注意：

- Python 的状态 schema 用 `TypedDict` 加类型注解；Go 版用 `Channel(name, reducer)` 显式声明，值是 `any`，经过 JSON 往返后数字会变成 `float64`；
- Python 节点返回 dict；Go 版返回 `State`、`Command` 或 `nil`；
- `interrupt` 在 Python 里抛异常，Go 版用 `panic` + `recover`，原理相同；
- 任务 ID 用“步号:节点名:序号”生成，存在 checkpoint 的 `Next` 里。恢复时靠它把 pending writes 和恢复值对回到同一个任务；
- 编译期检查要求条件边声明可能的目标（`AddConditionalEdges(from, router, dests...)`），`Command` 的目标在 `AddNode` 时声明。这相当于 Python 里 `Command[Literal["proceed", "cancel"]]` 的返回类型注解，LangGraph 也靠它画图。

---

## 10. 常见的坑

- **并行节点写同一个没有 reducer 的键**：报错，要么加 reducer，要么改成不同的键。
- **带 reducer 的字段想清空**：返回空列表不会清空，要用 `Overwrite`。
- **消息列表用 `operator.add`**：之后没法按 ID 修改某条消息，用 `add_messages`。
- **静态边和 `Command`/条件边混用**：两条路都会走。
- **用 `Command(update=...)` 继续对话**：会从最新 checkpoint 恢复，看起来像卡住。
- **以为同一个 `thread_id` 就会接着跑**：新输入是从入口开始、按 reducer 合并；继续要传 `None`，回答 interrupt 要传 `Command(resume=...)`。
- **`interrupt` 之前有非幂等副作用**：恢复时重复执行，重复扣款、重复插入。
- **用 try/except（或 Go 里的 recover）包住 `interrupt`**：暂停失效。
- **条件性地跳过 interrupt，或在 `while True` 里 interrupt**：按序号匹配会对错位置。
- **生产环境用 `InMemorySaver`**：进程重启全部丢失。
- **以为 `exit` 模式也能从中途崩溃恢复**：它只在退出时写。
- **改了被中断 thread 即将进入的节点名**：恢复失败。
- **checkpoint 无限增长**：长对话要清理或用增量存储。
- **用静态断点做审批**：它是调试工具。

---

## 11. 练习

### 练习 1：读懂并跑通（必做）

```bash
cd projects/24-langgraph && make test
```

回答：

1. `TestParallelNodesShareOneSuperStep` 里 `a` 为什么看到的 `b` 是 `nil`？如果改成“每个节点一完成就立刻把写入合并进共享状态”，会出什么问题？
2. `TestParallelInterruptsResumeByID` 最后 `a` 运行了 2 次、`b` 运行了 3 次。逐次说明每一次运行是哪次调用触发的。
3. `TestRecursionLimit` 设了上限 5，为什么 `loop` 只运行了 4 次？
4. 在 `TestUpdateStateAsNode` 里如果把 `asNode` 从 `"a"` 改成 `"b"`，`GetState` 的 `Next` 是什么？之后 `Continue` 会发生什么？

### 练习 2：durability 模式（必做）

给 `Config` 加 `Durability string`，支持 `"sync"`（默认，即现在的行为）和 `"exit"`：

- `exit` 模式下，一次 `Invoke`/`Continue`/`Resume` 运行期间产生的 checkpoint 和写入先缓存在内存里，**只在执行退出时**（正常结束、节点出错、interrupt）一起写进 checkpointer；
- 加一个测试钩子 `Config.CrashAfterStep int`：执行完第 N 个 super-step 后直接返回 `ErrCrash`，**不做退出时的写入**（模拟进程被杀）；
- 测试：
    - 线性图 `START → a → b`，`sync` 模式在第 2 步后崩溃，`Continue` 只重跑 `b`；
    - 同样的图，`exit` 模式在第 2 步后崩溃，thread 里没有任何 checkpoint，`Continue` 返回 `ErrEmptyInput`；
    - `exit` 模式遇到 interrupt 时，checkpoint 和 interrupt 写入都已落盘，`Resume` 正常工作。

### 练习 3：task——恢复时不重复的副作用（选做）

实现 `Runtime.Task(key string, fn func() (any, error)) (any, error)`，对应 Functional API 的 `@task`：

- 同一个任务里，第一次以某个 `key` 调用时执行 `fn`，结果**立刻**作为 `kind=task` 的写入挂到当前 checkpoint 上；
- 节点因为 interrupt 或失败而重跑时，同一个 `key` 直接返回存下来的结果，不再执行 `fn`；
- 结果同样必须能序列化；
- 测试：节点先 `rt.Task("charge", 扣款)`，再 `rt.Interrupt("确认发货？")`。恢复后扣款函数只执行了 1 次，而节点函数执行了 2 次。

---

## 12. 自测题

1. LangGraph 的状态由什么组成？reducer 的两个参数分别是什么？默认 reducer 是什么？
2. 为什么并行节点写同一个没有 reducer 的键会报错？
3. 什么是 super-step？同一个 super-step 里的节点能看到彼此的写入吗？
4. `Send` 解决了普通边解决不了的什么问题？
5. `Command` 有哪几种用法？为什么不能用 `Command(update=...)` 作为输入继续对话？
6. checkpoint 在什么时候写？`START → A → B → END` 跑完有几个 checkpoint？pending writes 解决什么问题？
7. 同一个 `thread_id` 有哪四种继续方式？
8. 三种 durability 模式的区别？“在第一个 checkpoint 落盘前崩溃”为什么需要在 checkpointer 之外记录？
9. `interrupt` 是怎么实现暂停和恢复的？由此推出的四条规则是什么？
10. 节点、task、子图、节点内普通函数，分别在什么情况下用？

## 13. 自测题答案

1. 状态是一组通道（schema 的键），每个通道有值和合并规则。reducer 是二元函数，左边是状态里的当前值，右边是节点返回的更新。默认 reducer 是覆盖：右边替换左边。
2. 同一个 super-step 里的节点并行运行、写入在步末合并。没有 reducer 时语义是覆盖，两个并行写入谁先谁后没有意义，只能报错（LangGraph 是 `InvalidUpdateError`），要求你显式给出合并规则。
3. super-step 是图执行的一个离散“节拍”：这一步被激活的节点并行运行，全部结束后统一合并写入、决定下一步。同一步里的节点看到的是这一步开始时的同一份快照，看不到彼此的写入。
4. 普通边在建图时就固定了拓扑，被选中的节点都拿到同一份共享状态；`Send` 可以在运行时根据数据决定对同一个节点启动几个实例，每个实例有自己的输入，结果通过 reducer 合并。这就是 map-reduce / orchestrator-worker。
5. 从节点返回（`update` + `goto`，从子图跳父图用 `graph=Command.PARENT`）、从工具返回、以及作为输入恢复 interrupt（`resume`）。任何 `Command` 作为输入都会从最新的 checkpoint 恢复，而不是从 `START` 开始，一个已经结束的 thread 就不会再运行任何节点，看起来像卡住；开始新一轮要传普通 dict。
6. 每个 super-step 边界写一个 checkpoint。`START → A → B → END` 有 4 个：输入前（step -1）、带输入（step 0）、A 之后（step 1）、B 之后（step 2）。pending writes 是任务级的写入：一个节点完成就把输出挂到当前 checkpoint 上，同一步里别的节点失败时，成功的节点恢复后不用重跑。
7. 提交新输入（`invoke(input)`，从入口开始，按 reducer 合并）；出错后继续（`invoke(None)`，重试未完成的工作）；回答 interrupt（`invoke(Command(resume=...))`，被中断的节点从头运行）；重放历史（`invoke(None, 历史快照的 config)`，之后的节点重新执行）。
8. `exit` 只在执行退出时写，中途崩溃无法恢复；`async` 在下一步执行的同时异步写，崩溃时有小概率丢失；`sync` 在下一步开始前同步写完，最可靠但有开销。第一个 checkpoint 落盘前崩溃时，thread 和“从来没被调用过”完全一样（`get_tuple` 返回 `None`），checkpointer 本身无法区分“没接收”和“接收了但丢了”，所以要在调用前在自己的存储里先记一笔（如 outbox）。
9. `interrupt()` 抛出一个特殊异常，运行时捕获后保存状态、把载荷交给调用方；恢复时运行时从头重跑整个节点，节点里第 i 次调用 `interrupt()` 直接返回第 i 个恢复值。四条规则：不要用裸 try/except 包住它；不要改变调用顺序（不条件跳过、不按会变的数据循环）；载荷必须可序列化；它之前的副作用必须幂等。
10. 之后可能分叉、或者需要 interrupt / 恢复 / time travel 边界的，做成图节点；有副作用或很贵、恢复时不能重复的，用 task；有自己的路由或状态 schema、或被多个父图复用的，做成子图；之后不分叉、跟节点一起重复也无所谓的，就是节点里的普通函数。

---

## 14. 参考答案

### 练习 1

1. 每个任务在运行前拿到这一步开始时状态的一份独立拷贝（`jsonCopy(view, &own)`），写入在所有任务结束后才统一合并。`a` 运行时 `b` 的写入还没合并，所以看到 `nil`。如果改成完成即合并：
    - 并发读写同一个 map，`go test -race` 直接报数据竞争；
    - 就算加锁，结果也取决于 goroutine 谁先跑完：`a` 有时看到 `b`、有时看不到，同样的输入产生不同的结果，checkpoint 和重放都失去意义；
    - 两个节点写同一个 LastValue 键时，“谁后完成谁赢”，冲突被悄悄掩盖。
2. `a` 第 1 次：`Invoke`，运行到 interrupt 抛出。`b` 第 1 次：同一次 `Invoke`，同样抛出。`a` 第 2 次：`Resume(ResumeMap{ia: "A"})`，有了恢复值，返回结果，结果作为 pending write 保存。`b` 第 2 次：同一次 `Resume`，`b` 没有恢复值，再次抛出（所以这次调用的结果里还有一个 interrupt）。之后的 `Resume(ResumeMap{ia: "again"})` 在检查阶段就返回 `ErrStaleResume`，不运行任何节点。`b` 第 3 次：`Resume("B")`，拿到恢复值完成；`a` 因为已有 pending write 不再运行。
3. 上限是 5 个 super-step。第 1 个 super-step 运行的是 `START` 任务（把输入写进状态），之后 4 个 super-step 各运行一次 `loop`。第 6 次进入循环时 `steps == 5`，返回 `ErrRecursion`。LangGraph 里输入也占一个 super-step（step 0 那个 checkpoint 就是它产生的）。
4. `asNode = "b"` 时，下一步由 `b` 的出边决定，`b` 没有出边，`Next` 为空，这个 thread 被视为已经结束。之后 `Continue` 不运行任何节点，直接返回当前状态 `[a, b, human]`，`Done` 为 `true`。这就是 `as_node` 的意义：它不只是标注“谁改的”，还决定了图从哪里继续。

### 练习 2

把“写 checkpoint / 写入”抽成一个 sink，`execute` 只和 sink 打交道：

```go
type sink interface {
	put(cp Checkpoint) (int, error)
	putWrites(cpID int, ws ...Write) error
	flush() error
}

// sync：直接写 checkpointer
type syncSink struct{ cp Checkpointer; thread string }

// exit：先缓存，用临时的负数 ID 让 Parent 和写入能互相引用，flush 时按顺序写入并重映射 ID
type exitSink struct {
	cp     Checkpointer
	thread string
	cps    []Checkpoint
	writes map[int][]Write
	nextID int // -1, -2, ...
}

func (s *exitSink) put(cp Checkpoint) (int, error) {
	s.nextID--
	cp.ID = s.nextID
	s.cps = append(s.cps, cp)
	return cp.ID, nil
}

func (s *exitSink) flush() error {
	real := map[int]int{}
	for _, cp := range s.cps {
		if p, ok := real[cp.Parent]; ok {
			cp.Parent = p
		}
		id, err := s.cp.Put(s.thread, cp)
		if err != nil {
			return err
		}
		real[cp.ID] = id
	}
	for tmp, ws := range s.writes {
		id := tmp
		if r, ok := real[tmp]; ok {
			id = r
		}
		if err := s.cp.PutWrites(s.thread, id, ws...); err != nil {
			return err
		}
	}
	return nil
}
```

要点：

- `Invoke` 的输入 checkpoint 也要走 sink，否则 `exit` 模式崩溃后还会留下一个输入 checkpoint，和官方描述的“运行结束前没有任何 checkpoint”不一致；
- `execute` 的每个返回路径（正常结束、`NodeError`、interrupt、`ErrInvalidUpdate`）都要 `flush`，**只有** `ErrCrash` 不 flush。可以用 `defer` 加一个 `crashed` 标志；
- 写入可能挂在一个已经存在的真实 checkpoint 上（比如 `Continue` 从已落盘的 checkpoint 开始），这时 ID 是正数，`real` 里查不到，原样使用；
- `exit` 模式崩溃后 `Continue` 返回 `ErrEmptyInput`，这正是 5.5 节说的“和从没调用过无法区分”。

### 练习 3

```go
type Runtime struct {
	task    Task
	resumes []any
	idx     int
	done    map[string]any // 上次已完成的 task：key → 结果
	persist func(Write) error
}

func (rt *Runtime) Task(key string, fn func() (any, error)) (any, error) {
	if v, ok := rt.done[key]; ok {
		return v, nil // 重跑时直接返回，不再执行 fn
	}
	v, err := fn()
	if err != nil {
		return nil, err
	}
	w := Write{TaskID: rt.task.ID, Kind: "task", Key: key, Resume: v}
	if err := rt.persist(w); err != nil { // 立刻落盘，不等这一步结束
		return nil, err
	}
	rt.done[key] = v
	return v, nil
}
```

`execute` 在构造 `Runtime` 时从 `writes` 里收集这个任务所有 `kind=task` 的写入填进 `done`，`persist` 闭包调用 `g.cp.PutWrites(cfg.Thread, cp.ID, w)`。`Write` 加一个 `Key` 字段。

要点：

- **必须立刻持久化**。如果等到这一步结束再写，节点在 interrupt 处抛出时这个结果就丢了，恢复时扣款会再执行一次，task 就失去了意义；
- key 要在节点的每次执行中保持一致，和 interrupt 必须保持调用顺序是同一类约束（官方叫 determinism）；
- `fn` 本身仍然可能在“执行完、落盘前”崩溃，这时恢复会再执行一次。task 把重复的窗口从“整个节点”缩小到“一次调用加一次写入”，彻底解决还要靠外部系统的幂等键（3.4 课的账本）。

---

## 完成标准

- `make test` 全部通过（15 个测试），`go test -race` 通过；
- 练习 1 的四个问题能讲清楚，尤其是第 2 题每一次运行的来源；
- 练习 2 写完，能解释 `exit` 模式为什么快、代价是什么，以及第一个 checkpoint 的问题；
- 能不看文档写出一个带 checkpointer、条件边、`Send` 和 `interrupt` 的 LangGraph 图，并说出每次 `invoke` 用什么输入；
- 能从“抛出 + 从头重跑 + 按序号匹配”推出 interrupt 的四条规则；
- 自测题 10 道能不看答案讲出来，尤其是第 6、7、9 题。

下一课是 4.2 OpenAI Agents SDK：同样的 agent、工具、交接、审批和状态，换一种以“run”为中心的抽象，可以和这一课的图对照着看。
