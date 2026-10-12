# 1.3 结构化输出：JSON Schema、strict 模式与约束解码

> 第 1 部分 · 第 3 课 ｜ 预计 5–6 小时 ｜ 前置：1.1 采样（logits、logit bias）、1.2 主流模型 API
>
> 配套练习：`projects/08-structured-output/`（纯 Go 标准库，20 个测试）。手写 JSON Schema 校验器、一个真正能跑的约束解码器（把不合法 token 的 logit 设成 -∞）、三家 strict 规则的检查与转换，以及“解析 → 校验 → 带错误重试”的抽取循环。

## 读前说明

Agent 的每一步几乎都要把模型的输出交给程序：工具参数、路由决策、抽取出的字段、评测打分。程序要的是**能直接解析、字段齐全、类型正确**的数据，模型天生输出的却是文本。这一课解决的就是两者之间的缝。

先看几个你迟早会遇到的问题：

- 提示词里写了“只输出 JSON”，为什么模型还是会在前面加一句“好的，结果如下：”？
- 开了 strict 模式，官方说“保证符合 schema”，为什么线上还是会解析失败？
- 为什么 OpenAI 要求所有字段都 `required`，而 Anthropic 不支持 `minimum`？
- 为什么同一个 schema，第一次请求明显更慢？
- 为什么把 `reasoning` 字段放在 `answer` 前面，结果会更准？
- 强制模型只能在 `["yes", "no"]` 里选，它真的“不知道”的时候会发生什么？

这一课分四部分：**为什么 → 原理（约束解码）→ 三家写法 → 工程实践**。第二部分是重点：我们会自己写一个约束解码器，亲眼看到“格式保证”是怎么来的，以及它保证不了什么。

本课结束时你能：

1. 区分三种层次的“让模型输出 JSON”：提示词、JSON mode、Structured Outputs（strict），说清各自保证什么；
2. 讲清约束解码的原理：schema → 语法 → 每一步屏蔽不合法 token，并能手写一个前缀检查器；
3. 说出约束解码的三个代价：首次编译延迟、对模型输出分布的扭曲、只管结构不管语义；
4. 写出 OpenAI、Anthropic、Gemini 三家结构化输出的请求，知道各自支持的 schema 子集和限制；
5. 设计对模型友好的 schema（字段顺序、描述、可空字段、enum），并把一个普通 schema 转换成各家 strict 能接受的形状；
6. 写出生产可用的抽取循环：处理拒答、截断、enum 大小写，用校验错误驱动重试。

**关于时效**：本课核对的是 2026 年 10 月三家的官方文档。三家对 JSON Schema 的支持范围变化很快，以官方文档的“支持的 schema”一节为准；练习里的检查规则就是照着那几节写的，文档变了，改规则表即可。

---

# 第一部分：为什么需要结构化输出

## 1. 三个层次

**层次一：只靠提示词。**“请只输出 JSON，格式如下……”。模型大多数时候会照做，但你会遇到：

| 现象 | 例子 | 原因 |
|---|---|---|
| 前后有废话 | `好的，结果如下：{...}` | 模型被训练得“有礼貌” |
| 代码围栏 | `` ```json\n{...}\n``` `` | 训练数据里 JSON 常出现在 Markdown 里 |
| 语法错误 | 尾逗号、单引号、注释、没转义的换行 | 模型在“像 JSON”，不是在“解析 JSON” |
| 字段错 | 少字段、多字段、`"age": "30"` | 没有任何机制强制 |
| 半截 | 撞上 max tokens | 生成被截断 |

**层次二：JSON mode。** OpenAI 的 `text.format: {type: "json_object"}`。它保证输出**是合法的 JSON**，但不保证符合你的结构。官方文档还有一个重要提醒：开 JSON mode 时必须在对话里明确要求输出 JSON，否则模型可能输出一串没完没了的空白，直到撞上 token 上限。现在官方建议能用 Structured Outputs 就别用 JSON mode。

**层次三：Structured Outputs（strict 模式）。** 你给一个 JSON Schema，服务端用**约束解码**保证输出符合它（在 schema 的支持范围内）。三家现在都有：

| 厂商 | 参数 | 2026 年 10 月的状态 |
|---|---|---|
| OpenAI | Responses 的 `text.format: {type: "json_schema", name, schema, strict: true}` | GA；也用于函数调用的 `strict: true` |
| Anthropic | `output_config.format: {type: "json_schema", schema}`；工具上的 `strict: true` | GA，不再需要 beta 头；旧的 `output_format` 参数已弃用 |
| Gemini | Interactions 的 `response_format: {type: "text", mime_type: "application/json", schema}` | GA；Gemini 3 系列可以和内置工具组合（预览） |

这一课的核心论点：**strict 模式把“格式错误”从一个概率问题变成了一个工程问题**。它解决的是层次一表格里的前四行；第五行（截断）和另外几种情况它解决不了，第 9 节专门讲。

## 2. 两个入口：响应格式 vs 工具参数

结构化输出有两个地方可以用：

| | 响应格式（response format） | 严格工具（strict tool） |
|---|---|---|
| 约束的是 | 模型的**最终回答** | 模型**调用工具时的参数** |
| 典型用途 | 抽取、分类、生成结构化报告、评测打分 | Agent 调用你的函数 |
| OpenAI | `text.format` | 函数定义上 `strict: true` |
| Anthropic | `output_config.format` | 工具定义上 `strict: true` |
| Gemini | `response_format` | 函数调用（参数本身有 schema） |

Gemini 文档的说法很清楚：**想要模型的回答是某种格式，用结构化输出；想要模型在对话过程中让你去做一件事，用函数调用。** 两者可以同时用：Agent 用严格工具调你的函数，最后用结构化输出返回一个可解析的总结。Anthropic 的文档专门给了同时使用的例子。

一个常见的反模式：为了拿到结构化结果，定义一个假工具 `return_result(result: {...})`，再用 `tool_choice` 强制模型调用它。在没有原生结构化输出的年代这是标准做法，现在直接用响应格式更清楚，也不会和真正的工具调用混在一起。

---

# 第二部分：原理：约束解码

## 3. 回到采样：把不合法的 token 屏蔽掉

1.1 课讲过，模型每一步输出整个词表上的 logits，采样器从中选一个 token。我们还讲了 logit bias：给某些 token 的 logit 加一个数，提高或降低它被选中的概率。

约束解码就是 logit bias 的极端形式：

```
每一步：
  logits = model(已生成的文本)
  对词表里每个 token t：
      如果 “已生成的文本 + t” 不可能被续写成一个符合 schema 的 JSON：
          logits[t] = -∞          ← 概率严格为 0，不是“很小”
  按正常的采样规则（温度、top-p……）从剩下的 token 里选
```

因为每一步选出来的 token 都“还有救”，所以只要生成正常结束，结果一定符合语法。不需要提示词恳求，不需要事后修复。

举个例子。schema 要求 `{"age": integer}`，已经生成了 `{"age":`。这时：

| 候选 token | 拼起来 | 还有救吗 | 处理 |
|---|---|---|---|
| `3` | `{"age":3` | 有，后面可以是 `0}` | 保留 |
| `"` | `{"age":"` | 没有，integer 不能以引号开头 | -∞ |
| 空格 | `{"age":` 后跟一个空格 | 本课的语法不允许空白 | -∞ |
| `-` | `{"age":-` | 有，后面可以是数字 | 保留 |
| `}` | `{"age":}` | 没有 | -∞ |

关键在于“还有救吗”这个判断。它叫**前缀检查**：给定一段前缀，它是不是某个合法文档的开头？

## 4. 手写前缀检查器

练习的 `grammar.go` 用一个递归下降解析器来做前缀检查。它和普通 JSON 解析器只有两点不同：

1. **输入提前用完不是错误**，而是“到这里为止没错”，返回 `Prefix`；
2. **遇到分支就每条路都试**（anyOf、可选字段、`true`/`false`），取最好的结果。

为了支持第 2 点，用了一个叫“续体传递”（continuation-passing）的写法：每个解析函数多接收一个回调 `k`，意思是“我解析完之后，从这个位置接着做什么”。

```go
type Status int

const (
	Invalid  Status = iota // 无论再接什么都不可能合法
	Prefix                 // 合法前缀，还没结束
	Complete               // 已经是一个完整、合法的值
)

type cont func(int) Status // 续体：从位置 i 开始接着解析

// lit 匹配一段固定文本。前缀在中途用完也算合法。
func (c *checker) lit(i int, s string, k cont) Status {
	for _, r := range s {
		if c.eof(i) {
			return Prefix
		}
		if c.s[i] != r {
			return Invalid
		}
		i++
	}
	return k(i)
}
```

**逐行解读**

- **`Status` 三种状态**：`Prefix` 和 `Complete` 的区别决定了 EOS（结束 token）能不能被选：只有 `Complete` 时才允许结束。
- **`lit`**：匹配固定文本，比如 `true`、`null`、`"name":`。匹配到一半输入用完（前缀是 `tr`），返回 `Prefix`；有一个字符对不上，返回 `Invalid`；全部匹配，交给续体 `k` 处理后面的部分。
- **为什么需要续体**：考虑可选字段。`{"a":"x"` 后面可以接 `,"b":...` 也可以直接接 `}`。普通解析器要么向前看、要么回溯；续体写法里，两条路都是一次函数调用，结果取较好的那个：

```go
func (c *checker) fields(i int, sc *Schema, idx int, first bool, k cont) Status {
	if idx == len(sc.Props) {
		return c.lit(i, "}", k) // 字段都处理完了，必须是 }
	}
	p := sc.Props[idx]
	key, _ := json.Marshal(p.Name)
	sep := ","
	if first {
		sep = ""
	}
	take := c.lit(i, sep+string(key)+":", func(j int) Status { // 路线 1：输出这个字段
		return c.value(j, p.Schema, func(m int) Status {
			return c.fields(m, sc, idx+1, false, k)
		})
	})
	if sc.Required[p.Name] || take == Complete {
		return take
	}
	return best(take, c.fields(i, sc, idx+1, first, k)) // 路线 2：可选字段，跳过
}
```

- **字段顺序固定**：键必须按 schema 里的顺序出现，而且不允许空白。这让语法变得简单、确定。OpenAI strict 模式的行为也是这样：输出按 schema 的键顺序生成。
- **不允许额外字段**：语法里根本没有“其他键”这条路，所以 `additionalProperties: false` 是自然的结果。这也解释了为什么 OpenAI 和 Anthropic 都**要求**对象设 `additionalProperties: false`：允许任意额外字段，语法就要接受任意键名和任意值，约束就失去了意义。

数字是一个有意思的例子：它没有结束符。`12` 已经完整，但还可以继续变成 `123`。

```go
func (c *checker) number(i int, frac bool, k cont) Status {
	if c.s[i] == '-' {
		i++
		if c.eof(i) {
			return Prefix
		}
	}
	if !isDigit(c.s[i]) {
		return Invalid
	}
	if c.s[i] == '0' {
		i++ // 0 后面不能再跟数字：01 不合法
	} else {
		for !c.eof(i) && isDigit(c.s[i]) {
			i++
		}
	}
	// ……小数部分省略……
	return k(i)
}
```

这里贪心地吃完所有数字再交给续体，因为数字后面不可能紧跟另一个数字。前缀恰好停在数字中间（比如 `{"age":3`）时，续体会在末尾返回 `Prefix`（后面还能接 `,`、`}`），顶层的数字则返回 `Complete`。两种都不是 `Invalid`，正好表达“可以结束、也可以继续”。

整个检查的入口：

```go
func (sc *Schema) Check(text string) Status {
	c := &checker{s: []rune(text), budget: 200000}
	return c.value(0, sc, func(i int) Status {
		if i == len(c.s) {
			return Complete
		}
		return Invalid // 值后面不允许再有任何字符
	})
}
```

`budget` 是一个保险：anyOf 套 anyOf、大量可选字段，分支数会指数增长。这正是 Anthropic 限制“可选参数最多 24 个、联合类型最多 16 个”的原因（第 11 节），它们的文档原话是：每个可选参数大约会让语法状态空间的一部分翻倍。

## 5. 解码循环和 token 边界

有了前缀检查，解码循环就很直接（`decode.go`）：

```go
func mask(logits []float64, vocab []string, text string, sc *Schema) {
	done := sc.Check(text) == Complete
	for id, tok := range vocab {
		if tok == EOS {
			if !done {
				logits[id] = math.Inf(-1) // 没写完不许结束
			}
			continue
		}
		if sc.Check(text+tok) == Invalid {
			logits[id] = math.Inf(-1)
		}
	}
}
```

**逐行解读**

- **EOS 也要管**：没写完就结束，得到的是半截 JSON。练习 2 第 3 条会让你删掉这几行，看看随机抽样测试里会出现多少半截输出。
- **按“文本 + token”检查，而不是按 token 检查**：真实分词器的 token 经常跨越 JSON 结构，比如 `",`、`":"`、`"}`（1.1 课讲过 BPE 会把高频组合合并）。一个 token 可能同时结束一个字符串、加一个逗号、开始下一个键。在拼接后的文本上检查，这些跨结构的 token 就自然被正确处理了。`TestConstrainedOutputAlwaysMatchesStructure` 的词表里专门加了这类 token。
- **复杂度**：这个实现每一步对整个词表各做一次完整检查，开销是“词表大小 × 文本长度”。真实词表有 20 万个 token，这样做太慢。

**生产实现怎么做**：把 schema **编译**成一个在 token 上运行的状态机（下推自动机），预先算好“在每个状态下哪些 token 合法”，解码时每步只是查表和更新状态。开源实现有 Outlines、XGrammar、llguidance，vLLM、SGLang 等推理引擎都集成了它们。这带来两个你在 API 上能观察到的现象：

1. **第一次用某个 schema 时明显更慢**：服务端在编译语法。OpenAI 文档说第一次请求会有额外延迟，之后同一个 schema 就没有了；Anthropic 文档说编译好的语法缓存 24 小时（从最后一次使用算起），改 schema 结构或者改工具集合会让缓存失效，只改 `name` 或 `description` 不会；
2. **schema 太复杂会被拒绝**：语法太大、编译太久。Anthropic 会返回 400 “Schema is too complex for compilation”，编译超时上限是 180 秒。

## 6. 跑起来看：不加约束 vs 加约束

练习用一个玩具模型 `ScriptModel` 模拟“格式不听话”的模型：它一心想输出

```
{"name": "Ann", "age": "30", "role": "Admin", "nickname": "A"}
```

这段文本是合法 JSON，但违反了 schema 的五处（有空格不算违规，校验器只看值）：`age` 是字符串、`role` 大小写不对、多了 `nickname`、少了 `email` 和 `tags`。

```
$ go test -run 'Unconstrained|Constrained' -v
    不加约束：JSON 合法，但有 5 处不符合 schema
    加约束：{"name":"","age":-0,"role":"admin","email":", ","tags":[" ","}"]}
    模型想说 unsure，被迫输出 {"answer":"no"}：格式保证了，意思可能被扭曲
```

第二行值得多看几眼。输出**完全符合 schema**，但内容一塌糊涂：名字是空的、年龄是 `-0`、邮箱是 `", "`。原因是这个玩具模型只会照着自己的目标文本逐字符对齐，一旦被约束逼着偏离（比如不许输出空格），后面就全乱了。

真实模型比它聪明得多，会根据已经生成的内容调整，但**同样的力量在起作用**：模型想走的路被堵死时，它只能在剩下的路里选一条，而那条路未必是好的。这引出约束解码的代价。

## 7. 代价：约束会扭曲输出分布

**例子一：被迫选择。**`TestConstrainedForcesAChoice` 里，schema 要求 `answer` 只能是 `yes` 或 `no`，模型想说 `unsure`。约束解码不会报错，只会让它在 `yes` 和 `no` 里挑一个。你拿到的结果格式完美，但它是被逼出来的。

**对策**：给模型一条“合法的退路”。

```json
{"type": "string", "enum": ["yes", "no", "unknown"]}
```

或者用可空字段 `{"type": ["string", "null"]}`，在描述里说明“信息不足时填 null”。schema 不给退路，模型就只能编。

**例子二：字段顺序就是思考顺序。**模型是自回归的（1.1 课第 2 节）：先生成的内容会影响后生成的内容，反过来不行。

```json
{"answer": "B", "reasoning": "因为……"}     ← 先给答案，再编理由
{"reasoning": "因为……", "answer": "B"}     ← 先推理，再给答案
```

第一种写法里，`reasoning` 只能为已经定下来的答案找补。所以**需要推理的字段放在结论字段前面**。OpenAI 的 strict 模式按 schema 的键顺序输出，所以你在 schema 里怎么排，它就怎么生成。

两个相关的坑：

- **Anthropic 会调整顺序**：文档说输出保持 schema 里的顺序，但有一个例外：**必填字段在前，可选字段在后**。想让推理字段排在最前面，就把它设为必填。
- **别要“完整推理过程”**：Anthropic 文档提醒，要求输出模型思考过程或逐步推理的字段可能触发 `reasoning_extraction` 类型的拒答，建议改为要一段“简短的解释”。推理模型的思考应该留在 thinking 块里（1.4 课）。

**例子三：格式本身会影响能力。**有研究报告过，强制严格格式时某些推理任务的准确率会下降（模型的“注意力”被格式占用了一部分）。工程上的常见做法是**分两步**：先让模型自由回答（可以开推理），再用一个便宜的调用把回答抽取成结构化数据。是否值得，用 Eval（第 06 模块）说话。

## 8. 语法只管结构，不管语义

```go
// 语法只管结构。minimum 这种语义约束，约束解码不管，要靠校验。
func TestGrammarDoesNotEnforceSemantics(t *testing.T) {
	sc := MustCompileJSON(`{"type":"object","properties":{"age":{"type":"integer","minimum":0}},...}`)
	out, _, _ := Generate(ScriptModel{`{"age":-5}`}, ByteVocab(), sc, 50, nil)
	// out == {"age":-5}：语法上完全合法
	// sc.Validate(...) 报错：$.age: must be >= 0
}
```

我们的语法没有实现 `minimum`。原则上可以做（数字范围也能写成语法，只是很别扭），但大多数生产实现也不做，或者只做一部分：

- **Anthropic**：不支持 `minimum`、`maximum`、`multipleOf`、`minLength`、`maxLength`，`minItems` 只支持 0 和 1。传了直接 400。SDK 的做法是把这些约束删掉、写进字段描述里（“`{minimum: 100}`”），拿到结果后再按原 schema 校验；
- **OpenAI**：支持 `minimum`、`maximum`、`pattern`、`format`、`minItems`、`maxItems` 等（微调模型除外）；
- **Gemini**：支持 `minimum`、`maximum`、`minItems`、`maxItems`、`enum`、`format`，没有列出字符串长度。

而像“`end_date` 必须晚于 `start_date`”、“`total` 等于各项之和”、“这个 SKU 必须存在于数据库”这种约束，**任何语法都表达不了**。Gemini 文档的最佳实践里写得很直白：输出在语法上是正确的 JSON，但仍然要在应用里校验值。

结论：**约束解码负责“能解析、结构对”，校验器负责“值对不对”，两者缺一不可。**练习 3 会让你把 `maxLength` 也加进语法，体会一下哪些约束适合放在语法层。

## 9. 保证失效的四种情况

strict 模式“保证符合 schema”有前提。下面四种情况，你仍然会拿到不符合 schema 的东西：

| 情况 | 现象 | 怎么识别 | 怎么处理 |
|---|---|---|---|
| **拒答** | 输出是一段拒绝说明，不是 JSON | Anthropic：`stop_reason: "refusal"`（HTTP 200）；OpenAI：输出里出现 `refusal` 内容 | 不要解析；展示提示，或者换模型、改请求 |
| **截断** | 半截 JSON | Anthropic：`max_tokens`；OpenAI：`status: "incomplete"`；Gemini：`incomplete` | 调大上限重发；同样的上限重试没用 |
| **enum 大小写** | `"Conversation Topic 3"` 而 enum 里是 `"Conversation topic 3"` | 校验失败，但 stop 正常 | Anthropic 文档明说不保证 enum / const 的大小写，常出现在空格后单词的首字母。比较时忽略大小写，并避免只有大小写不同的 enum 值 |
| **schema 被放宽了** | 结果违反了你“真正的” schema | 只能靠用原 schema 校验 | SDK 转换时删掉的约束、你自己为了兼容放宽的地方，都要事后校验 |

`TestConstrainedTruncatedByMaxTokens` 演示第二种：把 max tokens 设成 15，输出是一个**合法前缀**（`Check` 返回 `Prefix`），但解析失败。约束解码保证的是“每一步都还有救”，没保证“一定能写完”。

练习里的 `NormalizeEnums` 处理第三种。注意它只修“只差大小写”的情况，修了几处要记录下来：它是一个信号，说明 schema 设计可能有问题。

---

# 第三部分：三家写法

## 10. OpenAI：text.format

```bash
curl https://api.openai.com/v1/responses \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-6-astra",
    "input": [
      {"role": "system", "content": "从用户的文本里抽取联系人信息。"},
      {"role": "user", "content": "张三，邮箱 zhang@example.com，想了解企业版，下周二要演示。"}
    ],
    "text": {
      "format": {
        "type": "json_schema",
        "name": "contact",
        "strict": true,
        "schema": {
          "type": "object",
          "properties": {
            "name": {"type": "string"},
            "email": {"type": ["string", "null"]},
            "plan": {"type": "string", "enum": ["free", "pro", "enterprise", "unknown"]},
            "demo_requested": {"type": "boolean"}
          },
          "required": ["name", "email", "plan", "demo_requested"],
          "additionalProperties": false
        }
      }
    }
  }'
```

**逐行解读**

- **位置**：Responses API 里是 `text.format`；Chat Completions 里是 `response_format`。迁移时最容易写错的就是这个位置。
- **`name`**：必填，给这个格式起个名字。
- **`strict: true`**：打开约束解码。不写就只是“参考 schema”，不保证。
- **结果在哪**：和普通文本回答一样，在 `type: "message"` 的 output item 的 `output_text` 里（1.2 课第 4 节），是一个 JSON **字符串**，你要自己解析。SDK 的 `responses.parse()` 会帮你解析成 Pydantic / Zod 对象。

**strict 模式的规则**（官方文档“Supported schemas”一节）：

| 规则 | 说明 |
|---|---|
| 根必须是 object，且不能是 anyOf | Zod 的 `discriminatedUnion` 生成的就是根 anyOf，直接用会报错。把联合放到一个字段里 |
| **所有字段都必须在 `required` 里** | 想表达“可选”，用 `"type": ["string", "null"]`，模型必须输出这个键，没有就填 `null` |
| 每个对象都必须 `additionalProperties: false` | 第 4 节解释过原因 |
| 支持的类型 | string、number、boolean、integer、object、array、enum、anyOf |
| 支持的约束 | string：`pattern`、`format`（date-time、time、date、duration、email、hostname、ipv4、ipv6、uuid）；number：`multipleOf`、`maximum`、`exclusiveMaximum`、`minimum`、`exclusiveMinimum`；array：`minItems`、`maxItems` |
| 不支持 | `allOf`、`not`、`dependentRequired`、`dependentSchemas`、`if`/`then`/`else`；微调模型还不支持字符串、数字、数组的那些约束和 `patternProperties` |
| 递归 | 支持，用 `"$ref": "#"` 引用根，或者引用 `$defs` 里的定义 |
| 规模 | 最多 5000 个对象属性、10 层嵌套；所有属性名、定义名、enum 值、const 值加起来不超过 12 万字符；enum 值总数不超过 1000 个 |
| 键顺序 | 输出按 schema 里的键顺序生成 |

“所有字段都必须 required”是 OpenAI 最特别的一条。为什么？可选字段让语法里多出“跳过”这条分支（第 4 节的 `fields`），状态数成倍增长。OpenAI 选择让你用 `null` 显式表达“没有”，语法里就只剩一条路。练习的 `ForOpenAI` 把普通 schema 里的可选字段自动改成“必填 + 可空”。

**拒答**：OpenAI 的拒答不遵循你的 schema，output 里的内容类型是 `refusal`（1.2 课的统一客户端已经把它映射成 `StopRefusal`）。

**函数调用的 strict**：函数定义上加 `"strict": true`，参数 schema 的规则和上面完全一样。

## 11. Anthropic：output_config.format

```bash
curl https://api.anthropic.com/v1/messages \
  -H "x-api-key: $ANTHROPIC_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-opus-5-5",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "从这段文本抽取联系人：张三，邮箱 zhang@example.com……"}],
    "output_config": {
      "format": {
        "type": "json_schema",
        "schema": {
          "type": "object",
          "properties": {
            "name": {"type": "string"},
            "email": {"type": "string"},
            "plan": {"type": "string"},
            "demo_requested": {"type": "boolean"}
          },
          "required": ["name", "email", "plan", "demo_requested"],
          "additionalProperties": false
        }
      }
    }
  }'
```

**逐行解读**

- **位置**：`output_config.format`。早期 beta 用的是顶层 `output_format` 参数加 `structured-outputs-2025-11-13` beta 头，现在已弃用；新代码不需要任何 beta 头。
- **结果在哪**：响应的 text 块里，是一个 JSON 字符串。
- **严格工具**：工具定义上 `"strict": true`，用同一套语法约束工具名和参数。

**规则和限制**（官方文档“JSON Schema limitations”和“Schema complexity limits”）：

| 类别 | 内容 |
|---|---|
| 支持 | 全部基本类型；`enum`（只能是字符串、数字、布尔、null）；`const`；`anyOf`、`allOf`（`allOf` 不能配 `$ref`）；`$ref`、`$defs`、`definitions`（不支持外部引用）；`default`；`required`；`additionalProperties`（**必须是 false**）；字符串 format：date-time、time、date、duration、email、hostname、uri、ipv4、ipv6、uuid；`minItems` 只支持 0 和 1 |
| 不支持（传了返回 400） | **递归 schema**；数字约束（`minimum`、`maximum`、`multipleOf`）；字符串长度（`minLength`、`maxLength`）；`minItems` 大于 1 等数组约束；`additionalProperties` 不是 false |
| 正则 `pattern` | 支持简单的：`^...$`、`*` `+` `?`、简单的 `{n,m}`、`[]` `.` `\d` `\w` `\s`、分组；不支持反向引用、前后断言、`\b`、大范围的 `{n,m}` |
| 单请求复杂度上限 | strict 工具最多 20 个；所有 strict schema 里的可选参数合计最多 24 个；使用联合类型（`anyOf` 或类型数组）的参数合计最多 16 个 |
| 键顺序 | 保持 schema 顺序，但**必填字段在前、可选字段在后** |
| 不兼容 | Citations（引用）；预填充 assistant 消息（prefill） |

注意复杂度上限是**整个请求的合计**：4 个 strict 工具各有 6 个可选参数，就到了 24 的上限。文档给的降复杂度建议依次是：只把关键工具设为 strict、能必填就必填、拍平嵌套、拆成多个请求。

**Token 成本和缓存**：开启结构化输出时，服务端会自动注入一段系统提示说明格式要求，所以输入 token 会略多；修改 `output_config.format` 会让这段对话的 Prompt Cache 失效（1.5 课）。

**语法的作用范围**：语法只约束模型的直接输出，不约束 thinking 块、工具调用和工具结果。所以开扩展思考时，模型可以先自由思考，最终回答再受 schema 约束。这正好解决了第 7 节“格式影响推理”的问题。

**SDK 的转换**：Python 的 `client.messages.parse()` 和 `transform_schema()` 会把不支持的约束删掉、写进描述、给每个对象加 `additionalProperties: false`，然后**用原 schema 校验响应**。练习的 `ForAnthropic` 照这个思路实现，`TestAnthropicCheckAndTransform` 验证了一个关键点：`score: 9` 能通过发出去的（放宽后的）schema，但必须被原 schema 拒绝。

## 12. Gemini：response_format

```bash
curl -X POST "https://generativelanguage.googleapis.com/v1beta/interactions" \
  -H "x-goog-api-key: $GEMINI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.8-flash",
    "input": "从这段文本抽取联系人：张三，邮箱 zhang@example.com……",
    "response_format": {
      "type": "text",
      "mime_type": "application/json",
      "schema": {
        "type": "object",
        "properties": {
          "name": {"type": "string"},
          "email": {"type": ["string", "null"]},
          "plan": {"type": "string", "enum": ["free", "pro", "enterprise"]}
        },
        "required": ["name", "plan"]
      }
    }
  }'
```

**逐行解读**

- **位置**：Interactions API 的顶层 `response_format`，`type: "text"` + `mime_type: "application/json"` + `schema`。`response_format` 也可以是数组，用来同时要多种输出模态。
- **历史包袱**：2026 年 5 月的破坏性变更之前，写法是 `response_mime_type` + `response_format`（直接放 schema）；老的 `generateContent` 接口用的是 `generationConfig.responseMimeType` + `responseSchema`，而且早期的 `responseSchema` 用的是大写类型名（`"type": "OBJECT"`）的 OpenAPI 子集。网上的示例三种写法都有，看清楚是哪个接口、哪个版本。
- **结果在哪**：`model_output` step 的文本里（SDK 的 `interaction.output_text`）。
- **流式**：文档说流式返回的片段是合法的部分 JSON 字符串，拼起来就是最终对象。

**支持的 schema 子集**（官方文档“JSON schema support”）：

| 类型 | 支持的关键字 |
|---|---|
| 通用 | `type`（含 `null`，用类型数组表达可空）、`title`、`description` |
| object | `properties`、`required`、`additionalProperties`（布尔**或 schema**） |
| string | `enum`、`format`（如 date-time、date、time） |
| number / integer | `enum`、`minimum`、`maximum` |
| array | `items`、`prefixItems`（元组）、`minItems`、`maxItems` |
| 组合 | `anyOf`；递归用 `$ref`（文档示例里有组织架构树） |

和另外两家相比，Gemini **不要求** `additionalProperties: false`，也**不要求**所有字段必填，可选字段可以真正地省略。文档的限制一节只写了两条：不是所有 JSON Schema 特性都支持；特别大或嵌套特别深的 schema 可能被拒绝。

**和工具组合**：Gemini 3 系列（预览功能）可以把结构化输出和 Google 搜索、URL Context、代码执行、文件搜索、函数调用组合使用，比如“搜索之后按 schema 返回比赛结果”。

## 13. 三家对比

| | OpenAI | Anthropic | Gemini |
|---|---|---|---|
| 参数位置 | `text.format` | `output_config.format` | `response_format` |
| 必须 `additionalProperties: false` | 是 | 是 | 否 |
| 所有字段必须 required | **是**（可选用 null 表达） | 否（但上限 24 个可选参数） | 否 |
| 数字范围 | 支持 | **不支持** | 支持 |
| 字符串长度 | 未列入支持（`pattern`、`format` 支持） | **不支持** | 未列出 |
| 数组长度 | `minItems`、`maxItems` | `minItems` 仅 0 / 1 | `minItems`、`maxItems` |
| 递归 | 支持 | **不支持** | 支持 |
| 根是 anyOf | 不允许 | 未禁止 | 未禁止 |
| 输出键顺序 | schema 顺序 | 必填在前，然后 schema 顺序 | 未明确 |
| 首次编译延迟 | 有 | 有（缓存 24 小时） | 未说明 |
| 已知坑 | 拒答不遵循 schema | enum 大小写不保证；拒答；与 Citations 不兼容 | 文档自己说“仍要校验值” |

**结论**：没有一个 schema 能原样同时满足三家的 strict 规则（比如 OpenAI 要求全部 required，Anthropic 不支持递归和数字范围）。所以统一客户端需要**一份“真正的” schema，加上按厂商转换的函数**，发出去的是转换后的，校验用的是原始的。这就是练习的 `CheckOpenAI` / `ForOpenAI` / `CheckAnthropic` / `ForAnthropic`。

---

# 第四部分：工程实践

## 14. 设计对模型友好的 schema

schema 既是给程序的类型定义，也是给模型的提示词。模型能看到字段名、描述、enum 值，它们都会影响输出质量。

| 原则 | 不好 | 好 |
|---|---|---|
| 字段名自解释 | `{"v": number}` | `{"price_cny": number}` |
| 写 `description` | 没有描述 | `"description": "含税总价，单位人民币元"` |
| 推理在结论前 | `answer`, `reason` | `reason`, `answer` |
| 给退路 | `enum: ["yes", "no"]` | `enum: ["yes", "no", "unknown"]` 或可空 |
| 用 enum 不用自由文本 | `"category": string` | `"category": {"enum": [...]}` |
| 避免只差大小写的 enum | `"Topic 1"`, `"topic 1"` | 改成不同的词 |
| 少用可选、少用联合 | 十几个可选字段、到处 `anyOf` | 能必填就必填；联合放到最需要的地方 |
| 拍平 | 五层嵌套 | 两三层 |
| 敏感信息别放进 schema | enum 里列病人姓名 | schema 会被单独缓存，Anthropic 文档明确说 PHI 不能出现在属性名、enum、const、pattern 里 |

**schema 从类型生成，别手写两份**。Python 用 Pydantic，TypeScript 用 Zod，Go 可以从 struct tag 生成。手写的 JSON Schema 和代码里的类型迟早会不一致。OpenAI 文档也建议：从类型定义自动生成 schema，或者加一个 CI 步骤检查两者一致。

TypeScript 里用 Zod 定义一次，同时得到 schema 和类型：

```ts
import { z } from "zod";

const Contact = z.object({
  name: z.string().describe("姓名"),
  email: z.string().nullable(),                       // 可空，适配 OpenAI 的“全部必填”
  plan: z.enum(["free", "pro", "enterprise", "unknown"]),
  demo_requested: z.boolean(),
});
type Contact = z.infer<typeof Contact>;               // 从 schema 推导出 TS 类型

const schema = z.toJSONSchema(Contact);               // 发给 API 的 JSON Schema
const parsed = Contact.parse(JSON.parse(text));       // 拿到结果后校验，失败会抛错
```

对照 Go：

| TypeScript（Zod） | Go（本课练习） | 说明 |
|---|---|---|
| `z.object({...})` | schema JSON + `MustCompileJSON` | Go 没有运行时类型构造器，练习直接写 JSON Schema 再编译 |
| `.describe("姓名")` | `"description": "姓名"` | 给模型看的说明 |
| `z.string().nullable()` | `{"type": ["string", "null"]}` | 可空 |
| `z.enum([...])` | `{"enum": [...]}` | 枚举 |
| `z.infer<typeof Contact>` | 手写对应的 `struct` | TS 能从 schema 推导类型，Go 做不到，只能反过来从 struct 生成 schema（反射或代码生成） |
| `z.toJSONSchema(Contact)` | `Compact(doc)` | 得到发给 API 的 schema |
| `Contact.parse(obj)` 抛 `ZodError` | `sc.Validate(v)` 返回 `[]ValidationError` | 一个抛异常，一个返回错误列表；两边都带出错路径 |

## 15. 抽取循环：校验错误驱动重试

```go
func Extract(ctx context.Context, g Generator, sc *Schema, maxAttempts int) (any, int, error) {
	var feedback []string
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		text, stop, err := g.Generate(ctx, feedback)
		if err != nil {
			return nil, attempt, err
		}
		switch stop {
		case "refusal":
			return nil, attempt, ErrRefusal   // ① 拒答不重试
		case "length":
			return nil, attempt, ErrTruncated // ② 同样的上限重试还是会截断
		}
		v, err := ParseOrdered([]byte(stripFence(text))) // ③ 去掉 ```json 围栏
		if err != nil {
			feedback = []string{"输出不是合法 JSON：" + err.Error()}
			continue
		}
		errs := sc.Validate(v) // ④ 用“原始” schema 校验
		if len(errs) == 0 {
			return v, attempt, nil
		}
		feedback = feedback[:0]
		for _, e := range errs {
			feedback = append(feedback, e.Error()) // ⑤ 带路径的错误原样喂回去
		}
	}
	return nil, maxAttempts, &ExtractError{Attempts: maxAttempts, Last: feedback}
}
```

**逐行解读**

- **① 拒答**：模型拒绝了，换个格式重问一般没用，而且会浪费钱。交给上层决定：提示用户、换模型（Anthropic 提供服务端的模型回退）、或者记录下来做 Eval。
- **② 截断**：输出写到一半就停了。用同样的 `max_tokens` 重试，大概率还是截断，正确做法是调大上限，或者让 schema 产生更短的输出（比如限制数组长度）。
- **③ 围栏**：没开 strict 的模型、或者用 OpenAI 兼容接口的国产模型，经常会包一层 Markdown。顺手去掉比让模型重来便宜。
- **④ 原始 schema**：哪怕开了 strict，发出去的 schema 可能被你或 SDK 放宽过（第 11 节），所以必须用真正的 schema 再校验一次。
- **⑤ 错误带路径**：`$.items[2].price: must be >= 0` 比“格式不对，请重试”有用得多，模型通常一次就能改对。`Validate` 返回**全部**错误而不是第一个，这样模型一次能改完。

`TestExtractRetriesWithFeedback` 模拟了一个典型过程：第一次输出前面带了一句废话、JSON 半截；第二次包了代码围栏、`age` 是 -1；第三次正确。断言第二次和第三次调用收到的反馈分别是“不是合法 JSON”和 `$.age: must be >= 0`。

**重试几次合适？**一般 2–3 次。每次重试都要再付一次完整输入的钱（重试时历史更长），到第三次还不对，通常是 schema 或提示词有问题，继续重试只是烧钱。

## 16. 流式结构化输出

结构化输出也可以流式。好处是能逐字段展示（比如生成一个长报告，先显示标题），坏处是中途的 JSON 是不完整的，不能直接 `JSON.parse`。

两种做法：

1. **部分 JSON 解析**：把当前前缀“补全”成合法 JSON 再解析（补上未闭合的引号、括号），得到一个字段不全的对象，前端只渲染已经确定的字段。OpenAI 文档建议流式结构化输出交给 SDK 处理；Anthropic 文档在流式工具参数一节推荐用 Pydantic 的部分 JSON 解析或 SDK 的流式辅助函数；
2. **按字段边界推送**：用本课的前缀检查器的思路，追踪“当前在哪个字段里”，一个字段完成时推一次。

不管哪种，**最终结果只在流正常结束后、按完整 JSON 校验一次**才算数。中途展示的东西都是临时的，和 0.4 课“EOF 不等于业务成功”是同一个道理。

## 17. 什么时候用哪个

| 场景 | 选择 |
|---|---|
| 从文本或图片里抽取字段、分类、打标签 | 结构化输出（响应格式） |
| LLM 当评委打分（第 06 模块） | 结构化输出，`reason` 在 `score` 前面 |
| Agent 调用你的函数 | 工具调用，关键工具开 strict |
| 路由（选择下一步走哪个分支） | 结构化输出，用 enum |
| 需要深度推理的任务，结果还要结构化 | 推理模型自由思考（thinking 不受约束），最终回答用结构化输出；或者两步：先回答、再抽取 |
| 模型或兼容接口不支持 strict | 提示词 + 示例 + `Extract` 循环兜底 |

---

## 本课小结

| 知识点 | 一句话 | 练习里的代码 |
|---|---|---|
| 三个层次 | 提示词不保证；JSON mode 只保证是 JSON；strict 保证符合 schema（在支持范围内） | `TestUnconstrainedModelBreaksSchema` |
| 约束解码 | 每一步把“接上就没救”的 token 的 logit 设成 -∞ | `mask` |
| 前缀检查 | 输入用完返回 Prefix；分支每条都试；续体传递 | `grammar.go`、`TestCheckPrefixes` |
| EOS | 只有 Complete 时才允许结束 | `TestConstrainedRandomSamplingProperty` |
| token 边界 | 在“文本 + token”上检查，跨结构的 token 自然处理 | `TestConstrainedOutputAlwaysMatchesStructure` |
| 编译与缓存 | 首次请求有延迟；Anthropic 缓存 24 小时；太复杂会 400 | 第 5 节 |
| 分布扭曲 | 不给退路就只能编；推理字段放前面 | `TestConstrainedForcesAChoice` |
| 结构 vs 语义 | 语法管结构，校验器管值 | `TestGrammarDoesNotEnforceSemantics` |
| 保证失效 | 拒答、截断、enum 大小写、schema 被放宽 | `TestConstrainedTruncatedByMaxTokens`、`NormalizeEnums` |
| 三家规则 | OpenAI 全部 required；Anthropic 无数字范围、无递归；Gemini 最宽松 | `CheckOpenAI`、`CheckAnthropic` |
| 转换 | 一份真正的 schema，按厂商转换，用原 schema 校验 | `ForOpenAI`、`ForAnthropic` |
| 抽取循环 | 拒答截断不重试；带路径的错误喂回去；2–3 次为限 | `Extract` |

---

## 练习（今天做完）

练习目录：`projects/08-structured-output/`。

```bash
cd projects/08-structured-output
make test      # go vet + go test -race -v，20 个测试
```

| 文件 | 内容 |
|---|---|
| `ordered.go` | 保序的 JSON 对象 `Obj`、`ParseOrdered`（数字保留为 `json.Number`） |
| `schema.go` | JSON Schema 子集的编译与校验，返回带路径的全部错误 |
| `grammar.go` | 前缀检查器：约束解码的“语法” |
| `decode.go` | 解码循环、`mask`、玩具模型 `ScriptModel`、词表 |
| `strict.go` | 三家 strict 规则检查，`ForOpenAI`、`ForAnthropic` 转换 |
| `extract.go` | `Extract` 抽取循环、`NormalizeEnums` |
| `structured_test.go` | 20 个测试 |

### 练习 1：读输出，做两个实验

1. 运行 `make test`，找到四行 `t.Logf`。用一句话分别说明：为什么“JSON 合法”不等于“符合 schema”；为什么加了约束的输出内容会变成乱码；模型想说 `unsure` 时发生了什么；随机抽样测试证明了什么。
2. 在 `TestConstrainedForcesAChoice` 的 enum 里加上 `"unknown"`，再把模型的目标改成 `{"answer":"unknown"}`，观察输出。再把目标改回 `unsure`，输出变成了什么？为什么？
3. 把 `TestConstrainedOutputAlwaysMatchesStructure` 里的模型目标去掉所有空格、把 `age` 改成数字 `30`、`role` 改成 `admin`、删掉 `nickname`、补上 `"email":null,"tags":[]`，再看输出。对比原来的乱码，说明“模型的意图和语法一致时”约束解码几乎没有代价。

### 练习 2：改坏它，看哪个测试失败

每次只改一处，跑 `go test`，记录失败的测试名和原因，然后改回来：

1. `grammar.go` 的 `str` 里删掉 `case r < 0x20: return Invalid`；
2. `fields` 里把最后四行（可选字段的跳过分支）改成只有 `return take`；
3. `mask` 里删掉对 EOS 的限制（把 `if !done { ... }` 换成 `_ = done`）。为什么连 `TestConstrainedDeadEnd` 也失败了？
4. `Extract` 里把 `errs := sc.Validate(v)` 改成 `errs := []ValidationError(nil)`；
5. `ForOpenAI` 里把 `props[i].Val = nullable(kv.Val)` 改成 `props[i].Val = kv.Val`；
6. `schema.go` 的对象校验里删掉对 `AdditionalAllowed` 的检查；
7. `Extract` 里不调用 `stripFence`；
8. `number` 里把处理前导零的 `if c.s[i] == '0' {...} else {...}` 换成只有那个 `for` 循环。

### 练习 3：把 maxLength 和 minLength 放进语法

现在 `str` 里有一个变量 `n` 记录已经生成的字符数，但没用上。修改 `str`，让：

- 字符数已经达到 `MaxLength` 时，下一个字符只能是结束引号；
- 字符数还没到 `MinLength` 时，不许输出结束引号；
- 转义序列（`\n`、`\u00e9`）算一个字符。

新建 `maxlen_test.go`，写两个测试：

- `TestCheckStringLength`：schema 是 `{"type":"string","minLength":2,"maxLength":3}`，断言 `"a"` 是 Invalid、`"ab"` 和 `"abc"` 是 Complete、`"abcd` 是 Invalid、`"中文字"` 是 Complete、`"\n\t"` 是 Complete、四个 `\u00e9` 是 Invalid；
- `TestConstrainedRespectsMaxLength`：`title` 的 `maxLength` 是 5，模型想输出 `{"title":"a very long title"}`，断言约束解码的结果正好是 `{"title":"a ver"}`。

做完想一想：为什么 Anthropic 选择不在语法里支持 `maxLength`？（提示：想想 `maxLength: 10000` 会让状态机变成什么样。）

### 选做：增量检查

现在每一步、每个候选 token 都从头重新解析整个前缀，开销是 O(词表 × 长度)，一次生成就是 O(词表 × 长度²)。给 `checker` 加一个“可恢复的状态”：先把已生成的前缀解析一次，得到一组“下一步可能的解析状态”，再只在这些状态上尝试每个 token。写一个 benchmark，对比 200 个 token 的输出两种实现的耗时。这就是 Outlines、XGrammar 预编译状态机要解决的问题。

---

## 自测题

1. 提示词要求“只输出 JSON”、JSON mode、strict 模式，三者分别保证什么？各举一个它们防不住的失败。
2. 约束解码在每一步具体做了什么？它和 1.1 课的 logit bias 有什么关系？
3. 前缀检查返回 Prefix 和 Complete 的区别是什么？这个区别在解码循环里用在哪？
4. 为什么要在“已生成文本 + token”上做检查，而不是单独检查 token？举一个跨结构 token 的例子。
5. 为什么同一个 schema 第一次请求更慢？Anthropic 的语法缓存在什么情况下会失效？
6. 为什么 OpenAI 和 Anthropic 都要求 `additionalProperties: false`？OpenAI 为什么还要求所有字段都 required？想表达“可选”怎么办？
7. schema 是 `{"answer": {"enum": ["yes","no"]}}`，模型其实不知道答案。会发生什么？怎么改 schema？
8. 下面两个 schema 哪个更好？为什么？在 Anthropic 上还要注意什么？
   `{"answer": string, "reasoning": string}` 和 `{"reasoning": string, "answer": string}`
9. strict 模式下，哪四种情况仍然会拿到不符合 schema 的结果？分别怎么识别、怎么处理？
10. schema 里有 `"age": {"type": "integer", "minimum": 0}`，发给 Anthropic 会怎样？SDK 是怎么处理的？你的代码还要做什么？
11. 统一客户端为什么需要“一份真正的 schema + 按厂商转换”，而不是写一份三家都能用的 schema？
12. `Extract` 循环里，拒答和截断为什么不重试？校验错误为什么要带 JSON 路径、而且要返回全部错误？
13. 推理模型需要深度思考，结果又要结构化，有哪两种做法？
14. Zod 的 `z.infer` 能从 schema 推导出 TS 类型，Go 为什么做不到？Go 里通常怎么保证 schema 和 struct 一致？

---

# 参考答案与详解

## 自测题答案

**1.** 提示词：什么都不保证，常见失败是前面加一句“好的”、包代码围栏、尾逗号。JSON mode：保证输出是合法 JSON，但不保证结构，常见失败是少字段、类型错（`"age": "30"`）、多字段；而且忘了在提示词里要求 JSON 时可能输出无尽的空白。strict 模式：在 schema 的支持范围内保证结构正确，防不住的是拒答、截断、enum 大小写（Anthropic），以及所有语义约束（`end_date` 晚于 `start_date`）。

**2.** 每一步拿到模型给整个词表的 logits 后，对每个候选 token 判断“已生成文本 + 这个 token”是否还能被续写成合法文档，不能的就把 logit 设成 -∞，然后照常采样。它是 logit bias 的极端形式：logit bias 是加一个有限的数，概率变小但不为 0；约束解码加的是 -∞，概率严格为 0。

**3.** Prefix：合法但还没结束，后面必须继续写；Complete：已经是完整合法的值（有时还能继续，比如顶层数字）。解码循环用它控制 EOS：只有当前文本是 Complete 时才允许选结束 token。不加这条限制，模型可能在中途结束，得到半截 JSON（练习 2 第 3 条）。

**4.** 真实分词器的 token 经常跨越 JSON 结构，比如 `",` 同时结束一个字符串并输出逗号，`":"` 包含键的结束、冒号和下一个字符串的开始。单独看 token 无法判断它是否合法，必须放在上下文里。在拼接后的文本上检查，所有跨结构的 token 都自然被正确处理。

**5.** 服务端要把 schema 编译成一个在 token 上运行的状态机（预先算好每个状态下哪些 token 合法），编译需要时间，之后会缓存。Anthropic 的缓存从最后一次使用起保留 24 小时，修改 schema 结构、或者修改请求里的工具集合（同时用结构化输出和工具时）会让缓存失效，只改 `name` 或 `description` 不会。

**6.** 允许额外属性，语法就要接受任意键名和任意类型的值，约束就形同虚设，而且状态空间会爆炸。OpenAI 要求全部 required，是因为每个可选字段都会让语法多一条“跳过”的分支，组合起来状态数指数增长；全部必填后语法只有一条路。表达“可选”的方法是可空：`"type": ["string", "null"]`，模型必须输出这个键，没有值就填 `null`。读结果的代码要能处理 `null`。

**7.** 约束解码不会报错，模型只能在 `yes` 和 `no` 里选一个，你得到一个格式完美但可能是瞎猜的答案，而且没有任何信号告诉你它是猜的。改法：给退路，`enum` 里加 `"unknown"`，或者用可空类型并在 `description` 里说明“信息不足时填 null”；需要的话再加一个 `confidence` 字段。

**8.** 第二个更好。模型是自回归的，先生成的内容影响后生成的内容。先写 `answer` 再写 `reasoning`，推理只能为已定的答案找补；先写 `reasoning`，答案才能建立在推理之上。OpenAI strict 按 schema 的键顺序输出，所以顺序直接生效。Anthropic 上要注意：输出是“必填字段在前、可选字段在后”，所以 `reasoning` 要设为必填才能保证排在前面；另外不要要求“完整的逐步推理过程”，可能触发 `reasoning_extraction` 拒答，改为要“简短解释”。

**9.** 拒答：Anthropic 是 `stop_reason: "refusal"`（HTTP 200），OpenAI 是 `refusal` 类型的内容，不解析，交给上层处理。截断：`max_tokens` / `incomplete`，输出是合法前缀但不完整，调大上限重发。enum 大小写：Anthropic 文档说明不保证，校验失败但 stop 正常，忽略大小写比较、用 `NormalizeEnums` 修正并记录、避免只差大小写的 enum 值。schema 被放宽：发出去的是转换后的 schema，结果可能违反原 schema，只能用原 schema 校验发现。

**10.** Anthropic 不支持数字约束，原样发送会返回 400。SDK 的做法：从发送的 schema 里删掉 `minimum`，在字段描述里写上 `{minimum: 0}` 提示模型，拿到响应后用原 schema 校验。你的代码（如果不用 SDK 的 parse 辅助）必须自己用原 schema 再校验一次：`-5` 能通过发出去的 schema，但不符合你的业务要求。

**11.** 三家的规则互相冲突：OpenAI 要求全部 required（可选要用可空表达），Anthropic 不支持递归和数字范围，Gemini 允许真正的可选字段。写一份“三家都能用”的 schema，只能取三者的交集：没有递归、没有数字范围、全部必填……表达力损失太大，而且你真正的业务约束就丢了。正确做法是一份完整的、表达业务含义的 schema，按厂商转换成它能接受的形状（`ForOpenAI`、`ForAnthropic`），校验永远用原始那份。

**12.** 拒答：换格式重问一般改变不了结果，还要再付一次钱；应该交给上层（提示、换模型、记录）。截断：用同样的上限重试，大概率还是截断，要调大上限或缩短输出。错误带路径，模型才知道改哪里（`$.items[2].price: must be >= 0` 比“格式错误”精确得多）；返回全部错误，模型一次能改完，不用一个错误重试一轮。

**13.** 一：用推理模型，让它在 thinking 里自由思考，最终回答受 schema 约束（Anthropic 文档明确说语法不约束 thinking 块）。二：分两步，先让模型自由回答，再用一个便宜的模型把回答抽取成结构化数据。哪种更好、值不值得多一次调用，用 Eval 决定。

**14.** TypeScript 的类型系统是结构化的，Zod 的 schema 对象在类型层面携带了完整的结构信息，`z.infer` 在编译期把它“读”出来。Go 的类型必须在编译前写出来，运行时构造的值无法变成一个新类型。Go 里通常反过来：以 struct 为准，用反射（读 struct 字段和 tag）或代码生成从 struct 生成 JSON Schema，再加一个测试保证生成结果和提交的 schema 文件一致。

## 练习参考答案

### 练习 1

第 2 题：加了 `"unknown"` 且目标是 `unknown` 时，输出就是 `{"answer":"unknown"}`，模型的意图和语法一致。目标改回 `unsure` 时，输出变成 `{"answer":"unknown"}`：`u` 开头的候选只剩 `unknown`，`un` 两个字符对上了目标，模型一路被引导进了“unknown”。这正是“给退路”的效果：模型不知道时，有一个诚实的出口。

第 3 题：目标改成符合 schema 的紧凑 JSON 后，加约束的输出和目标完全一样。约束解码的代价只在“模型想走的路被堵住”时出现；模型本来就想输出合法结果时，mask 掉的都是它本来就不会选的 token。

### 练习 2

| 改动 | 失败的测试 | 原因 |
|---|---|---|
| 1. 删掉控制字符检查 | `TestCheckOptionalFieldsAndStrings` | 字符串里的裸换行被当成合法，实际 JSON 不允许 |
| 2. 删掉可选字段的跳过分支 | `TestCheckOptionalFieldsAndStrings` | `{"a":"x","c":true}` 跳过了可选的 `b`，被判成 Invalid |
| 3. EOS 不受限制 | `TestConstrainedOutputAlwaysMatchesStructure`、`TestConstrainedRandomSamplingProperty`、`TestConstrainedDeadEnd` | 前两个：模型可以在中途结束，得到半截 JSON。第三个：词表里没有能表达 `é` 的 token，本来所有候选都被屏蔽、返回 `ErrDeadEnd`；现在 EOS 总是可选，于是“正常结束”了，返回了一个不完整的结果，错误被掩盖 |
| 4. 不校验 | `TestExtractRetriesWithFeedback`、`TestExtractRefusalTruncationAndGiveUp` | `age: -1` 和缺字段的结果被直接接受 |
| 5. 可选字段不改成可空 | `TestForOpenAI` | `summary` 变成必填但不可空，模型被迫编造一个值；测试断言它必须是 `["string","null"]` |
| 6. 不检查额外属性 | `TestValidateCollectsAllErrors` | `extra` 没被报出来 |
| 7. 不去围栏 | `TestExtractRetriesWithFeedback` | 第二次回复被当成不合法 JSON，反馈内容和断言不符 |
| 8. 不处理前导零 | `TestCheckPrefixes`、`TestConstrainedRandomSamplingProperty` | `01` 被判为合法前缀；随机抽样里出现 `"id":00` 这样的输出，`ParseOrdered` 解析失败 |

第 3 条值得多想一下：一个“兜底”式的放宽（总允许结束）不仅产生错误结果，还会把本来能被检测出来的错误（`ErrDeadEnd`）藏起来。

### 练习 3

`grammar.go` 的 `str` 里，在循环的 `switch` 开头加两处判断：

```go
		case r == '"':
			if sc.MinLength != nil && n < *sc.MinLength {
				return Invalid // 还没到最短长度，不许收尾
			}
			return k(i + 1)
		case sc.MaxLength != nil && n >= *sc.MaxLength:
			return Invalid // 已经够长了，只能收尾
		case r < 0x20:
			return Invalid
```

**逐行解读**

- **判断放在读到字符的时候**：`n` 是已经接受的字符数。读到结束引号时检查下限；读到任何其他字符（包括转义序列的开头 `\`）时检查上限：`n >= max` 说明再加一个就超了。
- **转义算一个字符**：转义分支吃掉 `\n` 两个字节或 `\u00e9` 六个字节，最后统一 `n++` 一次，所以按“解码后的字符”计数，和 `Validate` 里 `utf8.RuneCountInString` 的口径一致。
- **case 的顺序**：结束引号的判断必须在上限判断之前，否则字符数正好等于上限时，连结束引号都会被拒绝，进入死胡同。

`maxlen_test.go`：

```go
package structured

import "testing"

func TestCheckStringLength(t *testing.T) {
	sc := MustCompileJSON(`{"type":"string","minLength":2,"maxLength":3}`)
	for in, want := range map[string]Status{
		`"a"`: Invalid, `"ab"`: Complete, `"abc"`: Complete, `"abcd`: Invalid,
		`"中文字"`: Complete, `"\n\t"`: Complete, `"\u00e9\u00e9\u00e9\u00e9`: Invalid,
	} {
		if got := sc.Check(in); got != want {
			t.Errorf("Check(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestConstrainedRespectsMaxLength(t *testing.T) {
	sc := MustCompileJSON(`{"type":"object","properties":{"title":{"type":"string","maxLength":5}},"required":["title"],"additionalProperties":false}`)
	out, finish, err := Generate(ScriptModel{`{"title":"a very long title"}`}, ByteVocab(), sc, 100, nil)
	if err != nil || finish != "stop" || out != `{"title":"a ver"}` {
		t.Fatalf("out=%s finish=%s err=%v", out, finish, err)
	}
}
```

第二个测试的结果说明了语法层长度约束的副作用：模型的标题被**硬截断**在第 5 个字符，而不是写一个更短的标题。语义上更好的做法往往是在描述里写“不超过 5 个字”，让模型自己规划，再用校验兜底。

**为什么 Anthropic 不在语法里支持 maxLength**：长度上限要求状态机“数数”。`maxLength: 10000` 意味着字符串内部要区分 10001 种状态，和其他字段组合后，编译出的状态机会非常大，编译时间和缓存成本都受不了。把它降级成描述加事后校验，是一个工程取舍。

---

## 完成标准

- [ ] `make test` 20 个测试全部通过，能说出每个测试在验证什么；
- [ ] 练习 1：三个实验都做了，能解释“意图和语法冲突”与“意图和语法一致”时输出的差别；
- [ ] 练习 2：八处改动都做过，尤其能解释第 3 条为什么会让 `TestConstrainedDeadEnd` 失败；
- [ ] 练习 3：`maxLength` / `minLength` 进了语法，`maxlen_test.go` 两个测试通过 `go test -race`；
- [ ] 能不看课件写出三家结构化输出的请求（参数位置、`strict`、schema 规则）；
- [ ] 能把一个带可选字段和 `minimum` 的 schema 手动转换成 OpenAI 和 Anthropic 能接受的形状；
- [ ] 自测题 14 道都能讲清楚，尤其是第 3、6、7、9、11 题。

做到以上几条，1.3 就可以勾选。下一课 **1.4 推理模型**：reasoning effort、thinking budget、推理 token 的成本，什么任务值得开推理，以及推理块在多轮工具调用里的生命周期。
