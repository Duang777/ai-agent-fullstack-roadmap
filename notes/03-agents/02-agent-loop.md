# 3.2 Agent Loop 设计：停止条件、预算、重试

> 第 3 部分 · 第 2 课 ｜ 预计 3–4 小时 ｜ 前置：3.1 ReAct、1.4 推理模型、1.5 Prompt Caching
>
> 配套练习：`projects/18-loop/`（纯 Go 标准库，14 个测试）。在 3.1 的 ReAct 循环外面加上生产需要的三层：把两家的停止原因归一成 7 种并分别处理（续写、原样发回、拒绝、窗口满）；五个维度的预算（步数、工具次数、token、费用、墙钟）和预算用完时的收尾调用；可重试错误的分类、全抖动指数退避、retry-after 下限、总等待上限、等待中可取消；单个工具的超时。

## 读前说明

3.1 课的 ReAct 循环只有两个出口：模型不再调用工具（正常结束），或者步数到了上限。真实环境里，一次模型调用可能有七八种结局：

- 输出写到一半被 `max_tokens` 截断；
- 服务端工具（网页搜索之类）跑到迭代上限，暂停了；
- 模型拒绝回答；
- 上下文窗口被撑满；
- 请求本身失败：429 限流、529 过载、500、网络断开、额度用完……

每一种的正确处理都不同。处理错了，轻则浪费钱，重则把半截结果当成答案交给用户，或者对一个“额度用完”的错误无限重试。

先看几个问题：

- `stop_reason` 是 `max_tokens` 时，能不能直接把内容当回答？截断的是一个工具调用呢？
- 预算只限步数够不够？一个步数很少的任务为什么也可能花掉很多钱？
- 预算用完时，直接报错和让模型“总结一下”哪个好？
- 同样是 429，为什么有的应该重试，有的重试一万次也没用？
- 指数退避为什么要加随机？retry-after 头该怎么用？
- 你自己写了重试，SDK 也有自动重试，会发生什么？

本课结束时你能：

1. 把各家的停止原因归一，并给每一种写出正确的处理；
2. 设计多维度的预算，在每次调用前检查，预算用完时让模型收尾；
3. 区分可重试和不可重试的错误，写出带抖动、带上限、可取消的重试；
4. 给工具调用加超时，并把超时变成模型能理解的观察结果。

**关于来源**：停止原因来自 Anthropic 文档 *Handling stop reasons* 和 OpenAI *Reasoning* 指南（Responses 的 `incomplete_details`）；错误与重试来自 Anthropic *Errors*、*Rate limits*，OpenAI *Error codes*、*Rate limits*，Gemini *Troubleshooting*。均为 2026-10 文档。

---

## 1. 循环的骨架

和 3.1 相比，循环的每一轮多了两件事：调用前查预算，调用时包一层重试；调用后不再只看“有没有工具调用”，而是看归一后的停止原因：

```text
for {
    检查 ctx
    检查预算 ── 超了 → （可选）收尾调用 → 返回
    带重试地调用模型 ── 失败 → 返回
    按停止原因分支：
        done        → 返回回答
        tool_use    → 执行工具（带超时），继续
        paused      → 原样发回，继续
        truncated   → 续写（有上限），或者报错
        refused     → 返回拒绝
        context_full→ 返回，交给压缩（1.6 课）
        unknown     → 返回错误
}
```

不管从哪个出口离开，都返回 `Result`：回答、停止原因（`done`、`budget:cost`、`refused` ……）、花了多少（步数、工具次数、token、费用）、完整历史。失败时这些信息比成功时更重要，排查和计费都要用。

---

## 2. 停止原因

### 2.1 两家的原始值

**Anthropic Messages API** 的每个成功响应都有 `stop_reason`，文档的速查表：

| 值 | 什么时候出现 | 怎么处理 |
|---|---|---|
| `end_turn` | 自然结束 | 使用回答 |
| `max_tokens` | 撞到你设的 `max_tokens` | 调大，或者续写 |
| `stop_sequence` | 输出了你设的停止序列 | 看 `stop_sequence` 字段是哪个 |
| `tool_use` | 要调用工具 | 执行工具，返回结果 |
| `pause_turn` | 服务端工具循环到了迭代上限（默认每个请求 10 次） | 把 assistant 内容原样发回，让它继续 |
| `refusal` | 拒绝回答 | 看 `stop_details`，换备用模型重试 |
| `model_context_window_exceeded` | 回答撑满了上下文窗口 | 当作截断处理 |

**OpenAI Responses API** 没有一个统一的字段：

- `status: "completed"`，output 里有 `function_call` → 要调工具；没有 → 正常结束；
- `status: "incomplete"`，看 `incomplete_details.reason`：`max_output_tokens` 是截断（推理模型可能**在推理阶段就耗尽**，一个可见字都没有，但输入和推理 token 照样计费，1.4 课），`content_filter` 是被内容过滤拦下。

### 2.2 归一

循环只认 7 种，供应商差异留在适配层：

```go
func FromAnthropic(stopReason string) Stop {
	switch stopReason {
	case "end_turn", "stop_sequence":
		return StopDone
	case "tool_use":
		return StopToolUse
	case "max_tokens":
		return StopTruncated
	case "pause_turn":
		return StopPaused
	case "refusal":
		return StopRefused
	case "model_context_window_exceeded":
		return StopContextFull
	}
	return StopUnknown // 新增的值不要当成正常结束
}
```

最后一行最重要。API 会加新的停止原因（`pause_turn`、`refusal`、`model_context_window_exceeded` 都是后来加的）。如果你的 `default` 分支是“当作正常结束”，新值出现时，半截的或者被拒绝的内容就会被当成答案。未知的值一律当错误，日志里能看到，再补映射（`TestStopMapping`）。

### 2.3 每种怎么处理

**截断（开头第一个问题）**：不能直接当回答，内容是不完整的。两种情况：

- 截断的是普通文本：可以续写。把半截回答放回历史，再追加一条要求“从断开的地方接着写”的用户消息。续写要有次数上限，否则一个本来就写不完的任务会一直续下去（`TestTruncatedLimits`）；
- 截断时有工具调用：参数 JSON 只写了一半，**既不能执行，也没法续写**，直接报 `ErrTruncated`。正确的修法是调大 `max_tokens`。

**暂停**：服务端工具（网页搜索、代码执行）在 Anthropic 那边跑循环，到迭代上限就暂停。把 assistant 消息原样追加到历史，**不加任何用户消息**，再发一次请求，服务端就会接着跑（`TestPausedAndTruncated` 检查第二次请求的最后一条是 assistant）。暂停也算一步，同样受预算约束。

**拒绝**：返回 `ErrRefused`。文档建议读 `stop_details` 后在备用模型上重试，这是练习 3。不要把拒绝的内容当回答展示，也不要在同一个模型上原样重试。

**窗口满**：返回 `ErrContextFull`，由上层压缩历史（1.6 课的预算内组装、摘要）后再继续。循环自己不应该偷偷截掉历史，那会破坏工具调用和结果的配对。

---

## 3. 预算

### 3.1 为什么要五个维度

开头第二个问题。3.1 只限步数，但步数和花费没有固定关系：

- 一步可以很贵：工具返回一个 10 万 token 的网页，之后每一步都要把它发回去（1.6 课），推理模型一步可能想几万 token；
- 一步可以很慢：一个工具跑了几分钟；
- 一条消息可以有很多个工具调用：步数只算了一次，工具调了二十次，每次都可能有副作用或按次计费。

所以练习里的预算有五项，任何一项为 0 表示不限：

```go
type Budget struct {
	MaxSteps     int           // 模型调用次数
	MaxToolCalls int           // 工具调用次数
	MaxTokens    int           // 累计 token
	MaxCost      int64         // 累计费用，微美元
	MaxWall      time.Duration // 墙钟时间
}
```

### 3.2 费用用整数，向上取整

```go
// Price 每百万 token 的价格，单位微美元（1 美元 = 1e6）
func (p Price) Cost(u Usage) int64 {
	n := int64(u.Input)*p.Input + int64(u.CachedInput)*p.CachedInput + int64(u.Output)*p.Output
	return (n + 999_999) / 1_000_000
}
```

- **整数**：浮点数累加几千次会有误差，账对不上。用微美元（或者更细）存整数是计费系统的常规做法；
- **向上取整**：一个 token 也要算钱（`TestCostRoundsUp`）。向下取整的话，大量小请求加起来可以悄悄突破预算；
- **缓存命中的输入单独计价**（1.5 课）。`Usage.Input` 不含缓存部分，避免重复计算。推理 token 按输出计价，适配层要把它算进 `Output`（1.4 课）。

价格表要按模型配置，并且随供应商调价更新。课件里的数字只是测试用。

### 3.3 什么时候检查

**每次调用模型之前**检查：已经超了就不要再发请求（`Budget.Check`）。这意味着预算是“软上限”：最后一次调用可能让总量略超过上限，因为调用之前不知道它会花多少。要做成硬上限，需要调用前预估这一次最多花多少（输入 token 加上 `max_tokens`），剩余预算不够就不发，这是练习 2。

只有**成功**的调用计入步数和用量（`TestRetryInLoop` 里两次失败一次成功，`Steps` 是 1）。失败的请求一般不计费；如果你的供应商对某些失败也计费，就要在适配层把那部分用量报上来。

墙钟时间通过注入的 `Now` 函数检查，测试里用假时钟推进（`TestBudgetWallClock`），不用真的等一分钟。

### 3.4 预算用完：收尾调用

开头第三个问题。直接报错，用户拿到的是“任务失败”，前面九步查到的东西全部丢掉。更好的做法是再做**一次**特殊调用：

```go
if cfg.Finalize {
	h = append(h, Message{Role: User, Content: "预算已用完，不能再调用工具。请根据目前已有的信息给出最终回答，并说明哪些部分没能完成。"})
	r, e := call(CallOpts{NoTools: true})
	if e == nil && r.Stop == StopDone {
		h = append(h, r.Message)
		return res("finalized:"+be.Dim, r.Message.Content), err
	}
}
```

三个要点（`TestFinalizeOnBudget`）：

- **禁用工具**：`NoTools` 在适配层对应不传 tools，或者设置 `tool_choice` 为 none，否则模型很可能接着调工具；
- **告诉模型为什么停**，并要求它说明哪些部分没完成。否则它可能编出没查到的内容来凑一个完整答案；
- **仍然返回预算错误**，`Reason` 是 `finalized:steps`。调用方要知道这是一个不完整的结果，界面上可以标出来，也可以让用户选择追加预算继续。

收尾调用本身也要花钱。它只有一次，而且没有工具，花费有限；对费用非常敏感的场景，可以在预算里预留一小部分给它。

---

## 4. 重试

### 4.1 哪些该重试

开头第四个问题。三家文档的说法基本一致：

- **429 限流**：Anthropic 按每分钟请求数、输入 token、输出 token 限流，超过任何一项返回 429，有 `retry-after` 头时等那么多秒再试；OpenAI 同样建议遵守 `Retry-After`；Gemini 是 `429 RESOURCE_EXHAUSTED`；
- **5xx**：Anthropic 的 500 `api_error`、504 `timeout_error`、529 `overloaded_error`；OpenAI 的 503；Gemini 的 `503 UNAVAILABLE`；
- **网络错误**：连接被重置、读超时。

不该重试的：

- **400、401、403、404、413**：请求本身有问题，重试结果一样；
- **额度和花费类的 429**：Anthropic 的月度花费上限也返回 429、类型也是 `rate_limit_error`，但**没有** `retry-after` 头，`error.details.error_code` 是 `enforced_spend_limit_reached`，文档明确说重试（包括 SDK 的自动重试）会一直失败；OpenAI 的 `credit_balance_exhausted`（预付额度用完）同理。OpenAI 文档的原话是不要重试额度、计费等需要你去处理的错误；
- **`context.Canceled`**：用户不要了。

```go
func Retryable(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		return true // 连接被重置、读超时之类：重试
	}
	switch {
	case ae.Status == 429:
		return !notRetryableCodes[ae.Code]
	case ae.Status == 408 || ae.Status == 409:
		return true
	case ae.Status >= 500:
		return true
	}
	return false
}
```

`TestRetryable` 列了两组用例。注意判断额度类 429 靠的是错误码，不是“有没有 retry-after”：没有 retry-after 的普通限流也存在，这时退回指数退避。

### 4.2 等多久

```go
func (r Retry) Delay(attempt int, err error) time.Duration {
	var ae *APIError
	if errors.As(err, &ae) && ae.RetryAfter > 0 {
		return ae.RetryAfter + time.Duration(r.Rand.Int63n(int64(r.Base)+1))
	}
	d := r.Base << attempt
	if d <= 0 || d > r.Cap { // <=0 是移位溢出
		d = r.Cap
	}
	return time.Duration(r.Rand.Int63n(int64(d) + 1))
}
```

开头第五个问题：

- **有 retry-after**：OpenAI 文档说把它当作**最小值**，至少等这么久，再加一点随机，免得很多客户端在同一时刻一起重试；
- **没有 retry-after**：指数退避加抖动。这里用的是“全抖动”：在 `[0, min(Cap, Base·2^attempt)]` 里均匀取值。不加抖动的话，同一时刻被限流的一批客户端会在完全相同的时刻重试，再次一起被限流；
- **封顶**：`Base << attempt` 在 attempt 很大时会溢出成负数或 0，要一并处理（`TestRetryDelay` 测了 attempt = 70）。

随机数源和 `Sleep` 都是注入的，测试里不真的等，也能断言等待时间。

### 4.3 什么时候放弃

```go
func (r Retry) Do(ctx context.Context, f func(context.Context) error) error {
	for a := 0; a < r.MaxAttempts; a++ {
		if err = f(ctx); err == nil {
			return nil
		}
		if !Retryable(err) {
			return err
		}
		...
		d := r.Delay(a, err)
		if r.MaxTotal > 0 && waited+d > r.MaxTotal {
			return &RetryError{a + 1, err}
		}
		if e := r.Sleep(ctx, d); e != nil {
			return e
		}
		waited += d
	}
	return &RetryError{r.MaxAttempts, err}
}
```

- **两个上限**：次数和总等待时间。OpenAI 文档要求同时限制两者。只限次数的话，一个 `retry-after: 3600` 会让请求挂一个小时（`TestRetryMaxTotalAndCancel` 第一部分：4 秒加 4 秒之后，再等 4 秒就超过 10 秒总上限，于是放弃）；
- **等待可取消**：`SleepCtx` 用 `select` 同时等定时器和 `ctx.Done()`，用户取消时立即返回，而不是睡完一小时再发现（第二部分）；
- **保留原始错误**：`RetryError` 实现了 `Unwrap`，上层可以 `errors.As` 拿到最后一次的 `APIError`，知道是 529 还是 503（`TestRetryInLoop`）。

### 4.4 别叠两层重试

开头最后一个问题。官方 SDK 默认就会重试：Anthropic SDK 对连接错误、限流和 5xx 默认重试 2 次，遵守 `retry-after`，用 `max_retries` 配置；OpenAI 的 SDK 自动重试符合条件的 429 和 503；Gemini 的 Python SDK 默认最多重试 4 次，初始约 1 秒、最长 60 秒。

如果你在外面再包一层 3 次重试，最坏情况是 3 × 3 = 9 次请求，等待时间也成倍增加，限流时还会加重拥堵。OpenAI 文档的建议是：在应用层自己管重试时，要么关掉 SDK 的重试，要么把它算进你的上限。练习里的 `Retry` 假设 SDK 重试已经关掉。

### 4.5 流式响应中途出错

Anthropic 文档提到，流式响应可能在返回 200 之后才出错（SSE 里的 error 事件），这时不走上面的 HTTP 错误流程。适配层要把这种错误也转成 `APIError`；而且已经收到的半截内容不能当成完整回答，重试要从头再来（0.4 课的流式协议）。

---

## 5. 工具超时

模型调用有重试，工具调用也需要保护。一个卡住的工具会占满墙钟预算，而模型完全不知道发生了什么：

```go
tctx, cancel = context.WithTimeout(ctx, cfg.ToolTimeout)
defer cancel()
out, err := t(tctx, c.Args)
if err != nil {
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return Message{..., IsError: true, Content: "工具超时（" + cfg.ToolTimeout.String() + "），可以缩小范围再试"}
	}
	...
}
```

- 超时和 3.1 的工具错误一样，是**观察结果**，交给模型决定换个做法（`TestToolTimeout`）；
- `ctx.Err() == nil` 用来区分“工具自己超时”和“整个任务被取消”：后者不应该包装成观察结果；
- 工具必须遵守 `ctx`，否则超时只是不再等它，goroutine 和它占用的资源还在（2.5 课的进程组就是为了这个）。

工具调用要不要自动重试，取决于它是否**幂等**。查询类工具可以重试，发邮件、下单不行。这个话题在 3.4 长任务和模块 08 高可用里展开。

---

## 本课小结

1. 一次模型调用有七八种结局，循环要按归一后的停止原因分别处理；未知的原因一律当错误。
2. 截断的文本可以有限次续写；截断的工具调用既不能执行也不能续写。
3. `pause_turn` 原样发回、不加用户消息；拒绝不要原样重试；窗口满交给压缩。
4. 预算至少五个维度：步数、工具次数、token、费用、墙钟。费用用整数、向上取整，每次调用前检查。
5. 预算用完时做一次禁用工具的收尾调用，告诉模型为什么停，并仍然向调用方报告预算错误。
6. 429 限流、5xx、网络错误重试；4xx 和额度类 429 不重试；用户取消不重试。
7. retry-after 是下限，没有时用全抖动指数退避；同时限制次数和总等待，等待要可取消，保留原始错误。
8. 应用层和 SDK 的重试不要叠加；工具要有超时，超时是观察结果。

---

## 练习（今天做完）

### 练习 1：跑通并读懂

```bash
cd projects/18-loop
make test
```

回答：

1. 把 `FromAnthropic` 的 `default` 改成 `return StopDone`，哪个测试会失败？在生产里会造成什么问题？
2. `TestRetryInLoop` 里为什么 `Spent.Steps` 是 1 而不是 3？如果把失败的调用也计入步数，会有什么后果？
3. `TestFinalizeOnBudget` 里，为什么收尾成功后仍然返回 `BudgetError`？

### 练习 2：硬性的费用上限

现在的预算是软上限：最后一次调用可能让费用超过 `MaxCost`。

要求：给 `Budget` 加一个 `Reserve(s Spent, est Usage, p Price) error`，用“已花费 + 这次调用的最坏情况”判断。最坏情况的输入用估算的 token 数（1.6 课），输出用这次请求的 `max_tokens`。在 `Run` 里每次调用前，先 `Check` 再 `Reserve`。

测试：已花 9000 微美元，上限 10000；估算 1000 输入、4096 输出（$3 / $15 每百万）应拒绝；100 输入、40 输出应放行。

### 练习 3：备用模型

Anthropic 文档对 `refusal` 的建议是在备用模型上重试；连续 529 过载时，换一个供应商也比一直等好。

要求：`Config` 加 `Fallback Model`。主模型返回 `StopRefused`，或者重试用完后最后一个错误是 529/503 时，换成备用模型重新发这一步（历史不变），整个任务最多切换一次。`Result` 里记录是否切换过。拒绝的那条消息不要放进历史。

测试：主模型第一步拒绝，备用模型正常回答，`Answer` 来自备用模型、历史里没有拒绝的消息；主模型连续 3 次 529 后切到备用模型成功；备用模型也拒绝时返回 `ErrRefused`，不再切换。

---

## 自测题

1. 列出 Anthropic 的 7 种 `stop_reason` 和各自的处理方式。OpenAI Responses 怎么判断截断和工具调用？
2. 为什么未知的停止原因不能当作正常结束？
3. 截断时什么情况可以续写，什么情况不行？续写为什么要有上限？
4. `pause_turn` 应该怎么处理？为什么不加用户消息？
5. 预算为什么不能只限步数？举出三种步数少但代价大的情况。
6. 费用为什么用整数、为什么向上取整？“每次调用前检查”为什么只是软上限？
7. 预算用完时的收尾调用要注意哪三点？
8. 哪些错误该重试，哪些不该？Anthropic 的花费上限 429 和普通限流 429 怎么区分？
9. retry-after 怎么用？为什么退避要加随机？为什么要同时限制次数和总时间？
10. 应用层和 SDK 都重试会怎样？工具超时为什么要变成观察结果，又怎么和“任务被取消”区分？

---

# 参考答案与详解

## 自测题答案

**1.** `end_turn` 使用回答；`max_tokens` 调大或续写；`stop_sequence` 看是哪个停止序列；`tool_use` 执行工具返回结果；`pause_turn` 原样发回继续；`refusal` 读 `stop_details`、换备用模型；`model_context_window_exceeded` 当作截断，交给压缩。OpenAI Responses 看 `status`：`incomplete` 且 `incomplete_details.reason` 为 `max_output_tokens` 是截断（可能在推理阶段就耗尽，没有可见输出但照样计费），`content_filter` 是被过滤；`completed` 时看 output 里有没有 `function_call` 判断是否要调工具。

**2.** API 会新增停止原因。当成正常结束，新出现的截断、暂停或拒绝的内容就会作为答案交给用户，而且没有任何报错，很难发现。当作错误，日志里马上能看到，补上映射即可。

**3.** 截断的是普通文本时可以续写：半截回答放回历史，要求从断点接着写。截断时有工具调用不行：参数 JSON 不完整，不能执行，续写也接不上。上限是因为有的任务在当前 `max_tokens` 下本来就写不完，无限续写只会一直花钱，正确做法是调大 `max_tokens` 或拆分任务。

**4.** 把 assistant 内容原样追加到历史，不加任何用户消息，再发一次请求，服务端会接着跑完剩下的服务端工具循环。加了用户消息就变成新的一轮对话，模型可能放弃没完成的搜索，转而回应这条新消息。

**5.** 步数和代价不成比例。工具返回超长内容，之后每一步都要重发；推理模型一步思考几万 token；一个工具卡住几分钟；一条消息里有很多工具调用，每次都有副作用或按次计费。所以要同时限 token、费用、墙钟和工具次数。

**6.** 浮点数累加有误差，账对不上，用微美元整数存储是计费常规做法。向上取整保证每个 token 都计费，大量小请求不会靠舍入悄悄突破预算。调用前检查只知道已经花了多少，不知道这一次会花多少，最后一次调用可能超出上限；要做成硬上限，需要预估这次的最坏情况（练习 2）。

**7.** 禁用工具（不传 tools 或 `tool_choice` 设为 none），否则模型会接着调用；明确告诉模型预算用完了，并要求说明没完成的部分，避免它编造；仍然向调用方返回预算错误，标记结果不完整，让界面或用户决定下一步。

**8.** 该重试：429 限流、5xx（500、503、504、529）、408/409、网络错误。不该重试：400、401、403、404、413 等请求本身有问题的错误，额度和花费类 429，以及用户取消。Anthropic 的花费上限 429 类型同样是 `rate_limit_error`，但没有 `retry-after` 头，`error.details.error_code` 是 `enforced_spend_limit_reached`；判断要靠错误码，因为普通限流也可能没有 retry-after。

**9.** retry-after 当作最小等待时间，至少等这么久再加一点随机。不加随机的话，同时被限流的客户端会在同一时刻重试，再次一起被限流。只限次数，一个很长的 retry-after 会让请求挂很久；只限时间，大量快速失败会打出很多请求。两个都要限，等待还要能被 `ctx` 取消。

**10.** 次数会相乘（应用层 3 次 × SDK 3 次 = 最坏 9 次），等待时间成倍增加，限流时加重拥堵；要么关掉 SDK 重试，要么把它算进上限。工具超时变成观察结果，模型才能换个做法（缩小范围、换工具），而不是让整个任务失败。区分方法是检查外层 `ctx`：外层 `ctx.Err()` 为 nil 而工具返回 `DeadlineExceeded`，说明是工具自己的超时；外层也出错了，说明是整个任务被取消或超时，应该直接结束循环。

## 练习参考答案

### 练习 1

1. `TestStopMapping` 的 `something_new` 用例会失败（`TestRefusedContextUnknown` 的 `StopUnknown` 用例走的是另一条路径，不受影响）。生产里，API 新增一种停止原因时，那种情况下的内容会被当成正常回答交给用户：可能是半截的、可能是暂停中的、可能是拒绝，而且没有任何报错或告警。
2. 前两次请求失败了，没有产生模型输出，一般也不计费；步数代表“模型做了几步决策”。如果失败也计入步数，一次限流高峰就可能把步数预算耗光，任务明明一步都没做就因为“预算用完”而结束；而且失败率高时步数统计和真实工作量对不上，影响排查和成本分析。
3. 收尾回答是基于不完整信息给出的。返回 `BudgetError`（以及 `Reason` 为 `finalized:steps`）让调用方知道结果不完整：界面上可以标注，可以提示用户追加预算继续，评测和监控里也能把这类结果和真正完成的任务分开统计。只返回回答、不报错，调用方就会把它当成完整结果。

### 练习 2

```go
func (b Budget) Reserve(s Spent, est Usage, p Price) error {
	next := s
	next.Usage.Add(est)
	next.Cost += p.Cost(est)
	if b.MaxTokens > 0 && next.Usage.Total() > b.MaxTokens {
		return &BudgetError{"tokens", int64(next.Usage.Total()), int64(b.MaxTokens)}
	}
	if b.MaxCost > 0 && next.Cost > b.MaxCost {
		return &BudgetError{"cost", next.Cost, b.MaxCost}
	}
	return nil
}
```

注意单位：价格是“每百万 token 的微美元”。`Price{Input: 3_000_000, Output: 15_000_000}` 下，1000 输入、4096 输出的最坏费用是 (1000 × 3e6 + 4096 × 15e6) / 1e6 = 64440 微美元，远超剩余的 1000，拒绝；100 输入、40 输出是 900 微美元，放行。

在 `Run` 里，`Check` 之后、`call` 之前调用 `Reserve`。估算的输入 token 可以用 1.6 课的估算函数，或者各家的 token 计数接口；输出用 `max_tokens`，推理模型要把推理预算算进去。这样做偏保守：模型通常用不满 `max_tokens`，所以剩余预算较少时会提前停下。可以在预算不够时调小这一次的 `max_tokens`，而不是直接拒绝。

### 练习 3

```go
// Config 增加：
Fallback Model

// Result 增加：
UsedFallback bool

// Run 里，调用模型的地方：
model := cfg.Model
switched := false
...
r, err := callWith(model, CallOpts{})
if !switched && cfg.Fallback != nil && shouldFallback(r, err) {
	switched, model = true, cfg.Fallback
	r, err = callWith(model, CallOpts{}) // 历史 h 没变，拒绝的消息没有追加
}

func shouldFallback(r Response, err error) bool {
	if err == nil {
		return r.Stop == StopRefused
	}
	var ae *APIError
	return errors.As(err, &ae) && (ae.Status == 529 || ae.Status == 503)
}
```

要点：`call` 改成接收模型参数；切换后后续步骤都用备用模型（`model` 变量在循环外）；`switched` 保证最多切一次，备用模型也拒绝时走正常的 `StopRefused` 分支返回 `ErrRefused`。注意两家模型的消息格式不同，跨供应商切换时适配层要把历史转换过去（1.2 课），推理块之类不能跨供应商的内容要去掉；跨模型切换还会让 Prompt Cache 失效（1.5 课），第一次调用的费用会明显偏高。Anthropic 文档有一节专门讲换备用模型时怎样避免缓存费用付两次。

---

## 完成标准

- `make test` 全部通过（14 个测试）；
- 练习 1 的三个问题能讲清楚；
- 练习 2 的 `Reserve` 写完并接入 `Run`，测试覆盖拒绝和放行两种情况；
- 练习 3 的备用模型写完，拒绝、过载切换和“最多切一次”三个用例都有测试；
- 自测题 10 道能不看答案讲出来，尤其是第 3、6、8、9 题。
