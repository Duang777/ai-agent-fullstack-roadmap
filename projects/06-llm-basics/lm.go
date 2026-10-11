package llmbasics

import (
	"math"
	"math/rand/v2"
)

// Bigram 是最小的“语言模型”：只看上一个 token，统计下一个 token 出现的次数。
// 它没有注意力、没有参数训练，但对外接口和真正的 LLM 一样：
// 给定上下文 → 返回整个词表上的 logits。用它把分词器和采样器串成完整的生成循环。
type Bigram struct {
	vocab  int
	counts map[int]map[int]float64
}

func TrainBigram(ids []int, vocab int) *Bigram {
	m := &Bigram{vocab: vocab, counts: map[int]map[int]float64{}}
	for i := 0; i+1 < len(ids); i++ {
		row := m.counts[ids[i]]
		if row == nil {
			row = map[int]float64{}
			m.counts[ids[i]] = row
		}
		row[ids[i+1]]++
	}
	return m
}

// Logits 返回 log(次数 + 平滑)。加一点平滑让没见过的组合概率很小但不为 0。
func (m *Bigram) Logits(context []int) []float64 {
	out := make([]float64, m.vocab)
	row := m.counts[context[len(context)-1]]
	for i := range out {
		out[i] = math.Log(row[i] + 1e-3)
	}
	return out
}

// Model 是生成循环需要的唯一接口：真正的 LLM 推理服务对外也就是这件事。
type Model interface {
	Logits(context []int) []float64
}

// GenerateResult 记录一次生成。FinishReason 对应 API 里的 finish_reason / stop_reason。
type GenerateResult struct {
	IDs          []int
	FinishReason string // "stop"：遇到停止 token；"length"：用完 maxTokens
}

// Generate 是自回归生成循环：每一步把“prompt + 已生成”交给模型，采样一个 token，追加，重复。
// 输出 token 一个一个出来，所以流式输出是自然的，而“输出 token 比输入 token 贵”也源于此：
// 输入可以一次并行算完（prefill），输出只能一步一步算（decode）。
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
