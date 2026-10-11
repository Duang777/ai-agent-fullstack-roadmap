package llmbasics

import (
	"math"
	"math/rand/v2"
	"sort"
)

// SamplingParams 对应各家 API 里常见的采样参数。零值的含义和 API 一致：不启用。
type SamplingParams struct {
	Temperature       float64         // 0 表示贪心（总选概率最大的）；1 表示原始分布
	TopK              int             // 0 表示不限制
	TopP              float64         // 0 或 1 表示不限制
	MinP              float64         // 0 表示不限制；保留 p >= MinP * p_max 的 token
	RepetitionPenalty float64         // 1 或 0 表示不惩罚；>1 压低已出现 token（HF / vLLM 风格，乘法）
	PresencePenalty   float64         // OpenAI 风格：出现过就减一次（加法）
	FrequencyPenalty  float64         // OpenAI 风格：出现几次减几次（加法）
	LogitBias         map[int]float64 // 直接给某些 token 的 logit 加偏置；-100 约等于禁用
}

// Softmax 把 logits 变成概率。先减最大值再取 exp，避免 exp(1000) 溢出成 +Inf。
// -Inf 的 logit（被过滤掉的 token）概率严格为 0。
func Softmax(logits []float64) []float64 {
	maxL := math.Inf(-1)
	for _, l := range logits {
		maxL = math.Max(maxL, l)
	}
	probs := make([]float64, len(logits))
	if math.IsInf(maxL, -1) {
		return probs // 全被过滤：返回全 0，由调用方报错
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

// Process 按推理引擎的常见顺序处理 logits，返回新切片（不修改入参）：
//  1. logit bias → 2. 重复/存在/频率惩罚 → 3. 温度 → 4. top-k → 5. top-p → 6. min-p
//
// 被过滤的 token 设为 -Inf。history 是已经生成（以及 prompt 中）的 token。
// 不同引擎的顺序略有差别，这里选的是 vLLM / HF 的主流顺序：先改分数，再除温度，最后截断。
func Process(logits []float64, history []int, p SamplingParams) []float64 {
	out := append([]float64(nil), logits...)

	for id, b := range p.LogitBias {
		if id >= 0 && id < len(out) {
			out[id] += b
		}
	}

	if len(history) > 0 {
		seen := map[int]int{}
		for _, id := range history {
			seen[id]++
		}
		for id, n := range seen {
			if id < 0 || id >= len(out) {
				continue
			}
			if p.RepetitionPenalty > 0 && p.RepetitionPenalty != 1 {
				// 正 logit 除以惩罚、负 logit 乘以惩罚，两种情况都是“变得更不可能”。
				if out[id] > 0 {
					out[id] /= p.RepetitionPenalty
				} else {
					out[id] *= p.RepetitionPenalty
				}
			}
			out[id] -= p.PresencePenalty + p.FrequencyPenalty*float64(n)
		}
	}

	if p.Temperature > 0 && p.Temperature != 1 {
		for i := range out {
			out[i] /= p.Temperature
		}
	}

	if p.TopK > 0 && p.TopK < len(out) {
		idx := argsortDesc(out)
		for _, i := range idx[p.TopK:] {
			out[i] = math.Inf(-1)
		}
	}

	if p.TopP > 0 && p.TopP < 1 {
		probs := Softmax(out)
		idx := argsortDesc(probs)
		cum := 0.0
		for _, i := range idx {
			if cum >= p.TopP { // 前面已经凑够 TopP，后面的全部丢掉
				out[i] = math.Inf(-1)
				continue
			}
			cum += probs[i]
		}
	}

	if p.MinP > 0 {
		probs := Softmax(out)
		maxP := 0.0
		for _, q := range probs {
			maxP = math.Max(maxP, q)
		}
		for i, q := range probs {
			if q < p.MinP*maxP {
				out[i] = math.Inf(-1)
			}
		}
	}
	return out
}

// Sample 选出下一个 token。Temperature == 0 时走贪心，完全确定；
// 否则在处理后的分布上按概率抽样。rng 由调用方传入：同一个种子 → 同一串结果。
func Sample(logits []float64, history []int, p SamplingParams, rng *rand.Rand) int {
	if p.Temperature == 0 {
		// 贪心也要先应用 bias 和惩罚，否则 LogitBias 在 temperature=0 时会失效。
		q := p
		q.Temperature, q.TopK, q.TopP, q.MinP = 1, 0, 0, 0
		return Argmax(Process(logits, history, q))
	}
	probs := Softmax(Process(logits, history, p))
	r := rng.Float64()
	cum := 0.0
	last := -1
	for i, q := range probs {
		if q == 0 {
			continue
		}
		cum += q
		last = i
		if r < cum {
			return i
		}
	}
	return last // 浮点累加误差导致 cum 略小于 1 时，落到最后一个非零项
}

// Argmax 返回最大值的下标；并列时取最小下标。
func Argmax(xs []float64) int {
	best := 0
	for i, x := range xs {
		if x > xs[best] {
			best = i
		}
	}
	return best
}

// argsortDesc 返回按值从大到小排列的下标；值相同按下标升序，保证确定。
func argsortDesc(xs []float64) []int {
	idx := make([]int, len(xs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return xs[idx[a]] > xs[idx[b]] })
	return idx
}

// Entropy 以 bit 为单位衡量分布有多“散”。温度越高熵越大。
func Entropy(probs []float64) float64 {
	h := 0.0
	for _, q := range probs {
		if q > 0 {
			h -= q * math.Log2(q)
		}
	}
	return h
}
