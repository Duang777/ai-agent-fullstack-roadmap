# 2.1 Function Calling / Tool Use：机制、格式与并行调用

> 第 2 部分 · 第 1 课 ｜ 预计 5–6 小时 ｜ 前置：1.2 主流模型 API、1.3 结构化输出（JSON Schema、strict）、1.5 Prompt Caching、1.6 上下文工程
>
> 配套练习：`projects/12-tools/`（纯 Go 标准库，13 个测试）。写一个工具定义，检查它、渲染成两家的格式、校验模型给的参数；把两家的调用和结果互相转换，检查 Anthropic 消息历史有没有违反格式规则；把交错到达的流式参数片段拼回完整调用；实现一个执行器：只读工具并发、相同调用去重、有副作用的工具串行且失败即停、所有错误都变成结果回给模型；最后用一个最小循环把它们串起来。

## 读前说明

模块 02 讲“模型怎么动手”。这一课是地基：**工具调用到底是怎么一回事**。后面的 MCP（2.2）、A2A（2.3）、Computer Use（2.4）、代码沙箱（2.5），本质上都是在这个机制上加了一层协议或一种特殊的工具。

先把最重要的一句话说清楚：**模型自己不执行任何工具**。它只会输出一段结构化的文本，意思是“请帮我用这些参数调用这个函数”。真正去查数据库、发请求、写文件的，是你的代码。执行完以后，你把结果作为新的输入发回去，模型再决定下一步。所谓 Agent，就是把这个过程放进一个循环里。

先看几个你会遇到的问题：

- OpenAI 返回的 `arguments` 是什么类型？Anthropic 的 `input` 呢？直接 `json.Unmarshal` 到同一个结构体会出什么问题？
- 模型一次返回了 3 个工具调用，你把 3 个结果分成 3 条 user 消息发回去，API 没报错，为什么之后模型不再并行调用了？
- 工具执行抛了异常，是让整个请求失败，还是把错误告诉模型？
- 模型同时要求“写文件”和“提交代码”，写文件失败了，提交还要执行吗？
- 流式输出时，两个并行调用的参数片段交错到达，怎么拼回去？
- 工具定义写得越详细越好吗？给模型 80 个工具会怎样？

本课结束时你能：

1. 讲清工具调用的完整流程：定义、模型发起调用、你执行、结果回传、模型继续；
2. 写出两家的工具定义和结果格式，说清 `strict` 的要求和 `tool_choice` 的几种模式；
3. 正确处理并行调用：结果怎么回传、哪些能并发执行、失败了怎么办；
4. 在流式输出中按 index 拼接参数片段；
5. 设计好用的工具：名字、描述、参数、返回值、错误信息，以及控制工具的数量。

**关于时效**：本课按 2026 年 10 月 OpenAI Function calling 指南和 Anthropic Tool use 文档核对。示例模型名用文档里的 `gpt-6-astra` 和 `claude-opus-5-5`。工具调用相关的行为（哪些模型支持强制调用、哪些支持工具搜索）在不同模型之间差别很大，写代码前请以文档为准。Gemini 的函数调用思路相同（模型返回函数调用，你返回函数结果），格式差异见 1.2 课，本课不展开。

---

# 第一部分：机制

## 1. 一次工具调用的五步

OpenAI 的文档把流程分成五步，两家都一样：

1. 你发请求，带上模型可以调用的工具列表；
2. 模型返回一个或多个工具调用（名字 + 参数）；
3. 你的代码执行这些调用；
4. 你再发一次请求，带上之前的全部内容和工具结果；
5. 模型给出最终回答，或者继续发起新的调用。

这里有三个容易忽略的点：

- **工具定义会变成提示词**。OpenAI 文档说，函数定义被注入到系统消息里，占上下文、按输入 token 计费；Anthropic 文档直接给出了模板：API 用你的工具定义、工具配置和系统提示拼出一段特殊的系统提示。所以 1.6 课讲的“工具描述也是提示词”是字面意义上的；
- **每次请求都要把工具列表带上**。API 是无状态的（`previous_response_id` 只是帮你存了历史），下一轮不带工具，模型就不知道有这些工具。而且工具列表在缓存前缀的最前面，改了它整个缓存都失效（1.5 课）；
- **模型可能一次调用零个、一个或多个工具**。OpenAI 文档的原话是：最好假设有多个。代码里永远用循环处理调用列表，不要只取第一个。

## 2. 两家的格式

同一件事（查北京天气），两家的格式差别不小：

| | OpenAI Responses | Anthropic Messages |
|---|---|---|
| 工具定义 | `{type: "function", name, description, parameters, strict}` | `{name, description, input_schema}`，可选 `strict`、`input_examples` |
| 模型发起调用 | `output` 里的 `function_call` 项 | `content` 里的 `tool_use` 块，`stop_reason` 是 `tool_use` |
| 调用 ID | `call_id`（另外还有一个 `id`，别用错） | `id` |
| 参数 | `arguments`：**JSON 字符串** | `input`：**JSON 对象** |
| 结果 | 追加一项 `{type: "function_call_output", call_id, output}` | 下一条 user 消息里的 `{type: "tool_result", tool_use_id, content, is_error}` |
| 错误 | 没有专门字段，写进 `output` 字符串 | `is_error: true` |
| 推理模型 | `reasoning` 项必须和工具结果一起原样传回 | 思考块必须和对应的 `tool_result` 一起原样传回（1.4 课） |

开头的第一个问题就在这张表里：OpenAI 的 `arguments` 是一个字符串，内容是 JSON；Anthropic 的 `input` 本身就是 JSON 对象。练习的 `ParseOpenAI` 和 `ParseAnthropic` 把两者都归一化成 `json.RawMessage`：

```go
// OpenAI：arguments 是字符串，内容才是 JSON
calls = append(calls, Call{ID: it.CallID, Name: it.Name, Args: json.RawMessage(it.Arguments)})

// Anthropic：input 本身就是 JSON
calls = append(calls, Call{ID: b.ID, Name: b.Name, Args: b.Input})
```

如果你用一个 `Args map[string]any` 字段去同时解析两家，OpenAI 那边会因为类型不匹配而失败（字符串不能解析成对象）。

还有一个细节：OpenAI 的 `function_call` 项有两个 ID，`id`（形如 `fc_...`，是这个输出项的 ID）和 `call_id`（形如 `call_...`，是用来对应结果的）。回传结果时用的是 `call_id`。

## 3. strict：让参数一定符合 schema

1.3 课讲过结构化输出。工具参数是同一个机制：

- **OpenAI**：`strict: true` 用结构化输出保证参数符合 schema。要求每个对象都设 `additionalProperties: false`，所有字段都列进 `required`；可选字段用 `["string", "null"]` 这样的类型表示。Responses API 不写 `strict` 时会尝试自动规范化成 strict，做不到就退回非 strict（这时返回的工具定义里显示 `strict: false`）；Chat Completions 默认不是 strict。文档建议总是开启；
- **Anthropic**：工具定义上加 `strict: true`，保证输入完全符合 schema，不会缺参数、不会类型错误。

练习的 `Tool.Lint(true)` 会检查 strict 的要求，`Tool.OpenAI()` 渲染出的定义里可选参数 `unit` 是 `["string","null"]`。

**即使开了 strict，也要自己校验**。原因有三个：不是所有模型、所有路径都支持 strict；strict 只保证“形状”对，不保证“值”合理（城市名拼错、日期在过去）；你的代码不应该信任任何外部输入，模型的输出也是外部输入。练习的 `Validate` 检查了必填、类型、枚举、未知参数、null，`TestValidate` 有 7 个用例。

## 4. tool_choice：控制模型要不要调、调哪个

| 意图 | OpenAI | Anthropic |
|---|---|---|
| 模型自己决定（默认） | `"auto"` | `{type: "auto"}` |
| 必须调用至少一个 | `"required"` | `{type: "any"}` |
| 必须调用指定的一个 | `{type: "function", name}` | `{type: "tool", name}` |
| 不许调用 | `"none"` | `{type: "none"}` |
| 只允许其中几个 | `{type: "allowed_tools", mode, tools}` | 无直接对应 |

几个要知道的限制和技巧：

- **Anthropic 的强制调用有限制**：Opus 5.5、Sonnet 5.5、Fable 5.1、Mythos 5.1 不支持 `any` 和 `tool`，会返回 400；文档建议改用 `auto` 加 strict，或者用结构化输出。开了手动 extended thinking 的请求也不支持强制调用。在支持的模型上，强制调用会**跳过思考**，响应直接以工具调用开头，而且模型不会在调用前写任何说明文字；
- **改 `tool_choice` 会打掉缓存**：Anthropic 文档说，改 `tool_choice` 会让缓存的消息块失效（工具定义和系统提示仍然缓存）；
- **OpenAI 的 `allowed_tools` 就是为缓存设计的**：工具列表保持不变（缓存前缀不变），只通过 `allowed_tools` 限制这一轮能用哪几个。这是 1.5 课“稳定前缀”的直接应用。

## 5. 自定义工具：不是所有输入都适合 JSON

OpenAI 还有一种 `custom` 工具：模型给你的不是 JSON 参数，而是一段任意文本，比如一段代码、一个 SQL、一个数学表达式。还可以用 Lark 或正则写一个语法（`format: {type: "grammar", syntax: "lark" | "regex", definition}`），约束模型生成的文本必须符合这个语法。

什么时候用：工具的输入天然就是一段文本（代码、补丁、查询语句），硬塞进 JSON 字符串里要处理大量转义，模型也更容易写错。文档提醒：语法要尽量简单，太复杂会被 API 拒绝，也更容易让模型“跑偏”。

---

# 第二部分：并行调用

## 6. 并行调用是什么

模型可以在一轮里发出多个调用。比如“北京和上海天气怎么样”，模型会一次返回两个 `get_weather` 调用，而不是查完一个再查另一个。这能省掉一整轮模型请求的延迟和费用。

两家默认都开启。关闭的方法：

- **OpenAI**：`parallel_tool_calls: false`，保证每轮调用零个或一个。另外，GPT-5 及以后支持的模型里，内置工具（网页搜索、代码解释器等）不能和函数调用放在同一个并行批次里；
- **Anthropic**：在 `tool_choice` 里设 `disable_parallel_tool_use: true`（不是顶层参数）。配合 `auto` 是“最多一个”，配合 `any` 或 `tool` 是“恰好一个”。

## 7. 结果怎么回传：一条消息，结果在前

Anthropic 的文档把规则写得很明确：

1. 每个 `tool_use` 都要有一个 `tool_result`，用 `tool_use_id` 对应；
2. 所有结果放在**紧接着的同一条** user 消息里，中间不能插别的消息；
3. 这条消息里 `tool_result` 必须在**最前面**，文字只能放在所有结果后面；
4. 某个调用你决定不执行，也要返回一个 `tool_result`，`is_error: true` 并说明原因。

违反第 3 条会直接 400。违反第 2 条（把结果拆成多条 user 消息）可能不报错，但文档说这会“教会”模型避免并行调用，这是排查“为什么模型不并行了”时最常见的原因。这就是开头第二个问题的答案。

练习的 `CheckHistory` 检查了五种情况：正确、文字在前、拆成两条、漏了一个、结果对不上调用。`AnthropicResults` 总是生成“一条消息、结果在前”的格式。

OpenAI 的格式简单一些：每个结果是一个独立的 `function_call_output` 项，按 `call_id` 对应，追加到 input 后面即可。练习的 `OpenAIOutputs` 生成这些项。

## 8. 执行顺序是你的事

Anthropic 文档说得很直白：API 不规定执行顺序，你可以并发、可以按顺序、可以混合，看工具做什么。原则是：

- **独立的只读操作**（查天气、读文件、搜索）：并发执行，降低延迟；
- **有副作用、共享状态、有顺序要求的操作**（写文件、提交、付款）：按顺序执行。

练习的 `Executor.Run` 就是这么做的：

```go
for i, c := range calls {
    // 未知工具、参数不合法：直接变成错误结果
    ...
    if t.ReadOnly {
        readOnly = append(readOnly, job{i, args})
    } else {
        sideEffect = append(sideEffect, job{i, args})
    }
}
// 只读：去重 + 并发（受 MaxParallel 限制）
// 有副作用：串行，失败即停
```

`TestParallelReadOnly` 的结果：4 个各需要 50ms 的只读调用，并发执行耗时约 51ms，串行需要约 200ms；把 `MaxParallel` 设成 1 就退化成串行。

几个设计细节：

- **结果顺序和调用顺序一致**。并发执行时完成的顺序是随机的，但写回结果时按原来的下标放，这样日志和调试都更清楚；
- **并发要有上限**。模型一次发 30 个搜索调用，你不想同时打出 30 个请求把下游打挂。练习用一个带缓冲的 channel 当信号量；
- **相同的只读调用只执行一次**。模型偶尔会在一批里发出重复的调用（OpenAI 文档提到，`gpt-4.1-nano-2025-04-14` 这个快照开启并行时有时会对同一个工具发出多个调用，建议关掉并行）。练习的 `canon` 把参数 JSON 的键排序，`{"city":"北京","unit":null}` 和 `{"unit":null,"city":"北京"}` 被视为同一个调用，只执行一次，但两个 `call_id` 都要有结果（`TestDedupe`）。注意只对只读工具去重，两次“付款”绝不能合并成一次。

## 9. 失败即停：有副作用的工具

开头的第四个问题：“写文件”失败了，“提交”还要执行吗？当然不要。提交一个没写成功的文件，比不提交更糟。

练习的规则：有副作用的调用按出现顺序串行执行，一旦有一个失败，后面所有有副作用的调用都不执行，返回：

```text
未执行：前面的 write_file 调用失败了。
```

`TestSideEffectsStopOnFailure` 验证了 `create_branch → write_file（失败）→ commit` 中 commit 没有被执行。这和 Anthropic 文档的建议一致；文档还说，Computer Use 和 Browser Use 工具的批量动作**必须**按顺序执行、遇到第一个失败就停。

还有一点：练习里的执行器把只读调用放在有副作用的调用之前执行。如果模型在一批里先发了“写文件”再发“读同一个文件”，这个顺序就错了。所以文档建议在系统提示里加一句“只把相互独立的调用放在同一批里”；练习 2 会让你改成保持相对顺序的分段执行。

## 10. 错误要变成结果，而不是异常

工具执行可能出现各种问题：模型拼错了工具名、参数不合法、下游超时、工具代码 panic。最差的处理方式是让整个 Agent 崩掉；最好的方式是**把错误作为结果回给模型**，让它自己调整。

`TestErrorsBecomeResults` 覆盖了四种情况，每一种都变成了 `IsError` 结果：

```text
1 → 未知工具 "get_wether"。可用工具：boom, get_weather
2 → 缺少必填参数 "unit"。请按工具定义修正参数后重试。
3 → 超时：get_weather 在 30ms 内没有返回。可以缩小范围后重试。
4 → 工具内部错误：nil map
```

Anthropic 文档专门强调：**错误信息要有指导性**。不要只写 `failed`，要写清出了什么问题、下一步该怎么做（比如“触发限流，60 秒后重试”）。模型拿到这样的信息，能自己恢复。文档还提到，参数不合法时，Claude 通常会带着修正重试 2 到 3 次，然后才向用户道歉。

实现上的几个点：

- 超时用 `context.WithTimeout`，工具函数要尊重 `ctx.Done()`。如果工具不检查 ctx，超时后执行器会返回，但那个 goroutine 还在跑，这是真实系统里要注意的资源泄漏；
- panic 用 `recover` 兜住，变成错误结果；
- 错误信息里不要泄露内部细节（堆栈、密钥、内部地址），模型会把它原样转述给用户。

---

# 第三部分：流式输出中的工具调用

## 11. 参数是一段一段到达的

开启流式后，工具调用的参数和普通文字一样，是一小段一小段到达的：

- **OpenAI**：先来 `response.output_item.added`（带 `output_index`、`call_id`、`name`，`arguments` 为空），然后是一串 `response.function_call_arguments.delta`，最后 `response.function_call_arguments.done` 和 `response.output_item.done`；
- **Anthropic**：`content_block_start`（`tool_use` 块，带 `id`、`name`），然后一串 `content_block_delta`，其中 `delta.type` 是 `input_json_delta`、片段在 `partial_json` 里，最后 `content_block_stop`。

并行调用时，不同调用的片段**可能交错到达**。唯一可靠的区分方式是 index：OpenAI 的 `output_index`，Anthropic 的 content block `index`。

练习把两家的事件归一化成 `start / delta / done` 三种，`Accumulator` 按 index 分别拼接。`TestStreamInterleaved` 模拟了两个调用的片段交错到达：

```text
start #0 get_weather
delta #0 {"city":"北
start #1 get_weather
delta #1 {"city":"上海",
delta #0 京","unit":null}
done  #0
delta #1 "unit":"celsius"}
done  #1
```

拼完后两个调用的参数分别是完整的 JSON。

三个要点：

1. **中途的片段不是合法 JSON**，可以拿来在界面上实时显示“正在调用 get_weather……”，但不能执行；
2. **只在 done 之后执行**，并且要再检查一次拼出来的是不是合法 JSON：如果流在中途断了，或者生成碰到了 `max_tokens`，你会拿到半截参数（`TestStreamErrors`）；
3. **无参数的工具可能一个片段都没有**，这时参数按 `{}` 处理。

0.4 课讲过 SSE 的解析，这里只是在那一层之上，多了一个“按 index 分组拼接”。

---

# 第四部分：设计好用的工具

## 12. 名字、描述、参数

两家的文档在这一点上高度一致。整理成一张清单：

**名字**
- Anthropic 要求匹配 `^[a-zA-Z0-9_-]{1,128}$`，空格和中文都不行；
- 工具多了以后加服务前缀：`github_list_prs`、`slack_send_message`，让选择没有歧义（OpenAI 还提供了 `namespace`，把相关工具分组）。

**描述**（Anthropic 说这是“对工具表现影响最大的因素”）
- 写清楚：做什么、什么时候用、**什么时候不用**、每个参数是什么意思、返回什么、有什么限制；
- Anthropic 建议每个工具至少 3 到 4 句话，复杂的更多；
- OpenAI 提出一个“实习生测试”：只给一个实习生你给模型的这些信息，他能正确使用这个函数吗？如果他会来问你问题，把答案写进描述里。

**参数**
- 用枚举和结构防止无效状态。OpenAI 的例子：`toggle_light(on: bool, off: bool)` 允许 on 和 off 同时为 true，应该改成一个枚举；
- **不要让模型填你已经知道的参数**。比如你的代码已经有了 `order_id`，就定义一个没有参数的 `submit_refund()`，`order_id` 由代码传入。模型填的参数越少，出错的机会越少，也越难被注入攻击利用；
- 复杂或格式敏感的参数，Anthropic 可以用 `input_examples` 给示例（必须符合 schema，否则 400；每个示例大约多 20 到 200 token）。

练习的 `Lint` 实现了其中几条：名字格式、描述长度、参数说明、strict 必填。一个写得很差的定义（名字有空格、描述只有“查天气”、参数没说明）会得到 4 条警告。

## 13. 返回值：只给有用的

工具的返回值会进入上下文，1.6 课讲过这是上下文增长最快的部分。Anthropic 的建议：

- 只返回模型下一步推理需要的字段；
- 用有意义、稳定的标识符（slug、UUID），而不是不透明的内部编号；
- 大结果分页或截断，告诉模型怎么拿更多（1.6 课的“保留头尾、按需读取”）。

还有一条安全上的提醒：**工具结果里的内容来自你控制不了的地方**（网页、邮件、用户上传、第三方 API），可能包含试图操纵模型的指令（间接提示词注入）。Anthropic 建议把这类不可信内容放在 `tool_result` 里，而不是系统提示或普通的用户文字里。模块 09 会细讲防御。

## 14. 工具的数量

工具越多，模型越难选对，工具定义占的上下文也越多：

- OpenAI 的软建议是：一轮开始时可用的函数**少于 20 个**，并且用评测测出你自己场景下的最佳数量；
- **合并经常连着调用的工具**：OpenAI 的例子是如果总是先 `query_location()` 再 `mark_location()`，就把后者合进前者；Anthropic 的建议是把 `create_pr`、`review_pr`、`merge_pr` 合成一个带 `action` 参数的工具；
- **工具很多时用工具搜索**：把不常用的工具设成 `defer_loading`，模型需要时先搜索、再加载定义。OpenAI 的 `tool_search` 只支持 `gpt-5.4` 及以后的模型。

一个实用的判断：如果两个工具的描述，你自己读了都分不清什么时候该用哪个，模型也分不清。

---

# 第五部分：把它们串起来

## 15. 最小的工具调用循环

练习的 `Loop` 是一个最小的 Agent：

```go
for steps = 1; steps <= maxSteps; steps++ {
    t := m(history)                 // 调模型
    history = append(history, assistantMessage(t))
    if len(t.Calls) == 0 {
        return t.Text, history, steps // 没有调用：这就是最终回答
    }
    history = append(history, AnthropicResults(ex.Run(ctx, t.Calls), "")) // 执行并回传
}
return "", history, maxSteps          // 兜底：步数用完
```

`TestLoop` 用一个假模型验证了两件事：

1. “北京和上海天气”需要 2 步（一步并行调两个工具，一步给出回答），生成的历史通过 `CheckHistory`；
2. 一个永远在调工具、从不回答的模型，会在 `maxSteps` 处停下。

`maxSteps` 是最粗的兜底。什么时候该停、预算怎么控制、失败了怎么重试，是 3.2 课 Agent Loop 设计的内容。下一课（2.2）讲 MCP：当工具不是写在你自己代码里，而是由一个独立的服务提供时，工具的发现、定义和调用怎么标准化。

---

## 本课小结

1. 模型不执行工具，只输出“请调用这个函数”的结构化文本；执行和回传是你的代码。工具定义会变成提示词，占上下文、算 token、在缓存前缀最前面。
2. OpenAI 的参数是 JSON 字符串、用 `call_id` 对应结果；Anthropic 的参数是 JSON 对象、用 `tool_use_id` 对应、有 `is_error`。推理模型的推理内容要和工具结果一起原样传回。
3. strict 保证参数形状正确，但值仍要自己校验；`tool_choice` 控制调不调、调哪个，要注意模型限制和对缓存的影响。
4. 并行调用的结果：Anthropic 放在同一条 user 消息里、结果在前；每个调用都要有结果。只读工具并发（有上限、可去重），有副作用的工具串行、失败即停。
5. 所有错误都变成有指导性的结果回给模型，而不是让循环崩掉。
6. 流式参数按 index 拼接，done 之后再执行，并再校验一次 JSON。
7. 好的工具：名字清楚、描述详细、参数少而严格、返回值精简、数量可控。

---

## 练习（今天做完）

### 练习 1：跑通并读懂

```bash
cd projects/12-tools
make test
```

回答：

1. 在 `TestParallelReadOnly` 里把 `MaxParallel` 设成 2，4 个调用大约要多久？为什么？
2. 如果去掉 `Run` 里“结果按原下标写回”的逻辑，改成按完成顺序 append，`TestLoop` 还能通过吗？对 OpenAI 和 Anthropic 分别有什么影响？
3. `TestDedupe` 里如果工具不是只读的（比如 `send_email`），为什么绝对不能去重？

### 练习 2：保持相对顺序的分段执行

现在的执行器把所有只读调用放在有副作用的调用之前，可能打乱模型的意图。写一个函数，把一批调用分成若干“阶段”：连续的只读调用放在同一个阶段里并发执行，每个有副作用的调用单独一个阶段，阶段之间按顺序执行。

```go
func Plan(calls []Call, readOnly func(name string) bool) [][]int
```

测试：`[read, read, write, read, read, write]` 应该分成 `[[0 1] [2] [3 4] [5]]`。

### 练习 3：在两家之间切换

有时你需要把一段 OpenAI 格式的历史换给 Anthropic 继续（比如降级、A/B 测试）。写一个转换函数：

```go
type OAItem struct {
    Type      string // message / function_call / function_call_output
    Role      string
    Content   string
    CallID    string
    Name      string
    Arguments string
    Output    string
}

func ToAnthropic(items []OAItem) ([]Message, error)
```

要求：连续的 `function_call` 合并成一条 assistant 消息里的多个 `tool_use` 块；紧接着的 `function_call_output` 合并成一条 user 消息里的多个 `tool_result` 块；`arguments` 不是合法 JSON 时返回错误。转换结果必须通过 `CheckHistory`。

---

## 自测题

1. “模型调用了工具”这句话准确吗？工具实际上是谁执行的？
2. OpenAI 的 `arguments` 和 Anthropic 的 `input` 有什么区别？OpenAI 的 `function_call` 项里 `id` 和 `call_id` 分别是什么？
3. 开了 strict 以后，为什么还要自己校验参数？
4. Anthropic 的哪些模型不支持 `tool_choice` 的 `any` 和 `tool`？这时怎么保证调用格式正确？
5. 为什么 OpenAI 推荐用 `allowed_tools` 限制工具，而不是每轮换一个工具列表？
6. 并行调用的结果在 Anthropic 里该怎么回传？拆成多条 user 消息会有什么后果？
7. 哪些工具调用可以并发执行？哪些不能？为什么？
8. 工具执行出错了，应该怎么处理？一个好的错误信息长什么样？
9. 流式输出中，两个并行调用的参数片段交错到达，怎么区分？什么时候可以执行？
10. 给模型 80 个工具有什么问题？有哪些办法？

---

# 参考答案与详解

## 自测题答案

**1.** 不准确。模型只输出一段结构化文本，表示“请用这些参数调用这个函数”。执行的是你的代码，执行完后你把结果作为新的输入发回给模型。

**2.** `arguments` 是一个字符串，内容是 JSON，需要再解析一次；`input` 本身就是 JSON 对象。`id`（`fc_...`）是输出项的 ID，`call_id`（`call_...`）用来把结果对应到调用，回传 `function_call_output` 时用 `call_id`。

**3.** 不是所有模型和路径都支持 strict；strict 只保证形状，不保证值合理（拼错的城市、过去的日期、越权的 ID）；模型的输出对你的代码来说是外部输入，任何外部输入都要校验。

**4.** Opus 5.5、Sonnet 5.5、Fable 5.1、Mythos 5.1，会返回 400。改用 `auto` 加 `strict: true` 保证调用时参数符合 schema，或者用结构化输出得到固定格式的回答；通过提示词引导模型选哪个工具。

**5.** 工具列表在缓存前缀的最前面，每轮换列表会让整个缓存失效。`allowed_tools` 让工具列表保持不变，只在这一轮限制可用的子集，缓存不受影响。

**6.** 所有 `tool_result` 放在紧接着的同一条 user 消息里，按 `tool_use_id` 对应，结果放在最前面，文字放在后面；每个调用都要有结果，没执行的也要返回 `is_error`。拆成多条消息可能不报错，但会让模型学会不再并行调用。

**7.** 相互独立的只读调用可以并发（查询、读取、搜索），要设并发上限，可以对完全相同的调用去重。有副作用、共享状态、有先后依赖的调用要按顺序执行，前一个失败了后面的不执行。原因：并发执行有副作用的操作，结果依赖于不确定的执行顺序，失败后也很难回滚。

**8.** 把错误作为结果回给模型（Anthropic 用 `is_error: true`，OpenAI 写进 `output`），不要让循环崩掉。好的错误信息说清出了什么问题、下一步该怎么做，比如“缺少必填参数 unit，请按定义补上后重试”“限流，60 秒后重试”；不泄露堆栈和内部信息。

**9.** 用 index 区分：OpenAI 的 `output_index`，Anthropic 的 content block `index`。按 index 分别拼接，只有收到这个调用的 done 事件、并确认拼出来的是合法 JSON 之后才执行。

**10.** 工具定义占大量上下文，而且工具越多模型越难选对。办法：合并经常一起用的工具；用 `action` 参数把一组相关操作合成一个工具；按场景只暴露需要的工具；用工具搜索延迟加载不常用的工具；用评测找到你场景下合适的数量（OpenAI 的软建议是一轮开始时少于 20 个）。

## 练习参考答案

### 练习 1

1. 大约 100ms。并发上限是 2，4 个调用分两批，每批 50ms。
2. `TestLoop` 仍然能通过，因为 Anthropic 按 `tool_use_id` 对应结果，`CheckHistory` 也是按 ID 检查的；OpenAI 同样按 `call_id` 对应。所以两家在协议上都不要求顺序。但按调用顺序写回能让日志、回放和测试都确定下来，否则同样的输入每次产生的历史不同，还会让 1.5 课的缓存前缀每次都不一样。
3. 有副作用的调用每执行一次就产生一次效果。模型发出两个相同的 `send_email`，可能是它真的想发两封，也可能是一个错误，但无论哪种，执行器都不应该替它做决定，更不能悄悄把两次合成一次而返回两个“成功”。正确的做法是在工具层面做幂等（模块 07 会讲幂等键），或者在系统提示里约束模型。

### 练习 2

```go
func Plan(calls []Call, readOnly func(name string) bool) [][]int {
    var stages [][]int
    for i, c := range calls {
        if readOnly(c.Name) {
            // 上一个阶段也是只读：并入
            if n := len(stages); n > 0 && readOnly(calls[stages[n-1][0]].Name) {
                stages[n-1] = append(stages[n-1], i)
                continue
            }
        }
        stages = append(stages, []int{i})
    }
    return stages
}
```

测试：

```go
ro := func(n string) bool { return n == "read" }
cs := []Call{{Name: "read"}, {Name: "read"}, {Name: "write"}, {Name: "read"}, {Name: "read"}, {Name: "write"}}
if fmt.Sprint(Plan(cs, ro)) != "[[0 1] [2] [3 4] [5]]" {
    t.Fatal(Plan(cs, ro))
}
```

接到执行器里：按阶段循环，只读阶段用原来的并发逻辑，有副作用的阶段串行执行；任何一个有副作用的阶段失败后，后面所有阶段都返回“未执行”。这样“写完再读”的意图被保留了下来，代价是并发度可能降低。

### 练习 3

```go
func ToAnthropic(items []OAItem) ([]Message, error) {
    var msgs []Message
    push := func(role string, b Block) {
        // 和上一条消息同一个角色：并进去，否则开一条新消息
        if n := len(msgs); n > 0 && msgs[n-1].Role == role {
            msgs[n-1].Content = append(msgs[n-1].Content, b)
            return
        }
        msgs = append(msgs, Message{Role: role, Content: []Block{b}})
    }
    for _, it := range items {
        switch it.Type {
        case "message":
            push(it.Role, Block{Type: "text", Text: it.Content})
        case "function_call":
            if !json.Valid([]byte(it.Arguments)) {
                return nil, fmt.Errorf("call %s 的 arguments 不是合法 JSON", it.CallID)
            }
            push("assistant", Block{Type: "tool_use", ID: it.CallID, Name: it.Name, Input: json.RawMessage(it.Arguments)})
        case "function_call_output":
            push("user", Block{Type: "tool_result", ToolUseID: it.CallID, Content: it.Output})
        }
    }
    return msgs, nil
}
```

用“用户提问 → 两个 function_call → 两个 function_call_output → 助手回答”测试，结果是 4 条消息，第 2 条有两个 `tool_use`，第 3 条有两个 `tool_result`，并且通过 `CheckHistory`。

讨论：这个转换在真实场景里还有几处会丢信息。OpenAI 的 `reasoning` 项是加密的，不能转给 Anthropic；Anthropic 的思考块带签名，也不能转给 OpenAI（1.4 课）。所以中途切换模型时，推理内容只能丢弃，模型要从可见的历史重新理解任务。另外，Responses API 的 message 内容实际上可以是数组（文字、图片），练习简化成了字符串。

---

## 完成标准

- `make test` 全部通过（13 个测试），能在日志里看到并行耗时和四种错误结果；
- 练习 1 的三个问题能讲清楚；
- 练习 2 的 `Plan` 写完并通过测试，能说清它比现在的执行器好在哪里、代价是什么；
- 练习 3 的 `ToAnthropic` 写完，转换结果通过 `CheckHistory`；
- 自测题 10 道能不看答案讲出来，尤其是第 2、6、7、8 题。
