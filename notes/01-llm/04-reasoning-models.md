# 1.4 推理模型：reasoning effort、thinking 与成本权衡

> 第 1 部分 · 第 4 课 ｜ 预计 5–6 小时 ｜ 前置：1.1 采样与自回归、1.2 主流模型 API（推理块回传、用量口径）
>
> 配套练习：`projects/09-reasoning/`（纯 Go 标准库，17 个测试）。把统一的推理强度映射到三家参数并处理不支持的取值；统一三家的推理 token 用量并算成本；用 HMAC 模拟思考块的签名，亲手验证“改一个字就 400”“改了前缀后面全失效”“换模型静默丢弃”；最后用一个能精确计算的模型回答“什么时候值得多想”。

## 读前说明

1.2 课我们已经和推理块打过交道：调完工具要把一段看不懂的加密字符串原样发回去，不发模型就“变笨变贵”。那一课只讲了“怎么传”，这一课讲清楚三件事：

1. **推理模型是什么**：那些看不见的 token 在做什么，为什么它们能让答案更好；
2. **怎么控制它**：三家的旋钮各叫什么、取值范围是什么、哪些组合会直接 400；
3. **值不值得**：推理 token 按输出价计费，什么任务该开、开多大、怎么用更少的钱拿到同样的正确率。

先看几个你会遇到的问题：

- 同样 200 个字的回答，为什么一次请求比另一次贵了 20 倍？
- `max_output_tokens` 设了 300，返回 `incomplete`，一个字都没有，钱却照扣了。为什么？
- 把 `reasoning.effort` 从 `low` 改成 `high`，为什么 Prompt Cache 全部失效了？
- 只是在客户端“整理了一下历史消息”，为什么 Anthropic 返回 400 “thinking blocks cannot be modified”？
- 把对话从一个模型切到另一个，为什么推理上下文悄悄没了？
- 什么任务开最高档也没用？什么任务开低档就够了？

本课结束时你能：

1. 解释推理 token 的作用和代价：看不见、按输出计费、占上下文、可能耗光输出上限；
2. 写出三家的推理控制参数：OpenAI 的 `reasoning.effort`，Anthropic 的 adaptive thinking + `output_config.effort`，Gemini 的 `thinking_level`，并处理不支持的取值；
3. 统一三家的推理 token 用量口径，算出一次调用的真实成本，诊断两种截断；
4. 讲清推理块的生命周期：签名绑定模型和前缀、保留策略、跨模型丢弃，并避开常见的客户端 bug；
5. 用“每个正确答案花多少 token”评估推理策略，知道逐级升级、多数投票各自的前提。

**关于时效**：本课按 2026 年 10 月三家的官方文档核对。推理相关的参数是这一年变化最快的部分（Anthropic 4.7 起不再接受 `budget_tokens`，OpenAI 加了 `max` 档和 `pro` 模式），模型名和默认值一定以模型页为准。练习里的能力表是摘录，生产代码应该从 Models API 读取。

---

# 第一部分：推理模型是什么

## 1. 从“一次过”到“先想再答”

1.1 课讲过，模型是自回归的：每一步只根据已经生成的内容预测下一个 token。普通模型回答问题时，从第一个字开始就是答案，中间没有草稿纸。一道题需要五步推导，它要么在“写答案”的过程中顺便推导，要么就直接猜。

推理模型的做法是：**先生成一段不给你看的推理，再生成答案**。推理里它会复述问题、尝试几种思路、检查中间结果、放弃走不通的路。Anthropic 文档的描述很直接：没有思考时，模型必须一次就做对，不能打草稿、不能检查、不能中途改主意；对证明、棘手的 bug、长任务来说，第一个思路往往不是最好的。

这种“用更多的生成来换更好的答案”叫**测试时计算**（test-time compute）：不改模型参数，只在推理阶段多花算力。它有两种基本形态：

| 形态 | 做法 | 本课对应 |
|---|---|---|
| 想得更久 | 单次生成更长的推理链 | 第二部分的 effort / thinking_level |
| 想得更多次 | 独立生成多个答案，投票或挑最好的 | 第 16、17 节的逐级升级和多数投票 |

推理模型为什么“会想”？公开资料里能确定的是：这类模型经过专门训练（通常包括在可验证任务上的强化学习），学会了产生有用的推理过程；具体训练细节各家没有公开，本课不展开。工程上你只需要记住两点：**推理是生成出来的 token，所以要花钱、要时间、要占上下文**；**推理的长度可以调，但不能精确控制**。

## 2. 那些看不见的 token

推理 token 有四个特点，后面所有的工程问题都从这里来：

| 特点 | 含义 | 后果 |
|---|---|---|
| **看不见** | 三家都不返回原始推理链：OpenAI 只给可选的摘要和 `encrypted_content`；Anthropic 的 thinking 块是摘要（由另一个模型生成）或干脆为空；Gemini 的 thought step 只有可选摘要和 `signature` | 你无法逐字检查模型怎么想的；摘要只能用来理解大致思路 |
| **按输出计费** | 计费按完整推理，不按你看到的摘要 | 摘要很短，账单很长 |
| **占上下文** | 当前轮的推理占上下文窗口；历史推理是否保留看模型 | 长对话里推理会挤占空间 |
| **算在输出上限里** | OpenAI 的 `max_output_tokens`、Anthropic 的 `max_tokens`、Gemini 的 `max_output_tokens` 都包含推理 | 上限设小了，推理阶段就能把它耗光 |

关于“看不见”：Anthropic 文档说得很清楚，没有任何 `display` 设置会返回原始思维链；而且在 Opus 5.5、Sonnet 5.5 等模型上，在回答正文里索要模型的内部推理，可能被以 `reasoning_extraction` 类型拒答。想看推理就读 thinking 块，别在提示词里要求“把思考过程写出来”（1.3 课第 7 节提过同样的坑）。

## 3. 推理和工具调用交织

Agent 场景里，推理不只发生在开头。模型调用工具、拿到结果后，还会再想一想再决定下一步。这叫**交错思考**（interleaved thinking）：

```
用户：北京今天要带伞吗？
助手：[思考：需要查天气] [调用 get_weather(北京)]
用户：[工具结果：小雨，18°C]
助手：[思考：有雨，建议带伞] [回答：要带伞，今天有小雨。]
```

几个要点：

- **整个工具循环是同一个 assistant 回合**。从模型的角度看，回合直到它给出最终回答才结束，中间的工具结果虽然以 user 消息的形式发回，但属于同一段连续的推理。练习的 `lastTurnStart` 就是按这个规则判断“最近一轮从哪开始”：只装着 `tool_result` 的 user 消息不算新的一轮；
- **中间的推理必须原样回传**，否则模型丢了“我为什么要调这个工具”的上下文（1.2 课第 6 节）；
- 三家现在都默认支持交错思考：OpenAI 的推理模型可以在工具调用之间思考；Anthropic 的 adaptive thinking 自动交错，不再需要 beta 头；Gemini 的 thought step 和 function call 在 `steps` 时间线上平级排列。
- **进度更新**：Anthropic 的部分新模型（Fable 5.1、Opus 5.5、Sonnet 5.5 等）会在工具调用之间写一段给用户看的进度说明，它也是一个带签名的 thinking 块。`display: "updates"`（beta）只返回这类块的文字，适合做“思考中……正在编辑 auth.py”这样的状态栏。

---

# 第二部分：怎么控制推理

## 4. OpenAI：reasoning.effort

```bash
curl https://api.openai.com/v1/responses \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-6-astra",
    "reasoning": {"effort": "low", "summary": "auto"},
    "max_output_tokens": 25000,
    "input": "写一个 bash 脚本，把 [1,2],[3,4],[5,6] 格式的矩阵转置后按同样格式输出。"
  }'
```

**逐行解读**

- **`reasoning.effort`**：取值和模型有关，可能包括 `none`、`minimal`、`low`、`medium`、`high`、`xhigh`、`max`。越低越快越省，越高想得越完整。模型在同一档内部也会自适应：简单任务少想，复杂任务多想。
- **不支持的取值直接 400**：GPT-6 Astra 不支持 `none`；GPT-6.1 Sol 不支持 `none` 和 `minimal`，默认 `medium`。默认值也因模型而异（GPT-5.5、GPT-5.6 默认 `medium`）。
- **`summary: "auto"`**：要推理摘要。不写就没有。部分模型使用摘要前需要先完成组织验证。
- **`max_output_tokens`**：包含推理 token。官方建议刚开始至少给推理和输出留 25000（第 9 节）。

官方对各档的建议（节选）：

| effort | 适合 |
|---|---|
| `none` | 延迟敏感、不需要推理的任务：语音、快速检索、分类 |
| `low` | 需要工具、规划、搜索但要省时省钱：数据分析、起草、客服 |
| `medium` | 质量和可靠性重要：Agent 编程、研究；多数场景的默认 |
| `high` | 难题、复杂调试、深度规划，质量比延迟重要 |
| `xhigh` | 深度研究、长时间运行的异步任务；**只在 Eval 证明有明显收益时用** |
| `max` | 最难的任务；在用 `xhigh` 的话，评估 `max` 是否更强 |

另外四个 2026 年的新东西：

- **`reasoning.mode: "pro"`**（GPT-5.6、GPT-6 系列）：和 effort 正交，做更多“模型工作”（官方说会聚合更多的模型工作来产出最终答案），token 用量和成本更高，按所选模型的标准单价计费；
- **`reasoning.context`**：`current_turn` 只让当前回合的推理可用；`all_turns` 把之前回合的推理也渲染进上下文（GPT-5.6 系列和 GPT-6.1 Sol 默认 `all_turns`，更早的模型默认 `current_turn`）。推理只能在同一模型家族内复用，跨家族会被自动忽略；
- **`configuration_update`**（GPT-6 系列）：在对话中途插入一个 `{"type": "configuration_update", "reasoning": {"effort": "high"}}` 条目来改推理强度，**请求级的 `reasoning.effort` 保持不变**，这样前缀不变，Prompt Cache 不会失效（第 10 节）；
- **`phase`**：GPT-5.5 / 5.4 在长工具流程里建议给 assistant 消息标 `commentary`（中间说明）或 `final_answer`，回放历史时要保留，否则开场白可能被当成最终答案。

## 5. Anthropic：adaptive thinking + effort

```bash
curl https://api.anthropic.com/v1/messages \
  -H "x-api-key: $ANTHROPIC_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-opus-5-5",
    "max_tokens": 64000,
    "thinking": {"type": "adaptive", "display": "summarized"},
    "output_config": {"effort": "high"},
    "messages": [{"role": "user", "content": "1071 和 462 的最大公约数是多少？"}]
  }'
```

**逐行解读**

- **两个旋钮**：`thinking` 管“要不要在 thinking 块里思考”，`effort` 管“整个回答花多少功夫”，包括思考的频率和深度、正文长度、工具调用次数。降成本先降 effort；需要硬上限用 `max_tokens`（effort 只是行为信号，不是 token 预算）。
- **`type: "adaptive"`**：模型自己决定想不想、想多深；低 effort 下简单问题可能完全不想。Opus 5.5、Sonnet 5.5、Haiku 5.5 等新模型默认就开着 adaptive，不用写；Opus 4.6 到 4.8、Sonnet 4.6 默认关闭，要显式写。
- **旧写法 `type: "enabled"` + `budget_tokens`**：固定思考预算（最少 1024）。4.6 上已弃用，**4.7 及之后的模型直接 400**。网上大量教程还是这个写法。
- **关掉思考**：因模型而异。Opus 5.5 关不掉（`disabled` 返回 400）；Sonnet 5.5 不接受 `disabled`，最低档是 `between_tools`（不做开头的思考，只在工具之间写进度更新），且只在 effort ≤ `high` 时可用；Haiku 5.5、Opus 5 在 effort ≤ `high` 时可以 `disabled`。
- **effort 取值**：`low`、`medium`、`high`、`xhigh`、`max`。大多数模型默认 `high`，**Opus 5.5 和 Haiku 5.5 默认 `medium`**；不是每个支持 `max` 的模型都支持 `xhigh`（比如 Sonnet 4.6）。
- **`display`**：`summarized` 返回摘要；`omitted` 返回空文本、只有签名（新模型的默认值，流式时首个正文 token 更快到，但**照样按完整思考计费**）；`updates`（beta）只返回进度更新的文字。
- **采样参数**：Opus 4.7 及之后的大部分新模型上，非默认的 `temperature`、`top_p`、`top_k` **不管开不开思考都返回 400**（1.1 课第 22 节说过的坑在这里更严格了）。

用量里的 `usage.output_tokens_details.thinking_tokens` 告诉你计费的输出 token 里有多少是思考；流式时这个拆分只在最后的 `message_delta` 事件里。

## 6. Gemini：thinking_level

```bash
curl -X POST "https://generativelanguage.googleapis.com/v1beta/interactions" \
  -H "x-goog-api-key: $GEMINI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.8-flash",
    "input": "前 50 个质数的和是多少？",
    "generation_config": {"thinking_level": "low", "thinking_summaries": "auto"}
  }'
```

**逐行解读**

- **`thinking_level`**：只有 `minimal`、`low`、`medium`、`high` 四档，每个模型支持的子集和默认值不同：

| 模型 | 默认 | 支持 |
|---|---|---|
| gemini-3.8-flash | medium | low、medium、high |
| gemini-3.6-flash | medium | minimal、low、medium、high |
| gemini-3.5-flash-lite | minimal | minimal、low、medium、high |
| gemini-3.1-pro-preview | high | low、medium、high |

- **`thinking_summaries: "auto"`**：要摘要。默认只返回最终输出。注意：简单请求、关闭摘要、或者图片类思考内容，都可能出现**只有 signature 没有 summary** 的 thought step，代码要能处理空摘要。
- **`max_output_tokens`**：包含思考 token，是基础设施强制的硬截断，**不会改变模型分配思考的方式**。撞上限时状态是 `incomplete`，输出被截断或为空，思考 token 照样计费。官方建议：想省钱省时间，**降 `thinking_level`，而不是把 `max_output_tokens` 设小**。
- **流式**：thought step 用 `thought_summary` delta 传摘要，最后一个 delta 是 `thought_signature`，然后 `step.stop`。
- **用量**：`total_thought_tokens` 单独给出，**`total_output_tokens` 不含思考**，计费是两者之和（和 OpenAI、Anthropic 的口径不同，第 8 节）。

## 7. 统一映射

三家的旋钮放在一起：

| | OpenAI | Anthropic | Gemini |
|---|---|---|---|
| 参数 | `reasoning.effort` | `thinking.type` + `output_config.effort` | `generation_config.thinking_level` |
| 档位 | none、minimal、low、medium、high、xhigh、max（按模型） | low、medium、high、xhigh、max（按模型） | minimal、low、medium、high（按模型） |
| 关掉推理 | `none`（部分模型不支持） | `disabled` 或 `between_tools`（部分模型关不掉，且只在 effort ≤ high） | 没有完全关闭的档位，最低 `minimal` |
| 摘要 | `reasoning.summary` | `thinking.display` | `thinking_summaries` |
| 硬上限 | `max_output_tokens`（含推理） | `max_tokens`（含思考） | `max_output_tokens`（含思考） |
| 中途改强度不破缓存 | `configuration_update` 条目 | 每条消息的 `output_config`（beta） | 未提供 |

统一客户端要做的事：**用一个统一的强度枚举，按模型能力表翻译成各家参数**；遇到不支持的取值，要么报错，要么降到最近的受支持档位并记录警告。

```go
type Effort int

const (
	EffortNone Effort = iota
	EffortMinimal
	EffortLow
	EffortMedium
	EffortHigh
	EffortXHigh
	EffortMax
)

func Resolve(model string, want Effort, strict bool) (Resolved, error) {
	c, ok := Models[model]
	if !ok {
		return Resolved{}, fmt.Errorf("unknown model %q", model)
	}
	var r Resolved

	// “关掉推理”在 Anthropic 是单独的开关，先处理。
	if c.Provider == Anthropic && want == EffortNone {
		if c.CanDisable {
			r.Effort = EffortNone
			r.Body = map[string]any{"thinking": map[string]any{"type": c.DisableAs}}
			return r, nil
		}
		if strict {
			return r, fmt.Errorf("%s: thinking cannot be disabled", model)
		}
		r.Warnings = append(r.Warnings, model+": thinking cannot be disabled, using lowest effort")
	}

	eff := want
	if !slices.Contains(c.Supported, want) {
		if strict {
			return r, fmt.Errorf("%s does not support effort %q", model, want)
		}
		eff = nearest(c.Supported, want)
		r.Warnings = append(r.Warnings, fmt.Sprintf("%s: effort %s not supported, using %s", model, want, eff))
	}
	// ……按厂商生成 Body……
}
```

**逐行解读**

- **枚举有顺序**：`EffortNone` 到 `EffortMax` 的整数值从小到大，`nearest` 靠这个算“距离”。三家的档位名大致对齐，但同名档位在不同模型上花的 token 不一样（Anthropic 文档专门提醒 Sonnet 5.5 的档位“重新校准过”，不能照搬 Sonnet 5 的设置），所以统一的只是“意图”，效果要靠 Eval 校准。
- **Anthropic 的“关掉”单独处理**：它不是 effort 的一个取值，而是 `thinking.type` 的另一种写法，而且每个模型写法不同（`disabled` 还是 `between_tools`）。练习的能力表用 `DisableAs` 字段记录。
- **strict 和 clamp 两种模式**：strict 适合开发期，尽早发现配置错误（等价于服务端的 400）；clamp 适合网关，一个请求打到不同模型时自动降级，但要把警告记进日志和 trace，否则你会以为自己在用 `xhigh`，其实跑的是 `high`。
- **距离相同取便宜的**：`claude-sonnet-4-6` 不支持 `xhigh`，`high` 和 `max` 一样近，选 `high`。原因是成本不对称：多想的那部分账单是实打实的，而少想的风险可以靠 Eval 和逐级升级补回来（第 16 节）。

`TestResolveStrictRejectsWhatTheServerRejects` 列了五个会被服务端拒绝的组合，`TestResolveClampPicksNearestCheaper` 验证降级结果。

---

# 第三部分：成本

## 8. 推理 token 按输出价计费

```go
func TestReasoningIsBilledAsOutput(t *testing.T) {
	p := Price{Input: 2, CacheRead: 0.2, Output: 10} // 假价格，每百万 token
	short := FromOpenAI(1000, 0, 200, 0)
	long := FromOpenAI(1000, 0, 8200, 8000) // 同样 200 个可见 token，多了 8000 推理
	// long.Cost(p) / short.Cost(p) == 21
}
```

用户看到的回答一模一样，成本差 21 倍。这是推理模型最反直觉的地方：**成本由你看不见的部分决定**。输出单价通常是输入单价的数倍（练习里假设 5 倍），而推理 token 全部按输出算。

这也意味着：

- 估算成本不能用“回答多长”，要看 `reasoning_tokens` / `thinking_tokens` / `total_thought_tokens` 的分布，最好按任务类型统计 P50 和 P95；
- `display: "omitted"` 或不要摘要**只省延迟、不省钱**；
- 降成本的主要手段是**降 effort**，其次是缩短输入（缓存、压缩上下文，1.5 课和第 03 模块），最后才是让回答变短。

三家的用量口径不一样，练习统一成“Input 含缓存、Output 含推理”（和 1.2 课一致）：

```go
// OpenAI：input_tokens 已含 cached_tokens；output_tokens 已含 reasoning_tokens。
func FromOpenAI(input, cached, output, reasoning int) Usage {
	return Usage{Input: input, CacheRead: cached, Output: output, Reasoning: reasoning}
}

// Anthropic：input_tokens 不含缓存，要把 cache_read 和 cache_creation 加回来；
// output_tokens 已含思考，output_tokens_details.thinking_tokens 是其中的思考部分。
func FromAnthropic(input, cacheRead, cacheCreation, output, thinking int) Usage {
	return Usage{Input: input + cacheRead + cacheCreation, CacheRead: cacheRead, Output: output, Reasoning: thinking}
}

// Gemini：total_output_tokens 不含思考，total_thought_tokens 单独给，计费是两者之和。
func FromGemini(input, output, thought int) Usage {
	return Usage{Input: input, Output: output + thought, Reasoning: thought}
}
```

**逐行解读**

- **OpenAI**：官方示例 `output_tokens: 1186` 里 1024 是推理，可见只有 162。
- **Anthropic**：`output_tokens` 已经包含思考，`thinking_tokens` 是其中多少是思考。输入那边的坑在 1.2 课讲过：`input_tokens` 不含缓存。
- **Gemini**：方向相反，`total_output_tokens` **不含**思考。官方流式示例里输出 171、思考 297，计费的输出是 468。把 Gemini 的 `total_output_tokens` 直接当成“总输出”，成本会算少一大半。
- `TestUsageNormalization` 用的就是三家文档里的真实数字。

## 9. 两种截断

推理模型撞上输出上限时，有两种完全不同的情况：

```go
func Diagnose(truncated bool, u Usage) (Diagnosis, string) {
	switch {
	case !truncated:
		return DiagOK, ""
	case u.Visible() == 0:
		return DiagExhaustedThinking, "...raise the output limit or lower the effort"
	default:
		return DiagTruncatedAnswer, "answer cut off: raise the output limit or ask for a shorter answer"
	}
}
```

| 情况 | 现象 | 处理 |
|---|---|---|
| **推理阶段就耗光** | 一个可见字都没有；输入和推理照样计费 | 调大上限，或者降 effort / thinking_level |
| **回答写了一半** | 有部分回答 | 调大上限，或者要求更短的回答 |

第一种最坑：用户等了半分钟，什么都没收到，账单却照常。OpenAI 文档专门写了这种情况，并建议刚开始实验时至少给推理和输出留 25000 token，熟悉了自己任务的推理用量后再调。Gemini 文档的建议是降 `thinking_level` 而不是把 `max_output_tokens` 设小，因为后者只是硬截断，不会让模型少想。

几条和上限相关的工程规则：

- **上限要按 effort 设**：Anthropic 文档建议 Opus 系列在 `xhigh`、`max` 下从 64k 起步；Sonnet 5.5 做 Agent 编程直接设到 128000（模型上限）并用流式；
- **大上限要流式**：Anthropic 的 SDK 在 `max_tokens` 超过 21333 时要求流式，避免 HTTP 超时（这是客户端校验，不是 API 限制）；
- **思考超过 32k 用 Batch**：单次请求思考特别长时，容易撞上系统超时和连接限制，Anthropic 建议改用批处理接口；
- **监控 `Visible() == 0` 的比例**：它是“上限设太小”的直接信号，放进第 09 模块的成本看板里。

## 10. 推理配置和 Prompt Cache

1.5 课会系统讲缓存，这里先说一个和推理直接相关的坑：**推理配置是前缀的一部分**。

- **Anthropic**：thinking 配置和最终生效的 effort 会被渲染进提示词。在 `adaptive`、`enabled`、`disabled` 之间切换，改 `budget_tokens`，改顶层 effort，都会让消息级的缓存断点失效（工具和 system 的断点也可能失效）。文档的实验：第三次请求把预算从 4000 改成 8000，`cache_read_input_tokens` 从 1370 变回 0；
- **OpenAI**：改请求级的 `reasoning.effort` 同样会改变前缀。GPT-6 系列提供 `configuration_update` 条目：在下一条用户消息前插入它，请求级的 effort 保持不变，前缀不变，缓存保住；
- **Anthropic 的对应做法**：支持的模型（Opus 5.5、Sonnet 5.5、Haiku 5.5 等）可以在 `messages` 里插一条 `role: "system"`、`content` 为空、带 `output_config.effort` 的消息（需要 beta 头 `mid-conversation-output-config-2026-07-01`），效果是从下一个有新输入的 user 回合开始换档，而且不破坏缓存。

经验规则：**一个长对话里，请求级的推理配置定下来就别动**；需要换档，用各家提供的“中途更新”机制，或者在会话边界换。

---

# 第四部分：推理块的生命周期

## 11. 为什么必须回传

推理块（OpenAI 的 reasoning item、Anthropic 的 thinking 块、Gemini 的 thought step）在多轮对话里的规则，归纳成三句话：

| | 工具循环内（同一回合） | 跨回合 | 没有工具调用 |
|---|---|---|---|
| Anthropic | **必须**原样回传 | **建议**全部回传，API 自己过滤 | **可以**省略之前回合的思考 |
| OpenAI | **强烈建议**回传最后一次函数调用附带的推理；连续多次调用时，从最后一条用户消息之后的推理、调用、结果都要原样回传 | 由 `reasoning.context` 决定是否使用 | 同左 |
| Gemini（无状态模式） | **必须**原样回传全部 thought 块 | 同左；切换模型时也要回传，后端负责兼容 | 同左 |

为什么工具循环内必须回传？模型调工具时，它的回答“暂停”了，等你把结果送回来它再接着写**同一个回答**。如果你把它之前的思考拿掉，它就得重新弄明白“我为什么要调这个工具、下一步打算干什么”，结果是更差、更贵，而且不报错。

最简单、也是三家都推荐的做法：**有状态模式**（OpenAI 的 `previous_response_id`、Gemini 的 `previous_interaction_id` + `store: true`）让服务端管理；**无状态模式**就把上一轮的全部输出原样放回历史。1.2 课的统一客户端用 `Raw` 字段做的就是后者。

## 12. 签名绑定了什么

练习用 HMAC 模拟了 Anthropic 文档描述的签名机制：

```go
// sign 生成签名：“模型 . 加密的完整思考 . MAC(模型, 前缀, 完整思考, 展示文本)”。
func (s *Server) sign(model, prefix, hidden, shown string) string {
	mac := hmac.New(sha256.New, s.key)
	fmt.Fprintf(mac, "%s\x00%s\x00%s\x00%s", model, prefix, hidden, shown)
	return model + "." + base64.StdEncoding.EncodeToString([]byte(hidden)) + "." + hex.EncodeToString(mac.Sum(nil))
}
```

**逐行解读**

- **签名里带着完整思考**：真实的 `signature` 是完整思考的加密副本。服务端收到回传的块时，从签名里还原完整思考放进上下文，而不是用你手里那段摘要。所以 `display: "omitted"` 的块虽然文本为空，回传后模型照样能“想起来”。练习用 base64 代替加密，只是为了能看出结构；真实签名是不透明的，**不要解析它**。
- **绑定模型**：每个模型只读得懂自己和一组固定模型产生的思考块。练习的 `Reads` 表照文档设置：Fable 5.1 读得懂 Opus 5.5 和 Opus 5 的块；Opus 5.5 读得懂 Opus 5，但读不懂 Fable 5.1。读不懂的块**静默丢弃，不报错，不计费**。
- **绑定前缀**：MAC 里包含“这个块之前所有内容”（system、tools、之前的消息）的哈希。前缀任何一处改了，这个块和**之后所有思考块**都失效。这是 Anthropic 从 Fable 5.1 开始的“preserved thinking”检查：2026 年 8 月 31 日之后创建的账号默认强制，更早的账号要在 `thinking.block_binding.prefix_mismatch_behavior` 里显式打开，可以选 `error`（整个请求 400）或 `drop_block`（丢掉失效的块继续）。
- **摘要也被签名覆盖**：`summarized` 的块，改摘要里一个标点就失效（`TestEditingThinkingSummaryIsRejected`）；`omitted` 的块，客户端往空文本里写什么都会被忽略（`TestOmittedDisplayIgnoresClientText`），这也是文档原话。

服务端的处理逻辑：

```go
for i, b := range msgs {
	if b.Kind != KindThinking && b.Kind != KindRedacted {
		out = append(out, b)
		continue
	}
	if invalidFrom >= 0 {
		dropped = append(dropped, Dropped{i, "after_invalid"}) // 前面有块失效了，后面全部作废
		continue
	}
	producer, hidden, ok := s.verify(system, tools, msgs[:i], b)
	switch {
	case !ok:
		if on == MismatchError {
			return nil, nil, fmt.Errorf("messages[%d]: %w", i, ErrModified)
		}
		invalidFrom = i
		dropped = append(dropped, Dropped{i, "invalid_signature"})
	case !s.canRead(model, producer):
		dropped = append(dropped, Dropped{i, "unreadable_model"}) // 换了模型：静默丢弃
	case i < lastTurn && !s.KeepAllTurns[model]:
		dropped = append(dropped, Dropped{i, "prior_turn"}) // 只保留最近一轮的模型
	default:
		out = append(out, Block{Role: b.Role, Kind: b.Kind, Text: hidden, Signature: b.Signature})
	}
}
```

`TestEditingPrefixInvalidatesLaterThinking` 演示了两种改前缀：改一个工具结果，它之后的那个思考块失效；改 system prompt，所有思考块都失效。`dropped` 列表对应真实 API 响应里的 `input_transformations`（需要 `thinking-binding-controls-2026-08-01` beta 头），它告诉你哪些块被丢了、为什么。

**这对客户端意味着：对话历史要只追加、不修改**。常见的“修改”包括：在 system prompt 里塞当前时间、每轮重写工具列表、客户端自己压缩或截断中间的消息、给旧消息打补丁。Anthropic 文档为每一种都给了替代方案：中途的 system 消息、`tool_addition` / `tool_removal` 块、每条消息的 `output_config`、服务端的压缩和上下文编辑。第 03 模块讲上下文工程时会详细展开。

## 13. 保留策略：哪些历史推理还在上下文里

即使你全部回传，服务端也不一定都用：

| | 规则 | 后果 |
|---|---|---|
| Anthropic “保留所有轮次”的模型（Opus 4.5 之后的 Opus、Sonnet 4.6 之后的 Sonnet、Haiku 5.5、Fable、Mythos） | 之前回合的思考块留在上下文，**按输入计费** | 长对话里推理不断累积，占上下文、花钱；需要时用 thinking block clearing 清理 |
| Anthropic “只保留最近一轮”的模型（更早的 Opus、Sonnet，Haiku 4.5 及之前） | 用户发来新一轮输入时，之前的思考块自动剥离 | 每轮的思考只是一次性成本 |
| OpenAI `current_turn` / `all_turns` | 是否把之前回合的推理渲染进下一次采样 | `all_turns` 有连续性，`current_turn` 省上下文 |
| Gemini | 无状态模式必须全部回传；有状态模式服务端管理 | — |

`TestLastTurnOnlyModelsStripOlderThinking` 验证了两件事：用户发了新问题后，上一轮的两个思考块被剥离；但在工具循环内（最后一条 user 消息是 `tool_result`），思考块必须保留。这正是“工具循环是同一回合”的含义。

## 14. 换模型、过滤和其他坑

**换模型**：`TestSwitchingModelsDropsUnreadableThinking` 里，从 Opus 5.5 切到 Fable 5.1，推理保住了；反方向切换，推理被静默丢弃。文档给的方向性很强：Fable 5.1 和 Mythos 5.1 能读更早模型的块，但从它们切走就会丢。OpenAI 同理，推理只在同一模型家族内复用（GPT-5.6 的 sol、terra、luna 之间可以，跨到 GPT-5.5 就不行）。Gemini 文档则说切换模型时照样回传，后端负责兼容。

**结论**：Anthropic 文档的建议是换模型时照样把思考块原样回传，由 API 决定丢不丢；只在“目标模型会忽略这些块”而你想省输入 token 时才自己剥离。跨厂商切换是另一回事：签名只有原厂商能验证，1.2 课的统一客户端在跨家时会丢掉推理块。

**按类型过滤**：

```go
// NaiveStrip 是一个常见的客户端 bug：只认识 text、thinking、tool_use 三种 assistant 块，
// 其他类型一律过滤掉，结果把 redacted_thinking 弄丢了，破坏了回传协议。
```

Anthropic 在部分推理因安全原因被加密时会返回 `redacted_thinking` 块，里面只有一个加密的 `data` 字段。如果你的代码按 `block.type == "thinking"` 挑选要回传的块，它会被静默丢掉。规则：**不认识的块类型，原样保留**。

**回合中途切换思考配置**：Anthropic 文档说，如果在工具调用和工具结果之间切换 thinking 配置，API 不报错，而是**静默关闭这次请求的思考**，还可能剥离会造成无效回合结构的思考块。检查方法是看响应里有没有 thinking 块。规则：**在回合之间切换，不要在回合内切换**。

**强制工具调用**：Opus 5.5、Sonnet 5.5、Fable 5.1、Mythos 5.1 在任何请求上都拒绝 `tool_choice: any` 或指定工具（400）；其他支持 adaptive 的模型可以强制，但强制时响应直接从工具调用开始，没有思考块。想让模型先想再调工具，用 `auto` 并在提示词里说明什么时候该用。

---

# 第五部分：什么时候值得多想

## 15. 先按任务分类

| 任务 | 推理有没有用 | 起步建议 |
|---|---|---|
| 分类、抽取、改写、翻译、简单问答 | 基本没用，有时还会过度思考 | 最低档（`none` / `minimal` / `low` / 关闭） |
| 客服、起草、执行明确的代码修改 | 有一点 | `low` |
| 多步工具调用、规划、Agent 编程、研究 | 明显 | `medium`，按 Eval 决定是否升 `high` |
| 数学证明、复杂调试、架构评审、安全审查 | 很大 | `high` 起步，`xhigh` / `max` 要有 Eval 证据 |
| 长时间运行的异步任务（几十分钟以上） | 很大 | `xhigh`，配合 Batch 或后台执行 |

两条原则，三家文档说法一致：

1. **effort 是调优旋钮，不是救命稻草**。OpenAI 的原话是“把 `reasoning.effort` 当作调优旋钮，而不是恢复质量的主要手段”。效果不好，先看提示词、上下文、工具设计；
2. **从文档建议的默认值起步，用 Eval 扫一遍**。Anthropic 对每个新模型都建议重新做一次 effort sweep，而不是照搬旧模型的设置；OpenAI 对 `xhigh` 的说法是“只在你的 Eval 显示出明显收益、足以抵消额外延迟和成本时才用”。

## 16. 逐级升级：先便宜地试，失败了再多想

如果有一个可靠的**验证器**（单元测试、Schema 校验、能执行的检查、一个能判对错的规则），就不必每题都用最高档：先用 `low` 试，验证失败再升 `medium`、`high`。

```go
// 成功率 = 1 - Π(1 - p_i)
// 期望 token = Σ tokens_i × Π_{j<i}(1 - p_j)     （只有前面都失败，才会跑到第 i 级）
func Escalate(tiers []Tier) Expect {
	fail, tokens := 1.0, 0.0
	for _, t := range tiers {
		tokens += fail * t.Tokens
		fail *= 1 - t.Success
	}
	return Expect{1 - fail, tokens}
}
```

练习里设了一组（虚构但量级合理的）数字：70% 简单题、30% 难题，各档的成功率和平均 token 如下。

| | low | medium | high |
|---|---|---|---|
| 简单题 | 95%，800 | 97%，3000 | 98%，9000 |
| 难题 | 30%，1500 | 60%，6000 | 80%，20000 |

`TestEscalationBeatsAlwaysHigh` 的输出：

```
永远 low ：成功率 75.5%， 1010 token/题， 1338 token/正确
永远 high：成功率 92.6%，12300 token/题，13283 token/正确
逐级升级 ：成功率 98.3%， 4064 token/题， 4134 token/正确
```

逐级升级同时赢了两边：比“永远 high”正确率更高、token 只有三分之一。原因有两个：大部分简单题在 `low` 就解决了，只有少数题走到 `high`；而难题在多次尝试下，总有一次成功的概率比单次 `high` 更高。

**但这个结果有三个前提，缺一个就不成立**：

1. **验证器可靠**。练习 3 会让你实现一个不完美的验证器：一半错误答案被放过时，成功率从 94.4% 掉到 56.6%。没有验证器，逐级升级就退化成“永远 low”；
2. **尝试相互独立**。公式假设每次尝试互不相关。实际上同一道题对模型“难”的原因往往是同一个，低档失败的题高档也更容易失败，所以真实收益会比公式算的少。这是一个乐观的上界；
3. **能接受延迟**。走到第三级的题要跑三次，P95 延迟会明显变长。交互式场景可以并行先跑 `low` 和 `high`，谁先通过验证用谁，用钱换时间。

**看“每个正确答案花多少 token”**，而不是只看成功率或只看成本。“永远 low” 的单题成本最低，但每答对一题要 1338 token；“永远 high” 是它的 10 倍。这个指标会在第 06 模块（Eval）和第 09 模块（成本治理）反复用到。

## 17. 多数投票：想得更多次

另一种测试时计算：同一个问题独立采样 n 次，取出现最多的答案（self-consistency）。

```go
func MajorityAccuracy(p float64, n int) float64 {
	acc := 0.0
	for k := n/2 + 1; k <= n; k++ {
		acc += binom(n, k) * math.Pow(p, float64(k)) * math.Pow(1-p, float64(n-k))
	}
	return acc
}
```

单次正确率 60% 时：

```
单次 60%，投票 1/3/5/15 次：60.0% / 64.8% / 68.3% / 78.7%
```

前提和限制：

- **答案要能比较是否相同**：选择题、数值、分类标签可以；开放式文本不行（要用另一个模型当评委挑，第 06 模块）；
- **单次正确率要超过 50%**：`MajorityAccuracy(0.4, 15)` 小于 0.4，投票会把错误的多数放大；
- **同样假设独立**：用相同提示词、相同温度采样，错误往往是相关的，真实收益比公式小；
- **成本是 n 倍**，但 n 次可以并行，延迟不变。

和逐级升级相比：投票不需要验证器，但需要答案可比较；逐级升级需要验证器，但对开放式任务也适用。两者也可以和“想得更久”组合。

## 18. 给推理模型写提示词

推理模型的提示词和普通模型有几处不同，三家文档的建议基本一致：

| 建议 | 原因 |
|---|---|
| 给清楚的目标、约束和输出格式，**不要规定每一步怎么想** | 模型自己会规划；你写的步骤可能比它想的更差 |
| 对 Agent 任务，说清什么算完成、怎么自我验证 | 推理模型会用这些信息检查自己的工作 |
| 不要求“把思考过程写进回答” | 浪费 token；在 Anthropic 新模型上还可能被拒答；要看推理就读 thinking 块或摘要 |
| 延迟敏感时，让模型先写一句简短的开场再深入思考 | OpenAI 的建议：首个可见 token 更早出现 |
| 输出很长的任务，提示模型少想一点 | Gemini 的建议：把思考预算留给真正难的部分 |
| 效果不好先改提示词和上下文，再考虑升档 | effort 是调优旋钮 |

---

## 本课小结

| 知识点 | 一句话 | 练习里的代码 |
|---|---|---|
| 推理 token | 看不见、按输出计费、占上下文、算在输出上限里 | `Usage`、`TestReasoningIsBilledAsOutput` |
| 三家旋钮 | `reasoning.effort` / adaptive + `effort` / `thinking_level` | `Resolve`、`Models` |
| 不支持的取值 | 服务端直接 400；网关里降到最近的便宜档并记录 | `TestResolveStrict…`、`TestResolveClamp…` |
| 用量口径 | OpenAI、Anthropic 的输出含推理；Gemini 的输出不含思考 | `FromOpenAI`、`FromAnthropic`、`FromGemini` |
| 两种截断 | 推理阶段耗光最坑：一个字没有，照样计费 | `Diagnose` |
| 配置与缓存 | 推理配置是前缀的一部分；中途改用 `configuration_update` / 每条消息的 `output_config` | 第 10 节 |
| 签名 | 带着完整思考，绑定模型和前缀；改一个字就失效 | `sign`、`verify`、`Render` |
| 只追加不修改 | 改前缀让之后所有思考块失效 | `TestEditingPrefixInvalidatesLaterThinking` |
| 保留策略 | 有的模型保留所有轮次（按输入计费），有的只留最近一轮；工具循环内必须保留 | `TestLastTurnOnly…` |
| 换模型 | 读不懂的思考静默丢弃，方向很重要 | `TestSwitchingModels…` |
| 值不值得 | 看每个正确答案花多少 token；逐级升级要可靠的验证器，投票要可比较的答案 | `Escalate`、`MajorityAccuracy` |

---

## 练习（今天做完）

练习目录：`projects/09-reasoning/`。

```bash
cd projects/09-reasoning
make test      # go vet + go test -race -v，17 个测试
```

| 文件 | 内容 |
|---|---|
| `effort.go` | 统一的推理强度枚举、模型能力表、`Resolve` 映射与降级 |
| `usage.go` | 三家用量统一、成本计算、截断诊断 |
| `history.go` | 思考块签名、前缀绑定、跨模型可读性、保留策略的模拟服务端 |
| `value.go` | 逐级升级与多数投票的期望计算 |
| `reasoning_test.go` | 17 个测试 |

### 练习 1：读输出，算一笔账

1. 运行 `make test`，找到三处 `t.Logf`。用自己的话解释：为什么“可见输出一样”成本能差 21 倍；为什么逐级升级能同时比“永远 high”更准、更便宜；为什么投票 15 次能把 60% 提到 78.7%。
2. 把 `easy` 和 `hard` 的比例从 70:30 改成 20:80（难题为主），再看三种策略的“token/正确”。逐级升级还划算吗？为什么？
3. 用 `FromGemini(62, 171, 297)` 和假价格 `Price{Input: 2, Output: 10}` 算一次成本；再假装你忘了加思考 token，用 `Usage{Input: 62, Output: 171}` 算一次。差了多少？

### 练习 2：改坏它，看哪个测试失败

每次只改一处，跑 `go test`，记录失败的测试名和原因，然后改回来：

1. `nearest` 里距离相同时改成取更高的档位（`s < best` 改成 `s > best`）；
2. `FromAnthropic` 里不把缓存加回输入（`Input: input`）；
3. `FromGemini` 里不把思考加进输出（`Output: output`）；
4. `prefixDigest` 里把思考块也按 `Text` 计入前缀（把 `if b.Kind == KindThinking || b.Kind == KindRedacted` 改成 `if false`）。为什么偏偏是 omitted 的测试失败？
5. `lastTurnStart` 里把只装 `tool_result` 的 user 消息也当成新一轮（去掉 `&& msgs[i].Kind != KindToolResult`）；
6. `Render` 里遇到失效的块后不再记录 `invalidFrom`（改成 `_ = invalidFrom`）；
7. `canRead` 永远返回 true；
8. `Diagnose` 里用 `u.Output == 0` 代替 `u.Visible() == 0`。

### 练习 3：不完美的验证器

现实里的验证器会犯两种错：把错误答案放过（false accept），把正确答案打回（false reject）。在 `value.go` 里实现：

```go
// EscalateVerified：最后一级不再验证，直接返回。
func EscalateVerified(tiers []Tier, falseAccept, falseReject float64) Expect
```

新建 `verify_test.go`，断言：

- `falseAccept = falseReject = 0` 时，结果和 `Escalate(hard)` 完全一样；
- `falseAccept = 0.5` 时，成功率和 token 都比完美验证器低（错误答案提前“通过”了）；
- `falseReject = 0.5` 时，token 比完美验证器高（正确答案被打回，多跑了高档）。

做完想一想：在你的 Agent 里，哪些步骤有可靠的验证器？哪些没有？没有的那些，逐级升级还值得用吗？

### 选做：一个推理网关

把 `Resolve`、`Usage`、`Diagnose` 组合成一个函数：输入任务类型和模型名，输出请求参数和建议的 `max_output_tokens`；每次响应回来后记录 `Visible() == 0` 的比例，超过 5% 时自动把该任务类型的上限调大一档。这是第 11 模块模型网关的一个雏形。

---

## 自测题

1. 推理 token 有哪四个特点？各自带来什么工程问题？
2. 为什么 `display: "omitted"` 不省钱？它省的是什么？
3. 写出三家控制推理强度的参数名和取值范围。哪家可以完全关掉推理？关掉的写法分别是什么？
4. 为什么给 GPT-6 Astra 传 `reasoning.effort: "none"` 会失败？给 Claude Opus 4.7 传 `thinking: {type: "enabled", budget_tokens: 10000}` 呢？
5. 统一客户端遇到模型不支持的 effort，有哪两种处理方式？各适合什么场景？降级时为什么选便宜的那一档？
6. 三家用量里，“输出 token”是否包含推理？如果把 Gemini 的 `total_output_tokens` 当成总输出，会发生什么？
7. 推理模型的两种截断分别是什么？哪一种更糟？各自怎么处理？
8. 为什么在长对话里改 effort 会让 Prompt Cache 失效？OpenAI 和 Anthropic 分别提供了什么不破坏缓存的换档方式？
9. Anthropic 的思考块签名绑定了哪两样东西？改了一个之前的工具结果，会发生什么？
10. “对话历史只追加、不修改”具体要求什么？举三个常见的违反方式。
11. 为什么只装着 `tool_result` 的 user 消息不算新的一轮？这对“只保留最近一轮思考”的模型意味着什么？
12. 按 `block.type == "thinking"` 过滤要回传的块，会丢掉什么？为什么这是 bug？
13. 逐级升级在什么前提下比“永远最高档”又准又省？哪个前提最容易不成立？
14. 多数投票需要什么前提？单次正确率 40% 时投票会怎样？
15. 给推理模型写提示词，和给普通模型写有哪些不同？

---

# 参考答案与详解

## 自测题答案

**1.** 看不见：只能拿到摘要或加密内容，无法逐字审查推理。按输出计费：成本由看不见的部分决定，同样的回答可能差几十倍。占上下文：当前轮的推理和（部分模型）历史推理会挤占窗口，长对话要管理。算在输出上限里：上限设小了，推理阶段就能耗光，一个字都不输出。

**2.** 计费按完整思考算，和返回什么无关。`omitted` 省的是延迟：服务端不再流式传输思考文本，只传签名，正文的第一个 token 更早到达。

**3.** OpenAI：`reasoning.effort`，none / minimal / low / medium / high / xhigh / max，按模型支持子集，`none` 就是不推理（部分模型不支持）。Anthropic：`thinking.type`（adaptive / disabled / between_tools，旧模型还有 enabled）+ `output_config.effort`（low / medium / high / xhigh / max）；能否关闭看模型：Haiku 5.5、Opus 5 在 effort ≤ high 时可以 `disabled`，Sonnet 5.5 只能 `between_tools`，Opus 5.5 关不掉。Gemini：`generation_config.thinking_level`，minimal / low / medium / high，没有完全关闭的档位。

**4.** GPT-6 Astra 不支持 `none`，文档明确说会返回 400。Opus 4.7 及之后的模型不再支持 `type: "enabled"` + `budget_tokens`，同样 400；要改成 `thinking: {type: "adaptive"}` + `output_config.effort`。

**5.** strict：直接报错，适合开发期，尽早发现配置错误。clamp：降到最近的受支持档位并记录警告，适合网关这种一个请求可能打到不同模型的场景，警告必须进日志和 trace。距离相同选便宜的，因为多想的成本是确定的，少想的风险可以靠 Eval 发现、靠逐级升级补回来。

**6.** OpenAI 的 `output_tokens` 包含 `reasoning_tokens`；Anthropic 的 `output_tokens` 包含思考，`thinking_tokens` 是其中的部分；Gemini 的 `total_output_tokens` **不含**思考，`total_thought_tokens` 单独给。把 Gemini 的 `total_output_tokens` 当总输出，思考的部分没算进去，成本严重低估（文档示例里少算了 297 / 468，约 63%）。

**7.** 推理阶段就耗光上限：没有任何可见输出，但输入和推理照样计费；回答写了一半：有部分回答。前者更糟，用户什么都没得到。前者调大上限或降低推理强度（Gemini 文档特别建议降 `thinking_level` 而不是缩小 `max_output_tokens`）；后者调大上限或要求更短的回答。

**8.** 推理配置被渲染进提示词，是前缀的一部分，改了前缀就变了，缓存匹配不上。OpenAI（GPT-6 系列）：保持请求级的 `reasoning.effort` 不变，在下一条用户消息前插入 `configuration_update` 条目。Anthropic（支持的模型，beta）：在 `messages` 里插一条 `role: "system"`、内容为空、带 `output_config.effort` 的消息。

**9.** 产生它的模型，和它之前的全部内容（system、tools、之前的消息）。改了一个之前的工具结果，那个工具结果之后的思考块和再往后所有思考块都失效：`error` 模式下整个请求 400，`drop_block` 模式下这些块被丢弃，模型失去这部分推理上下文。

**10.** 每个 assistant 回合原样发回，新内容只追加在 `messages` 末尾，system 和 tools 保持不变。常见违反：在 system prompt 里放当前时间等每轮变化的内容；每轮按需重写工具列表；客户端自己压缩、截断或改写中间的消息；按类型过滤丢掉不认识的块。

**11.** 工具循环是同一个 assistant 回合：模型调工具时回答暂停，工具结果送回后接着写同一个回答。对“只保留最近一轮”的模型，工具循环内的思考都属于当前回合，必须保留；只有用户发来带新输入的消息时，之前回合的思考才会被剥离。如果把 `tool_result` 也当成新一轮，就会在工具循环中途丢掉思考（练习 2 第 5 条）。

**12.** 会丢掉 `redacted_thinking` 块（安全原因被整段加密，没有可读文本）。回传协议要求最近一条 assistant 消息里连续的思考块序列和模型生成时完全一致，丢一个就破坏了序列，可能导致 400 或推理上下文丢失。规则是不认识的块类型原样保留。

**13.** 有可靠的验证器、各次尝试大致独立、能接受更长的尾延迟，而且简单任务占多数。最容易不成立的是验证器可靠：很多 Agent 步骤（写一段总结、做一个判断）没有能自动判对错的检查，验证器一放过错误答案，升级就不会发生。其次是独立性：同一道题对模型难的原因往往相同，真实收益比公式小。

**14.** 答案能比较是否相同（选择题、数值、标签），单次正确率高于 50%，各次采样大致独立。单次 40% 时，投票会放大错误的多数，次数越多正确率越低。

**15.** 给目标、约束和输出格式，不要规定每一步怎么想；对 Agent 任务说清完成标准和自我验证方式；不要求把思考过程写进回答（浪费 token，Anthropic 新模型上可能被拒答）；延迟敏感时让模型先写一句开场；长输出任务提示模型少想；效果不好先改提示词和上下文，再考虑升档。

## 练习参考答案

### 练习 1

第 2 题：难题占 80% 时，三种策略分别是：永远 low 成功率 43.0%、每答对一题 3163 token；永远 high 83.6%、21292 token；逐级升级 95.5%、9666 token。逐级升级依然最好，但单题成本从 4064 涨到 9233，是简单题为主时的两倍多：难题多了，走到高档的题就多，而且每道难题都先白付了 low 和 medium 的钱。它还划算的原因是这组数字里难题在 low、medium 上仍有 30%、60% 的成功率；如果低档在难题上几乎全错，前两级就是纯浪费。更好的做法是先用一个便宜的分类器判断难度，把明显的难题直接送到高档。

第 3 题：正确的用量是输入 62、输出 468，成本 `(62×2 + 468×10) / 1e6`；忘了思考 token 时输出只有 171。按假价格，正确成本是 0.004804 美元，漏算后是 0.001834 美元，只有真实成本的 38%。

### 练习 2

| 改动 | 失败的测试 | 原因 |
|---|---|---|
| 1. 平局取更高档 | `TestResolveClampPicksNearestCheaper` | `claude-sonnet-4-6` 的 `xhigh` 被降到 `max`，比要求的还贵 |
| 2. Anthropic 不加回缓存 | `TestUsageNormalization` | 输入从 1387 变成 17，缓存命中率算出来超过 100% |
| 3. Gemini 不加思考 | `TestUsageNormalization` | 输出少了 297，成本严重低估 |
| 4. 思考块按 Text 计入前缀 | `TestOmittedDisplayIgnoresClientText` | 客户端往 omitted 块里写字，改变了第二个思考块的前缀，第二个块的签名失效。summarized 的测试不受影响，因为它们没改 Text；只有“允许改、应该被忽略”的情况会暴露这个 bug |
| 5. tool_result 算新一轮 | `TestLastTurnOnlyModelsStripOlderThinking` | 工具循环内的思考被当成“上一轮”剥掉了 |
| 6. 不记录失效位置 | `TestEditingPrefixInvalidatesLaterThinking` | 改 system prompt 后，第一个块失效，但第二个块被单独验证；它的前缀里包含第一个块的签名（没变），于是又“有效”了。真实规则是失效块之后的思考块全部作废 |
| 7. 永远读得懂 | `TestSwitchingModelsDropsUnreadableThinking` | Fable 5.1 的思考被 Opus 5.5 当成可读的放进上下文 |
| 8. 用 Output 判断 | `TestDiagnoseTruncation` | 推理耗光时 `Output` 是 300 不是 0，被误诊成“回答写了一半” |

第 6 条值得多想：“这个块之后全部作废”不是多余的保守，因为后面的块的前缀虽然字节上没变，但它们是在一个已经无效的上下文之上产生的。

### 练习 3

```go
func EscalateVerified(tiers []Tier, falseAccept, falseReject float64) Expect {
	reach, succ, tok := 1.0, 0.0, 0.0
	for i, t := range tiers {
		tok += reach * t.Tokens
		if i == len(tiers)-1 {
			succ += reach * t.Success // 最后一级不验证，对就是对，错就是错
			break
		}
		acceptCorrect := t.Success * (1 - falseReject)
		acceptWrong := (1 - t.Success) * falseAccept
		succ += reach * acceptCorrect
		reach *= 1 - acceptCorrect - acceptWrong // 被打回的才会走到下一级
	}
	return Expect{succ, tok}
}
```

**逐行解读**

- **`reach`**：走到这一级的概率。只有前面每一级都“没通过验证”才会到这里。
- **通过有两种**：答对且没被误判（`acceptCorrect`），答错但被放过（`acceptWrong`）。只有前者计入成功；后者结束了流程，却带着错误答案。
- **完美验证器时退化为 `Escalate`**：`acceptWrong = 0`，`acceptCorrect = p`，每一级的 `reach` 乘以 `1 - p`，和原公式一致；最后一级 `succ += reach × p` 也一样。

`verify_test.go`：

```go
package reasoning

import (
	"math"
	"testing"
)

func TestEscalateVerified(t *testing.T) {
	perfect, plain := EscalateVerified(hard, 0, 0), Escalate(hard)
	if math.Abs(perfect.Success-plain.Success) > 1e-12 || math.Abs(perfect.Tokens-plain.Tokens) > 1e-9 {
		t.Fatalf("perfect verifier must equal Escalate: %+v %+v", perfect, plain)
	}
	sloppy := EscalateVerified(hard, 0.5, 0)
	if !(sloppy.Success < perfect.Success && sloppy.Tokens < perfect.Tokens) {
		t.Fatalf("%+v", sloppy)
	}
	paranoid := EscalateVerified(hard, 0, 0.5)
	if !(paranoid.Tokens > perfect.Tokens) {
		t.Fatalf("%+v", paranoid)
	}
}
```

在难题上跑出来的数字：

```
完美验证      成功率 94.4%，11300 token
一半错答被放过 成功率 56.6%， 5000 token
一半对答被打回 成功率 88.1%，18500 token
```

两种错误的代价不对称：放过错误答案直接毁掉正确率（94.4% → 56.6%），而且看起来还“更省钱”，在成本看板上甚至像是优化；打回正确答案只是多花钱、多花时间（11300 → 18500 token），正确率也会下降一些，因为本来对的答案被换成了高档重做的结果，而最后一级不验证。所以验证器宁可严一点：**false accept 比 false reject 危险得多**。

---

## 完成标准

- [ ] `make test` 17 个测试全部通过，能说出每个测试在验证什么；
- [ ] 练习 1：三个问题都算过，能解释难题为主时逐级升级为什么变差；
- [ ] 练习 2：八处改动都做过，尤其能解释第 4、6 条；
- [ ] 练习 3：`EscalateVerified` 实现完成，`verify_test.go` 通过 `go test -race`，能说出为什么 false accept 比 false reject 更危险；
- [ ] 能不看课件写出三家控制推理强度的请求，知道哪些组合会 400；
- [ ] 能对着一次响应的 usage 算出真实成本，并判断是哪种截断；
- [ ] 自测题 15 道都能讲清楚，尤其是第 6、8、9、11、13 题。

做到以上几条，1.4 就可以勾选。下一课 **1.5 Prompt Caching**：前缀匹配的原理、三家缓存的写法和计费、缓存断点放在哪里，以及本课提到的“推理配置是前缀的一部分”在实际系统里怎么管理。
