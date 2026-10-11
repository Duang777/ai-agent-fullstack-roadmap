# 1.1 Transformer、Tokenizer 与采样参数：从一段文本到下一个 token

> 第 1 部分 · 第 1 课 ｜ 预计 5–6 小时 ｜ 前置：0.1 Go、0.4 流式协议
>
> 配套练习：`projects/06-llm-basics/`（纯 Go，无第三方依赖，21 个测试）。手写字节级 BPE 分词器、采样器、因果注意力和 KV Cache，再用一个最小的 bigram 模型把它们串成完整的生成循环。

## 读前说明

做 Agent 的人不需要会训练大模型，但必须知道一次 API 调用里到底发生了什么。下面这些问题每天都会遇到，答案全在这一课：

- 为什么中文比英文贵？为什么同一段话换个模型 token 数就变了？
- 为什么流式输出里会偶尔冒出 `�`？
- 为什么输出 token 比输入 token 贵好几倍？为什么上下文越长越慢、越贵？
- `temperature=0` 为什么还是每次不一样？为什么新模型干脆不让设 `temperature` 了？
- 模型为什么会陷入“同一句话无限重复”？

这一课分四部分：**全景 → 分词器 → Transformer → 采样**。每一部分都对应练习里一段能跑、有测试的 Go 代码。数学只讲到能写代码为止。

本课结束时你能：

1. 画出“文本 → token → 向量 → N 层 Transformer → logits → 采样 → 下一个 token”的完整链路，说清楚 prefill 和 decode 的区别；
2. 手算 BPE 的合并过程，自己实现字节级 BPE 的训练、编码、解码；
3. 解释 token 对 Agent 工程的影响：计费、上下文预算、中文成本、流式乱码；
4. 写出缩放点积注意力和因果掩码，解释为什么要除以 √d，为什么 KV Cache 有效，以及它占多少显存；
5. 实现 temperature、top-k、top-p、min-p、重复惩罚和 logit bias，知道它们的处理顺序；
6. 知道 2026 年三家主流 API 对采样参数的实际限制，以及“temperature=0 也不确定”的真实原因。

---

# 第一部分：全景

## 1. 一次调用的完整链路

你调用一次 `messages.create`，服务端做的事情可以压缩成下面这张图：

```
"智能体调用工具"                          ← 你发的文本（加上 system、历史消息、工具定义）
   │ ① Tokenizer：文本 → token id
   ▼
[101, 2203, 5521, 873]                    ← 整数序列
   │ ② Embedding：每个 id 查表得到一个向量（比如 4096 维）
   ▼
4 × 4096 的矩阵
   │ ③ N 层 Transformer（每层：注意力 + 前馈网络）
   ▼
4 × 4096 的矩阵（每个位置“读过”前文之后的表示）
   │ ④ 取最后一个位置，乘输出矩阵 → 词表上每个 token 一个分数
   ▼
logits：长度 = 词表大小（比如 200k）的一组实数
   │ ⑤ 采样：temperature / top-p / … → 选出一个 id
   ▼
next id = 3317（“，”）
   │ ⑥ 追加到序列末尾，回到 ③；直到遇到结束 token 或达到 max_tokens
```

记住三个关键事实：

- **模型只做一件事**：给定前面所有 token，输出“下一个 token”在整个词表上的分数。其他一切（对话、工具调用、JSON 输出、思考）都是在这件事上搭出来的。
- **模型看不见文字**，只看见 token id。所以“strawberry 里有几个 r”这种题难，是因为模型看到的是 `str` `aw` `berry` 三个整数，而不是十个字母。
- **采样不是模型的一部分**。模型给出分数，怎么从分数里选，是推理引擎根据你的参数决定的。这就是练习里 `Model` 接口只有一个 `Logits` 方法的原因。

## 2. 自回归：为什么输出要一个一个吐

第 ⑥ 步决定了生成是**自回归**的：第 10 个 token 要等第 9 个出来才能算。练习里的生成循环就是这件事的全部：

```go
// projects/06-llm-basics/lm.go
func Generate(m Model, prompt []int, maxTokens int, stop map[int]bool,
	p SamplingParams, rng *rand.Rand) GenerateResult {
	ctx := append([]int(nil), prompt...)
	var out []int
	for range maxTokens {
		next := Sample(m.Logits(ctx), ctx, p, rng)
		if stop[next] {
			return GenerateResult{IDs: out, FinishReason: "stop"}
		}
		out = append(out, next)
		ctx = append(ctx, next)
	}
	return GenerateResult{IDs: out, FinishReason: "length"}
}
```

逐行看：

- `ctx := append([]int(nil), prompt...)`：复制一份 prompt。直接 `ctx := prompt` 再 `append`，可能写进调用方切片的底层数组（0.1 讲过的切片共享坑）。
- `for range maxTokens`：Go 1.22 起可以 `range` 一个整数。`maxTokens` 就是 API 里的 `max_tokens` / `max_output_tokens`，是**硬上限**，不是期望长度。
- `m.Logits(ctx)`：每一步都把“prompt + 已生成”整段交给模型。真正的推理引擎不会每次重算整段，靠的是第 14 节的 KV Cache。
- `Sample(..., ctx, ...)`：历史也传给采样器，因为重复惩罚要知道哪些 token 出现过。
- 两种返回对应 API 里的 `finish_reason` / `stop_reason`：`"stop"` 是模型自己说完了（或碰到停止序列），`"length"` 是被 `max_tokens` 截断。**Agent 必须检查这个字段**：被截断的工具调用参数是半截 JSON，直接 `json.Unmarshal` 会失败。

这个循环解释了两个工程现象：

| 现象 | 原因 |
|---|---|
| 首 token 延迟（TTFT）和输入长度相关 | 输入的所有 token 可以**一次并行**算完，这一步叫 **prefill**；输入越长，prefill 越久 |
| 输出速度（tokens/s）基本恒定，输出越长总时间线性增长 | 之后每个 token 只能**一步一步**算，这一步叫 **decode** |
| 输出单价通常是输入的数倍 | prefill 一次能喂满 GPU 算力；decode 每步只算一个 token，GPU 大部分时间在等显存读权重，单位 token 的成本高得多 |
| 流式输出是“自然的” | token 本来就是一个一个出来的，SSE 只是把它们按出生顺序推给你（0.4 课） |

---

# 第二部分：Tokenizer

## 3. 为什么不按字、也不按词切

词表大小是一个权衡：

| 方案 | 词表 | 问题 |
|---|---|---|
| 按字节 | 256 | 序列太长：一个汉字 3 个 token，上下文窗口和算力都浪费在“拼字”上 |
| 按字符 | Unicode 有十几万个字符 | 罕见字符训练不到；英文一个单词拆成五六个字母，仍然很长 |
| 按词 | 几十万到无穷 | 新词、拼写错误、代码标识符永远切不出来，只能变成 `<unk>` |
| **子词（BPE 等）** | 常见 5 万–25 万 | 常见词一个 token，罕见词拆成片段，任何输入都能表示 |

当前主流模型都用子词。OpenAI 的 `cl100k_base`（GPT-4 时代）词表约 10 万，`o200k_base`（GPT-4o 之后）约 20 万。

## 4. BPE 训练：手算一遍

BPE（Byte Pair Encoding）的训练只有一句话：**反复把出现次数最多的相邻两个 token 合并成一个新 token，直到词表够大。**

用教科书例子 `aaabdaaabac` 手算：

```
初始：a a a b d a a a b a c
相邻对计数：aa ×4，ab ×2，ba ×1，bd ×1，da ×1，ac ×1
第 1 次合并：aa → Z          Z a b d Z a b a c
相邻对计数：Za ×2，ab ×2，… → 并列取第一个
第 2 次合并：Za → Y          Y b d Y b a c
第 3 次合并：Yb → X          X d X a c
```

学到三条规则：`aa→Z`、`Za→Y`、`Yb→X`，规则的**顺序**就是它们的优先级。练习里有一个测试专门验证第一条：

```go
func TestBPEFirstMerge(t *testing.T) {
	tok, err := Train("aaabdaaabac", 257) // 256 个字节 + 1 条合并
	...
	if got := string(tok.TokenBytes(256)); got != "aa" { ... }
}
```

训练的核心循环：

```go
// projects/06-llm-basics/bpe.go
for next := 256; next < vocabSize; next++ {
	stats := map[Pair]int{}
	for i, s := range seqs {
		for j := 0; j+1 < len(s); j++ {
			stats[Pair{s[j], s[j+1]}] += counts[words[i]]
		}
	}
	best, ok := mostFrequent(stats)
	if !ok {
		break
	}
	t.merges[best] = next
	t.vocab = append(t.vocab, concat(t.vocab[best.A], t.vocab[best.B]))
	for i := range seqs {
		seqs[i] = merge(seqs[i], best, next)
	}
}
```

- `next := 256`：0–255 留给单个字节，新 token 的 id 从 256 开始递增。**id 越小，学得越早，优先级越高**，编码时会用到这一点。
- `stats[...] += counts[words[i]]`：语料先被切成片段（第 6 节），相同片段只存一份，计数乘上出现次数。真实分词器在几百 GB 语料上训练，这个优化必不可少。
- `mostFrequent`：次数并列时选 `(A,B)` 最小的。Go 的 map 遍历顺序是随机的，不做这一步，同一份语料每次会学出不同的词表。`TestBPETrainDeterministic` 验证的就是这个。
- `t.vocab = append(..., concat(...))`：新 token 的字节串就是两个旧 token 字节串的拼接。解码只需要查这张表。
- 每学一条规则就把所有序列重新合并一遍。这是 O(合并数 × 语料长度) 的朴素实现，练习够用；生产实现（如 Hugging Face `tokenizers`）用优先队列和增量更新。

## 5. 字节级：永远没有 `<unk>`

练习的初始词表是 256 个**字节**，不是字符。好处是任何输入都能编码：中文、emoji、二进制垃圾，最坏情况也就是每个字节一个 token。

```go
func TestBPERoundTrip(t *testing.T) {
	cases := []string{
		"智能体调用工具，然后读取结果。",
		"emoji 也行 🤖🚀",
		string([]byte{0xff, 0xfe, 0x00}), // 非法 UTF-8 也要原样还原
		...
	}
	for _, s := range cases {
		if got := tok.Decode(tok.Encode(s)); got != s { ... }
	}
}
```

代价也来自这里：**一个 token 不一定是完整的字符**。第 9 节会看到，这会直接导致流式输出出现乱码。

## 6. 预分词：合并不跨空格

如果直接在整段文本上跑 BPE，会学出 `"dog. The"` 这种横跨标点和空格的怪 token，而 `"dog."` 和 `"dog!"` 里的 `dog` 反而学不成同一个。所以训练前先用正则把文本切成片段，**合并只在片段内部发生**：

```go
var pretokenRe = regexp.MustCompile(`'s|'t|'re|'ve|'m|'ll|'d| ?\p{L}+| ?\p{N}+| ?[^\s\p{L}\p{N}]+|\s+`)
```

- ` ?\p{L}+`：可选的**一个前导空格** + 一串字母（`\p{L}` 包括汉字）。所以英文 token 通常长这样：`" model"`、`" the"`，空格贴在词的前面。
- ` ?\p{N}+`：数字单独成段。GPT-4 的规则更进一步，把数字限制成最多 3 位一组，下面的实测里 `1234567890…` 被切成 `123` `456` `789`，就是这个规则的结果。
- `'s|'t|…`：英文缩写单独切。

这是 GPT-2 规则的简化版。练习里训练 600 词表时学到的前 5 条合并是 `" t"`、`"he"`、`" a"`、`" the"`、一个汉字的前两个字节 `e4 b8`，可以看到空格总是在前面。`TestBPENoMergeAcrossSpaces` 检查所有学到的 token 都不会把空格夹在中间。

## 7. 编码：按学到的顺序合并

编码不是“找当前最频繁的 pair”（编码时没有统计数据），而是**在所有能合并的 pair 里，选学得最早（id 最小）的那条规则**：

```go
for len(seq) >= 2 {
	bestID, bestPair := -1, Pair{}
	for j := 0; j+1 < len(seq); j++ {
		p := Pair{seq[j], seq[j+1]}
		if id, ok := t.merges[p]; ok && (bestID == -1 || id < bestID) {
			bestID, bestPair = id, p
		}
	}
	if bestID == -1 {
		break
	}
	seq = merge(seq, bestPair, bestID)
}
```

为什么必须按顺序？看 `TestBPEEncodeFollowsMergeRank`：先学会 `bc`（256），后学会 `ab`（257）。编码 `"abc"`：

- 按规则顺序：先用 `bc`，得到 `a` + `bc`，和训练时的切法一致；
- 如果“从左往右遇到哪个就合并哪个”：先碰到 `ab`，得到 `ab` + `c`。

两种结果都能无损解码，所以往返测试发现不了这个 bug。但模型在训练时只见过第一种切法，第二种切法的 token 序列对它来说是“没见过的写法”，效果会变差。这类 bug 只能用专门的测试抓。

## 8. 真实分词器：实测

用 OpenAI 的 `tiktoken` 实测（练习目录里的 `tokens.py`，`uv run tokens.py` 即可复现）：

| 文本 | UTF-8 字节 | cl100k_base | o200k_base |
|---|---|---|---|
| `Hello, how are you today?` | 25 | 7 | 7 |
| `The quick brown fox jumps over the lazy dog.` | 44 | 10 | 10 |
| `你好，今天过得怎么样？` | 33 | 12 | 7 |
| `人工智能体需要可靠的工具调用。` | 45 | 16 | 9 |
| ` 12345678901234567890` | 21 | 8 | 8 |
| `strawberry` | 10 | 3（`str` `aw` `berry`） | 3（`st` `raw` `berry`） |

几个结论：

- **英文两代分词器几乎一样**，大约 4 个字符一个 token。
- **中文差别很大**：o200k 把“人工”“智能”“需要”“可靠”“工具”“调用”都学成了单个 token，同一句话少了 44% 的 token。换句话说，**同样的中文内容，换模型可能便宜近一半，上下文也能多装近一倍**。
- cl100k 甚至把“智”拆成了两个 token：`e6 99` 和 `ba`。单独看任何一个都不是合法 UTF-8。
- 练习里的小分词器（语料 2 KB，词表 400）更极端：英文 0.43 token/字符，中文 1.57 token/字符（`TestChineseCostsMoreTokens` 会打印）。原因一样：训练语料以英文为主，中文的合并学得少。

## 9. 对 Agent 工程的直接影响

**计费和预算一律按 token 算。** 上下文窗口、价格、每分钟限流（TPM）、`max_tokens` 都是 token 数。做预算控制时：

- 不要用“字符数 ÷ 4”估算中文，实测会低估 2–4 倍；
- 不同厂商、不同代际的分词器不同，同一段文本的 token 数不能互相换算；
- 要精确计数，用厂商提供的计数接口（Anthropic 和 Gemini 都有 count tokens 接口，OpenAI 可以用 `tiktoken` 离线算），或者直接读响应里的 `usage` 字段。第 07 模块的成本指标就基于 `usage`。

**token 边界不是字符边界，流式输出必须缓冲。** 服务端每生成一个 token 就推一个 SSE 事件。如果某个 token 是半个汉字，直接 `string(bytes)` 推给前端，就会出现 `�`。练习里的 `StreamDecoder` 把不完整的 UTF-8 尾巴扣在缓冲里，等下一个 token 补齐：

```go
func (d *StreamDecoder) Push(id int) string {
	d.buf = append(d.buf, d.tok.vocab[id]...)
	n := completePrefix(d.buf)
	out := string(d.buf[:n])
	d.buf = append(d.buf[:0], d.buf[n:]...)
	return out
}

func completePrefix(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-3; i-- {
		c := b[i]
		if c&0xC0 == 0x80 { // 10xxxxxx：续字节，继续往前找
			continue
		}
		need := 0
		switch {
		case c&0x80 == 0x00:
			need = 1
		case c&0xE0 == 0xC0:
			need = 2
		case c&0xF0 == 0xE0:
			need = 3
		case c&0xF8 == 0xF0:
			need = 4
		default:
			return len(b)
		}
		if len(b)-i < need {
			return i
		}
		return len(b)
	}
	return len(b)
}
```

- UTF-8 编码规则：起始字节的高位告诉你这个字符一共几个字节（`0xxxxxxx` 1 个，`110xxxxx` 2 个，`1110xxxx` 3 个，`11110xxx` 4 个），后续字节都是 `10xxxxxx`。
- 所以只要从尾部往前找到最后一个起始字节，看它后面的字节够不够数：不够，就扣住从它开始的部分；够，就全部输出。
- 最多往回看 3 个字节：一个字符最多 4 字节，结尾那个没收完的字符最多已经收到 3 个字节。
- `d.buf = append(d.buf[:0], d.buf[n:]...)`：把剩下的尾巴挪到缓冲开头，复用底层数组，不重新分配内存。
- `default: return len(b)`：碰到非法起始字节（比如 `0xff`），不要无限等待，直接输出，交给 `string()` 变成 `U+FFFD`。

`TestStreamDecoderNeverSplitsRunes` 用纯字节词表（每个汉字必然拆成 3 个 token）验证：每次 `Push` 返回的都是合法 UTF-8，拼起来等于原文；同时证明“不缓冲直接输出”一定会产生非法 UTF-8。

**你在 TS 客户端里也会遇到同一个问题**，不过这时是网络分块（0.4 课的 `ReadableStream`）切断了字符。浏览器和 Node 的 `TextDecoder` 内置了同样的缓冲：

```ts
const decoder = new TextDecoder();                      // ① 默认 UTF-8
for await (const chunk of response.body!) {             // ② chunk: Uint8Array
  const text = decoder.decode(chunk, { stream: true }); // ③ 扣住不完整的尾巴
  render(text);
}
render(decoder.decode());                               // ④ 流结束，吐出剩余字节
```

对照 Go 逐行看：

| TS | Go（练习） | 说明 |
|---|---|---|
| ① `new TextDecoder()` | `NewStreamDecoder(tok)` | 创建一个带内部缓冲的解码器；TS 版输入是字节，Go 版输入是 token id，再查表成字节 |
| ② `for await (const chunk of response.body!)` | 调用方的 `for _, id := range ids` | 一块一块地喂；`!` 是告诉 TS 编译器 `body` 不为 null |
| ③ `decoder.decode(chunk, { stream: true })` | `d.Push(id)` | `stream: true` 就是“后面还有数据，结尾不完整的字符先别解”，等价于 `completePrefix` 加缓冲 |
| ④ `decoder.decode()` | `d.Flush()` | 不带参数调用表示流结束，剩下的字节全部输出，不完整的变成 `U+FFFD` |

常见错误是每个 chunk `new TextDecoder().decode(chunk)`，或者忘了 `{ stream: true }`。在英文测试里永远发现不了，一到中文就出 `�`。

---

# 第三部分：Transformer

这部分不推导训练，只讲推理时的数据流，以及和 Agent 工程直接相关的几个结论：注意力为什么是 O(n²)，KV Cache 为什么能省算力却吃显存，为什么长上下文贵。

## 10. 嵌入：把 id 变成向量

每个 token id 查一张 `词表大小 × d` 的表，得到一个 d 维向量（d 常见 2048–8192）。这张表是训练学出来的，意思相近的 token，向量也相近。

向量本身不带位置信息：“猫追狗”和“狗追猫”的三个向量完全一样。所以要注入位置。现在主流模型用 **RoPE（旋转位置编码）**：不是给向量加一个位置向量，而是在算注意力之前，把 q 和 k 按位置旋转一个角度，使得 q·k 只和两个位置的**相对距离**有关。你需要知道的工程结论是：上下文窗口能从 8K 扩到 128K、1M，很大程度上靠的是对 RoPE 的插值和外推技巧；超出训练长度太多，效果会明显退化（1.5 课讲长上下文退化）。

## 11. 注意力：每个位置去前文里“查资料”

注意力的直觉：每个位置拿着一个**问题**（query）去问前文所有位置，每个位置有一个**标签**（key）和一份**内容**（value）。问题和标签越匹配，就从那份内容里拿越多。

公式：

```
Attention(Q, K, V) = softmax(Q·Kᵀ / √d) · V
```

Q、K、V 都是 n×d 矩阵（n 是序列长度），由输入向量分别乘三个学到的矩阵得来。练习里的实现：

```go
// projects/06-llm-basics/attention.go
func Attention(q, k, v Matrix, causal bool) (Matrix, Matrix) {
	n, d := len(q), len(q[0])
	scale := 1 / math.Sqrt(float64(d))
	weights := make(Matrix, n)
	out := make(Matrix, n)
	for i := range n {
		scores := make([]float64, len(k))
		for j := range k {
			if causal && j > i {
				scores[j] = math.Inf(-1)
				continue
			}
			scores[j] = dot(q[i], k[j]) * scale
		}
		w := Softmax(scores)
		weights[i] = w
		out[i] = make([]float64, len(v[0]))
		for j := range v {
			if w[j] == 0 {
				continue
			}
			for t := range v[j] {
				out[i][t] += w[j] * v[j][t]
			}
		}
	}
	return out, weights
}
```

- 外层 `for i`：每个位置算一次。内层 `for j`：和每个位置的 key 做点积。**两层循环就是 O(n²)**：上下文从 8K 到 128K，长 16 倍，注意力的计算量涨 256 倍。这是长上下文贵的根本原因之一。
- `dot(q[i], k[j]) * scale`：点积衡量“问题和标签有多匹配”，然后乘 1/√d（下一段解释）。
- `Softmax(scores)`：把分数变成和为 1 的权重。
- 最后一段：输出是所有 value 的加权和。权重为 0 的位置直接跳过。
- 同时返回 `weights`，方便测试检查“未来位置的权重是不是严格为 0”。

**为什么除以 √d？** q 和 k 的每一维大致是均值 0、方差 1 的随机数，d 维点积的方差是 d，标准差是 √d。d=512 时点积动辄几十，softmax 之后几乎变成 one-hot：只看一个位置，其他全是 0，训练时梯度也几乎为 0。除以 √d 把方差拉回 1。`TestScalingKeepsSoftmaxSoft` 实测：d=512、32 个位置，不缩放时注意力分布的熵只有 **0.148 bit**（几乎只看一个位置），缩放后是 **4.35 bit**（上限是 log₂32 = 5）。

## 12. 因果掩码：只能看过去

生成模型（GPT、Claude、Gemini 都是 decoder-only）的位置 i **只能看到 0..i**。实现就是那一行：

```go
if causal && j > i {
	scores[j] = math.Inf(-1)
	continue
}
```

-∞ 经过 softmax 变成严格的 0（`Softmax` 先减最大值再取 exp，`exp(-Inf) = 0`）。`TestCausalMask` 用最直接的方式验证：修改最后一个位置的 K、V，前 5 个位置的输出必须完全不变；去掉掩码（`causal=false`，BERT 那样的双向注意力）之后，第 0 个位置的输出会跟着变。

因果掩码带来两个结论：

1. **训练可以并行**：整段文本一次喂进去，每个位置预测下一个 token，掩码保证它看不到答案。
2. **前文的计算结果不会因为后文而改变**，所以可以缓存。这就是 KV Cache 和 Prompt Caching 成立的前提。

## 13. 一层 Transformer 还有什么

单个注意力只是一层的一半。完整的一层大致是：

```
x = x + Attention(RMSNorm(x))     ← 多头注意力：在序列方向上交换信息
x = x + FFN(RMSNorm(x))           ← 前馈网络：在每个位置内部做变换，大部分参数在这里
```

- **多头（multi-head）**：把 d 维切成 h 份，每份独立算一套注意力再拼起来。不同的头可以关注不同的关系（语法、指代、代码括号匹配……）。
- **GQA（分组查询注意力）**：多个 query 头共享一组 K、V 头。比如 32 个 query 头只配 8 个 KV 头，KV Cache 直接变成 1/4。Llama 3、Qwen 等主流开源模型都用它。
- **残差连接 `x = x + ...`**：每层只学“增量”，几十上百层叠起来也能训练。
- **RMSNorm**：把向量缩放到稳定的数值范围。
- **FFN**：通常是“升维 4 倍左右 → 激活函数 → 降维”，现在多用 SwiGLU。MoE 模型（如 DeepSeek、Qwen3 的 MoE 版本）把 FFN 换成几十上百个“专家”，每个 token 只激活其中几个，所以“总参数”和“激活参数”是两个数。

把这样的层叠 N 次（8B 模型约 32 层，70B 约 80 层），最后一层输出乘一个 `d × 词表大小` 的矩阵，就得到 logits。

## 14. KV Cache：用显存换算力

生成第 t 个 token 时，前 t-1 个位置的 K、V 在上一步已经算过，而且因果掩码保证它们不会变。所以只需要算新 token 自己的 q、k、v，把 k、v 追加进缓存，再用这一个 q 去看缓存：

```go
func (c *KVCache) Step(q, k, v []float64) []float64 {
	c.K = append(c.K, k)
	c.V = append(c.V, v)
	scale := 1 / math.Sqrt(float64(len(q)))
	scores := make([]float64, len(c.K))
	for j := range c.K {
		scores[j] = dot(q, c.K[j]) * scale
	}
	w := Softmax(scores)
	...
}
```

- 不需要掩码：缓存里只有过去和现在，天然满足因果。
- 每一步的计算量从“整段重算”降到“和缓存里每个位置做一次点积”。
- `TestKVCacheMatchesFull` 验证增量计算的每一步都和整段计算的对应行完全一致（误差 < 1e-9）。**这种“优化前后结果一致”的测试，是写任何性能优化时都该有的。**

代价是显存。每个 token 要在每一层存一份 K 和一份 V：

```
KV Cache 字节数 = 2 × 层数 × KV 头数 × 头维度 × token 数 × 每个数的字节数
```

| 模型规格（GQA, fp16） | 每 token | 32K 上下文 | 128K 上下文 |
|---|---|---|---|
| 8B 级：32 层、8 KV 头、头维度 128 | 128 KiB | 4 GiB | 16 GiB |
| 70B 级：80 层、8 KV 头、头维度 128 | 320 KiB | 10 GiB | 40 GiB |

（第一行就是练习里 `KVCacheBytes(32, 8, 128, 1, 2)` 的断言值。）

这张表解释了很多工程现象：

- **一个 128K 的长请求可能独占一张卡的大部分显存**，所以长上下文请求的并发上不去，单价也更高；
- **Prompt Caching 的本质**就是把这些 K、V 留在服务端，下次同样的前缀直接复用，跳过 prefill（1.5 课详细讲）。所以缓存命中要求**前缀完全一致**，system prompt 里放一个时间戳就会让缓存全部失效；
- vLLM 的 PagedAttention、SGLang 的 RadixAttention 都是在管理这块显存（09 模块）。

---

# 第四部分：采样

## 15. 从 logits 到概率：数值稳定的 softmax

```go
func Softmax(logits []float64) []float64 {
	maxL := math.Inf(-1)
	for _, l := range logits {
		maxL = math.Max(maxL, l)
	}
	probs := make([]float64, len(logits))
	if math.IsInf(maxL, -1) {
		return probs
	}
	sum := 0.0
	for i, l := range logits {
		probs[i] = math.Exp(l - maxL)
		sum += probs[i]
	}
	for i := range probs {
		probs[i] /= sum
	}
	return probs
}
```

- 公式是 `p_i = exp(l_i) / Σ exp(l_j)`。
- **先减最大值**：`exp(1000)` 在 float64 里是 `+Inf`，`Inf/Inf = NaN`。减去最大值后最大的那项是 `exp(0)=1`，不会溢出，而结果数学上完全不变（分子分母同乘 `exp(-max)`）。`TestSoftmaxStable` 用 `[1000, 1001, 1002]` 验证；把 `- maxL` 删掉，这个测试立刻失败。
- `if math.IsInf(maxL, -1)`：所有 token 都被过滤成 -∞ 时返回全 0，而不是 `0/0 = NaN`。

## 16. 温度：把分布拉尖或压平

```go
if p.Temperature > 0 && p.Temperature != 1 {
	for i := range out {
		out[i] /= p.Temperature
	}
}
```

logits 除以 T 再做 softmax：

- **T < 1**：差距被放大，高概率 token 更高，分布更“尖”，输出更保守、更重复；
- **T = 1**：模型原始分布；
- **T > 1**：差距被缩小，低概率 token 也有机会，输出更多样，也更容易胡说；
- **T → 0**：退化成 argmax，也就是贪心。所以 `Sample` 里 `Temperature == 0` 单独走贪心分支，避免除以 0。

`TestTemperatureEntropy` 验证温度从 0.2 升到 3，分布的熵单调增大。

## 17. 截断：top-k、top-p、min-p

温度只改变形状，不会把任何 token 的概率变成 0。词表有 20 万个 token，长尾里大量“完全不合理”的 token 加起来可能占几个百分点，采样几百步总会抽中一个。截断就是把长尾直接砍掉。

**top-k**：只保留分数最高的 k 个。问题是 k 固定：模型很确定时（下一个字几乎只能是“。”），k=40 仍然留了 39 个垃圾；模型不确定时（开放式写作），k=40 又可能太少。

**top-p（nucleus）**：按概率从高到低累加，保留**累计概率刚好达到 p 的最小集合**：

```go
if p.TopP > 0 && p.TopP < 1 {
	probs := Softmax(out)
	idx := argsortDesc(probs)
	cum := 0.0
	for _, i := range idx {
		if cum >= p.TopP {
			out[i] = math.Inf(-1)
			continue
		}
		cum += probs[i]
	}
}
```

注意判断在**累加之前**：只要前面还没凑够 p，当前这个就保留，哪怕加上它之后超过了 p。也就是说“跨过阈值的那一个”要留下。`TestTopP` 的例子：概率 0.5、0.3、0.15、0.05，`top_p=0.6` 时保留前两个（0.5 不够，加上 0.3 才够）。如果写成“先累加，超过就删”，跨过阈值的 0.3 会被删掉，只剩一个，测试会失败。这是最常见的 top-p 实现 bug。

**min-p**：保留概率 ≥ `min_p × 最大概率` 的 token。阈值随模型的把握自动缩放：

```go
func TestMinPAdaptsToConfidence(t *testing.T) {
	confident := 概率 [0.9, 0.05, 0.03, 0.02]   // min_p=0.1 → 阈值 0.09 → 只留第一个
	unsure    := 概率 [0.3, 0.28, 0.22, 0.2]    // min_p=0.1 → 阈值 0.03 → 全部保留
}
```

min-p 在开源推理引擎（vLLM、llama.cpp、SGLang）里很常用，闭源 API 一般不开放。

## 18. 惩罚与 logit bias

```go
for id, n := range seen {
	if p.RepetitionPenalty > 0 && p.RepetitionPenalty != 1 {
		if out[id] > 0 {
			out[id] /= p.RepetitionPenalty
		} else {
			out[id] *= p.RepetitionPenalty
		}
	}
	out[id] -= p.PresencePenalty + p.FrequencyPenalty*float64(n)
}
```

三种惩罚来自两个流派：

| 参数 | 来源 | 做法 | 效果 |
|---|---|---|---|
| `presence_penalty` | OpenAI | 出现过就减一个固定值 | 鼓励换话题 |
| `frequency_penalty` | OpenAI | 出现 n 次减 n 倍 | 抑制同一个词反复出现 |
| `repetition_penalty` | HF / vLLM | 乘法：正 logit 除以 r，负 logit 乘以 r | 两种情况都让它更不可能 |

乘法惩罚为什么要分正负？如果对 -1 也“除以 2”，得到 -0.5，反而变大了。`TestPenaltiesAndBias` 用 `[2, 2, -1, 0]` 检查了这一点。

**logit bias** 直接给指定 token 的分数加偏置，-100 约等于禁用，+100 约等于强制。Agent 里的用法：禁止输出某些 token，或者做分类时只允许“是”“否”两个 token。注意它是按 **token id** 指定的，换模型（换分词器）就要重新查 id。

一个容易漏的点：**温度为 0 时 bias 也要生效**。练习里的贪心分支先走一遍 `Process`（只开 bias 和惩罚），再取 argmax。如果直接 `Argmax(logits)`，`TestPenaltiesAndBias` 最后一个断言会失败。

## 19. 处理顺序

```go
// Process 按推理引擎的常见顺序处理 logits：
//  1. logit bias → 2. 重复/存在/频率惩罚 → 3. 温度 → 4. top-k → 5. top-p → 6. min-p
```

规律是**先改分数，再调形状，最后截断**。顺序会影响结果：top-p 在温度之后做，同样的 `top_p=0.9` 在高温下会保留更多 token。不同引擎的顺序不完全一样（比如有的把 min-p 放在温度之前），所以**同一组参数在 vLLM 和 llama.cpp 里效果可能不同**。

## 20. 贪心的问题：重复循环

练习的 bigram 模型（只看上一个 token）在 500 词表上，以 `"The model"` 为开头生成 25 个 token，实测：

| 参数 | 输出 |
|---|---|
| `temperature=0` | ` wabababababababababababab`（被 `max_tokens` 截断） |
| `temperature=0, frequency_penalty=1` | ` wabie`（遇到句号停止） |
| `temperature=0.7, top_p=0.9` | ` as a sency is measured in tokens` |
| `temperature=1.5`，不截断 | ` or\xbb\x93构笡\xe5est…`（乱码和随机汉字） |

这四行就是采样参数的全部故事：

1. **贪心会陷入循环**：`a` 后面最可能是 `b`，`b` 后面最可能是 `a`，于是永远 `abab`。真实大模型看的是整段上下文，不会这么傻，但同样存在“一句话无限重复”的退化，尤其在长输出和低温度下。
2. **惩罚能打破循环**，但也会误伤必须重复的东西（代码里的变量名、JSON 里的键名），所以 Agent 场景一般不开或只开很小。
3. **适度的温度加截断**最接近语料的样子。
4. **高温不截断**会从长尾里抽出垃圾。练习的模型给每个没见过的组合留了 `1e-3` 的平滑概率，几百个这样的 token 加起来，高温下就很容易被抽中，而且字节级 token 抽出来就是乱码。这就是 top-p 存在的理由。

## 21. temperature=0 不等于确定

很多人以为 `temperature=0` 就能保证每次输出一样，然后在 Eval 里用它做“确定性”回归测试。实际不行。

Thinking Machines Lab 在 2025 年 9 月发表的《Defeating Nondeterminism in LLM Inference》做了实验：用 Qwen3-235B，在 temperature 0 下对同一个 prompt 采样 1000 次，每次 1000 个 token，得到 **80 种不同的输出**。前 102 个 token 全部相同，第 103 个 token 开始分叉：992 次生成了 “Queens, New York”，8 次生成了 “New York City”。

原因不是常说的“GPU 并行导致浮点误差随机”（单个 kernel 在固定输入形状下其实是确定的），而是**服务端的批大小随负载变化**：你的请求和多少个别人的请求拼在一个 batch 里，决定了矩阵乘法和归约的切分方式，浮点加法顺序随之改变，最后一位的误差在几十层里放大，某一步两个 token 的分数恰好接近时就翻转了。他们换成“批不变”的 kernel 之后，1000 次输出完全一致，代价是吞吐下降。

工程结论：

- **不要把“输出逐字相同”当成 Agent 测试的断言。** 第 06 模块会讲：确定性的部分用假模型和录制回放测（就像这一课的 bigram 和下面练习 3 的脚本模型），概率性的部分用 Eval 统计成功率；
- 有的 API 提供 `seed` 参数，也只承诺“尽力而为”。

## 22. 2026 年各家 API 的实际情况

本地推理引擎几乎所有参数都能调，但主流闭源 API 的趋势是**越来越不让调**，尤其是推理模型：

| 厂商 | 现状 |
|---|---|
| OpenAI | GPT-5 系列的推理模型只有在 `reasoning.effort` 为 `none` 时才接受 `temperature`、`top_p`、`logprobs`；开启推理时传这些参数会直接返回 400。替代手段是 `reasoning.effort`、`text.verbosity` 和 `max_output_tokens` |
| Anthropic | 从 Claude Opus 4.1、Sonnet 4.5 这一代起，`temperature` 和 `top_p` 不能同时指定；Claude 4.7 及之后的模型不再支持 `temperature`、`top_p`、`top_k`，设成非默认值返回 400，官方建议用 prompt 引导行为 |
| Google | Gemini 3 的 `temperature` 范围是 0–2，但官方“强烈建议”保持默认值 1.0，调低可能导致循环或在复杂推理任务上性能下降 |

为什么？推理模型在训练（强化学习）时就是在默认温度下采样思考过程的，换个温度等于让它在没练过的分布上推理；温度太低还容易陷入第 20 节那种循环。

对 Agent 代码的直接要求：

1. **不要在公共代码里硬编码 `temperature: 0`**。换一个模型名，请求就会 400。很多开源项目的 issue 都是这么来的。
2. 采样参数应该是**按模型配置**的：模型能力表里记录“支持哪些参数、默认值是多少”，请求构造时只发支持的参数。第 11 模块的模型网关会做这件事。
3. 想要“稳定”，靠结构化输出（1.3 课）、更好的 prompt 和 Eval，而不是靠温度。

## 23. 回到生成循环：停止条件

除了 `max_tokens` 和模型的结束 token，API 还支持**停止序列**（`stop` / `stop_sequences`）：输出里出现指定字符串就停。它有两个实现细节，下面的练习 3 会让你亲手踩一遍：

- 停止序列是**字符串**，可能横跨多个 token（`</answer>` 在大多数分词器里是 3–4 个 token），所以要在解码后的文本上匹配，不能在 token id 上匹配；
- 流式输出时，如果已经把 `</answer>` 的前 5 个字节推给了用户，后面才发现是停止序列，就收不回来了。所以要在末尾**扣住**最多“停止序列长度 - 1”个字节，确认不是停止序列的开头再推出去。

---

## 本课小结

| 知识点 | 一句话 | 练习里的代码 |
|---|---|---|
| 自回归 | 每一步只出一个 token，prefill 并行、decode 串行 | `Generate` |
| BPE 训练 | 反复合并最频繁的相邻对，顺序即优先级 | `Train`、`mostFrequent` |
| BPE 编码 | 按学到的顺序合并，不是从左到右 | `Encode`、`TestBPEEncodeFollowsMergeRank` |
| 字节级 | 永远没有 `<unk>`，但 token 可能是半个字符 | `TestBPERoundTrip` |
| 流式解码 | 扣住不完整的 UTF-8 尾巴 | `StreamDecoder`、TS 的 `{ stream: true }` |
| 注意力 | softmax(QKᵀ/√d)V，O(n²) | `Attention` |
| 因果掩码 | 只看过去，-∞ 变成严格的 0 | `TestCausalMask` |
| KV Cache | 缓存过去的 K、V，用显存换算力 | `KVCache`、`KVCacheBytes` |
| softmax | 先减最大值 | `Softmax` |
| 温度 | 除以 T，T→0 退化为贪心 | `Process`、`Sample` |
| top-p | 保留累计概率达到 p 的最小集合，跨阈值那个要留 | `TestTopP` |
| min-p | 阈值随最大概率缩放 | `TestMinPAdaptsToConfidence` |
| 惩罚 | 加法（OpenAI）与乘法（HF），乘法要分正负 | `TestPenaltiesAndBias` |
| 不确定性 | temperature=0 也会变，根源是批大小 | 第 21 节 |
| API 现状 | 推理模型越来越不让调采样参数 | 第 22 节 |

---

## 练习（今天做完）

练习目录：`projects/06-llm-basics/`。

```bash
cd projects/06-llm-basics
make test        # go vet + go test -race -v
make tokens      # 用 tiktoken 对比 cl100k_base 和 o200k_base（需要 uv）
```

### 练习 1：读懂测试输出，做三个小实验

1. 运行 `make test`，找到三行 `t.Logf` 打印：中英文 token/字符比、注意力熵、贪心生成结果。每一行用一句话解释它说明了什么。
2. 修改 `TestChineseCostsMoreTokens` 里的词表大小（400 → 1000 → 2000），记录中文 token/字符比的变化。到某个词表大小之后为什么降不下去了？（提示：语料只有 2 KB。）
3. 运行 `make tokens`，再往 `tokens.py` 里加三段你自己的文本：一段 Go 代码、一段 JSON 工具调用参数、一段中英混排。记录两个分词器的 token 数，看哪一类文本两代分词器差别最大。

### 练习 2：改坏它，看哪个测试失败

每次只改一处，跑 `go test`，记录失败的测试名和原因，然后改回来：

1. `Softmax` 里把 `math.Exp(l - maxL)` 改成 `math.Exp(l)`；
2. top-p 循环改成“先 `cum += probs[i]`，如果 `cum > p.TopP` 就删掉当前 token”；
3. `StreamDecoder.Push` 里把 `n := completePrefix(d.buf)` 改成 `n := len(d.buf)`；
4. `Attention` 里删掉 `causal && j > i` 的判断（提示：会有两个测试失败）；
5. `Sample` 的贪心分支改成直接 `return Argmax(logits)`；
6. `Encode` 里把条件改成 `ok && bestID == -1`（只用第一个遇到的可合并 pair）。想一想：为什么 `TestBPERoundTrip` 还能通过？

### 练习 3：跨 token 的停止序列 + 流式推送

在 `lm.go` 里新增：

```go
// GenerateText 生成文本，遇到 stops 中任意一个字符串就停止（停止序列本身不输出）。
// 每当有可以安全推给用户的文本时调用 emit。
// 返回完整输出（不含停止序列）和 finish_reason（"stop" 或 "length"）。
func GenerateText(m Model, tok *Tokenizer, prompt []int, maxTokens int, stops []string,
	p SamplingParams, rng *rand.Rand, emit func(string)) (string, string)
```

要求：

- 停止序列可能横跨多个 token；
- `emit` 推出去的内容拼起来必须等于返回的文本，**不能包含停止序列的任何前缀片段**；
- `emit` 推出去的每一段都必须是合法 UTF-8；
- 写一个测试：用一个“按脚本依次输出指定 token”的假模型，脚本内容是 `答案是42。</answer>后面不该出现`，用纯字节分词器（`newByteTokenizer()`），停止序列 `</answer>`。断言返回 `答案是42。` 和 `"stop"`，并且没有任何一段 `emit` 包含 `<`。

### 选做：top-k 用堆实现

现在的 top-k 对整个词表排序，O(V log V)。真实词表 20 万，每步都排序太浪费。用 `container/heap` 维护一个大小为 k 的最小堆，改成 O(V log k)，写一个 benchmark 对比 V=200000、k=50 时两种实现的速度。

---

## 自测题

1. 为什么说“模型不是一个字一个字地读”？用 `strawberry` 解释为什么数字母这类题对模型难。
2. prefill 和 decode 分别是什么？为什么输入很长时首 token 延迟会变长，而输出的 tokens/s 基本不变？
3. 为什么输出 token 的单价通常比输入高好几倍？
4. BPE 训练时为什么要处理“次数并列”的情况？不处理会怎样？
5. BPE 编码时为什么要按合并规则的学习顺序，而不是从左到右遇到就合并？两种做法都能无损还原，错误的那种到底错在哪？
6. 同一句中文在 cl100k_base 是 16 个 token，在 o200k_base 是 9 个。这对 Agent 的成本和上下文预算意味着什么？为什么不能用“字符数 ÷ 4”估 token？
7. 流式输出里偶尔出现 `�` 的根本原因是什么？服务端和客户端分别应该怎么处理？
8. 注意力为什么要除以 √d？不除会怎样？
9. KV Cache 为什么不会改变输出结果？一个 70B 级模型（80 层、8 KV 头、头维度 128、fp16）跑 128K 上下文，KV Cache 大约多大？
10. Prompt Caching 为什么要求“前缀完全一致”？在 system prompt 开头放当前时间会有什么后果？
11. top-p=0.6，概率分别是 0.5、0.3、0.15、0.05，保留哪些？为什么跨过阈值的那个要保留？
12. 为什么 `repetition_penalty` 对正负 logit 要分别处理？
13. `temperature=0` 为什么仍然可能每次输出不同？这对 Agent 的测试策略有什么影响？
14. 你的 Agent 框架在所有请求里都写死了 `temperature: 0`。换成 Claude 4.7 之后的模型或开启推理的 GPT-5 系列模型，会发生什么？应该怎么改？

---

# 参考答案与详解

## 自测题答案

**1.** 模型的输入是 token id，不是字符。`strawberry` 在 cl100k_base 里是 `str` `aw` `berry` 三个整数，在 o200k_base 里是 `st` `raw` `berry`。模型从来没有直接“看到”过十个字母，要回答“有几个 r”，它必须从训练数据里间接学到“`berry` 这个 token 里有两个 r”这种知识，然后再加起来。这和人看图数东西不是一回事。现在的推理模型能答对，靠的是在思考过程中把单词逐字母拼出来（每个字母成为单独的 token），再数。

**2.** prefill 是把输入的所有 token 一次性送进模型，算出每个位置每一层的 K、V（存进 KV Cache），以及最后一个位置的 logits，从而得到第一个输出 token。因为所有输入 token 同时已知，可以组成大矩阵并行计算，但总计算量和输入长度成正比（注意力部分甚至是平方），所以输入越长，首 token 越晚。decode 是之后每一步只处理一个新 token：算它的 q、k、v，和 KV Cache 做注意力，得到下一个 token。每步的工作量差不多（只是缓存略微变长），所以输出速度基本恒定。

**3.** decode 每一步只为一个 token（或者一个 batch 里每个请求各一个 token）做计算，但要把整个模型的权重从显存读一遍。这时瓶颈是显存带宽而不是算力，GPU 的计算单元大部分时间在等数据。prefill 则是一次处理成千上万个 token，同一份权重读一次用很多次，算力利用率高得多。所以生成一个输出 token 占用的 GPU 时间远多于处理一个输入 token，定价自然高。

**4.** 并列时如果不定规则，选哪个就取决于遍历顺序。Go 的 map 遍历顺序每次都是随机的，所以同一份语料每次训练会学出不同的合并规则和词表，这意味着同一段文本在两次训练出的分词器里得到不同的 id，模型和分词器就对不上了。练习里用“`(A,B)` 字典序最小”打破并列，`TestBPETrainDeterministic` 训练两次逐个比较词表。

**5.** 训练时，每条规则都是在“前面的规则已经全部应用过”的序列上学出来的。编码时只有按同样的顺序应用规则，得到的切分才和模型训练时见过的切分一致。从左到右遇到就合并，会产生训练时从未出现过的切法（练习里 `abc` 被切成 `ab`+`c` 而不是 `a`+`bc`）。这两种切法解码后都是 `abc`，所以往返测试发现不了；但对模型来说，`[ab, c]` 是一个它在训练里几乎没见过的 token 序列，等于给它一段“拼写奇怪”的输入，理解和生成质量都会下降。这种 bug 只能用专门构造的测试来抓。

**6.** token 数直接决定三件事：钱（按 token 计费）、上下文能装多少（窗口按 token 算）、限流（TPM 按 token 算）。同样的中文内容，o200k 比 cl100k 少约 44% 的 token：费用少近一半，同样的窗口能多装近一倍的历史或检索结果。“字符数 ÷ 4”是英文的经验值（英文约 4 个字符一个 token）；中文在 o200k 下约 1.5 个字符一个 token，在 cl100k 下接近 1 个字符一个 token，在训练语料偏英文的小分词器里甚至一个字要好几个 token。用英文经验值估中文，会把 token 数低估 2–4 倍，导致上下文超限或预算失控。（Go 里 `len(text)` 还是字节数而不是字符数，一个汉字算 3，又是另一种偏差。）正确做法是用厂商的计数接口或离线分词库，事后以响应里的 `usage` 为准。

**7.** 字节级 BPE 的 token 边界不保证落在 UTF-8 字符边界上，一个汉字的 3 个字节可能分属两个 token（cl100k 把“智”拆成 `e6 99` 和 `ba`）。服务端如果每个 token 单独转字符串推送，半个字符就会被替换成 `U+FFFD`，也就是 `�`。另外，网络分块也可能在字符中间切开。处理方法：服务端解码时扣住不完整的 UTF-8 尾巴，等下一个 token 补齐（练习的 `StreamDecoder`）；客户端用 `TextDecoder` 的 `{ stream: true }` 模式（或 Go 里用 `bufio` 按完整字符读），流结束时再 flush 一次。

**8.** q、k 每一维近似均值 0、方差 1，d 维点积的方差是 d。d 很大时点积的绝对值很大，softmax 会极度偏向最大的那一项，接近 one-hot。后果是：每个位置只“看”一个位置，信息混合能力很差；训练时 softmax 在饱和区，梯度接近 0，学不动。除以 √d 让点积方差回到 1 左右。练习实测 d=512 时，不缩放的注意力熵是 0.148 bit，缩放后是 4.35 bit（上限 5）。

**9.** 因果掩码保证位置 i 的 K、V 只依赖 0..i 的输入，后面生成什么都不会改变它们，所以缓存下来复用和重新计算的结果在数学上完全相同（`TestKVCacheMatchesFull` 验证误差 < 1e-9）。大小：`2 × 80 × 8 × 128 × 2 字节 = 327,680 字节 = 320 KiB` 每 token；128K token（131,072）× 320 KiB = 40 GiB。这只是一个请求的缓存，还不算模型权重本身（70B fp16 约 140 GB）。

**10.** Prompt Caching 缓存的是前缀对应的 KV Cache。位置 i 的 K、V 取决于 0..i 的所有 token，只要前缀中任何一个 token 不同，从那个位置往后的所有 K、V 都会不同，缓存就无法复用。在 system prompt 开头放当前时间，每次请求的第一段就不一样，后面所有内容（工具定义、长文档、历史消息）的缓存全部失效，每次都要重新 prefill，既慢又贵。正确做法是把不变的内容放前面、变化的内容（时间、用户输入）放最后。

**11.** 保留 0.5 和 0.3。按从高到低累加：拿到 0.5 时累计 0.5 < 0.6，还没够，继续；拿到 0.3 时累计 0.8 ≥ 0.6，这一个也要保留，因为没有它就凑不够 0.6；之后的 0.15、0.05 全部丢掉。top-p 的定义是“累计概率达到 p 的**最小**集合”，跨过阈值的那个元素正是让集合达到 p 的必要成员。如果把它丢掉，保留的集合累计概率只有 0.5，不满足定义；在极端情况下（第一个 token 概率就超过 p），错误实现会把所有 token 都删掉。

**12.** 惩罚的目的是“让已经出现过的 token 更不可能”，也就是让 logit 变小。正 logit 除以 r（r>1）会变小，没问题；但负 logit 除以 r 会变大（-1 / 2 = -0.5），反而更可能被选中，所以负数要乘以 r（-1 × 2 = -2）。加法惩罚（presence/frequency）不存在这个问题，因为减一个正数总是变小。

**13.** 服务端会把很多用户的请求拼在一个 batch 里一起算，batch 的大小随负载变化。batch 大小会改变矩阵乘法、归一化、注意力 kernel 的切分和归约方式，浮点加法顺序随之变化，产生最后几位的差异。这些差异在几十层里放大，当某一步最高的两个 token 分数非常接近时，argmax 就可能翻转，之后的输出完全分叉。Thinking Machines 的实验里，1000 次 temperature 0 采样得到 80 种结果。影响：Agent 的测试不能断言“输出逐字相同”。确定性逻辑（循环控制、工具调度、解析）用假模型或录制回放来测；模型行为用 Eval 在多次运行上统计成功率和稳定性（pass^k），这是第 06 模块的内容。

**14.** 请求会直接失败：Claude 4.7 及之后的模型对非默认的 `temperature` 返回 400；GPT-5 系列在开启推理时不接受 `temperature`，同样返回 400。Gemini 3 虽然接受，但官方建议保持 1.0，设成 0 可能导致循环或推理质量下降。改法：把采样参数从业务代码里拿出来，放进按模型维护的配置（模型能力表），记录每个模型支持哪些参数和推荐默认值；构造请求时只发送该模型支持且明确配置了的参数，默认什么都不发，让厂商用它自己的默认值。需要“稳定输出”时，用结构化输出、更清晰的 prompt 和 Eval 来保证，而不是依赖温度。

## 练习参考答案

### 练习 1

1. 三行日志的含义：
   - `tokens/字符：英文 0.43，中文 1.57`：在以英文为主的小语料上训练的分词器，英文平均 2–3 个字符合成一个 token，中文平均一个字要 1.5 个 token 以上（一个汉字 3 个字节，只有少数常见字被合并）。这就是“分词器偏向训练语料的语言”。
   - `d=512 时注意力熵：不缩放 0.148 bit，缩放后 4.350 bit`：不缩放时注意力几乎只看一个位置，缩放后在 32 个位置上分布得比较均匀（上限 5 bit）。
   - `贪心：" wabababab…"（length）`：bigram 模型在贪心下陷入两 token 循环，直到 `max_tokens` 才停。
2. 词表从 400 增大到 1000、2000，中文 token/字符比会先明显下降，之后基本不动。原因是语料只有 2 KB，里面的中文只有几百个字，能学的合并很快就学完了：所有片段都已经合并成单个 token，`mostFrequent` 返回 `ok=false`，训练提前结束（可以打印 `VocabSize()` 验证，词表根本到不了 2000）。真实分词器的中文效率取决于训练语料里有多少中文，这正是 o200k 比 cl100k 对中文好得多的原因。
3. 一般会看到：英文代码两代差别不大；JSON 里的英文键名差别也不大；中英混排和纯中文差别最大。如果你的 Agent 主要处理中文，选模型时 token 效率是一个实打实的成本因素。

### 练习 2

| 改动 | 失败的测试 | 原因 |
|---|---|---|
| 1. 去掉 `- maxL` | `TestSoftmaxStable` | `exp(1000)` 溢出成 `+Inf`，`Inf/Inf = NaN` |
| 2. top-p 先累加再判断 | `TestTopP` | 跨过阈值的 token 被删掉，`top_p=0.6` 只剩一个 |
| 3. 不扣住尾巴 | `TestStreamDecoderNeverSplitsRunes` | 半个汉字被直接输出，`utf8.ValidString` 失败 |
| 4. 去掉因果判断 | `TestCausalMask`、`TestKVCacheMatchesFull` | 过去位置看到了未来；整段计算变成双向注意力，和只看过去的增量计算对不上 |
| 5. 贪心直接 argmax | `TestPenaltiesAndBias` | `temperature=0` 时 logit bias 失效，被禁用的 token 仍然被选中 |
| 6. 只用第一个可合并 pair | `TestBPEEncodeFollowsMergeRank` | `abc` 被切成 `ab`+`c` |

第 4 条的第二个失败值得注意：`TestKVCacheMatchesFull` 本来是测 KV Cache 的，但它把“整段计算”当作标准答案。标准答案错了，测试也会失败。这说明**等价性测试同时保护了两边**。

第 6 条里 `TestBPERoundTrip` 还能通过，是因为解码只是把每个 token 的字节拼起来，不管怎么切，拼回去都是原文。往返测试只能证明“无损”，不能证明“切法正确”。

### 练习 3

```go
// GenerateText 支持字符串级停止序列，并通过 emit 流式推送文本。
func GenerateText(m Model, tok *Tokenizer, prompt []int, maxTokens int, stops []string,
	p SamplingParams, rng *rand.Rand, emit func(string)) (string, string) {
	ctx := append([]int(nil), prompt...)
	var text []byte
	sent := 0 // text[:sent] 已经推给用户
	hold := 0 // 末尾最多要扣住的字节数
	for _, s := range stops {
		hold = max(hold, len(s)-1)
	}
	flush := func(upto int) {
		upto = sent + completePrefix(text[sent:upto]) // 不在汉字中间切
		if upto > sent {
			emit(string(text[sent:upto]))
			sent = upto
		}
	}
	for range maxTokens {
		next := Sample(m.Logits(ctx), ctx, p, rng)
		ctx = append(ctx, next)
		text = append(text, tok.TokenBytes(next)...)

		from := max(0, sent-hold) // 更早的部分已经确认不含停止序列
		for _, s := range stops {
			if i := strings.Index(string(text[from:]), s); i >= 0 {
				cut := from + i
				flush(cut)
				return string(text[:cut]), "stop"
			}
		}
		flush(max(sent, len(text)-hold))
	}
	if sent < len(text) {
		emit(string(text[sent:]))
	}
	return string(text), "length"
}
```

要点：

- **在字节上匹配，而不是在 token 上匹配**：每来一个 token 就把它的字节追加到 `text`，在文本上找停止序列。这样无论 `</answer>` 被切成几个 token、在哪里切，都能找到。
- **扣住 `len(stop)-1` 个字节**：`text` 末尾这么多字节可能是停止序列的开头，暂时不推。比如已经收到“答案是42。”加上 `</answer>` 的前 5 个字节，末尾 8 个字节先扣着；下一个 token 如果补成了 `</answer>`，就在 `<` 之前截断，用户永远看不到这半截标签。
- **`from := max(0, sent-hold)`**：只在可能包含新匹配的区域里搜索，避免每步扫描整段文本（O(n²)）。停止序列最早可能从“已发送位置往前 hold 字节”开始。严格来说，已发送的部分不可能包含停止序列的开头（否则当时就不会发送），这里多往前看一段只是保险。
- **`flush` 里再套一层 `completePrefix`**：扣住停止序列前缀之后，剩下要推的部分末尾仍然可能是半个汉字，两种扣留要叠加。
- **`finish_reason`**：找到停止序列返回 `"stop"`；跑满 `maxTokens` 返回 `"length"`，并把扣住的尾巴全部推出去。

测试：

```go
// script 是按脚本依次输出指定 token 的假模型：
// 让目标 token 的 logit 为 0，其他都是 -50，贪心必然选中它。
type script struct {
	ids []int
	i   int
}

func (s *script) Logits(ctx []int) []float64 {
	out := make([]float64, 256)
	for j := range out {
		out[j] = -50
	}
	out[s.ids[s.i]] = 0
	s.i++
	return out
}

func TestStopAcrossTokens(t *testing.T) {
	tok := newByteTokenizer()
	ids := tok.Encode("答案是42。</answer>后面不该出现")
	var chunks []string
	text, reason := GenerateText(&script{ids: ids}, tok, []int{1}, 100, []string{"</answer>"},
		SamplingParams{Temperature: 0}, rand.New(rand.NewPCG(1, 1)),
		func(s string) { chunks = append(chunks, s) })

	if text != "答案是42。" || reason != "stop" {
		t.Fatalf("got %q %s", text, reason)
	}
	if strings.Join(chunks, "") != text {
		t.Fatalf("emitted %q", chunks)
	}
	for _, c := range chunks {
		if strings.Contains(c, "<") || !utf8.ValidString(c) {
			t.Fatalf("bad chunk %q", c)
		}
	}
}
```

这个假模型是 Agent 测试里最常用的技巧（第 06 模块会系统讲）：**把模型换成一个按脚本输出的假实现，让概率性的东西变成确定性的**，于是“停止序列”“工具调用解析”“流式推送”这些纯逻辑都可以写成普通的单元测试。

### 选做：top-k 用堆

```go
type minHeap []struct {
	v float64
	i int
}

func (h minHeap) Len() int           { return len(h) }
func (h minHeap) Less(a, b int) bool { return h[a].v < h[b].v }
func (h minHeap) Swap(a, b int)      { h[a], h[b] = h[b], h[a] }
func (h *minHeap) Push(x any)        { *h = append(*h, x.(struct{ v float64; i int })) }
func (h *minHeap) Pop() any          { old := *h; x := old[len(old)-1]; *h = old[:len(old)-1]; return x }

func topKHeap(xs []float64, k int) []int {
	h := make(minHeap, 0, k)
	for i, v := range xs {
		if h.Len() < k {
			heap.Push(&h, struct{ v float64; i int }{v, i})
		} else if v > h[0].v { // 比堆里最小的大，就替换它
			h[0] = struct{ v float64; i int }{v, i}
			heap.Fix(&h, 0)
		}
	}
	out := make([]int, h.Len())
	for i, e := range h {
		out[i] = e.i
	}
	return out
}
```

堆里始终保存“目前最大的 k 个”，堆顶是其中最小的。每个元素只需和堆顶比一次，大于堆顶才替换并下沉，O(log k)。V=200000、k=50 时，排序要做约 V·log₂V ≈ 350 万次比较，堆约 V + (替换次数)·log₂k，通常快一个数量级。注意并列值的处理：排序版用 `SliceStable` 保证并列时取小下标，堆版不保证，需要的话在 `Less` 里加上下标比较。

---

## 完成标准

- [ ] `make test` 21 个测试全部通过，能说出每个测试在验证什么；
- [ ] 练习 1：记录了词表大小对中文 token 比的影响，并能解释为什么到某个大小后降不下去；用 `make tokens` 对比了自己的三段文本；
- [ ] 练习 2：六处改动都做过，每一处都能说出失败的测试和根本原因，尤其是第 4 条为什么有两个测试失败、第 6 条为什么往返测试发现不了；
- [ ] 练习 3：`GenerateText` 和 `TestStopAcrossTokens` 通过 `go test -race`；
- [ ] 能不看课件画出第 1 节的链路图，并在图上标出 prefill、decode、KV Cache、采样分别在哪一步；
- [ ] 自测题 14 道都能讲清楚，尤其是第 5、7、9、13、14 题；
- [ ] 能凭记忆写出数值稳定的 softmax 和正确的 top-p 循环。

做到以上几条，1.1 就可以勾选。下一课 **1.2 主流 API**：OpenAI Responses、Anthropic Messages、Gemini，三家的消息结构、工具调用、流式事件格式逐一对比，并用 Go 写一个统一的客户端接口。
