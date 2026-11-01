# 3.5 Agent 状态与数据模型：会话、消息、run、step、工具调用，事件溯源与完整重放

> 第 3 部分 · 第 5 课 ｜ 预计 3–4 小时 ｜ 前置：3.2 Agent Loop、3.4 长任务 Agent、0.6 数据库（事务、唯一约束）
>
> 配套练习：`projects/21-state/`（纯 Go 标准库，15 个测试）。把 3.2 的 Agent 循环改成“先写事件、再往下走”：run 的全部历史只存在一条只追加的事件流里；运行状态、消息历史、会话下的 run 列表都是从事件折叠出来的投影，随时可以丢掉重建；快照加尾部事件等于全量折叠；旧版本事件在读取时升级；任何一次 run 都能在新代码上用录像完整重放，模型和工具一次都不真调用，代码改动导致请求变化时会精确报告在第几步分叉。

## 读前说明

3.4 解决了“run 中途停下来还能接着跑”。用的是检查点：每一步之后把当前状态整体存一份，恢复时读最新那份。

检查点回答的是“现在是什么状态”。线上跑起来以后，你会更常被问到另外几类问题：

- 昨晚第 3812 次 run 失败了，它当时到底给模型发了什么、模型回了什么、哪个工具先出的错？
- 我改了系统提示词，上周那 200 个出过问题的 run，现在还会走同样的路吗？
- 这个用户的会话里一共有几个 run、花了多少 token、每次调用了哪些工具？
- 产品说要加一个“工具调用耗时”字段，过去三个月的数据能补出来吗？

只存最新状态，这些问题都答不了：中间过程被覆盖掉了。这一课讲另一种存法：不存状态，存“发生过的事”，状态从事件推出来。这就是事件溯源（event sourcing）。

先想几个问题：

- 一条“消息”应该是一张表里的一行，还是从别的东西推出来的？
- 事件里应该存“模型的回复”，还是存“发给模型的请求”？都存的话，请求那么大怎么办？
- 改了事件的字段，旧数据怎么办？能直接 `UPDATE` 吗？
- “重放”和“重新运行”有什么区别？

---

## 1. 三种存法

同一个 agent run，常见三种存法：

| 存法 | 存的是什么 | 典型例子 | 能回答 | 回答不了 |
|---|---|---|---|---|
| 消息数组 | 一个会话一个 `[]Message`，每轮追加 | 大多数聊天应用的 `messages` 表；OpenAI Conversations API 的 items | 下一轮发给模型什么 | 某次工具调用花了多久、哪次 run 失败在哪一步、当时用的哪个版本的提示词 |
| 快照（检查点） | 每一步之后的完整状态 | 3.4 的 `Checkpoint`；LangGraph checkpointer | 当前状态；回到某一步 | 两步之间具体发生了什么（除非每个快照都留着并互相对比） |
| 事件日志 | 每件发生过的事，按顺序只追加 | 本课的 `events` 表 | 以上全部，以及以后才想到的问题 | 需要额外的投影才能快速查询 |

三者不是互斥的。成熟的系统一般是：**事件日志是唯一的事实来源，消息数组和快照都是从它推出来的投影**。消息数组给模型用，快照给恢复和快速加载用，统计表给看板用。

LangGraph 其实已经站在中间：它的持久层有两张表，`checkpoints` 每个超步一行完整状态，`writes` 每个节点输出一行。`writes` 本质上就是事件。它新加的 `DeltaChannel`（beta，`langgraph>=1.2`）更进一步：对 `messages` 这种只增长的通道，检查点里不再存完整值，而是存一个占位，加载时沿父检查点链把写入一条条用 reducer 重放出来。这就是“快照 + 事件”的混合。

---

## 2. 实体：会话、run、step、工具调用、消息

先把名词定下来。下面这套划分和 OpenAI Agents API、LangGraph 的概念基本能对上。

| 实体 | 含义 | 生命周期 | 对应概念 |
|---|---|---|---|
| Session（会话） | 一个用户和 agent 的一段长期对话 | 天、周 | LangGraph `thread`；OpenAI `conversation` |
| Run | 用户发一次输入，agent 跑到给出最终回复或失败为止 | 秒到小时 | LangGraph 一次 `invoke`；OpenAI 一次 `responses.create`（带工具时可能多个模型调用） |
| Step | run 里的一次模型调用，以及它触发的工具调用 | 秒 | LangGraph 超步（大致） |
| Tool call | 一次工具执行，有参数、结果、是否出错、耗时 | 毫秒到分钟 | OpenAI `function_call` / `function_call_output` item |
| Message | 发给模型的历史里的一条 | — | 由上面几个推出来 |

关系是一对多一路往下：一个会话有多个 run，一个 run 有多个 step，一个 step 有零到多个工具调用。

**消息为什么是推出来的？** 因为模型看到的历史，是 run 里发生的事按某种规则拼出来的：用户输入变成 user 消息，模型回复变成 assistant 消息，工具结果变成 tool 消息。规则会变：比如以后你想把很长的工具结果截断再给模型（1.6 上下文工程），或者把早期轮次压缩成摘要（OpenAI 的 compaction）。如果消息是一张独立的表，你就得维护“原始的”和“给模型的”两份，迟早不一致。从事件推出来，规则改了重新推一遍就行。

有一个例外要注意：OpenAI Responses API 在 `store: false` 时，文档要求你把上一次响应的 `output` 数组**完整**放回下一次的输入里，包括加密的 reasoning item 和 assistant 消息的 `phase` 字段，否则推理模型会把中间更新当成最终答案。这类厂商私有的、你无法从别处还原的内容，要在事件里原样存下来（`raw_output` 字段），而不是只存你解析出来的文本。

---

## 3. 事件溯源

### 3.1 基本操作只有两个

- **追加**：`Append(runID, expect, events...)`。一个 run 一条流，事件按 `seq` 编号，从 1 开始连续递增。`expect` 是调用方以为的“当前最后一个 seq”，和实际不一致就返回冲突。
- **读取**：`Read(runID, after)`，读某个 seq 之后的所有事件。

没有更新，没有删除。状态是读出所有事件、依次应用得到的：

```go
func Fold(evs []Event) (RunView, error) {
	var v RunView
	for _, e := range evs {
		if err := v.Apply(e); err != nil {
			return v, err
		}
	}
	return v, nil
}
```

`expect` 就是 3.4 里检查点 `seq` 的同一个思路：乐观锁。两个 worker 同时以为自己是这个 run 的执行者，都基于 seq=5 追加，只有先到的成功，后到的拿到冲突，知道有人抢先了，应该放弃而不是覆盖。练习里 `TestConcurrentAppendOnlyOneWins` 用 20 个 goroutine 同时追加验证这一点。数据库里这个约束就是 `UNIQUE (run_id, seq)`，不需要加锁。

### 3.2 事件是事实，不是命令

事件名用过去式：`model_called`、`tool_finished`，不用 `call_model`、`run_tool`。区别不只是语法：

- 命令可以被拒绝（“扣款”可能失败）；事件已经发生，不能拒绝（“已扣款”就是已扣款）。
- `Apply` 只能根据事件更新状态，不能再去做事。它不读时钟、不调用外部服务、不生成随机数，同样的事件序列永远得到同样的状态。

第二条是重放能成立的前提。时间、随机数、模型输出、工具结果这些不确定的东西，都在**产生事件的时候**确定下来写进事件，`Apply` 只读不算。

### 3.3 一个 run 写哪些事件

练习里的事件类型：

| 事件 | 什么时候写 | 关键字段 |
|---|---|---|
| `run_started` | run 开始 | `session_id`、`input`、`agent_version` |
| `model_called` | 模型返回之后 | `step`、`request_hash`、`response`、`usage` |
| `tool_started` | 执行工具**之前** | `step`、`call`、`idem_key` |
| `tool_finished` | 工具返回之后 | `call_id`、`result`、`is_error` |
| `run_completed` | 模型给出最终回复 | `final` |
| `run_failed` | 模型报错或步数用完 | `step`、`error` |

一次带一个工具调用的 run，事件序列是：

```text
1 run_started
2 model_called   step=1  response 里有 tool call c1
3 tool_started   c1
4 tool_finished  c1
5 model_called   step=2  response 是最终文本
6 run_completed
```

**为什么工具要拆成两个事件？** 和 3.4 的三个崩溃时刻是同一个道理。只有 `tool_started` 没有 `tool_finished` 的调用，说明进程在执行途中死了：外部系统可能执行了，也可能没有。恢复时要先处理它们（用幂等键去外部系统查，或者交给人），不能当作没发生。`RunView.Open()` 就是找这些调用的。`idem_key` 用 `runID/callID`，同一个调用无论重试几次键都一样。

**为什么 `model_called` 存请求指纹而不存完整请求？** 完整请求就是到这一步为止的全部历史，而历史已经能从前面的事件推出来，再存一遍是 O(n²) 的空间。存一个指纹（版本号 + 工具列表 + 历史的哈希）足够做两件事：审计时验证“推出来的历史”和“当时真正发出去的”一致；重放时发现分叉（第 6 节）。如果你的提示词模板会注入时间、检索结果这类每次都变的内容，那些内容本身要作为事件存下来（比如 `context_retrieved`），否则重放时推不出来。

### 3.4 原子性：一次追加的多条事件

`Append` 可以一次写多条事件，要求全写或全不写。比如“工具结果 + 下一步开始”需要同时生效时。数据库里就是一个事务。练习里只是一次一条，`MemStore.Append` 的循环在锁内，满足原子性。

---

## 4. 投影：从事件推出读模型

### 4.1 RunView：单个 run 的状态

`RunView` 是练习里最主要的投影，`Apply` 按事件类型更新它：

```go
case ModelCalled:
	d, err := decode[ModelCalledData](e)
	...
	v.Steps = append(v.Steps, StepView{N: d.Step, RequestHash: d.RequestHash, Response: d.Response, Usage: d.Usage})
	v.Usage.Input += d.Usage.Input
	v.Usage.Output += d.Usage.Output
	v.Messages = append(v.Messages, d.Response)
```

它同时维护了几样东西：步骤列表（给调试界面）、累计用量（给计费）、消息历史（给模型）。它们来自同一份事件，不可能互相矛盾。

`Apply` 开头有两道检查：

- `e.Seq != v.LastSeq+1` 就拒绝：跳号或重复说明读到的数据有问题，宁可报错也不要算出一个错的状态；
- `upcast` 失败就拒绝：遇到不认识的事件类型或版本（比如新代码写的、旧代码在读），同样报错。

还有一道是业务上的：`tool_finished` 找不到对应的 `tool_started` 就报错。这些检查让投影出错时**立刻**失败，而不是带着错误状态继续跑。

### 4.2 跨 run 的投影：会话索引

“这个会话有哪些 run”需要看所有 run 的事件。做法是给全局事件一个位置号 `pos`（数据库里就是 `bigserial` 主键），投影记住自己读到哪里，定期追赶：

```go
func (ix *SessionIndex) CatchUp(ctx context.Context, s EventStore) error {
	for {
		evs, err := s.ReadAll(ctx, ix.Pos, 100)
		...
		for _, e := range evs {
			if e.Type == RunStarted {
				...
				ix.Runs[d.SessionID] = append(ix.Runs[d.SessionID], e.RunID)
			}
			ix.Pos = e.Pos
		}
	}
}
```

两点工程细节：

- **位置和投影结果要一起保存**（同一个事务）。先存结果后存位置，中间崩溃就会重复处理；反过来就会漏。练习是内存实现，Postgres 里就是 `projection_positions` 表和投影表在一个事务里更新。
- **投影是最终一致的**。写完事件到投影追上有延迟。需要“读到自己刚写的”时（比如 run 结束立刻展示结果），直接用 `Load` 从事件流现算，不要读投影表。

注意 `bigserial` 有一个坑：并发事务里，pos 小的事务可能比 pos 大的晚提交，追赶时可能跳过它。生产上常见的处理是只读“已经提交了足够久”的事件，或者用逻辑复制 / 单写者分配位置。练习不涉及，知道有这回事即可。

### 4.3 会话历史

下一个 run 要看到会话之前的对话，就把该会话所有 run 的 `Messages` 按顺序拼起来：

```go
prior, _ := SessionMessages(ctx, store, ix, "s1")
agent.RunWithHistory(ctx, "s1", "r2", prior, "明天呢？")
```

这里正是上下文工程（1.6）插手的地方：历史太长就截断或摘要。因为摘要只是“另一种投影”，原始事件不受影响，以后换更好的摘要策略可以重新生成。

---

## 5. 快照与版本演进

### 5.1 快照

run 很长时，每次加载都从第 1 条事件折叠太慢。快照就是把某个时刻的 `RunView` 序列化存起来，加载时从快照开始，只应用之后的事件：

```go
func Load(ctx context.Context, s EventStore, runID string, snap *RunView) (RunView, error) {
	var v RunView
	if snap != nil {
		b, _ := json.Marshal(snap) // 深拷贝
		...
	}
	evs, err := s.Read(ctx, runID, v.LastSeq)
	...
}
```

`RunView.LastSeq` 记录了快照对应到哪一条事件，所以快照本身就知道从哪里接着读。

快照和 3.4 的检查点有什么区别？结构上很像，地位完全不同：**检查点是事实来源，丢了就丢了；快照只是缓存，丢了可以从事件重建**。所以快照可以随时删、可以因为投影逻辑改了而整体作废，不需要迁移。

练习的 `TestSnapshotPlusTailEqualsFullFold` 验证最重要的性质：快照 + 尾部事件得到的状态，和从头折叠完全相同（序列化后逐字节比较）。

### 5.2 事件的版本演进

事件只追加不修改，可业务一定会变。比如 `tool_finished` 第一版的字段叫 `output` 和 `error`，后来想改成 `result` 和 `is_error`。

不能 `UPDATE events SET data = ...`：那样就破坏了“事件是发生过的事实”，也让重放失去意义。标准做法叫 **upcasting**：每条事件带 `schema` 版本号，读的时候把旧版本转成新版本，原始数据不动。

```go
func upcast(e Event) (Event, error) {
	if e.Type == ToolFinished && e.Schema == 1 {
		var v1 struct {
			CallID string `json:"call_id"`
			Output string `json:"output"`
			Error  bool   `json:"error"`
		}
		...
		e.Data, e.Schema = b, 2
	}
	if want, ok := currentSchema[e.Type]; !ok || e.Schema != want {
		return e, fmt.Errorf("%w：%s v%d", ErrUnknownEvent, e.Type, e.Schema)
	}
	return e, nil
}
```

规则：

- 新增可选字段不用升版本（旧事件解码出零值，要能接受）；
- 改名、改类型、改含义要升版本并写升级函数，升级函数是纯函数；
- 版本只能往前升，多个版本链式升级（v1→v2→v3）；
- **不认识的版本要报错**。灰度发布时新代码先写了 v3，旧代码读到不能假装看懂。

这和 3.4 的“检查点带版本号”是一回事，只是粒度从整个检查点细化到每条事件。

---

## 6. 完整重放

### 6.1 重放、重新运行、时间旅行

三个容易混的概念：

| | 模型和工具 | 用途 |
|---|---|---|
| 重新运行 | 真调用 | 再跑一次同样的输入。模型输出不确定，结果多半不同 |
| LangGraph replay / time travel | 检查点之前不调用，之后**真调用** | 从某一步重新探索；`update_state` 改状态后分叉出新路径 |
| 完整重放（本课） | **一次都不调用**，用事件里录下的结果代替 | 在新代码上精确复现一次历史 run |

LangGraph 文档说得很清楚：从旧的 `checkpoint_id` 调用图时，之后的节点会重新执行，“包括任何 LLM 调用、API 请求和 interrupt”。这适合做“如果当时选另一条路会怎样”，但不能用来验证“新代码在完全相同的输入下行为是否一致”，因为模型的回复已经变了。

### 6.2 录像替身

完整重放的做法是：保留 agent 的全部代码（循环、历史拼接、工具分发、错误处理），只把模型和工具换成“读录像”的替身。

```go
a.Model = ModelFunc(func(_ context.Context, h []Message) (Message, Usage, error) {
	rec := models[mi]
	mi++
	if got := RequestHash(a.Version, a.toolNames(), h); got != rec.RequestHash {
		div = &Divergence{Step: rec.Step, Want: rec.RequestHash, Got: got}
		return Message{}, Usage{}, div
	}
	return rec.Response, rec.Usage, nil
})
```

关键是那个比较：替身在返回录下的回复之前，先计算新代码**此刻**要发给模型的请求指纹，和录制时的比。一致，说明新代码在这一步的行为和当时完全一样，返回录下的回复是合理的；不一致，说明新代码会给模型发不同的东西，录下的回复已经不能代表模型会怎么回答，重放在这里分叉，报告第几步。

工具替身同样检查名称和参数是否和录像一致，再返回录下的结果；录制时出错的，替身也返回同样的错误。

重放跑完后，用 `SameEvents` 逐条比较新产生的事件和录下的事件（忽略时间戳和全局位置）。完全一致，说明新代码对这个 run 是“行为等价”的。

### 6.3 重放用来做什么

- **调试**：线上失败的 run 拉下来本地重放，可以在任意一步打断点，不花一分钱 token，也不会再次触发副作用。`FirstToolError()` 直接告诉你第一个出错的工具在第几步。
- **回归测试**：改提示词、改工具定义、改历史拼接逻辑之前，拿一批历史 run 重放。哪些 run 会分叉、在第几步分叉，一目了然。分叉不一定是 bug，但你应该知道它。没分叉的 run 可以确定行为不变。
- **迁移验证**：换存储、重构循环之后，重放全部历史，事件逐条一致才算迁移正确。
- **评测数据**：分叉的那一步，正好是把“新请求”发给真模型做对比评测的起点（模块 08 评测会用到）。

`TestReplayDetectsDivergence` 测了两种分叉：改版本号（相当于改了系统提示词），以及多注册一个工具。两者都会在第 1 步分叉，因为第 1 步的请求就已经不同了。

### 6.4 什么会破坏重放

- `Apply` 里读了时钟或随机数；
- 拼历史时用了 map 遍历顺序（练习里 `toolNames()` 必须排序，`TestRequestHashStable` 专门测这个）；
- 请求里有不来自事件的内容（当前时间、实时检索结果），没有作为事件录下来；
- 厂商私有的不透明内容（加密 reasoning）没原样存。

---

## 7. 三家的存储模型对照

| | 状态放在哪 | 你自己要存什么 | 历史保留 |
|---|---|---|---|
| OpenAI Responses + `previous_response_id` | OpenAI 服务端，按响应链 | 只需最后一个 response id | 响应默认保存 30 天；`store: false` 关闭 |
| OpenAI Conversations API | OpenAI 服务端的 conversation 对象，存 messages、tool calls、tool outputs 等 items | conversation id | conversation 里的 items 不受 30 天限制 |
| OpenAI 手动管理（`store: false`） | 你自己 | 完整的 output items（含加密 reasoning、`phase`） | 你决定 |
| LangGraph checkpointer | 你选的后端（Postgres、SQLite、MongoDB 等） | 由 checkpointer 写 `checkpoints` + `writes` 两张表 | 你决定；可选 `EncryptedSerializer` 加密 |

两个提醒：

- 用了 `previous_response_id`，链上所有之前的输入 token 仍然按输入计费，服务端存状态不等于省钱（省钱靠 1.5 的 prompt caching）。
- 托管状态省事，但审计、重放、跨厂商迁移都要靠你自己的事件日志。比较稳妥的做法是：托管状态用来降低延迟和请求体大小，自己仍然写一份事件日志作为事实来源。

LangGraph 的三种持久化模式也值得对照：`exit` 只在图退出时写，`async` 在下一步执行时异步写，`sync` 每一步写完才继续。练习的 Agent 相当于 `sync`：每个事件 `Append` 成功才往下走。慢一点，但任何时刻崩溃都不会丢已经发生的事。

---

## 8. 表结构

练习目录里的 `schema.sql` 是 Postgres 版本，分两层：

**事实层**只有一张表：

```sql
CREATE TABLE events (
    pos         bigserial PRIMARY KEY,
    run_id      text NOT NULL,
    seq         int  NOT NULL,
    type        text NOT NULL,
    schema_ver  int  NOT NULL,
    data        jsonb NOT NULL,
    at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, seq)
);
```

`UNIQUE (run_id, seq)` 就是乐观锁：追加时写 `seq = expect + 1`，有人抢先就违反唯一约束。

**投影层**是 `runs`、`steps`、`tool_calls`、`projection_positions`，全部可以 `TRUNCATE` 后从 `events` 重建。几个设计点：

- `runs.last_seq`：投影到哪条了，用来判断投影是否落后；
- `tool_calls.ended_at IS NULL`：开始了没结束的调用，崩溃恢复和告警都查它；
- `tool_calls.idem_key UNIQUE`：外部系统去重用的键，也防止投影重复插入；
- `CREATE INDEX tool_calls_errors ON tool_calls (name) WHERE is_error`：部分索引，“哪个工具最近老出错”这类查询只扫出错的行。

`sessions` 不是从事件推出来的，是普通的实体表（用户创建、改名、删除会话）。不是所有东西都要事件溯源：**只有需要审计、重放、追溯历史的部分才值得**。会话标题改了就改了，没人需要重放它。

### 8.1 删除与隐私

只追加和“用户要求删除数据”是冲突的。几种做法：

- **按会话整体删**：事件按 `run_id` 组织，删一个会话就是删它所有 run 的事件和投影。最简单，大多数场景够用；
- **加密粉碎**（crypto-shredding）：敏感字段用每个用户一把的密钥加密存储，删除时只删密钥，事件还在但内容永远读不出来；
- **保留期**：事件超过 N 天归档到冷存储或删除，快照保留结果。

无论哪种，事件里本来就不该有密钥、token 这类凭据。工具参数如果可能含个人信息，在写事件之前就要想好怎么处理，而不是事后清洗。

---

## 9. 练习代码导读

| 文件 | 内容 |
|---|---|
| `events.go` | 事件类型和各自的数据结构；`EventStore` 接口；`MemStore`（`expect` 冲突检测、全局 `pos`）；`upcast` |
| `projection.go` | `RunView.Apply`（序号、版本、配对三道检查）；`Open`、`FirstToolError`；`Fold`、`Load`（快照）；`SessionIndex.CatchUp`；`SessionMessages` |
| `agent.go` | `Agent.Run` / `RunWithHistory`（每一步先写事件）；`RequestHash`；`Replay`（录像替身 + 分叉检测）；`SameEvents` |
| `schema.sql` | Postgres 参考表结构，练习 2 用 |
| `state_test.go` | 15 个测试 |

### 9.1 Run 的结构

和 3.2 的循环几乎一样，区别是每个动作之后都 `emit` 一个事件，`emit` 失败（比如冲突）就立刻返回：

```go
hash := RequestHash(a.Version, a.toolNames(), h)
m, u, err := a.Model.Next(ctx, h)
if err != nil {
	rec.emit(ctx, RunFailed, RunFailedData{Step: step, Error: err.Error()})
	break
}
rec.emit(ctx, ModelCalled, ModelCalledData{Step: step, RequestHash: hash, Response: m, Usage: u})
```

`recorder` 记着自己最后写到的 `seq`，下一次 `Append` 用它做 `expect`。函数最后返回的不是内存里的 `h`，而是 `Load` 从事件重新算出来的 `RunView`：保证调用方看到的就是存下来的。

### 9.2 Replay 的结构

`Replay` 先把录下的事件按类型拆成几个队列（模型回复、工具开始、工具结果），然后构造一个新的 `Agent`：版本和工具名集合来自线上 agent（这样指纹才有可比性），模型和工具实现换成从队列里取。它写入一个全新的 `MemStore`，跑完读出来就是“新代码在同样输入下产生的事件”。

录制时模型调用就失败的 run，模型队列在那一步是空的，替身会返回录下的错误，让失败也能原样重现（`TestReplayModelFailure`）。

---

## 10. 常见的坑

- **把消息表当事实来源**。加一个“工具耗时”字段时才发现历史数据里根本没有。
- **`UPDATE` 事件修数据**。修完以后重放结果和当时不一样，审计也失去意义。改用补偿事件或 upcast。
- **`Apply` 里有副作用**。投影重建一次就发一遍通知、扣一遍费。
- **只记 `tool_finished`**。进程在工具执行中崩溃，事件里看不出这个调用存在过。
- **投影位置和投影结果分开提交**。崩溃后要么漏要么重。
- **投影读不到自己的写**。run 刚结束就去查投影表，状态还是 running。
- **map 遍历顺序进了请求**。每次重放指纹都不一样，什么都检测不出来。
- **没存厂商的不透明内容**。手动管理状态时丢了加密 reasoning，模型行为变差还找不到原因。
- **所有东西都事件溯源**。会话标题、用户设置也做成事件，徒增复杂度。

---

## 11. 练习

### 练习 1：读懂并跑通（必做）

```bash
cd projects/21-state && make test
```

读完代码，回答：

1. `TestOpenToolCallAfterCrash` 里，工具 panic 之后事件流的最后一条是什么？恢复时应该怎么处理这个调用？
2. 把 `toolNames()` 里的 `sort.Strings` 删掉，哪个测试会失败？为什么不是每次都失败？
3. `Replay` 为什么要用线上 agent 的工具**名字**，却不用它的工具**实现**？

### 练习 2：Postgres 事件库（必做）

用 `schema.sql` 实现 `PgStore`，满足 `EventStore` 接口：

- `Append` 在一个事务里插入多条事件，`seq` 从 `expect+1` 开始；违反唯一约束时返回包装了 `ErrConflict` 的错误；
- 同一个事务里更新 `runs`、`steps`、`tool_calls` 投影（事件和投影同事务提交，就不存在“投影落后”）；
- `Read` 按 `seq` 排序，`ReadAll` 按 `pos` 排序并支持 `limit`；
- 把现有测试改成表驱动，`MemStore` 和 `PgStore` 都跑一遍。并发追加测试在 Postgres 上也必须只有 1 个成功。

再写一个 `Rebuild(ctx)`：`TRUNCATE` 三张投影表，从 `events` 全量重建，重建前后 `runs` 表内容一致。

### 练习 3：从某一步分叉（选做）

实现 `Fork(ctx, store, srcRun, atSeq, newRun)`：把 `srcRun` 的前 `atSeq` 条事件复制到 `newRun`（`run_started` 的 `session_id` 不变），然后让 agent 从这个状态接着**真跑**。

要求：

- `atSeq` 必须落在一个 `model_called` 或 `tool_finished` 之后，不能切在 `tool_started` 和 `tool_finished` 之间（否则新 run 一开始就有一个未完成的调用）；
- 新 run 的 `run_started` 加一个可选字段 `forked_from: {run_id, seq}`，不用升版本（想想为什么）；
- 写测试：分叉后的 run 前缀和原 run 一致，之后模型拿到的历史就是前缀推出来的历史。

这就是 LangGraph time travel 的事件溯源版本。

---

## 12. 自测题

1. 事件日志、快照、消息数组三种存法，分别能回答什么问题？为什么说事件日志是“以后才想到的问题”也能回答的那一种？
2. 为什么消息最好从事件推出来，而不是单独一张表？什么内容是例外，必须原样存？
3. `Append` 的 `expect` 参数解决什么问题？在 Postgres 里用什么实现？
4. 为什么工具调用要写 `tool_started` 和 `tool_finished` 两个事件？
5. `Apply` 为什么不能读时钟？如果业务确实需要时间，怎么办？
6. 快照和 3.4 的检查点结构很像，地位有什么不同？
7. 事件字段改名了，为什么不能直接 `UPDATE`？upcasting 怎么做？遇到不认识的版本应该怎样？
8. “完整重放”和 LangGraph 的 replay 有什么区别？各适合做什么？
9. 请求指纹在重放里起什么作用？哪些情况会让它在没改代码时也对不上？
10. 投影的读取位置为什么要和投影结果在同一个事务里提交？

## 13. 自测题答案

1. 消息数组只能回答“下一轮发给模型什么”；快照能回答“当前（或某一步）是什么状态”；事件日志记录了每件发生过的事，任何能从这些事推出来的问题都能回答。以后想加一个统计（比如工具耗时），只要写一个新投影从头折叠一遍，历史数据就有了；另外两种存法在写入时就丢掉了中间过程。
2. 给模型的历史是按规则从事件拼出来的，规则会变（截断、摘要、过滤）。单独存一张表就得同步维护原始数据和给模型的数据两份。例外是厂商私有、无法还原的内容，比如 OpenAI 的加密 reasoning item 和 `phase` 字段，要原样存在事件里。
3. 防止两个写入者基于同一个旧状态各自追加，造成覆盖或交错。`expect` 不等于当前最后一个 seq 就拒绝。Postgres 里用 `UNIQUE (run_id, seq)`，第二个插入同一 seq 的事务违反约束失败。
4. 只有一个事件时，进程在工具执行中崩溃，事件里就完全看不出这个调用存在过，恢复后会当它没发生，可能重复执行有副作用的操作。拆成两个，“有开始没结束”就是需要核对的调用，配合幂等键去外部系统查实际结果。
5. `Apply` 必须是确定性的：同样的事件序列永远得到同样的状态，否则投影重建、快照校验、重放都不成立。需要时间就在产生事件时取时间写进事件（事件的 `at` 字段或数据字段），`Apply` 只读不取。
6. 检查点是事实来源，丢了状态就没了，格式变化需要迁移；快照只是事件的缓存，丢了可以重建，投影逻辑改了可以整体作废。
7. `UPDATE` 改变了“发生过的事实”，重放结果会和当时不一致，审计也失去依据。upcasting：每条事件带版本号，读取时用纯函数把旧版本转成当前版本，原始数据不动，多个版本链式转换。遇到不认识的版本必须报错，不能假装看懂（比如灰度时新代码写了新版本、旧代码在读）。
8. LangGraph 从某个检查点继续时，之后的节点会真的重新执行，包括模型和 API 调用，适合探索“换一条路会怎样”（配合 `update_state` 分叉）。完整重放一次都不真调用，用录下的结果替代，适合在新代码上精确复现历史 run：调试、回归测试、迁移验证。
9. 替身在返回录下的模型回复前，先算新代码此刻要发的请求指纹，和录制时的比。一致说明新代码到这一步行为不变；不一致说明会发出不同请求，录下的回复不再可信，重放在此分叉并报告步骤。没改代码也对不上的原因：拼请求时用了 map 遍历顺序、`Apply` 读了时钟或随机数、请求里有未作为事件录下的内容（当前时间、实时检索结果）。
10. 分开提交时，先存结果后存位置，中间崩溃会重复处理同一批事件；先存位置后存结果，崩溃就会漏掉。同一个事务要么都提交要么都不提交。（即便如此，投影本身也最好写成幂等的，比如用 `idem_key` 唯一约束挡住重复插入。）

---

## 14. 参考答案

### 练习 1

1. 最后一条是调用 `pay` 的 `tool_started`，`idem_key` 为 `r/pay`，后面没有对应的 `tool_finished`。恢复时这个调用出现在 `Open()` 里。不能直接重新执行：先用幂等键去外部系统查这笔扣款是否已经发生，发生了就补写 `tool_finished`（结果取外部系统返回的），没发生再重试（仍用同一个幂等键）；查不到或外部系统不支持查询，就交给人决定（3.4 的审批流程）。
2. `TestRequestHashStable` 会失败。Go 的 map 遍历顺序是故意随机化的，但三个键时某些顺序会重复出现，50 次里大概率至少有一次和第一次不同，所以几乎总会失败，但理论上可能偶然通过。重放测试一般也会失败，因为录制和重放时的工具名顺序可能不同。
3. 工具名集合是请求的一部分（模型能看到有哪些工具），影响指纹，所以必须和线上一致，否则无从比较；而工具实现在重放时绝不能真的执行（会产生副作用、花钱、结果还不一样），所以换成读录像的替身。

### 练习 2

```go
type PgStore struct{ DB *sql.DB }

func (s *PgStore) Append(ctx context.Context, runID string, expect int, evs ...Event) (int, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for i, e := range evs {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO events (run_id, seq, type, schema_ver, data) VALUES ($1,$2,$3,$4,$5)`,
			runID, expect+i+1, e.Type, e.Schema, []byte(e.Data))
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			return 0, fmt.Errorf("%w：%s seq %d", ErrConflict, runID, expect+i+1)
		}
		if err != nil {
			return 0, err
		}
		if err := project(ctx, tx, runID, expect+i+1, e); err != nil {
			return 0, err
		}
	}
	return expect + len(evs), tx.Commit()
}
```

要点：

- 冲突靠唯一约束，不需要 `SELECT ... FOR UPDATE`。注意不能先 `SELECT max(seq)` 再插入，那中间有竞争窗口；
- `project` 按事件类型写投影：`run_started` 插 `runs`，`model_called` 插 `steps` 并累加 `runs` 的 token，`tool_started` 插 `tool_calls`，`tool_finished` 更新 `result / is_error / ended_at`，`run_completed / run_failed` 更新 `runs.status`。每次都把 `runs.last_seq` 更新为当前 seq；
- `project` 里先 `upcast` 再处理，和 `RunView.Apply` 用同一套升级逻辑；
- 事件和投影在同一个事务，所以这三张表永远不会落后于 `events`（会话索引这类跨 run 的投影仍可以异步追赶）；
- 表驱动测试：

```go
for _, tc := range []struct {
	name string
	new  func(t *testing.T) EventStore
}{
	{"mem", func(*testing.T) EventStore { return &MemStore{} }},
	{"pg", newPg}, // 每个测试建独立 schema 或 TRUNCATE
} { ... }
```

- `Rebuild`：一个事务里 `TRUNCATE runs, steps, tool_calls`，然后 `SELECT * FROM events ORDER BY pos`，每条调用 `project`。验证方式：重建前 `SELECT * FROM runs ORDER BY id` 存一份，重建后再查，逐行比较。

### 练习 3

```go
func Fork(ctx context.Context, s EventStore, src string, at int, dst string) (RunView, error) {
	evs, err := s.Read(ctx, src, 0)
	if err != nil {
		return RunView{}, err
	}
	if at < 1 || at > len(evs) {
		return RunView{}, fmt.Errorf("at 超出范围")
	}
	prefix := evs[:at]
	if v, _ := Fold(prefix); len(v.Open()) > 0 {
		return RunView{}, fmt.Errorf("不能切在工具执行中间")
	}
	copied := make([]Event, len(prefix))
	for i, e := range prefix {
		copied[i] = Event{Type: e.Type, Schema: e.Schema, Data: e.Data}
	}
	// 给 run_started 加 forked_from
	var d map[string]any
	_ = json.Unmarshal(copied[0].Data, &d)
	d["forked_from"] = map[string]any{"run_id": src, "seq": at}
	copied[0].Data, _ = json.Marshal(d)
	if _, err := s.Append(ctx, dst, 0, copied...); err != nil {
		return RunView{}, err
	}
	return Load(ctx, s, dst, nil)
}
```

然后给 `Agent` 加一个 `Continue(ctx, runID)`：`Load` 出 `RunView`，用 `v.Messages` 作为 `h`，`step` 从 `len(v.Steps)+1` 开始，`recorder.seq` 从 `v.LastSeq` 开始，进入同一个循环。可以把 `RunWithHistory` 的循环体抽成 `loop(ctx, rec, h, step)` 两边共用。

`forked_from` 不用升版本：它是新增的可选字段，旧代码解码 `RunStartedData` 时会忽略它，新代码读旧事件时得到零值。只有改名、改类型、改含义才需要升版本。

测试：录一个两步的 run，在第 2 条（`model_called` 之后）分叉，换一个脚本模型接着跑；检查新 run 前两条事件的类型和数据（除 `forked_from`）与原 run 一致，新模型第一次被调用时拿到的历史等于原 run 前缀推出的 `Messages`。再测切在 `tool_started` 之后会返回错误。

---

## 完成标准

- `make test` 全部通过（15 个测试），`go test -race` 通过；
- 练习 1 的三个问题能讲清楚，尤其是第 3 题“为什么用名字不用实现”；
- 练习 2 的 `PgStore` 写完，`MemStore` 和 `PgStore` 都通过表驱动测试，`Rebuild` 前后 `runs` 表一致；
- 能画出会话、run、step、工具调用、消息的关系，并说清楚哪些是事实、哪些是投影；
- 能说出完整重放和 LangGraph replay 的区别，以及请求指纹在什么情况下会误报；
- 自测题 10 道能不看答案讲出来，尤其是第 4、5、7、9 题。
