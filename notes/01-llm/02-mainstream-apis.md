# 1.2 主流模型 API：OpenAI Responses、Anthropic Messages、Gemini Interactions

> 第 1 部分 · 第 2 课 ｜ 预计 5–6 小时 ｜ 前置：0.4 流式协议、1.1 Transformer 与采样
>
> 配套练习：`projects/07-llm-api/`（纯 Go 标准库，20 个测试）。手写一个能同时对接三家的统一客户端：请求编码、响应解码、三套流式协议的状态机、错误分类、一个对三家完全相同的工具调用循环，以及“换模型时历史怎么迁移”。

## 读前说明

上一课讲的是模型内部，这一课讲你每天真正打交道的东西：HTTP 请求长什么样、回来的 JSON 怎么读、流式事件怎么拼。下面这些问题，不把三家的协议摸透是答不上来的：

- 同样是“调用工具”，为什么 OpenAI 的参数是字符串，Anthropic 和 Gemini 是对象？
- 带推理的模型调完工具，下一轮为什么必须把一段看不懂的加密字符串原样发回去？不发会怎样？
- Anthropic 账单上的 `input_tokens` 为什么比你算的少一大截？
- 流式输出时工具参数是一段段来的，什么时候才能 `JSON.parse`？
- 网关返回 529、429、400，哪些该重试？流式已经推了一半给用户，还能重试吗？
- 想从 Claude 换到 Gemini，已有的对话历史能直接发过去吗？

这一课分五部分：**全景 → OpenAI Responses → Anthropic Messages → Gemini Interactions → 统一客户端**。前四部分讲协议，第五部分把它们收进一个 Go 接口，练习里每个结论都有测试。

本课结束时你能：

1. 不看文档写出三家“带工具的一轮完整往返”的请求和响应 JSON，说清每个字段的作用；
2. 读懂三家的流式事件序列，知道哪个事件是终止信号、工具参数在哪一步才能解析；
3. 正确处理推理块：OpenAI 的 `encrypted_content`、Anthropic 的 `signature`、Gemini 的 thought signature；
4. 把三家的停止原因、用量、错误统一成一套口径，并知道每一处换算的依据；
5. 设计一个“同家无损、跨家降级”的统一客户端，写出对三家完全相同的 Agent 工具循环；
6. 判断什么错误能重试、流式输出在什么时机还能重试。

**关于时效**：API 变得很快。本课内容核对的是 2026 年 10 月三家的官方文档，示例里的模型名（`gpt-6-astra`、`claude-opus-5-5`、`gemini-3.8-flash`）也取自当时的文档。字段名以官方 API 参考为准；遇到文档自相矛盾的地方，本课会明确指出并在代码里做容错。

---

# 第一部分：全景

## 1. 三家一张表

先把最重要的差异放在一张表里，后面每一节都是在展开这张表。

| | OpenAI Responses | Anthropic Messages | Gemini Interactions |
|---|---|---|---|
| 端点 | `POST /v1/responses` | `POST /v1/messages` | `POST /v1beta/interactions` |
| 鉴权头 | `Authorization: Bearer KEY` | `x-api-key: KEY` + `anthropic-version: 2023-06-01` | `x-goog-api-key: KEY` |
| 系统提示 | 顶层 `instructions` | 顶层 `system` | 顶层 `system_instruction` |
| 对话结构 | 扁平的 `input` 数组，元素叫 item | `messages` 数组，user / assistant 交替，内容是 block 数组 | `input` 是 step 数组（时间线） |
| 工具定义 | `{"type":"function","name","parameters"}` | `{"name","input_schema"}` | `{"type":"function","name","parameters"}` |
| 工具调用 | 独立 item `function_call`，`arguments` 是 **JSON 字符串** | assistant 消息里的 `tool_use` block，`input` 是**对象** | 独立 step `function_call`，`arguments` 是**对象** |
| 工具结果 | 独立 item `function_call_output`，按 `call_id` 配对 | 下一条 user 消息里的 `tool_result` block，按 `tool_use_id` 配对 | 独立 step `function_result`，按 `call_id` 配对，还要带 `name` |
| 输出上限 | `max_output_tokens`（可选） | `max_tokens`（**必填**） | `generation_config.max_output_tokens`（可选） |
| “要你执行工具” | 输出里有 `function_call` | `stop_reason: "tool_use"` | `status: "requires_action"` |
| 被截断 | `status: "incomplete"` + `reason: "max_output_tokens"` | `stop_reason: "max_tokens"` | `status: "incomplete"` |
| 流式开关 | `"stream": true` | `"stream": true` | `"stream": true` |
| 流式终止事件 | `response.completed` / `.incomplete` / `.failed` | `message_stop` | `interaction.completed` / `requires_action` |
| 推理块回传 | reasoning item（含 `encrypted_content`） | thinking block（含 `signature`） | thought step（含 `signature`） |
| 服务端状态 | `store` + `previous_response_id` | 无（全量发送） | `store`（默认 true）+ `previous_interaction_id` |

一句话记忆：**OpenAI 和 Gemini 是“一条时间线上的条目”，Anthropic 是“两个人轮流说话，每句话由多个块组成”。** 工具结果在 Anthropic 里是“用户说的话”，在另外两家是独立条目。

## 2. 2026 年的大方向：从“补全接口”到“Agent 接口”

三家这两年都在做同一件事：把“给一段对话、补全下一句”的老接口，换成为 Agent 设计的新接口。

| 厂商 | 老接口 | 新接口 | 现状 |
|---|---|---|---|
| OpenAI | Chat Completions（`/v1/chat/completions`） | Responses（`/v1/responses`） | Chat Completions 仍可用，但 GPT-6 Astra 和 GPT-6.1 Sol 的工具调用**只支持 Responses**；官方说推理模型在 Responses 上效果更好 |
| Google | `generateContent` | Interactions | 2026 年 6 月 GA，官方推荐所有新项目使用，**新模型和新功能只上 Interactions**；`generateContent` 被称为 legacy 但继续支持 |
| Anthropic | — | Messages | 接口形状没变，持续加能力：服务端工具、`pause_turn`、拒答后的模型回退 |

新接口共同的变化：

1. **输出是“条目 / 步骤”的序列，不再是一段文本**。推理、工具调用、内置搜索、最终回答都是平级的条目，前端可以逐个渲染；
2. **服务端可以替你保存状态**。OpenAI 用 `previous_response_id`，Gemini 用 `previous_interaction_id`，下一轮只发新增内容；
3. **流式事件是“语义事件”**，带类型，不再是一堆 `delta` 让你猜。

**本课的选择：无状态模式**（OpenAI `store: false`，Gemini `store: false`，Anthropic 本来就无状态），每次把完整历史发过去。原因：

- **可移植**：历史在你手里，随时能换模型、换厂商（第 22 节）；
- **可审计、可回放**：每一次请求的完整输入都在你的日志里，第 06 模块做 Eval、第 07 模块做 Tracing 都依赖这一点；
- **数据合规**：Gemini 的 interaction 默认存储，付费层保留 55 天、免费层 1 天；不想让对话留在厂商那里，就得显式关掉；
- 代价是每轮都发全量历史，输入 token 变多。这笔钱靠 **Prompt Caching** 省回来（1.5 课），前提是你每轮发的前缀完全一致。

服务端状态什么时候值得用？对话很长、你不需要换模型、又想少传数据时。它和无状态模式不是二选一，第 03 模块讲长任务时会再权衡。

---

# 第二部分：OpenAI Responses

## 3. 请求：一切都是 item

**要解决的问题**：用户问天气，模型可以调用 `get_weather`。

```bash
curl https://api.openai.com/v1/responses \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-6-astra",
    "instructions": "你是一个简洁的助手。",
    "input": [
      {"role": "user", "content": "北京天气怎么样？"}
    ],
    "tools": [{
      "type": "function",
      "name": "get_weather",
      "description": "查询城市天气",
      "parameters": {
        "type": "object",
        "properties": {"city": {"type": "string"}},
        "required": ["city"],
        "additionalProperties": false
      },
      "strict": true
    }],
    "max_output_tokens": 2048,
    "store": false
  }'
```

**逐行解读**

- **`instructions`**：系统提示放在顶层，不是 `input` 里的一条 `system` 消息。用 `previous_response_id` 续聊时，`instructions` 不会被继承，每次都要重新传。
- **`input`**：可以是一个字符串，也可以是 item 数组。最常见的 item 是“消息”：`{"role": "user", "content": "..."}`，`content` 可以是字符串，也可以是 `[{"type":"input_text","text":"..."}]` 这样的内容数组（要传图片时用后者）。
- **`tools`**：Responses 的函数定义是**扁平的**：`type`、`name`、`parameters` 在同一层。Chat Completions 是 `{"type":"function","function":{...}}` 多套了一层，迁移时最容易写错。
- **`strict: true`**：让模型输出的参数严格符合 schema（1.3 课讲原理），要求 schema 里所有字段都列进 `required`、`additionalProperties: false`。
- **`store: false`**：不在 OpenAI 那边保存这次响应。注意这个字段**必须显式发 `false`**，在 Go 里不能给它加 `omitempty`，否则零值被省略，服务端就按默认值存储了。练习的 `TestOpenAIEncode` 专门检查这一点。

## 4. 响应：output 是一个 item 数组

```json
{
  "id": "resp_tool_1",
  "status": "completed",
  "output": [
    {"id": "rs_1", "type": "reasoning",
     "summary": [{"type": "summary_text", "text": "需要先查天气。"}],
     "encrypted_content": "gAAAA..."},
    {"id": "fc_1", "type": "function_call", "status": "completed",
     "call_id": "call_abc", "name": "get_weather",
     "arguments": "{\"city\":\"北京\"}"}
  ],
  "usage": {
    "input_tokens": 120, "input_tokens_details": {"cached_tokens": 64},
    "output_tokens": 90, "output_tokens_details": {"reasoning_tokens": 70},
    "total_tokens": 210
  }
}
```

**逐行解读**

- **`output` 里没有“回答”这个字段**。文本回答是一个 `type: "message"` 的 item，里面的 `content` 是 `output_text` 数组。SDK 提供的 `response.output_text` 只是把这些拼起来的便利属性，HTTP 响应里没有它。
- **`reasoning` item**：推理过程的容器。原始推理 token 不会给你，只有可选的 `summary`（要在请求里开 `reasoning.summary`）和一段 **`encrypted_content`**。无状态模式（`store: false`）下，推理 item 默认就带 `encrypted_content`；以前需要在 `include` 里写 `reasoning.encrypted_content`，现在这个写法仍然兼容，但不再必需。
- **`function_call` 的两个 id**：`id`（`fc_...`）是这个 item 自己的 id；**`call_id`**（`call_...`）是配对用的，回传结果时用它。拿错 id 是常见 bug。
- **`arguments` 是字符串**。要 `JSON.parse` 一次才能用；模型理论上可能生成不合法的 JSON（不开 strict 时），所以解析失败要当错误处理。练习里的 `argsObject` 就是干这个的，`TestInvalidToolArguments` 会喂一段截断的参数。
- **`status`**：`completed`、`incomplete`、`failed`，还有后台模式下的 `queued`、`in_progress`。**没有“tool_use”这种状态**，判断“要不要执行工具”看 `output` 里有没有 `function_call`。
- **`usage`**：`input_tokens` **已经包含**命中缓存的 `cached_tokens`；`output_tokens` **已经包含** `reasoning_tokens`。上面这个例子：90 个输出 token 里 70 个是看不见的推理。

**被截断时**：`status` 是 `incomplete`，`incomplete_details.reason` 是 `max_output_tokens`。推理模型有一个坑：上限可能在推理阶段就被耗光，一个可见字都没输出，但输入和推理 token 照样计费。官方建议刚开始至少给推理和输出留 25000 个 token。练习里 `TestOpenAITextAndIncomplete` 用的就是这种“300 个输出 token 全是推理”的响应。

## 5. 一轮完整的工具往返

拿到 `function_call` 后，执行工具，把结果作为 `function_call_output` 追加进 `input`，再请求一次：

```json
{
  "model": "gpt-6-astra",
  "store": false,
  "input": [
    {"role": "user", "content": "北京天气怎么样？"},
    {"id": "rs_1", "type": "reasoning", "summary": [...], "encrypted_content": "gAAAA..."},
    {"id": "fc_1", "type": "function_call", "call_id": "call_abc",
     "name": "get_weather", "arguments": "{\"city\":\"北京\"}"},
    {"type": "function_call_output", "call_id": "call_abc", "output": "晴，25°C"}
  ],
  "tools": [ ...同上... ]
}
```

**要点**

1. **上一轮的 output item 原样放回 `input`**，包括 reasoning item。官方原话是“强烈建议”把最后一次函数调用附带的推理 item 一起传回；模型连续调用多个函数时，从最后一条用户消息之后的所有 reasoning、function_call、function_call_output 都要原样传回。服务端会自动忽略无关的推理 item，所以最简单的做法就是**全部原样回传**。
2. **不传会怎样**：请求不会报错，但模型丢了推理上下文，要重新思考，效果变差、推理 token 变多。这种问题不报错、只“变笨变贵”，最难排查。
3. **`output` 字段是字符串**。工具返回结构化数据就自己序列化成 JSON 字符串。Responses 的 `function_call_output` **没有 `is_error` 字段**，工具出错只能写在文本里（练习里加了 `ERROR: ` 前缀）。
4. **每个 `function_call` 都必须有对应的 output**。模型一轮可能并行调用多个函数，少回一个就会报错。想强制每轮最多一个调用，设 `parallel_tool_calls: false`。
5. **助手消息的 `phase`**：GPT-5.4 之后的模型在长任务中会输出 `phase: "commentary"`（调用工具前的说明）和 `phase: "final_answer"`（最终回答）。手动管理历史时要**原样保留** `phase`，丢了它，模型可能把中间说明当成最终回答而提前结束。这又是一个“只能原样回传”的字段。

这几条合起来就是统一客户端的第一条设计原则：**发回同一家时，原样回放它给你的原始 JSON，不要“规范化”后再重建**。你重建时漏掉的任何字段（`encrypted_content`、`phase`、将来新增的字段）都会让模型悄悄变差。

## 6. 流式：语义事件

`"stream": true` 后，响应变成 SSE，每个事件的 `data` 里都有 `type`：

```
event: response.created
data: {"type":"response.created","response":{"id":"resp_s1","status":"in_progress",...}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,
       "item":{"type":"function_call","call_id":"call_s1","name":"get_weather","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"ci"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"ty\":\"上海\"}"}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","output_index":1,"arguments":"{\"city\":\"上海\"}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":1,"item":{...完整的 function_call...}}

event: response.completed
data: {"type":"response.completed","response":{...完整的 response 对象，和非流式一样...}}
```

| 事件 | 含义 | 统一客户端怎么用 |
|---|---|---|
| `response.created` / `in_progress` | 开始了 | 忽略 |
| `response.output_item.added` | 新 item 开始；如果是 `function_call`，此时已知 `name` 和 `call_id` | 发 `tool_start`，前端可以立刻显示“正在调用 get_weather…” |
| `response.output_text.delta` | 文本增量 | 发 `text` |
| `response.function_call_arguments.delta` | 参数 JSON 的一段 | 发 `tool_args`，**不要解析** |
| `response.function_call_arguments.done` / `output_item.done` | 这个 item 完整了 | 忽略（终止事件里有完整数据） |
| `response.completed` / `response.incomplete` | **终止**，携带完整 response | 直接复用非流式的解码函数 |
| `response.failed` / `error` | 失败 | 转成 `APIError` |

**一个省事的事实**：OpenAI 的终止事件里带着完整的 response 对象，最终结果不用自己拼，直接走非流式的解码路径。另外两家没这么好，要自己拼，这是第 19 节的内容。

**参数增量什么时候能解析？** 只有在 item 完成之后。`{"ci` 和 `ty":"上海"}` 单独都不是合法 JSON，中文字符也可能被劈在两段之间（1.1 课第 5 节）。想在参数还没生成完时就展示，需要“部分 JSON 解析器”，那是前端体验优化，不是正确性要求。

---

# 第三部分：Anthropic Messages

## 7. 请求：两个人轮流说话

```bash
curl https://api.anthropic.com/v1/messages \
  -H "x-api-key: $ANTHROPIC_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-opus-5-5",
    "max_tokens": 4096,
    "system": "你是一个简洁的助手。",
    "messages": [
      {"role": "user", "content": "北京天气怎么样？"}
    ],
    "tools": [{
      "name": "get_weather",
      "description": "查询城市天气",
      "input_schema": {
        "type": "object",
        "properties": {"city": {"type": "string"}},
        "required": ["city"]
      }
    }]
  }'
```

**逐行解读**

- **两个必需的头**：`x-api-key`（不是 `Authorization`）和 `anthropic-version`。版本号这些年一直是 `2023-06-01`，新功能通过 `anthropic-beta` 头单独开启。
- **`max_tokens` 必填**。另外两家可以不填，统一客户端在用户没给时要补一个默认值（练习里是 4096）。
- **`system` 在顶层**。也可以是 block 数组，用来给系统提示打缓存断点（1.5 课）。
- **`messages` 只有 `user` 和 `assistant` 两种角色**。没有 `tool` 角色、没有 `system` 角色。
- **工具定义叫 `input_schema`**，不叫 `parameters`。

## 8. 内容块与工具往返

响应：

```json
{
  "id": "msg_a1",
  "type": "message",
  "role": "assistant",
  "content": [
    {"type": "thinking", "thinking": "用户问天气，需要调用工具。", "signature": "EqQB..."},
    {"type": "text", "text": "我查一下。"},
    {"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "北京"}}
  ],
  "stop_reason": "tool_use",
  "usage": {"input_tokens": 30, "cache_creation_input_tokens": 0,
            "cache_read_input_tokens": 1000, "output_tokens": 60}
}
```

下一轮请求：

```json
"messages": [
  {"role": "user", "content": "北京天气怎么样？"},
  {"role": "assistant", "content": [
    {"type": "thinking", "thinking": "用户问天气，需要调用工具。", "signature": "EqQB..."},
    {"type": "text", "text": "我查一下。"},
    {"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "北京"}}
  ]},
  {"role": "user", "content": [
    {"type": "tool_result", "tool_use_id": "toolu_1", "content": "晴，25°C"}
  ]}
]
```

**要点**

1. **`input` 是对象**，不用再解析。但流式时它是一段段字符串（第 10 节）。
2. **工具结果是一条 user 消息**，里面放 `tool_result` block。规则很严格：
    - 上一轮的**每个** `tool_use` 都要有对应的 `tool_result`；
    - `tool_result` 必须**排在这条消息的最前面**，放在其他内容后面会报 400；
    - **不要在 `tool_result` 后面再加文字**。官方文档专门说了：这样做会让模型学到“工具结果后面用户总会再说点什么”，于是它直接结束这一轮，返回一个只有 2–3 个 token 的空回复，`stop_reason` 还是 `end_turn`。想补充信息，等这一轮结束后另发一条 user 消息。
3. **`is_error: true`**：工具执行失败时标记出来，模型会据此改参数重试或换思路。这是三家里唯一一个在协议层区分“工具出错”的（Gemini 的 `function_result` 也有 `is_error`，OpenAI 没有）。
4. **thinking block 必须原样回传**。开启扩展思考时，带工具调用的那一轮的 `thinking` block 要和 `signature` 一起原封不动放回 assistant 消息里，签名用来验证这段思考没被篡改。改一个字、删掉它，或者把 `redacted_thinking` 的加密内容弄丢，请求都可能直接失败。
5. **不能发空 text block**。`{"type":"text","text":""}` 会被拒绝，统一客户端在编码时要跳过空文本（`TestAnthropicEncode` 覆盖了这一条）。

## 9. stop_reason：七种结局

| 值 | 含义 | 怎么处理 | 统一口径 |
|---|---|---|---|
| `end_turn` | 正常说完 | 用结果 | `end` |
| `stop_sequence` | 碰到了你的停止序列 | 看 `stop_sequence` 字段是哪个 | `end` |
| `max_tokens` | 到了 `max_tokens` | 结果被截断；如果最后一个块是不完整的 `tool_use`，要调大上限重发 | `length` |
| `model_context_window_exceeded` | 输出撑满了上下文窗口 | 当作截断 | `length` |
| `tool_use` | 等你执行工具 | 执行后回传 `tool_result` | `tool_use` |
| `pause_turn` | 服务端工具（如 web search）循环到了迭代上限（默认 10 次） | **把这次的 assistant 内容原样发回去**，它会接着做 | `pause` |
| `refusal` | 拒绝回答（安全分类器触发） | 看 `stop_details`；可以换模型重试 | `refusal` |

两个容易混淆的点：

- **`refusal` 是 HTTP 200**，不是错误。只检查状态码的代码会把拒答当成正常回答展示给用户。
- **`pause_turn` 和 `tool_use` 不会同时出现**：需要你执行客户端工具时一定是 `tool_use`，`pause_turn` 只和服务端工具有关。练习的 `RunTools` 遇到 `pause` 时直接再发一次。

## 10. 流式：自己拼块

```
event: message_start
data: {"type":"message_start","message":{"id":"msg_a1","content":[],"usage":{"input_tokens":30,...}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"用户问天气，"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EqQB..."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: ping
data: {"type": "ping"}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\": \"北"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"京\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":60}}

event: message_stop
data: {"type":"message_stop"}
```

**逐行解读**

- **结构是固定的**：`message_start` → 若干个块（每块 `content_block_start` → 若干 `content_block_delta` → `content_block_stop`）→ `message_delta` → `message_stop`。中间随时可能插入 `ping`。
- **`index` 就是这个块在最终 `content` 数组里的位置**。拼装时按 index 存，最后按 index 排序。
- **四种 delta**：`text_delta`（文本）、`input_json_delta`（工具参数的一段 JSON 字符串）、`thinking_delta`（思考文本）、`signature_delta`（思考块的签名，在 `content_block_stop` 之前到达）。
- **`content_block_start` 里 `tool_use` 的 `input` 是 `{}`**，真正的参数在后面的 `input_json_delta` 里。拼完后在 `content_block_stop` 时解析；如果一个 delta 都没有（无参工具），参数就是空字符串，要当成 `{}`。
- **官方说明**：当前模型一次只吐出一个完整的“键和值”，然后切成多段 delta 发出来，所以工具参数会“一卡一卡”地到。想要更细粒度、更低延迟，可以在工具定义上开 `eager_input_streaming`。
- **`stop_reason` 只在 `message_delta` 里**，`message_start` 里是 `null`。
- **`message_delta` 里的 `usage` 是累计值**，直接覆盖，不要累加。
- **`error` 事件**：高峰期可能在流中途收到 `{"type":"error","error":{"type":"overloaded_error",...}}`，对应非流式时的 HTTP 529。这时 HTTP 状态码早就是 200 了，只能靠事件判断。
- **未知事件要忽略**。官方的版本策略明确说会新增事件类型，解析器遇到不认识的类型不能报错。

**断线续传**：流断了，Claude 4.5 及更早的模型可以把已收到的部分放进一条 assistant 消息，让模型接着写（prefill）。**Claude 4.6 及之后改了**：要发一条 **user** 消息，内容是“你之前的回答在 […] 处中断了，从那里接着写”。工具调用块和思考块不能部分恢复，只能从最近的文本块续写。

## 11. 三家的用量口径不一样

这是统一客户端里最容易算错钱的地方：

| | 输入里包含缓存吗 | 输出里包含推理吗 | 依据 |
|---|---|---|---|
| OpenAI | **包含**：`input_tokens` 已含 `cached_tokens` | **包含**：`output_tokens` 已含 `reasoning_tokens` | usage 示例：75 + 1186 = 1261，1186 里有 1024 是推理 |
| Anthropic | **不包含**：`input_tokens` 只是缓存断点之后的部分，总输入 = `input_tokens` + `cache_creation_input_tokens` + `cache_read_input_tokens` | 思考 token 计入 `output_tokens` | Messages API 的 usage 定义 |
| Gemini Interactions | `total_input_tokens` 含缓存，`total_cached_tokens` 单列 | **不包含**：思考单列在 `total_thought_tokens` | 官方示例：7 + 20 + 22 = 49 = `total_tokens` |

练习里统一成：**`InputTokens` 含缓存，`OutputTokens` 含推理**（和 OpenAI 一致）。于是：

- Anthropic：`InputTokens = input + cache_creation + cache_read`。`TestAnthropicGenerateUsageIncludesCache` 里，`input_tokens` 字段只有 30，真实输入是 1030；只看这个字段，你的成本报表会少算 97%。
- Gemini：`OutputTokens = total_output + total_thought`。

为什么要统一？第 09 模块做成本治理时，所有厂商的账要能放在一张表里比，口径不一致，比较就没有意义。注意**统一的是“数量”，不是“价格”**：缓存读、缓存写、推理 token 各家单价不同，算钱时还要按厂商的价目表分别乘。

---

# 第四部分：Gemini Interactions

## 12. 请求与 steps 时间线

```bash
curl -X POST "https://generativelanguage.googleapis.com/v1beta/interactions" \
  -H "x-goog-api-key: $GEMINI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.8-flash",
    "system_instruction": "你是一个简洁的助手。",
    "input": [
      {"type": "user_input", "content": [{"type": "text", "text": "北京天气怎么样？"}]}
    ],
    "tools": [{
      "type": "function",
      "name": "get_weather",
      "description": "查询城市天气",
      "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}
    }],
    "generation_config": {"max_output_tokens": 2048},
    "store": false
  }'
```

响应：

```json
{
  "id": "int_g1",
  "status": "requires_action",
  "steps": [
    {"type": "thought", "signature": "thought_sig_abcd",
     "summary": [{"type": "text", "text": "需要查天气。"}]},
    {"type": "function_call", "id": "fc_g1", "name": "get_weather",
     "arguments": {"city": "北京"}}
  ],
  "usage": {"total_input_tokens": 100, "total_cached_tokens": 40,
            "total_output_tokens": 25, "total_thought_tokens": 30, "total_tokens": 155}
}
```

**逐行解读**

- **`input`** 可以是字符串、内容数组或 **step 数组**。无状态多轮对话用 step 数组：用户说的话是 `user_input`，模型说的话是 `model_output`，两者的 `content` 都是内容数组。
- **`steps` 是一条时间线**：`thought`（思考）、`function_call`、`function_result`、`model_output`，以及内置工具的 `google_search_call` / `google_search_result` 等。`POST` 只返回模型产出的 step；`GET /interactions/{id}` 返回包括 `user_input` 在内的完整时间线。
- **`status`**：`completed`、`requires_action`（需要你执行工具）、`incomplete`（比如撞上输出上限）、`failed`、`cancelled`，后台模式还有 `in_progress`、`queued`。
- **`arguments` 是对象**，和 Anthropic 一样。
- **`thought` step 带 `signature`**。无状态模式下，下一轮要把它原样放回 `input`。
- **`generation_config`** 放采样相关的配置（`max_output_tokens`、`temperature`、`thinking_level`……）；输出格式（JSON Schema、图片、音频）放在顶层的 `response_format`，1.3 课讲。
- **作用域**：用 `previous_interaction_id` 续聊时，只有对话历史会被继承；`tools`、`system_instruction`、`generation_config` 都是**本次 interaction 有效**，每次都要重新传。

下一轮：

```json
"input": [
  {"type": "user_input", "content": [{"type": "text", "text": "北京天气怎么样？"}]},
  {"type": "thought", "signature": "thought_sig_abcd", "summary": [...]},
  {"type": "function_call", "id": "fc_g1", "name": "get_weather", "arguments": {"city": "北京"}},
  {"type": "function_result", "call_id": "fc_g1", "name": "get_weather",
   "result": [{"type": "text", "text": "晴，25°C"}]}
]
```

`function_result` 用 `call_id` 配对，同时还要带 `name`；`result` 可以是内容数组、对象或字符串；工具出错时可以加 `is_error: true`。

## 13. 流式：step 事件，以及文档自相矛盾时怎么办

```
event: interaction.created
data: {"event_type":"interaction.created","interaction":{"id":"int_gs1","status":"in_progress"}}

event: step.start
data: {"event_type":"step.start","index":0,"step":{"type":"thought"}}

event: step.delta
data: {"event_type":"step.delta","index":0,"delta":{"type":"thought_signature","signature":"sig_..."}}

event: step.stop
data: {"event_type":"step.stop","index":0}

event: step.start
data: {"event_type":"step.start","index":1,"step":{"type":"function_call","id":"fc_gs1","name":"get_weather"}}

event: step.delta
data: {"event_type":"step.delta","index":1,"delta":{"type":"arguments_delta","arguments":"{\"city\": \"广州\"}"}}

event: step.stop
data: {"event_type":"step.stop","index":1,"status":"waiting"}

event: interaction.requires_action
data: {"interaction":{"id":"int_gs1","status":"requires_action","usage":{...}}}
```

结构和 Anthropic 很像：`step.start` → `step.delta`* → `step.stop`，按 `index` 拼。不同的地方：

- **终止事件里的 interaction 可能是“部分的”**：参考手册说流式生命周期事件里的 interaction“可能省略只有非流式响应才有的字段”，所以 `steps` 要自己拼，不能指望终止事件给全；
- **每个事件可以带 `event_id`**，用来从某个事件之后恢复流。这和 0.4 课的 `Last-Event-ID` 是同一个思路，只不过放在了业务层；
- **思考签名**以 `thought_signature` 类型的 delta 单独到达，思考摘要以 `thought_summary` delta 到达。

**文档自相矛盾的地方**。写这一课时，同一家的三份官方材料对“工具参数增量”的写法不一致：

| 来源 | delta 的 type | 字段 |
|---|---|---|
| API 参考手册（`ArgumentsDelta`） | `arguments_delta` | `arguments` |
| 迁移指南的 SSE 示例 | `arguments` | `partial_arguments` |
| 2026 年 5 月破坏性变更说明 | 文字描述为 `arguments_delta` | — |

事件的判别字段也有两种写法：参考手册的示例是 `event_type`，部分指南示例是 `type`。

工程上的处理：**以参考手册为准，同时兼容另一种写法**，并且用测试把两种写法都钉住。练习的 `gemini_stream_tool.sse` 故意混用了两种写法，`TestGeminiStream` 要求都能拼出 `{"city": "广州"}`。这类容错写一次就够了，比线上某天突然“工具参数全是空的”好查得多。

**接口演进的速度**：Interactions API 在 2026 年 5 月做过一次破坏性变更：响应从扁平的 `outputs` 数组改成 `steps` 时间线，流式事件从 `content.delta` 改成 `step.delta`，旧格式 6 月 8 日下线。用 REST 直连的人可以用 `Api-Revision` 请求头选择版本。这就是为什么第五部分要把厂商协议关在 `openai.go`、`anthropic.go`、`gemini.go` 三个文件里：协议变了只改一个文件，Agent 逻辑一行不动。

## 14. 老接口 generateContent 对照

大量现有代码、教程和兼容层还在用 `generateContent`，要能读懂：

| | `generateContent` | Interactions |
|---|---|---|
| 端点 | `/v1beta/models/{model}:generateContent`，流式是单独的 `:streamGenerateContent` | `/v1beta/interactions`，流式用 `"stream": true` |
| 对话 | `contents: [{"role": "user" 或 "model", "parts": [...]}]` | `input: [step, ...]` |
| 系统提示 | `systemInstruction: {"parts": [...]}` | `system_instruction: "..."` |
| 工具定义 | `tools: [{"functionDeclarations": [...]}]`（多包一层） | `tools: [{"type":"function", ...}]` |
| 工具调用 | `parts` 里的 `{"functionCall": {"name", "args"}}` | `function_call` step |
| 工具结果 | `role: "user"` 的 `parts` 里放 `{"functionResponse": {"name", "response"}}` | `function_result` step |
| 结果 | `candidates[0].content.parts`，`finishReason`，`usageMetadata` | `steps`，`status`，`usage` |
| 流式工具调用 | 函数调用在一个 chunk 里**整块到达**，看不到参数生成过程 | 参数以增量到达 |
| 字段命名 | camelCase | snake_case |

两点提醒：

- 有些功能目前只在 `generateContent` 上有：Batch API、显式缓存、自定义安全设置、Python 的自动函数调用。用到这些就还得走老接口；
- Gemini 也提供 **OpenAI 兼容端点**，可以用 OpenAI 的 SDK 调 Gemini。兼容层的问题第 23 节讲。

---

# 第五部分：统一客户端

## 15. 统一什么，不统一什么

**要解决的问题**：Agent 代码（工具循环、记忆、评测、成本统计）不应该知道背后是哪家模型；但每家都有自己独特的东西，硬塞进一个“最大公约数”会丢信息。

练习采用三条原则：

1. **统一“最小公共语义”**：文本、工具调用、工具结果、推理块、停止原因、用量。这是 Agent 循环真正需要的全部；
2. **同一家，原样回放**：每个 `Part` 都记着它来自哪家（`Provider`）和原始 JSON（`Raw`）。发回同一家时直接用 `Raw`，`encrypted_content`、`signature`、`phase`、将来新增的字段一个都不会丢；
3. **换一家，按规范化字段重建**：文本、工具调用、工具结果有对应格式，可以转换；推理块和厂商特有的内容（服务端工具记录）对方看不懂，丢掉。

这就是“**同家无损、跨家降级**”。很多开源框架只做了第 1、3 条，每次都从规范化字段重建请求，于是推理模型在多轮工具调用里会悄悄变差，而且不报错。

## 16. 类型设计

```go
type Part struct {
	Type PartType // text | tool_call | tool_result | reasoning | opaque

	Text string // text；reasoning 的可读摘要

	CallID  string          // tool_call / tool_result
	Name    string
	Args    json.RawMessage // tool_call：一定是 JSON 对象
	Result  string          // tool_result
	IsError bool

	Provider string          // 这块内容来自哪家
	Raw      json.RawMessage // 它在原协议里的 JSON，发回同一家时原样回放
}

type Message struct {
	Role  Role // 只有 user / assistant
	Parts []Part
}

type Response struct {
	ID      string
	Message Message
	Stop    StopReason // end | length | tool_use | refusal | pause | other
	RawStop string     // 原始值，排查问题用
	Usage   Usage      // InputTokens 含缓存，OutputTokens 含推理
}
```

**逐行解读**

- **一个 `Part` 结构体，用 `Type` 区分种类**，而不是五个类型加一个接口。Go 没有和类型系统结合的 tagged union，接口加类型断言写起来更啰嗦，序列化也麻烦。代价是一些字段只对某些类型有意义，靠注释约定。
- **为什么只有两种 `Role`**：工具结果放在 user 消息里，这是 Anthropic 的模型，也是三家里最严格的。编码给 OpenAI、Gemini 时再拆成独立的 item / step，这个方向的转换是无损的；反过来（从“独立条目”合并成“user 消息”）就要处理顺序和分组。**选最严格的那家做内部表示，往宽松的方向转换。**
- **`Args` 是 `json.RawMessage`，而且一定是对象**：OpenAI 给的是字符串，解码时解析一次；Anthropic 和 Gemini 给的是对象，直接存。所有来源都过一遍 `argsObject`：校验、压缩成紧凑格式。这样三家来源的参数字节一致，转给 OpenAI 时（它要字符串）也稳定，日志里也好比对。
- **`PartOpaque`**：OpenAI 的 `web_search_call`、Anthropic 的 `server_tool_use`、Gemini 的 `google_search_result`……统一层不理解，但同一家要能原样回放（Anthropic 的 `pause_turn` 就要求把这些块发回去）。
- **`RawStop`**：规范化会丢信息（`end_turn` 和 `stop_sequence` 都变成 `end`），出问题时要能看到原始值。

## 17. Provider 接口：Go 的 iter.Seq2 对照 TS 的 AsyncGenerator

```go
type Provider interface {
	Name() string
	Generate(ctx context.Context, req *Request) (*Response, error)
	Stream(ctx context.Context, req *Request) iter.Seq2[Event, error]
}

// 用法（Go 1.23 起）
for ev, err := range p.Stream(ctx, &req) {
	if err != nil {
		return err
	}
	if ev.Type == llm.EventText {
		fmt.Print(ev.Text)
	}
}
```

同样的接口用 TypeScript 写：

```ts
interface Provider {
  name: string;
  generate(req: Request, signal?: AbortSignal): Promise<Response>;
  stream(req: Request, signal?: AbortSignal): AsyncGenerator<LLMEvent, void>;
}

// 用法
for await (const ev of p.stream(req, ac.signal)) {
  if (ev.type === "text") process.stdout.write(ev.text);
}
```

**逐行对照**

| Go | TypeScript | 说明 |
|---|---|---|
| `iter.Seq2[Event, error]` | `AsyncGenerator<LLMEvent, void>` | 都是“拉取式”的事件序列：调用方要一个，生产方给一个 |
| `for ev, err := range seq` | `for await (const ev of gen)` | Go 1.23 的 range-over-func；TS 的异步迭代 |
| 错误通过第二个返回值 `err` 产出 | 错误通过 `throw` 抛出，`for await` 外面 `try/catch` | Go 的错误是值，所以序列里要带一个 `error` 位置 |
| `ctx context.Context` | `signal?: AbortSignal` | 取消信号，0.1、0.2 课讲过：ctx 取消 ⇔ `ac.abort()` |
| 调用方 `break` → `yield` 返回 `false` → 生产方 `return`，`defer resp.Body.Close()` 关掉连接 | 调用方 `break` → 运行时调用 `gen.return()` → 生成器里的 `finally { reader.cancel() }` 执行 | 两边都要保证“消费方不要了，连接就断开”，否则上游模型还在生成、还在计费 |
| 生产方函数 `func(yield func(Event, error) bool)` | `async function*`，里面用 `yield ev` | Go 里 `yield` 是一个普通函数参数，返回值告诉你还要不要继续 |

最后一行是关键差异：**Go 的 `yield` 返回 `false` 时，你必须停止**，继续调用会 panic。所以 `runStream` 里每个 `yield` 都检查返回值：

```go
for _, e := range out {
	if !yield(e, nil) { // 调用方 break 了：defer 关闭连接，上游停止生成
		return
	}
}
```

`TestStreamBreakClosesConnection` 验证的就是这件事：调用方收到 3 个事件后 `break`，服务端的 `r.Context()` 必须在 2 秒内被取消。

为什么不用 channel（`<-chan Event`）？channel 版本要额外起一个 goroutine 往里写，调用方中途不读了，这个 goroutine 就会永远阻塞在发送上，除非再加一个取消机制。迭代器是同步拉取的，没有这个问题。

## 18. 三层检查，写一次给三家用

0.4 课补充的“三层状态机”在这里落成了两个函数：

```go
// post：① HTTP 层、② SSE 层
if resp.StatusCode < 200 || resp.StatusCode > 299 {
	defer resp.Body.Close()
	return nil, parseAPIError(provider, resp) // 解析三家各自的错误体
}
if stream && !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
	resp.Body.Close()
	return nil, &APIError{..., Type: "bad_content_type"}
}

// runStream：③ 业务层
ev, err := r.next()
if err == io.EOF {
	if !dec.done() { // HTTP 流正常结束 ≠ 模型输出完了
		yield(Event{}, ErrIncompleteStream)
	}
	return
}
```

每家只需要实现一个很小的状态机接口：

```go
type streamDecoder interface {
	feed(ev sseEvent) ([]Event, error) // 喂一个 SSE 事件，吐出 0 个或多个统一事件
	done() bool                        // 收到终止事件了吗
}
```

三家的终止事件分别是 `response.completed`（及 `incomplete`、`failed`）、`message_stop`、`interaction.completed`（及 `requires_action`）。`TestAnthropicStreamTruncatedIsError` 用一段在工具参数中途断掉的流验证：用户已经看到了“我查一下。”，但没有 `message_stop`，整体必须判失败。

## 19. 拼装：流式结果必须和非流式一模一样

Anthropic 和 Gemini 的终止事件不带完整结果，要自己拼。思路：**按 index 收集每个块，结束时按 index 排序，重建出和非流式响应完全相同的 JSON，再交给非流式的解码函数**。

```go
func (s *anStream) build() (*Response, error) {
	// ……按 index 排序……
	for _, i := range idx {
		b := s.blocks[i]
		var raw json.RawMessage
		switch b.typ {
		case "text":
			raw = rawOf(map[string]string{"type": "text", "text": b.text.String()})
		case "thinking":
			raw = rawOf(map[string]string{"type": "thinking", "thinking": b.text.String(), "signature": b.signature})
		case "tool_use", "server_tool_use":
			input, err := argsObject(b.json.String()) // 参数在这里第一次被完整解析
			if err != nil {
				return nil, err
			}
			raw = rawOf(map[string]any{"type": b.typ, "id": b.id, "name": b.name, "input": input})
		default: // redacted_thinking、各种 *_tool_result：start 里就是完整内容
			raw = b.start
		}
		r.Content = append(r.Content, raw)
	}
	return r.toResponse() // 复用非流式的解码
}
```

**逐行解读**

- **复用非流式解码**：流式和非流式只在“怎么得到完整 JSON”上不同，后面的规范化逻辑只写一份，就不会出现“流式时停止原因映射漏了一个”这种不一致。
- **思考块要把 `signature_delta` 拼回去**，否则下一轮回传时签名是空的。
- **`default` 分支**：`redacted_thinking`（加密的思考）和服务端工具结果没有 delta，`content_block_start` 里就是完整内容，原样保存。
- **测试方法**：`TestAnthropicStreamMatchesGenerate` 用一对内容相同的流式 / 非流式 fixture，逐块比较类型、文本、参数、Raw、停止原因和用量。“两条路径结果必须相等”比手写断言更能抓 bug，和 0.4 课“任意位置切块结果都一样”的模糊测试是同一个思想。

## 20. 错误与重试

| 情况 | 例子 | 重试？ |
|---|---|---|
| 限流 | 429（OpenAI `rate_limit_exceeded`，Gemini `RESOURCE_EXHAUSTED`，Anthropic `rate_limit_error`） | 是，**优先按 `Retry-After` 等** |
| 过载 | Anthropic 529 `overloaded_error`；流中途的 `error` 事件 | 是 |
| 服务端错误 | 500、502、503、504 | 是 |
| 请求错误 | 400（参数不支持、schema 不合法、`tool_use` 缺少 `tool_result`） | **否**，重试一万次也是 400 |
| 鉴权 | 401、403 | 否 |
| 流断了 | `ErrIncompleteStream` | 是，但**只在还没推给用户任何内容时**（练习 3） |
| 拒答 | `stop: refusal`（HTTP 200） | 不是错误；可以换模型重试 |

三家的错误体形状也不同，`parseAPIError` 一个函数兼容三种：

```
OpenAI:    {"error":{"message":"...","type":"...","code":"..."}}
Anthropic: {"type":"error","error":{"type":"overloaded_error","message":"..."}}
Gemini:    {"error":{"code":429,"message":"...","status":"RESOURCE_EXHAUSTED"}}
```

最常见的 400 之一，就是 1.1 课第 22 节说的：给不支持的模型传了 `temperature`。比如 GPT-6 Astra 不支持 `reasoning.effort: "none"`，传了直接 400。所以**参数要按模型能力表来发**，第 11 模块的模型网关会做这件事。`TestHTTPErrors` 用表驱动测试覆盖了三家的 6 种错误。

## 21. 对三家完全相同的 Agent 循环

```go
func RunTools(ctx context.Context, p Provider, req Request, tools map[string]ToolFunc, maxSteps int) (*Response, []Message, error) {
	history := append([]Message(nil), req.Messages...)
	for step := 0; step < maxSteps; step++ {
		r := req
		r.Messages = history
		resp, err := p.Generate(ctx, &r)
		if err != nil {
			return nil, history, err
		}
		history = append(history, resp.Message) // ① 助手消息原样进历史
		switch resp.Stop {
		case StopToolUse:
		case StopPause: // ② 服务端工具暂停：原样再发一次
			continue
		default:
			return resp, history, nil
		}
		results := Message{Role: RoleUser}
		for _, c := range resp.Message.ToolCalls() { // ③ 每个调用都必须有结果
			res := Part{Type: PartToolResult, CallID: c.CallID, Name: c.Name}
			fn, ok := tools[c.Name]
			if !ok {
				res.Result, res.IsError = "unknown tool: "+c.Name, true
			} else if out, err := fn(ctx, c.Args); err != nil {
				res.Result, res.IsError = err.Error(), true // ④ 工具出错交给模型，不中断循环
			} else {
				res.Result = out
			}
			results.Parts = append(results.Parts, res)
		}
		history = append(history, results)
	}
	return nil, history, ErrMaxSteps // ⑤ 步数上限
}
```

**逐行解读**

- **① 原样进历史**：`resp.Message` 里每个 Part 都带着 `Raw`，下一轮编码时同一家直接回放。推理块、签名、`phase` 全在里面，循环本身完全不用知道它们的存在。
- **② `pause`**：只有 Anthropic 会出现，但循环不需要知道是哪家。
- **③ 每个调用都要有结果**：模型并行调用了 3 个工具，你只回了 2 个，三家都会报错。连“工具不存在”也要回一个错误结果。
- **④ 工具出错不中断**：参数错了、外部服务超时，把错误信息作为 `is_error` 结果交给模型，它通常会改正参数重试。只有 ctx 被取消这类“整体要停”的情况才应该中断循环（第 03 模块细化）。
- **⑤ 步数上限**：模型可能陷入“一直调工具”的循环，必须有上限。`TestRunToolsUnknownToolAndMaxSteps` 模拟了一个一直调用不存在工具的模型。

`TestRunToolsAllProviders` 用同一个 `RunTools` 对三家各跑一轮“调工具 → 回答”，然后检查第二个请求的原始 JSON：OpenAI 的 `encrypted_content`、Anthropic 的 `signature`、Gemini 的 thought `signature` 都必须被原样带了回去，工具结果都按各家格式和调用配上了对。

这个循环现在是串行执行工具、没有超时、没有审批、没有检查点。第 03 模块会在这个骨架上逐个加上去。

## 22. 换模型：历史怎么迁移

**场景**：前半段用 Claude，后半段想换 Gemini（更便宜、上下文更长，或者 Claude 拒答了要回退）。

编码时的规则已经在每家的 `encode` 里了：

```go
if part.Provider == "gemini" && len(part.Raw) > 0 { // 同一家：原样回放
	out.Input = append(out.Input, part.Raw)
	continue
}
switch part.Type {
case PartText:       // → user_input / model_output
case PartToolCall:   // → function_call，arguments 是对象
case PartToolResult: // → function_result，带 name
// PartReasoning、PartOpaque：别家的，丢掉
}
```

`TestCrossProviderHistory` 把一段带 thinking 的 Anthropic 历史分别编码给 OpenAI 和 Gemini，断言：思考块和签名一个字节都没有泄漏过去，工具调用和结果都转成了对方的格式。

**为什么推理块必须丢掉**：签名和加密内容只有原厂商能验证、解密，发给别家要么报错，要么被当成普通文本塞进上下文。连同一家都有限制：OpenAI 的文档说推理只能在同一个模型家族里复用（GPT-5.6 系列内部可以，跨到 GPT-5.5 就会被忽略）。

**代价**：换模型后，新模型看不到之前的推理过程，只能看到“说了什么、调了什么工具、拿到了什么结果”。对大多数任务够用；对很依赖前面推理的长任务，换模型前可以让旧模型先写一段总结，作为文本放进历史。

## 23. 官方 SDK、OpenAI 兼容层，还要自己写吗？

本课手写客户端是为了**看清协议**。生产环境的选择：

| 方案 | 优点 | 缺点 |
|---|---|---|
| 各家官方 SDK | 新功能第一时间支持；类型完整；内置重试、流式拼装 | 三套 API，Agent 代码要自己做适配层 |
| OpenAI 兼容端点（Gemini、大多数国产模型都提供） | 一套 SDK 调多家 | 只覆盖“公共子集”；推理块、签名、厂商特有功能往往不支持或行为不同；错误码和停止原因映射各家不一 |
| 第三方统一 SDK（Vercel AI SDK 等，第 04 模块讲） | 现成的统一抽象 | 抽象层的取舍不一定合你的需要；新功能有延迟 |
| 自己的薄适配层（本课） | 完全可控；能做“同家无损” | 要自己跟进协议变化 |

本课程主线项目的选择：**底层用官方 SDK 或直连 HTTP，上面套一层自己的 `Provider` 接口**。接口是自己的，Agent 逻辑、评测、成本统计都依赖它；每家的实现可以随时从手写换成官方 SDK，只要测试还过。

---

## 本课小结

| 知识点 | 一句话 | 练习里的代码 |
|---|---|---|
| 三家形状 | OpenAI、Gemini 是时间线上的条目；Anthropic 是轮流说话、每句多个块 | `encode` × 3 |
| 工具参数 | OpenAI 是字符串，另两家是对象；统一成紧凑的 JSON 对象 | `argsObject` |
| 工具结果 | OpenAI 无 `is_error`；Anthropic 放在 user 消息最前面；Gemini 要带 `name` | `TestOpenAIEncode` 等 |
| 推理块 | `encrypted_content` / `signature` / thought `signature`，必须原样回传 | `TestRunToolsAllProviders` |
| `store: false` | 无状态，可移植可审计；Go 里不能 `omitempty` | `oaRequest.Store` |
| 停止原因 | 七种原始值归成六种；`refusal` 是 HTTP 200 | `anStop`、`toResponse` |
| 用量口径 | Anthropic 输入不含缓存，Gemini 输出不含思考 | `TestAnthropicGenerateUsageIncludesCache` |
| 流式终止 | 没收到终止事件就是失败 | `runStream`、`ErrIncompleteStream` |
| 流式拼装 | 按 index 收集、排序，重建 JSON，复用非流式解码 | `anStream.build`、`TestAnthropicStreamMatchesGenerate` |
| 文档矛盾 | 以参考手册为准、兼容另一种写法、用测试钉住 | `TestGeminiStream` |
| 迭代器 | `iter.Seq2` ⇔ `AsyncGenerator`；`yield` 返回 false 必须停 | `TestStreamBreakClosesConnection` |
| 重试 | 429 / 5xx / 529 / 断流可重试，4xx 不行；先听 `Retry-After` | `APIError.Retryable`、`TestHTTPErrors` |
| Agent 循环 | 原样进历史、每个调用都回结果、工具出错交给模型、有步数上限 | `RunTools` |
| 换模型 | 同家无损、跨家降级，推理块丢掉 | `TestCrossProviderHistory` |

---

## 练习（今天做完）

练习目录：`projects/07-llm-api/`。

```bash
cd projects/07-llm-api
make test                    # go vet + go test -race -v，20 个测试
```

| 文件 | 内容 |
|---|---|
| `llm.go` | 统一类型、`Provider` 接口、错误分类、HTTP 与三层检查、`runStream`、`Collect` |
| `sse.go` | SSE 解析器（0.4 课的规范，按字节读行） |
| `openai.go` / `anthropic.go` / `gemini.go` | 三家的编码、解码、流式状态机 |
| `agent.go` | `RunTools` 工具循环 |
| `llm_test.go` + `testdata/` | 20 个测试，fixture 按 2026 年 10 月官方文档的格式手写 |
| `cmd/chat/` | 用真实 API Key 试一下的命令行 |

### 练习 1：读测试，再打一次真实 API

1. 运行 `make test`，找到三行 `t.Logf`：推理耗光输出上限、Anthropic 的真实输入、断流判失败。每一行用一句话说明它防住了什么线上问题。
2. 打开 `testdata/` 里的三个 `*_stream_tool.sse`，对照第 6、10、13 节，在纸上画出每家“工具调用”从第一个事件到终止事件的状态变化。
3. 如果你有任意一家的 API Key（国内可以用兼容 OpenAI 的模型服务，见选做），运行：

```bash
export ANTHROPIC_API_KEY=...   # 或 OPENAI_API_KEY / GEMINI_API_KEY
go run ./cmd/chat -p anthropic -m 你的模型名 "用一句话解释 KV Cache"
go run ./cmd/chat -p anthropic -m 你的模型名 -tools "现在几点？再算一下 17*23"
```

记录 stderr 里打印的 `stop`、`input`、`cached`、`output`、`reasoning`。连续跑两次第二条命令，看 `cached` 有没有变化，想一想为什么（提示：1.5 课）。

### 练习 2：改坏它，看哪个测试失败

每次只改一处，跑 `go test`，记录失败的测试名和原因（没有测试失败的，想清楚为什么），然后改回来：

1. `oaRequest` 的 `Store` 字段加上 `omitempty`；
2. `OpenAI.encode` 里删掉 `if part.Provider == "openai" && len(part.Raw) > 0` 这个分支（提示：想一想线上的表现会是什么）；
3. `anUsage.normalize` 里 `InputTokens` 只用 `u.InputTokens`；
4. `anStream.feed` 里 `message_delta` 的 usage 改成累加（`s.usage.OutputTokens += ...`）；
5. `runStream` 里把 `if !dec.done() { yield(Event{}, ErrIncompleteStream) }` 改成直接 `return`；
6. `gmStream.feed` 里删掉 `"arguments"` 这个 case 标签，只保留 `"arguments_delta"`；
7. `runStream` 里把 `if !yield(e, nil) { return }` 改成 `yield(e, nil)`（提示：看 panic 信息）；
8. `RunTools` 里工具出错时改成 `return nil, history, err`。哪个测试会失败？如果没有测试失败，补一个。

### 练习 3：给任意 Provider 加上重试

新建 `retry.go`，实现一个包装器：

```go
// Retry 给任意 Provider 加上重试。它本身也是一个 Provider。
type Retry struct {
	Provider
	MaxAttempts int           // 总尝试次数，含第一次
	Base        time.Duration // 第一次退避的上限
	Max         time.Duration // 单次退避的上限
	Sleep       func(ctx context.Context, d time.Duration) error // 测试时替换掉
}
```

要求：

- `Generate`：只重试 `IsRetryable` 的错误；第 n 次退避在 `[0, min(Base·2ⁿ, Max))` 里均匀随机（全抖动）；服务端给了 `Retry-After` 就按它等；等待期间 ctx 取消要立刻返回；
- `Stream`：**只有在还没有推出任何事件时才重试**；已经推出过事件再失败，直接把错误交给调用方；
- 写两个测试：
    - 服务端依次返回 529、带 `Retry-After: 3` 的 429、正常结果，断言最终成功、等了两次、第二次正好等 3 秒；
    - 服务端第一次返回 503，第二次返回 `anthropic_stream_truncated.sse`，断言最终错误是 `ErrIncompleteStream`，而且服务端只被请求了 2 次。

### 选做 1：加一个 Chat Completions Provider

国内大多数模型服务（以及 Gemini 的兼容端点）提供的是 OpenAI **Chat Completions** 兼容接口：`POST /v1/chat/completions`，`messages` 里有 `system`、`tool` 角色，工具定义多包一层 `function`，工具调用在 `message.tool_calls` 里，流式用 `choices[0].delta` 增量、以 `data: [DONE]` 结束。实现一个 `ChatCompletions` Provider，让 `TestRunToolsAllProviders` 也能跑它。想一想：推理块在这个协议里放在哪？（提示：各家兼容层不一样，有的放在 `reasoning_content` 字段，有的不返回。）

### 选做 2：Gemini 的 generateContent Provider

按第 14 节的对照表实现 `GeminiLegacy`，注意 `role: "model"`、camelCase、函数调用在流式里整块到达。

---

## 自测题

1. 同样是工具调用参数，OpenAI Responses 给的是什么类型，Anthropic 和 Gemini 给的是什么类型？统一客户端怎么处理？
2. OpenAI 的 `function_call` item 里有 `id` 和 `call_id`，回传结果时用哪个？
3. 带推理的模型调完工具，下一轮不把 reasoning item 发回去，会报错吗？会有什么后果？
4. 为什么 Go 里 `Store bool` 字段不能加 `omitempty`？
5. Anthropic 的工具结果放在哪里？有哪三条摆放规则？违反最后一条会出现什么现象？
6. Anthropic 返回 `stop_reason: "refusal"` 时 HTTP 状态码是多少？这对错误处理意味着什么？`pause_turn` 又该怎么处理？
7. Anthropic 响应里 `input_tokens: 30`、`cache_read_input_tokens: 1000`、`cache_creation_input_tokens: 0`，这次请求的总输入是多少？Gemini 的 `total_output_tokens: 25`、`total_thought_tokens: 30`，统一口径的输出是多少？依据是什么？
8. 三家流式的终止事件分别是什么？哪一家的终止事件里带着完整结果？
9. Anthropic 流式里，工具参数在什么时候才能 `JSON.parse`？无参工具会是什么情况？`message_delta` 的 usage 应该覆盖还是累加？
10. Gemini 官方文档里工具参数增量有两种写法，你的解析器应该怎么办？
11. Go 的 `iter.Seq2` 里 `yield` 返回 `false` 意味着什么？如果忽略它继续 `yield` 会怎样？TypeScript 的 `AsyncGenerator` 里对应的机制是什么？
12. 为什么统一客户端发回同一家时要用 `Raw` 原样回放，而不是用规范化字段重建？
13. 下面哪些错误该重试：400 `Unsupported parameter: 'temperature'`、429、Anthropic 529、401、流中途的 `overloaded_error`、流断了但已经给用户推了一半文字？
14. 对话前半段用 Claude（开了扩展思考），后半段换成 Gemini，历史里的 thinking block 应该怎么处理？为什么？换模型后会损失什么？

---

# 参考答案与详解

## 自测题答案

**1.** OpenAI Responses 的 `arguments` 是 **JSON 字符串**，Anthropic 的 `input` 和 Gemini 的 `arguments` 是 **JSON 对象**。统一客户端把所有来源都过一遍 `argsObject`：空串当 `{}`，不是对象就报错，合法的压缩成紧凑格式，存成 `json.RawMessage`。编码给 OpenAI 时再 `string(args)` 转成字符串。

**2.** 用 **`call_id`**（`call_...`）。`id`（`fc_...`）是这个 item 自己的 id，只在原样回放 item 时有意义。用错了，服务端找不到对应的调用，会报“没有对应 output 的 function_call”之类的错误。

**3.** 一般**不会报错**，这正是它危险的地方。后果是模型丢掉了做出这次调用时的推理上下文，要重新推理：效果变差、推理 token 变多、更贵。官方强烈建议把最后一次函数调用之后的所有 reasoning、function_call、function_call_output 原样传回。无状态模式下 reasoning item 里有 `encrypted_content`，就是让你原样带回去的。

**4.** `omitempty` 会在值为零值时省略字段，`false` 正好是 bool 的零值。于是 `store: false` 永远发不出去，服务端按默认值（存储）处理，你以为没存，其实存了。这类“想关的开关关不掉”的 bug 只能靠检查发出去的 JSON 来发现，`TestOpenAIEncode` 专门断言了请求里有 `"store":false`。

**5.** 放在下一条 **user** 消息里，作为 `tool_result` block。规则：上一轮每个 `tool_use` 都要有对应的 `tool_result`；`tool_result` 要排在这条消息最前面；`tool_result` 后面不要再加文字。违反最后一条，模型会学到“工具结果后面用户还会说话”，于是直接结束这一轮，返回一个只有 2–3 个 token 的空回复，`stop_reason` 是 `end_turn`，看起来像正常结束。

**6.** **200**。拒答是正常响应，只检查 HTTP 状态码的代码会把拒答当成正常回答。必须检查 `stop_reason`（统一口径里的 `Stop`），拒答时可以展示提示、换模型重试。`pause_turn` 表示服务端工具循环（比如 web search）到了迭代上限，把这次的 assistant 内容原样追加进历史再请求一次即可继续，`RunTools` 里就是 `continue`。

**7.** Anthropic：`30 + 0 + 1000 = 1030`，其中 1000 命中缓存。依据：Anthropic 的 `input_tokens` 只统计最后一个缓存断点之后的部分，缓存读写单独列出。Gemini：`25 + 30 = 55`。依据：Interactions 参考手册的示例里 `total_tokens` = 输入 + 输出 + 思考（7 + 20 + 22 = 49），说明 `total_output_tokens` 不含思考；统一口径的输出要包含推理，所以相加。

**8.** OpenAI：`response.completed`（以及 `response.incomplete`、`response.failed`）；Anthropic：`message_stop`；Gemini：`interaction.completed`（需要执行工具时是 `interaction.requires_action`）。**OpenAI** 的终止事件里带完整的 response 对象，可以直接复用非流式解码；另外两家要自己按 index 拼。

**9.** 在这个块的 `content_block_stop` 之后，把所有 `input_json_delta` 的 `partial_json` 拼起来再解析。中途的片段（`{"city": "北`）不是合法 JSON。无参工具可能一个 delta 都没有（或者只有一个空字符串的 delta），拼出来是空串，要当成 `{}`。`message_delta` 里的 usage 是**累计值**，要覆盖，累加会重复计数（练习 2 第 4 条）。

**10.** 以 API 参考手册为准（`arguments_delta` + `arguments`），同时兼容迁移指南的写法（`arguments` + `partial_arguments`），并在测试里用两种写法的 fixture 把行为钉住。不要只信一份示例；也不要因为文档矛盾就放弃流式。厂商修文档、改实现时，你的测试会第一时间告诉你哪种写法还在用。

**11.** 调用方不要更多数据了（`break`、`return` 或 panic）。生产方必须立刻停止并清理资源，在我们的实现里是 `return`，`defer resp.Body.Close()` 关闭 HTTP 连接，上游随之停止生成。忽略它继续调用 `yield`，Go 运行时会 panic（`range function continued iteration after function for loop body returned false`）。TypeScript 里对应的是：`for await` 中途 `break` 时运行时调用生成器的 `return()`，生成器里的 `finally` 块执行，在那里 `reader.cancel()` 释放连接。

**12.** 规范化字段只覆盖“公共语义”，而厂商响应里还有很多统一层不认识、但模型需要的东西：OpenAI 的 `encrypted_content`、`phase`，Anthropic 的 `signature`、`redacted_thinking` 的加密内容，Gemini 的 thought `signature`，以及未来新增的任何字段。重建时漏掉任何一个，轻则模型变笨变贵，重则请求被拒。原样回放保证同一家“一个字节都不丢”，统一层也不用追着每家的新字段改代码。

**13.** 该重试：429（按 `Retry-After`）、529、流中途的 `overloaded_error`。不该重试：400 参数不支持（改请求，不是重试）、401（检查 Key）。流断了但已经推给用户一半文字：**不能直接重试**，重试会从头再生成一遍，用户看到重复或前后不一致的内容。可选的做法：把错误报给用户由他决定；或者按第 10 节的办法续写；或者从一开始就把“生成”和“推送”解耦（0.4 课第 9 节的消息缓冲）。

**14.** 丢掉。thinking block 的签名只有 Anthropic 能验证，Gemini 不认识这种块，发过去要么被拒、要么被当成垃圾文本。规范化字段里的文本、工具调用、工具结果正常转换。损失是：新模型看不到之前的推理过程，只能看到对话内容和工具记录。对依赖前文推理的长任务，换模型前可以让原模型先写一段文字总结，作为普通文本留在历史里。

## 练习参考答案

### 练习 2

| 改动 | 失败的测试 | 原因 |
|---|---|---|
| 1. `Store` 加 `omitempty` | `TestOpenAIEncode` | `"store":false` 被省略 |
| 2. 删掉 OpenAI 原样回放分支 | `TestRunToolsAllProviders/openai` | 第二个请求里没有 `encrypted_content`（reasoning 被当成“别家的”丢了）。线上表现：不报错，只是变笨变贵 |
| 3. 输入不加缓存 | `TestAnthropicGenerateUsageIncludesCache`（`TestAnthropicStreamMatchesGenerate` 不会失败） | 30 ≠ 1030。流式和非流式用的是同一个 `normalize`，两边一起错，比较型测试发现不了，所以需要独立断言 |
| 4. usage 累加 | `TestAnthropicStreamMatchesGenerate` | 流式的 output 变成 1 + 60 = 61，和非流式的 60 不一致 |
| 5. 断流直接 return | 没有测试失败 | 没有错误也没有 Done，`Collect` 兜底返回了 `ErrIncompleteStream`，所以 `TestAnthropicStreamTruncatedIsError` **仍然通过**。但直接 `range` 流的调用方会以为正常结束。补一个测试：不用 `Collect`，自己 `range`，断言要么收到 `EventDone`、要么收到非 nil 的 err |
| 6. 删掉 `"arguments"` | `TestGeminiStream` | 第二段参数被丢弃，拼出来是 `{"city": `，解析失败 |
| 7. 忽略 `yield` 返回值 | `TestStreamBreakClosesConnection` | panic：range function continued iteration after function for loop body returned false |
| 8. 工具出错中断循环 | 没有测试失败 | 现有测试里的工具都不出错。补一个：工具第一次返回 error、模型第二轮正常回答，断言 `RunTools` 成功且历史里有一条 `IsError` 的结果 |

第 3、5、8 条是本练习想让你注意的：**测试通过不等于行为正确**。比较型测试抓不到两条路径一起错，兜底逻辑会掩盖下层的 bug。

### 练习 3：`retry.go`

```go
package llm

import (
	"context"
	"errors"
	"iter"
	"math/rand/v2"
	"time"
)

// Retry 给任意 Provider 加上重试：指数退避 + 全抖动，优先听服务端的 Retry-After。
// 流式只在“还没有推出任何事件”时重试：推出去的东西收不回来。
type Retry struct {
	Provider
	MaxAttempts int                                              // 总尝试次数，含第一次
	Base        time.Duration                                    // 第一次退避的上限
	Max         time.Duration                                    // 单次退避的上限
	Sleep       func(ctx context.Context, d time.Duration) error // 测试时替换掉
}

func (r *Retry) attempts() int {
	if r.MaxAttempts < 1 {
		return 1
	}
	return r.MaxAttempts
}

func (r *Retry) delay(attempt int, err error) time.Duration {
	var ae *APIError
	if errors.As(err, &ae) && ae.RetryAfter > 0 {
		return ae.RetryAfter // 服务端说了等多久，就等多久
	}
	d := r.Base << attempt // Base, 2Base, 4Base ...
	if d <= 0 || d > r.Max {
		d = r.Max
	}
	if d <= 0 {
		return 0
	}
	return rand.N(d) // 全抖动：[0, d) 均匀分布，避免一群客户端同时重试
}

func (r *Retry) wait(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *Retry) Generate(ctx context.Context, req *Request) (*Response, error) {
	var last error
	for attempt := 0; attempt < r.attempts(); attempt++ {
		resp, err := r.Provider.Generate(ctx, req)
		if err == nil || !IsRetryable(err) {
			return resp, err
		}
		last = err
		if attempt == r.attempts()-1 {
			break
		}
		if err := r.wait(ctx, r.delay(attempt, err)); err != nil {
			return nil, err
		}
	}
	return nil, last
}

func (r *Retry) Stream(ctx context.Context, req *Request) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		for attempt := 0; ; attempt++ {
			emitted := false
			var failed error
			for ev, err := range r.Provider.Stream(ctx, req) {
				if err != nil {
					failed = err
					break
				}
				emitted = true
				if !yield(ev, nil) {
					return
				}
			}
			if failed == nil {
				return
			}
			if emitted || !IsRetryable(failed) || attempt == r.attempts()-1 {
				yield(Event{}, failed)
				return
			}
			if err := r.wait(ctx, r.delay(attempt, failed)); err != nil {
				yield(Event{}, err)
				return
			}
		}
	}
}
```

**逐行解读**

- **嵌入 `Provider`**：`Retry` 自动获得 `Name()`，又覆盖了 `Generate` 和 `Stream`，所以它本身就是一个 `Provider`，可以套在任何实现外面，也可以再被别的包装器套住（第 08 模块的熔断、第 11 模块的模型回退都是这个形状）。
- **`delay`**：先看 `Retry-After`；没有就用指数退避加全抖动。`r.Base << attempt` 在 attempt 很大时会溢出成负数，所以要检查 `d <= 0`。`rand.N` 来自 `math/rand/v2`，参数必须大于 0。
- **为什么要抖动**：限流时一大批客户端同时失败，如果都在 1 秒、2 秒、4 秒整点重试，会再次同时打爆服务。在区间里随机分散开，峰值就削平了。
- **`Sleep` 可注入**：测试里替换成“记录时长、立刻返回”，测试不用真的等，还能断言等了多久。
- **`Stream` 的 `emitted`**：推出过任何事件之后就不再重试。注意内层 `range` 里 `break` 之后，内层迭代器会收到 `yield` 返回 `false`，关闭那次的 HTTP 连接，资源不泄漏。

测试：

```go
package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetry(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			w.WriteHeader(529)
			io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"x"}}`)
		case 2:
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(429)
			io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"x"}}`)
		default:
			b, _ := os.ReadFile("testdata/anthropic_tool.json")
			w.Write(b)
		}
	}))
	defer srv.Close()
	var waits []time.Duration
	r := &Retry{Provider: &Anthropic{BaseURL: srv.URL}, MaxAttempts: 4, Base: time.Second, Max: 10 * time.Second,
		Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }}
	resp, err := r.Generate(context.Background(), &Request{Model: "m"})
	if err != nil || resp.Stop != StopToolUse {
		t.Fatal(err)
	}
	if len(waits) != 2 || waits[0] >= time.Second || waits[1] != 3*time.Second {
		t.Fatalf("waits %v", waits)
	}
}

func TestRetryStreamOnlyBeforeFirstEvent(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := n.Add(1)
		if k == 1 {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := os.ReadFile("testdata/anthropic_stream_truncated.sse")
		w.Write(b)
	}))
	defer srv.Close()
	r := &Retry{Provider: &Anthropic{BaseURL: srv.URL}, MaxAttempts: 5, Base: time.Millisecond, Max: time.Millisecond}
	_, err := Collect(r.Stream(context.Background(), &Request{Model: "m"}), nil)
	if !errors.Is(err, ErrIncompleteStream) {
		t.Fatalf("got %v", err)
	}
	if n.Load() != 2 {
		t.Fatalf("attempts %d: must not retry after events were emitted", n.Load())
	}
}
```

---

## 完成标准

- [ ] `make test` 20 个测试全部通过，能说出每个测试在防什么问题；
- [ ] 练习 1：画出了三家“工具调用”的流式状态变化；有 Key 的话用 `cmd/chat` 跑通了一次普通对话和一次工具调用，记录了用量；
- [ ] 练习 2：八处改动都做过，尤其能解释第 2、3、5 条为什么“测试发现不了”或“线上不报错”，并给第 5、8 条补上了测试；
- [ ] 练习 3：`retry.go` 和两个测试通过 `go test -race`；
- [ ] 能不看课件写出三家“带工具调用的一轮完整往返”的请求和响应 JSON；
- [ ] 能说出三家用量口径的两处差异和换算依据；
- [ ] 自测题 14 道都能讲清楚，尤其是第 3、5、7、12、13 题。

做到以上几条，1.2 就可以勾选。下一课 **1.3 结构化输出**：JSON Schema、strict 模式、constrained decoding 的原理（回到 1.1 课的采样：在每一步把不合法的 token 的 logit 设成 -∞），以及三家结构化输出的差异。
