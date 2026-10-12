package structured

import (
	"errors"
	"math"
	"math/rand/v2"
)

// Model 是一个玩具语言模型：给定已经生成的文本，对词表里每个 token 打分（logits）。
type Model interface {
	Logits(prefix string, vocab []string) []float64
}

// EOS 是词表里的结束 token。约束解码时，只有当前文本已经 Complete 才允许选它。
const EOS = "<eos>"

var ErrDeadEnd = errors.New("structured: no token can extend the prefix (vocabulary cannot express the schema)")

// Generate 是普通的贪心解码：每步选分数最高的 token。sc 为 nil 时不加约束。
//
// 加约束时，每一步对每个候选 token 问一次 Check(text+token)：Invalid 的 logit 设成 -∞。
// 这就是 1.1 课 logit bias 的极端形式：不是“降低概率”，而是“概率严格为 0”。
func Generate(m Model, vocab []string, sc *Schema, maxTokens int, rng *rand.Rand) (text, finish string, err error) {
	for step := 0; step < maxTokens; step++ {
		logits := m.Logits(text, vocab)
		if sc != nil {
			mask(logits, vocab, text, sc)
		}
		id := pick(logits, rng)
		if id < 0 {
			return text, "", ErrDeadEnd
		}
		if vocab[id] == EOS {
			return text, "stop", nil
		}
		text += vocab[id]
	}
	return text, "length", nil // 撞上 max tokens：约束也救不了，JSON 是半截的
}

func mask(logits []float64, vocab []string, text string, sc *Schema) {
	done := sc.Check(text) == Complete
	for id, tok := range vocab {
		if tok == EOS {
			if !done {
				logits[id] = math.Inf(-1)
			}
			continue
		}
		if sc.Check(text+tok) == Invalid {
			logits[id] = math.Inf(-1)
		}
	}
}

// pick：rng 为 nil 时贪心（并列取小下标），否则按 softmax 抽样。全是 -∞ 返回 -1。
func pick(logits []float64, rng *rand.Rand) int {
	best, bi := math.Inf(-1), -1
	for i, l := range logits {
		if l > best {
			best, bi = l, i
		}
	}
	if bi < 0 || rng == nil {
		return bi
	}
	var sum float64
	p := make([]float64, len(logits))
	for i, l := range logits {
		p[i] = math.Exp(l - best)
		sum += p[i]
	}
	u := rng.Float64() * sum
	for i := range p {
		u -= p[i]
		if u <= 0 && p[i] > 0 {
			return i
		}
	}
	return bi
}

// ScriptModel 想输出 Target：它给“能接上 Target 当前位置”的 token 打高分，越长越高。
// 用它来模拟一个“格式不听话”的模型：Target 本身往往不符合 schema。
type ScriptModel struct{ Target string }

func (m ScriptModel) Logits(prefix string, vocab []string) []float64 {
	out := make([]float64, len(vocab))
	rest := ""
	if len(prefix) < len(m.Target) {
		rest = m.Target[len(prefix):]
	}
	for i, tok := range vocab {
		switch {
		case tok == EOS:
			if rest == "" {
				out[i] = 5
			} else {
				out[i] = -5
			}
		case rest != "" && len(tok) <= len(rest) && rest[:len(tok)] == tok:
			out[i] = 2 + float64(len(tok)) // 匹配越长越想要
		case rest == "" && (tok == `"` || tok == "}" || tok == "]"):
			out[i] = 1 // 想说的说完了：倾向于收尾
		default:
			out[i] = -1
		}
	}
	return out
}

// ByteVocab 返回所有可打印 ASCII 单字符 + 一些多字符 token + EOS。
// 多字符 token 故意跨越 JSON 结构（比如 `",` 和 `":`），真实分词器也是这样。
func ByteVocab(extra ...string) []string {
	v := []string{EOS}
	for c := 0x20; c < 0x7f; c++ {
		v = append(v, string(rune(c)))
	}
	return append(v, extra...)
}
