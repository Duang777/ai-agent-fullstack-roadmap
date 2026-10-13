# 1.5 Prompt Caching：前缀匹配、三家写法与缓存断点

> 第 1 部分 · 第 5 课 ｜ 预计 5–6 小时 ｜ 前置：1.1 KV Cache、1.2 主流模型 API（用量口径）、1.4 推理模型（推理配置与缓存）
>
> 配套练习：`projects/10-prompt-cache/`（纯 Go 标准库，17 个测试）。用累计前缀哈希模拟一台机器上的缓存索引，亲手验证“断点放错位置永远不命中”“回看窗口只有 20 块”“空闲超过 TTL 就重新付写入费”；再把三家的读写倍率统一成一个成本模型，算出 30 轮 Agent 会话里不同断点策略差多少钱。

## 读前说明

一个 Agent 每调一次模型，都要把工具定义、系统提示词、整段历史重新发一遍。第 30 轮的时候，你发出去的 4 万个 token 里，可能 3.9 万个和上一轮一字不差。Prompt Caching 做的事情很朴素：**这些和上次一样的开头，别再算一遍了**。

做对了，输入成本能降到原来的两成以下，首 token 延迟也明显变短；做错了，你不但省不到钱，还会每一轮多付 25% 的“写缓存”费用，而且 API 不会报任何错。这一课把“做对”拆成三件事：

1. **原理**：缓存的到底是什么，为什么只能匹配前缀，为什么改一个字后面全部作废；
2. **写法**：Anthropic 的 `cache_control`、OpenAI 的 `prompt_cache_options` 与 `prompt_cache_breakpoint`、Gemini 的隐式缓存与 `cachedContents`，各自的计费、TTL、最小长度；
3. **断点放哪**：按变化频率分层，避开时间戳、回看窗口、模式切换这些静默失效的坑，并且会诊断“为什么没命中”。

先看几个你会遇到的问题：

- 明明每次请求的系统提示词都一样，为什么 `cache_read_input_tokens` 永远是 0？
- 加了缓存以后账单反而涨了 25%，哪里出了问题？
- 一轮里并行调了 15 个工具、塞了 10 段检索结果，下一轮为什么突然完全不命中？
- 系统提示词只有 800 token，要不要为了缓存把它“凑”到 1024？
- 用户平均 10 分钟回一次消息，5 分钟 TTL 和 1 小时 TTL 选哪个？
- 想在对话中途换一下 `reasoning.effort` 或者加一个工具，怎样才能不把前面的缓存全部打掉？

本课结束时你能：

1. 讲清 Prompt Caching 缓存的是 KV 状态、按累计前缀哈希匹配、命中会续期；
2. 写出三家的缓存写法，说出各自的最小长度、TTL、读写倍率和用量字段；
3. 把三家的用量换算成同一个“普通输入当量”，算出盈亏平衡点和 TTL 的取舍；
4. 按“变化频率”放断点，知道哪些操作会让缓存失效，以及 Agent 场景下怎么做到只追加；
5. 用分叉诊断和静态检查找出没命中的原因。

**关于时效**：本课按 2026 年 10 月三家的官方文档核对。这一年缓存规则变化很大：OpenAI 从 GPT-5.6 起加了显式断点和写入费，Anthropic 加了自动缓存和 `max_tokens: 0` 预热，Gemini 的 Interactions API 只支持隐式缓存。倍率、最小长度和 TTL 都按模型区分，一定以定价页和模型页为准。练习里的规则是摘录，只为理解机制。

---

# 第一部分：缓存的到底是什么

## 1. 回顾：KV Cache

1.1 课讲过自回归生成：模型每生成一个 token，都要对前面所有 token 做注意力。为了不每步都重算，推理引擎会把前面每个 token 在每一层的 Key 和 Value 存下来，这就是 KV Cache。

在**一次请求内部**，KV Cache 一直都在用。Prompt Caching 把它延伸到了**请求之间**：

```text
请求 1：[工具][系统提示][用户问题 A]  → 算完整个前缀的 KV，存起来
请求 2：[工具][系统提示][用户问题 B]  → [工具][系统提示] 的 KV 直接拿来用，只算 [用户问题 B]
```

两个要点：

- **缓存的是 KV 张量，不是文本，也不是回答**。命中缓存不会让输出变得一样，模型照样重新生成（OpenAI 文档明确说了这一点）；
- **省掉的是“预填充”阶段**。长提示词的大部分延迟花在预填充上，所以命中缓存既省钱又降低首 token 延迟。输出 token 的生成速度和价格不受影响。

## 2. 为什么只能匹配前缀

第 i 个 token 的 KV 取决于它**前面所有的 token**（因果注意力）。所以：

- 两段提示词前 8000 个 token 完全相同，这 8000 个 token 的 KV 就完全相同，可以复用；
- 第 100 个 token 变了，第 100 个之后每个 token 的 KV 都变了，哪怕后面的文本一字不差。

这就是 Prompt Caching 的铁律：**只缓存前缀，改动之后的所有内容作废**。

实现上，服务端不会逐 token 比较，而是对“到某个位置为止的全部内容”算一个累计哈希，用它做缓存键。练习里的 `prefixes` 就是这样做的：

```go
// 每个位置的哈希 = H(上一个位置的哈希 + 本块内容)
for i, b := range blocks {
    h := sha256.New()
    h.Write(prev[:])
    h.Write([]byte(b.Kind))
    h.Write([]byte{0})
    h.Write([]byte(b.Text))
    copy(prev[:], h.Sum(nil))
    hs[i] = prev
}
```

累计哈希有一个直接后果：**改动第 i 块，第 i 块及之后所有位置的哈希都变**。所以 Anthropic 文档说缓存有层级 `tools → system → messages`：改工具定义，三层全部失效；改系统提示，system 和 messages 失效；只改最后一条消息，前面都还在。

还有一个容易忽略的点：被哈希的是**渲染后的完整提示词**，不只是你传的文本。三家都会把一些请求参数渲染进提示词里（比如推理配置、工具的使用说明、结构化输出的 schema），这些参数一变，前缀也就变了。第 14 节会列出完整清单。

## 3. 缓存存在哪里

缓存不是全局共享的：

- **按组织隔离**，相同的提示词在不同组织之间永远不会共享。Anthropic 在 Claude API 上还按 workspace 隔离（Bedrock 和 Google Cloud 只按组织）；
- **存在具体的机器上**。OpenAI 文档写得很直白：请求必须路由到持有匹配条目的那台机器才能命中。路由依据之一是开头若干 token 的哈希，所以“前缀相同”的请求天然更容易落在同一台机器上；
- **有寿命**。条目有 TTL，过期就没了；每次命中会把寿命刷新，而且刷新不另收费。

OpenAI 的 `prompt_cache_key` 就和路由有关：GPT-5.6 之前的模型，靠一个稳定的 key 把相关请求路由到一起，文档建议每个 key 每分钟大约 15 个请求，流量更大就按确定性规则分到多个 key 上。GPT-5.6 及以后路由自动处理，key 只用来做**按客户分开计账**，同时防止“缓存命中探测”：攻击者提交候选提示词，观察有没有命中，来推断别的用户发过什么。多租户应用应该按用户或客户给不同的 key。

---

# 第二部分：三家的写法

## 4. Anthropic：`cache_control`

Anthropic 的缓存需要你**主动开**，有两种方式。

**自动缓存**：请求顶层加一个 `cache_control`，系统把断点放在最后一个可缓存块上，并且随对话增长自动往后移。

```json
{
  "model": "claude-opus-5-5",
  "max_tokens": 1024,
  "cache_control": { "type": "ephemeral" },
  "system": "你是一个代码助手……",
  "messages": [{ "role": "user", "content": "……" }]
}
```

**显式断点**：把 `cache_control` 放在某个内容块上，精确控制缓存到哪里为止。

```json
{
  "tools": [
    { "name": "search", "...": "..." },
    { "name": "read_file", "...": "...", "cache_control": { "type": "ephemeral" } }
  ],
  "system": [
    { "type": "text", "text": "长规则与示例……", "cache_control": { "type": "ephemeral", "ttl": "1h" } }
  ],
  "messages": [ "..." ]
}
```

规则要点（2026-10 文档）：

| 项目 | 规则 |
|---|---|
| 断点数量 | 最多 4 个；自动缓存占其中 1 个。4 个显式断点再加自动缓存会 400 |
| 写入位置 | **只在断点处写一个条目**，前面的位置不会写 |
| 读取 | 从断点往回最多查 20 个位置（含断点本身），找“以前的请求写过的条目”；连续的 `tool_use` 或 `tool_result` 算一个位置 |
| 最小长度 | 按模型：Opus 5.5、Sonnet 5.5、Haiku 5.5 等是 512；Opus 4.7 是 2048；Haiku 4.5 是 4096。不够就**静默不缓存**，不报错 |
| TTL | 默认 5 分钟，可选 `"ttl": "1h"`；从请求**开始**计时，生成时间也算在内；命中免费续期 |
| 混用 TTL | 1 小时的断点必须排在 5 分钟断点之前 |
| 并发 | 条目在第一个响应开始返回后才可用；并发请求想命中，要先等第一个开始响应 |
| 预热 | `max_tokens: 0` 只写缓存不生成，不收输出费；断点要放在共享前缀末尾，不要放在占位的用户消息上 |

用量字段有三个，**加起来才是总输入**：

```text
总输入 = cache_read_input_tokens + cache_creation_input_tokens + input_tokens
```

`input_tokens` 只是**最后一个断点之后**的部分。只看 `input_tokens` 估算上下文长度，会严重低估。另外 `cache_creation` 子对象会把写入拆成 `ephemeral_5m_input_tokens` 和 `ephemeral_1h_input_tokens`。

Anthropic 的计费倍率：5 分钟写入 1.25×、1 小时写入 2×、读取 0.1×；Opus 5.5 和 Sonnet 5.5 的读取是 0.05×，Fable 5.1、Mythos 5.1 是 0.025×。倍率会和 Batch 折扣、长上下文定价叠加。

## 5. OpenAI：`prompt_cache_options` 与 `prompt_cache_breakpoint`

OpenAI 的缓存**默认开启**。GPT-5.6 是一个分水岭：

| | GPT-5.6 及以后 | 更早的模型 |
|---|---|---|
| 隐式断点 | 最新一个“可作断点的消息”结尾 | 固定间隔（GPT-5.5 每 2048 token） |
| 显式断点 | 支持 | 不支持 |
| 最小长度 | 1024 个可见输入 token | 随请求设置变化 |
| 写入费 | 1.25× | 无 |
| 读取 | 0.1×（GPT-6.1 Sol 为 0.05×） | 模型自己的缓存价 |
| 寿命 | `prompt_cache_options.ttl`，只支持 `"30m"` | `prompt_cache_retention`：`in_memory` 或 `24h` |

**implicit 模式**（默认）：在最新一个可作断点的消息末尾放隐式断点。可作断点的是：用户消息、连续工具结果中的最后一个、开头连续 developer 消息中的最后一个。查找时还会看更早的最多 20 个消息结尾。

**explicit 模式**：只认你标的断点。

```json
{
  "model": "gpt-6.1-sol",
  "prompt_cache_options": { "mode": "explicit" },
  "input": [
    {
      "role": "developer",
      "content": [{
        "type": "input_text",
        "text": "稳定的规则和参考资料……",
        "prompt_cache_breakpoint": { "mode": "explicit" }
      }]
    },
    { "role": "developer", "content": "当前时间、用户画像等动态内容……" },
    { "role": "user", "content": "用户这次的问题……" }
  ]
}
```

explicit 模式的好处是：**最后一个断点之后的内容按普通价算，不收写入费**。像“用同一份评分标准评几千条对话”这种场景，被评的对话不会再被复用，没必要花 1.25× 写进缓存。

几个注意点：

- 顶层 `instructions` 里不能放显式断点，要把可复用的指令放进 developer 消息的 `input_text` 块里；
- 每个请求最多写 4 个条目，implicit 的隐式断点占 1 个；
- 查找边界是：前 2 个和最后 50 个显式断点；implicit 模式再加上隐式断点、更早的最多 20 个消息结尾、开头 developer 块的结尾；
- `prompt_cache_options.prewarm: true` 只预热不生成，按写入价计费。

用量口径和 Anthropic **不同**：`input_tokens` 是总数，`cached_tokens` 和 `cache_write_tokens` 是它的两个组成部分。

```text
普通部分 = input_tokens - cached_tokens - cache_write_tokens
```

还要注意：OpenAI 的缓存 token **照样计入每分钟 token 限额**；Anthropic 文档则说缓存命中不扣速率限额（这是 1 小时 TTL 的卖点之一）。

## 6. Gemini：隐式缓存与 `cachedContents`

Gemini 有两套机制：

- **隐式缓存**：Gemini 2.5 及以后默认开启，命中了就自动打折，但**不保证命中**。最小长度：Gemini 3.8 Flash、3.6 Flash、3.1 Pro Preview 是 4096，2.5 系列是 2048。命中量在用量的 `total_cached_tokens`（Interactions）或 `usage_metadata`（generateContent）里；
- **显式缓存**：先 `POST /v1beta/cachedContents` 创建一个缓存对象（系统指令 + 文件等），之后的 `generateContent` 用 `cachedContent` 引用它。TTL 默认 1 小时，可以随时改 `ttl` 或 `expire_time`，也可以手动删除。

**Interactions API 只支持隐式缓存**，要用显式缓存得回到 legacy 的 generateContent。

显式缓存的计费结构和另外两家不一样：除了读缓存的折扣价，还要按 **token 数 × 存放时长** 收存储费，TTL 越长越贵，没有上下限。所以它适合“一大段内容在一段时间内被高频复用”的场景，比如对同一段视频反复提问。练习里的 `GeminiExplicit` 把创建、存储、读取三部分都做成了参数：

```go
func GeminiExplicit(cached, requests int, hours, readMult, storagePerTokenHour float64) float64 {
    return float64(cached) + float64(cached)*hours*storagePerTokenHour + float64(cached*requests)*readMult
}
```

单价请查定价页，测试里用的是示例值，只用来演示“用一次更贵，用十次更便宜”。

---

# 第三部分：算账

## 7. 把三家的口径统一起来

成本模型统一成“普通输入当量”：普通输入记 1，读缓存记 r，写缓存记 w。

```go
type Rates struct {
    Read  float64
    Write float64
}

func (r Rates) Units(u Usage) float64 {
    return float64(u.Input) + float64(u.Read)*r.Read + float64(u.Write)*r.Write
}
```

`Usage` 用 Anthropic 的三段式（Read、Write、Input 加起来是总数）。OpenAI 的字段要先换算：

| 统一字段 | Anthropic | OpenAI（GPT-5.6+） |
|---|---|---|
| Read | `cache_read_input_tokens` | `cached_tokens` |
| Write | `cache_creation_input_tokens` | `cache_write_tokens` |
| Input | `input_tokens` | `input_tokens - cached_tokens - cache_write_tokens` |

这和 1.4 课处理推理 token 是同一个问题：**同一个字段名，在不同厂商那里含义不同**。直接把 Anthropic 的 `input_tokens` 当总输入，或者把 OpenAI 的 `input_tokens` 当普通输入，成本都会算错。

## 8. 写一次读 N 次

按 r = 0.1、w = 1.25 算（OpenAI 文档给了同样的例子，练习的 `TestUnitsAndBreakEven` 会验证）：

| 用法 | 成本（普通输入的倍数） |
|---|---|
| 处理两次，不缓存 | 2× |
| 写一次 + 读一次 | 1.35× |
| 处理十次，不缓存 | 10× |
| 写一次 + 读九次 | 2.15× |
| **写十次、一次都没读到** | **12.5×** |

最后一行是这一课最重要的数字：**缓存没命中时，你付的不是 1×，而是 1.25×**。所有“加了缓存账单反而涨了”的问题，根源都是写了却没读到。

## 9. 最小长度陷阱

前缀不够最小长度就不缓存。那么一个 800 token 的系统提示词，值不值得补到 1024？

OpenAI 文档给了一个精确的公式。设最小长度 M、原前缀长度 L（L < M）、总请求数 N：

- 不补：每次都按普通价付 L，总共 N × L；
- 补到 M：写一次 + 读 N-1 次，总共 M × [w + (N-1) × r]。

盈亏平衡点：

```text
L_break-even = M × (r + (w - r) / N)
```

```go
func BreakEven(m int, r Rates, n int) float64 {
    return float64(m) * (r.Read + (r.Write-r.Read)/float64(n))
}
```

M = 1024、r = 0.1、w = 1.25 时，L_break-even = 102.4 + 1177.6 / N：

- N = 10，只要原前缀超过 220.16 token，补到 1024 就更便宜；
- N 越大越逼近 102.4。一个 102 token 以下的前缀，无论复用多少次，补长都不划算。

注意“补长”要补有用的东西：更多高质量示例、更完整的规则。为了凑数塞废话，会改变模型行为，要跑评测确认。

## 10. 5 分钟还是 1 小时

Anthropic 的 5 分钟 TTL 写入是 1.25×，1 小时是 2×。命中会免费续期，所以关键是**相邻两次请求的间隔**。

练习的 `TestTTLChoice`：一段 20000 token 的前缀，用户回复间隔依次是 3、12、7、25、4、18、9、40、2、15 分钟（共 11 次请求）：

| 方案 | 成本（普通输入当量） |
|---|---|
| 不缓存 | 220000 |
| 5 分钟 TTL | 206000 |
| 1 小时 TTL | 60000 |

5 分钟 TTL 只命中了 3 次（间隔 3、4、2 分钟），其余 7 次都过期重写，几乎没省钱；1 小时 TTL 写一次之后全部命中。反过来，如果间隔都在 1 到 4 分钟，5 分钟 TTL 每次命中都在续期，永远不会过期，2× 的写入价就是白花。

经验规则（Anthropic 文档也是这么写的）：

- 请求间隔稳定小于 5 分钟：用 5 分钟；
- 间隔在 5 分钟到 1 小时之间（等用户回复、后台子 Agent 跑得久）：用 1 小时；
- Batch 任务：先用一个 1 小时缓存的请求写入共享前缀，等它完成再提交其余请求，因为 batch 通常要跑 5 分钟到 1 小时。

另外别忘了 TTL 从**请求开始**计时：一个响应流了 4 分钟，下一个请求要在它结束后 1 分钟内发出，才能用上 5 分钟的缓存。

OpenAI GPT-5.6+ 只有 30 分钟一种 TTL，不用选。

---

# 第四部分：断点放在哪

## 11. 按变化频率分层

一个典型 Agent 请求的内容，变化频率差别很大：

| 内容 | 多久变一次 | 放在哪 |
|---|---|---|
| 工具定义 | 发版才变 | 最前面，断点 1 |
| 系统规则、示例 | 几天一变 | 断点 2 |
| 知识库、检索到的文档 | 每个会话或每天 | 断点 3 |
| 对话历史 | 每轮追加 | 断点 4，跟着最后一块走 |
| 当前时间、请求 ID、本次问题 | 每次都变 | 最后，不放断点 |

原则就一句话：**断点放在“跨请求保持不变的最后一块”上**。越稳定的越靠前，变化的永远放在最后。

## 12. 最常见的错误：断点放在变化的块上

假设 system 很长（第 1 到 5 块），第 6 块是“当前时间 + 用户问题”，你把断点放在第 6 块：

- 请求 1：在第 6 块写入，哈希里包含了时间；
- 请求 2：时间变了，第 6 块的哈希不同。往回查第 5、4、3、2、1 块，**这些位置从来没有写过条目**，所以不命中，又写一次。

很多人以为“往回查”会自动找到前面稳定的部分并缓存它。不会。**回看只能找到以前的请求在它们自己的断点处写过的条目**。正确做法是把断点移到第 5 块。

练习的 `TestBreakpointOnVolatileBlockNeverHits` 演示了这个过程：三次请求，每次都写 8050 token，读 0。

自动缓存也会掉进同一个坑：它把断点放在最后一块，而最后一块正是每次都变的那一块。这种结构要改用显式断点。OpenAI 的 implicit 模式也一样：“静态 developer 消息 + 动态用户消息”，隐式断点落在用户消息末尾，换个问题就不命中，要在静态内容后加一个显式断点（练习的 `TestOpenAIImplicitVsExplicit`）。

还有一个变体：**把时间戳写进系统提示词开头**。“你是一个助手，现在是 2026-10-13 20:41:07”，这一行让整个前缀每次都不同。练习的 30 轮 Agent 会话里，这样写的成本是不缓存的 125%，命中率 0%。时间要写，就放到最后一条消息里。

## 13. 20 块回看窗口

Anthropic 每个断点最多往回查 20 个位置。对话每轮追加不到 20 块时，把断点放在最后一块就够了：下一轮往回查，能找到上一轮在它末尾写的条目。

但如果一轮追加了很多块（比如一次塞进 25 段检索结果），新断点往回查 20 个位置，够不着上一轮的条目，就完全不命中。练习的 `TestLookbackWindow`：

- 第一轮 11 块，断点在第 10 块；
- 第二轮 36 块，只在最后一块放断点：读 0；
- 第二轮在第 10 块（上一轮末尾）也放一个断点：命中 2400 token。

两点说明：

- 连续的 `tool_use` 块算一个位置，连续的 `tool_result` 块也算一个，所以一轮里并行调 15 个工具不会单独把窗口撑爆；
- 补断点要**提前**：回看只找已经写过的条目，你得在条目还在窗口内时就有一个断点写在那里。

## 14. 什么会让缓存失效

除了改内容，下面这些**参数**也会改变渲染后的前缀（Anthropic 文档的失效表 + OpenAI 文档的设置表）：

| 改了什么 | Anthropic | OpenAI |
|---|---|---|
| 工具定义（名称、描述、参数、顺序） | 全部失效 | 失效 |
| `tool_choice` | messages 失效 | 用 `allowed_tools` 代替删工具 |
| 增删图片 | messages 失效 | 图片属于被缓存的内容 |
| 推理配置、effort | messages 一定失效，部分模型连 tools、system 一起失效 | `reasoning.effort` 会改隐藏指令 |
| 结构化输出 schema | 渲染进提示词 | `text.format` 会改前缀 |
| 模型 | 不同模型不共享 | 不同模型不共享 |
| 压缩 / 截断历史 | 前缀从改动处变化 | `context_management` 会改前缀 |
| 网页搜索、引用开关、fast 模式 | system 及之后失效 | — |

所以 1.4 课说“推理配置会影响缓存”，原因就在这里：推理配置被渲染进了提示词。

## 15. Agent 场景：只追加，不修改

1.4 课讲推理块的时候说过“历史只追加不修改”，这里是同一个原则的第二个理由：**任何修改都会让修改点之后的缓存作废**。

具体做法：

- **工具列表保持稳定**。某一轮不想让模型调工具，用 `tool_choice: "none"`（OpenAI）或 `allowed_tools` 限定可调用的工具，而不是从 `tools` 里删掉；
- **中途加工具不改 `tools`**。OpenAI 用 tool search 的 `defer_loading` 或 `additional_tools` 输入项，发现的工具追加在上下文末尾；Anthropic 在支持的模型上用 `tool_addition` 块（`inline-tools-2026-09-15` beta），定义写在对话中间的系统消息里；
- **中途换推理强度不改顶层参数**。OpenAI GPT-6 用 `configuration_update` 输入项；Anthropic 支持的模型可以在 `messages` 里放一条 `role: "system"` 的消息改 effort；
- **中途加系统指令不改顶层 `system`**。Anthropic 在支持的模型上可以往 `messages` 追加 `{"role": "system"}` 消息，前缀不变；
- **不要“整理”历史**。删掉中间几条消息、把旧的工具结果截短、重新排序，都会从改动点开始全部重写；
- **压缩要算总账**。压缩会让第一次请求的缓存命中下降，但之后每轮的输入变少了，总成本可能仍然更低。别只看命中率。

练习的 `Diagnose` 能告诉你两次请求从哪一块开始分叉：只追加、内容被改写、删除或调换顺序、历史被截断。

## 16. Go 序列化的一个细节

Anthropic 的排错清单里有一条：工具调用的 JSON 键顺序要稳定，因为“有些语言（比如 Swift、Go）在 JSON 转换时会打乱键的顺序”。

对 Go 要说得准确一点：

- Go 的 **map 遍历顺序是随机的**，每次运行都可能不同；
- 但 **`encoding/json` 序列化 map 时会按键排序**，输出是稳定的；
- 真正的风险是**自己遍历 map 拼字符串**，或者用了不排序的第三方库。

练习的 `TestJSONKeyOrder` 对同一个 map 跑 200 次：`NaiveJSON`（手写遍历）出现了不同的顺序，`CanonicalJSON`（`encoding/json`）每次都一样。

还有两个类似的坑：工具结果里带了随机的请求 ID 或时间戳；同一份系统提示词由模板渲染，模板里有不稳定的空白或者 map 遍历。

---

# 第五部分：诊断与监控

## 17. 看哪些指标

- **token 命中率** = 读缓存的 token / 总输入 token，按用户、会话、天聚合；
- **写读比**：写入的 token 和后续读到的 token 之比。写得多读得少，说明断点放错了或者 TTL 太短；
- **每次请求的输入当量**：用第 7 节的公式算，这才是真正的成本；
- **首 token 延迟**：命中前后对比。

OpenAI 文档里有两个参考：单轮评审（固定评分标准 + 不同的被评对话）的一个部署命中率约 70%；多轮 Agent（只追加历史 + 每个工具结果后加显式断点）的一个部署超过 90%。实际上限取决于你的上下文结构。

两家都提供了官方诊断工具：Anthropic 的 cache diagnostics 和 OpenAI 的 Prompt Cache Diagnostics，可以比较相邻两次请求并指出分叉位置。练习的 `Diagnose` 和 `Lint` 是同样思路的本地版本，适合放在测试或 CI 里，在请求发出去之前就拦住问题。

## 18. 三种策略的实测对比

练习的 `TestAgentStrategies` 模拟一个 Agent：工具 2000 token、系统提示 6000 token，跑 30 轮，每轮追加用户问题 200、工具调用 150、工具结果 800、回答 300 token，相邻两轮间隔 1 分钟。按 Anthropic 5 分钟 TTL 的倍率算：

| 策略 | 输入当量 | 相对不缓存 | 命中率 |
|---|---|---|---|
| 不缓存 | 876750 | 100% | — |
| 自动缓存（断点跟着最后一块） | 145462 | 16.6% | 94.3% |
| 只在 system 末尾放断点 | 669950 | 76.4% | — |
| 自动缓存，但 system 里写了时间戳 | 1095938 | 125.0% | 0% |
| 自动缓存，但每轮间隔 6 分钟 | 1095938 | 125.0% | 0% |

读这张表：

- 只缓存系统提示只省了 24%，因为到后期大部分 token 在对话历史里。**多轮场景的大头是历史，不是系统提示**；
- 时间戳和 TTL 过期的结果一模一样：每轮都写、从不读，比不缓存还贵 25%；
- 每轮间隔 6 分钟的情况说明：等用户回复的场景用 5 分钟 TTL，等于每轮都在交 25% 的罚款，应该换 1 小时 TTL。

---

## 本课小结

1. Prompt Caching 复用的是前缀的 KV 状态，按累计前缀哈希匹配，改动之后的内容全部作废；被哈希的是渲染后的完整提示词，很多请求参数也在里面。
2. Anthropic 需要主动开（自动缓存或最多 4 个显式断点），每个断点只在自己的位置写条目，回看 20 个位置；OpenAI 默认开，GPT-5.6+ 有 implicit 和 explicit 两种模式，写入 1.25×、TTL 30 分钟；Gemini 隐式缓存不保证命中，显式缓存另收存储费，Interactions API 只支持隐式。
3. 三家用量字段含义不同，先统一成读、写、普通三段，再按倍率换算。
4. 没命中的写入比不缓存更贵（1.25× 而不是 1×）。最小长度有盈亏平衡公式，TTL 按请求间隔选。
5. 断点放在跨请求不变的最后一块上，按变化频率分层；时间戳、请求 ID 放到最后；历史只追加不修改，工具、推理配置、系统指令的中途变更用追加式的接口。
6. 监控命中率和写读比，用分叉诊断找出没命中的原因。

---

## 练习（今天做完）

### 练习 1：跑通并读懂实验输出

```bash
cd projects/10-prompt-cache
make test
```

对照第 10 节和第 18 节的表，在测试日志里找到这些数字，并回答：

1. `TestAgentStrategies` 里，把每轮间隔改成 4 分钟，自动缓存的成本会变成多少？为什么和 1 分钟时一样？
2. 把 `AgentSession` 每轮追加的块数增加到 22 块（比如每轮塞进 21 段检索到的文档文本块），自动缓存的命中率会怎样？为什么？（注意：真实 API 里连续的 `tool_result` 会合并成一个位置，所以这里用普通文本块；练习的模拟器不做合并。）

### 练习 2：自动放断点 `PlaceBreakpoints`

写一个函数：

```go
func PlaceBreakpoints(blocks []Block, prevEnd int) []int
```

规则：

1. 在最后一个 `tool` 块放断点；
2. 在最后一个**不含时间戳**的 `system` 块放断点；遇到第一个含时间戳的 `system` 块就停止，它之后的内容不算静态前缀；
3. 如果最后一块不含时间戳，在最后一块放断点；如果 `prevEnd`（上一次请求最后一块的下标）到最后一块之间隔了 20 块或以上，在 `prevEnd` 也放一个；
4. 最多 4 个，超出时丢掉最靠前的。

至少写两个测试：一个“工具 + 静态 system + 动态 system + 用户消息”，一个“一轮追加 30 块”。

### 练习 3：混合 TTL 计费

Anthropic 文档定义了混合 TTL 的三个位置：

- A：最高命中的 token 数（没有命中就是 0）；
- B：A 之后最后一个 1 小时断点的 token 数（没有就等于 A）；
- C：最后一个断点的 token 数。

计费：A 按读价，B - A 按 1 小时写入价，C - B 按 5 分钟写入价，C 之后按普通价。写：

```go
func MixedTTLUnits(a, b, c, rest int) (float64, error)
```

要求 A ≤ B ≤ C，否则返回错误。用“工具 + 系统提示 10000 token 用 1 小时、对话历史 2000 token 用 5 分钟、问题 50 token、没有命中”验证结果是 22550。

---

## 自测题

1. Prompt Caching 缓存的是什么？命中缓存会让两次回答变得一样吗？
2. 为什么只改了对话第 3 条消息里的一个标点，第 3 条之后的缓存全部作废？
3. Anthropic 的 `input_tokens` 和 OpenAI 的 `input_tokens` 有什么区别？
4. 一个请求把断点放在“当前时间 + 用户问题”这一块上，为什么永远不命中？Anthropic 的回看机制为什么救不了它？
5. 写一次读九次，按 r = 0.1、w = 1.25 是多少倍？写十次一次都没读到呢？
6. OpenAI GPT-5.6+ 的 explicit 模式比 implicit 模式好在哪？什么场景该用？
7. 请求 1 用 implicit 模式，请求 2 保留完整前缀但切到 explicit-only，为什么不命中？
8. 用户平均 15 分钟回复一次，该选 5 分钟还是 1 小时 TTL？为什么？
9. 某一轮不想让模型调工具，为什么不应该把 `tools` 删掉？
10. Go 的 `encoding/json` 会打乱 map 的键顺序吗？风险到底在哪里？

---

# 参考答案与详解

## 自测题答案

**1.** 缓存的是前缀每个 token 在每一层的 Key 和 Value 张量，不是文本，也不是回答。命中只省掉预填充计算，模型照样重新生成，回答不保证一样。

**2.** 第 i 个 token 的 KV 取决于它之前的全部 token，所以第 3 条之后每个 token 的 KV 都变了。实现上缓存键是累计前缀哈希，改动点之后所有位置的哈希都会变。

**3.** Anthropic 的 `input_tokens` 只是最后一个断点之后的部分，总输入要把读、写、普通三项加起来；OpenAI 的 `input_tokens` 本身就是总数，`cached_tokens` 和 `cache_write_tokens` 是它的组成部分，普通部分要减出来。

**4.** 断点块每次都变，所以断点处的哈希每次都不同，写入的条目永远不会被下一次读到。回看只能找到以前的请求**在它们自己的断点处**写过的条目，而断点前面那些稳定的位置从来没被写过。要把断点移到最后一个稳定的块上。

**5.** 写一次读九次：1.25 + 9 × 0.1 = 2.15 倍，不缓存是 10 倍。写十次都没读到：10 × 1.25 = 12.5 倍，比不缓存还贵 25%。

**6.** explicit 模式只在你标的断点处写缓存，最后一个断点之后的内容按普通价计费，不收 1.25× 的写入费。适合“稳定前缀 + 不会被复用的变化后缀”：比如用同一份评分标准评大量对话、同一份文档回答不同用户的问题。

**7.** explicit-only 模式只查自己请求里的显式断点。请求 1 的条目写在用户消息末尾（隐式断点），请求 2 在那个位置没有显式断点，所以查不到。要么在请求 2 的同一位置补一个显式断点，要么保持 implicit 模式。

**8.** 选 1 小时。5 分钟 TTL 在间隔 15 分钟时几乎每次都过期，每次都付 1.25× 写入又读不到；1 小时 TTL 写一次 2×，之后每次读 0.1× 并续期。第 10 节的实验里两者差了三倍多。

**9.** 工具定义在前缀最前面，删掉一个工具，tools、system、messages 三层缓存全部失效。用 `tool_choice: "none"` 或 `allowed_tools` 限定可调用范围，工具列表保持不变。

**10.** 不会。Go 的 map 遍历顺序是随机的，但 `encoding/json` 序列化 map 时按键排序，输出稳定。风险在于自己遍历 map 拼 JSON，或者用了不排序的库。

## 练习参考答案

### 练习 1

1. 4 分钟间隔仍然在 5 分钟 TTL 之内，而且每次命中都会续期，条目永远不会过期，所以成本和 1 分钟时完全一样（145462）。超过 5 分钟才会开始过期。
2. 命中率会掉到 0（实测 0%）。每轮追加 22 块，新的断点往回查 20 个位置，够不着上一轮末尾写的条目。修复方法是练习 2 的第 3 条规则：在上一轮末尾也放一个断点。

### 练习 2

```go
func PlaceBreakpoints(blocks []Block, prevEnd int) []int {
    var bps []int
    lastTool, lastStatic := -1, -1
    for i, b := range blocks {
        if b.Kind == "tool" {
            lastTool = i
        }
        if b.Kind == "system" && !volatile.MatchString(b.Text) {
            lastStatic = i
        }
        if b.Kind == "system" && volatile.MatchString(b.Text) {
            break // 动态 system 之后的内容每次都不同，静态前缀到此为止
        }
    }
    add := func(i int) {
        if i >= 0 && (len(bps) == 0 || bps[len(bps)-1] < i) {
            bps = append(bps, i)
        }
    }
    add(lastTool)
    add(lastStatic)
    last := len(blocks) - 1
    if last > lastStatic && !volatile.MatchString(blocks[last].Text) {
        if prevEnd > lastStatic && last-prevEnd >= 20 {
            add(prevEnd)
        }
        add(last)
    }
    for len(bps) > 4 { // 超出上限时丢掉最靠前的（工具最稳定，也最小）
        bps = bps[1:]
    }
    return bps
}
```

测试：

```go
func TestPlaceBreakpoints(t *testing.T) {
    b := []Block{
        {Kind: "tool", Text: "t"},
        {Kind: "system", Text: "rules"},
        {Kind: "system", Text: "今天 2026-10-13 20:41"},
        {Kind: "user", Text: "q"},
    }
    if got := PlaceBreakpoints(b, -1); !reflect.DeepEqual(got, []int{0, 1, 3}) {
        t.Fatal(got)
    }
    g := []Block{{Kind: "system", Text: "rules"}}
    for i := 0; i < 30; i++ {
        g = append(g, Block{Kind: "user", Text: fmt.Sprint(i)})
    }
    if got := PlaceBreakpoints(g, 5); !reflect.DeepEqual(got, []int{0, 5, 30}) {
        t.Fatal(got)
    }
}
```

讨论两点：

- 第一个例子里，最后一块（用户问题）也放了断点。如果用户问题每次都不同，这个断点只会写入、不会被读到，Anthropic 下每次多付 25%。更好的规则是区分“会被下一轮继承的历史”和“只用一次的后缀”：多轮对话里，本轮的问题会成为下一轮历史的一部分，值得写；单轮评审里则不值得。这正是 OpenAI explicit 模式要解决的问题；
- 时间戳检测用的是正则，只能发现常见格式。生产里更可靠的做法是从源头约束：模板里禁止在静态区插入动态变量。

### 练习 3

```go
func MixedTTLUnits(a, b, c, rest int) (float64, error) {
    if !(0 <= a && a <= b && b <= c) {
        return 0, errors.New("1h 断点必须在 5m 断点之前，且 A ≤ B ≤ C")
    }
    return float64(a)*0.1 + float64(b-a)*2.0 + float64(c-b)*1.25 + float64(rest), nil
}
```

验证：A = 0、B = 10000、C = 12000、rest = 50，结果是 0 + 20000 + 2500 + 50 = 22550。

为什么要求 1 小时的断点在前？因为前缀是累计的：如果 5 分钟的条目在前、1 小时的在后，5 分钟的过期以后，1 小时那个条目的前缀虽然还在，但它依赖的更短前缀已经不在了，计费和续期的语义就说不清楚。规定长 TTL 在前，保证“越靠前越持久”，和第 11 节“越稳定越靠前”是同一个道理。

---

## 完成标准

- `make test` 全部通过（17 个测试），能在日志里找到第 10 节和第 18 节表格里的数字；
- 练习 1 的两个问题能用“续期”和“回看窗口”解释清楚；
- 练习 2 的 `PlaceBreakpoints` 和至少两个测试写完并通过；
- 练习 3 的 `MixedTTLUnits` 通过 22550 的验证；
- 自测题 10 道能不看答案讲出来，尤其是第 4、5、7 题。
