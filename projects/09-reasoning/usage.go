package reasoning

import "fmt"

// Usage 是统一后的用量口径（和 1.2 课一致）：Input 含缓存，Output 含推理。
type Usage struct {
	Input     int // 全部输入 token，含缓存命中
	CacheRead int // 其中命中缓存的部分
	Output    int // 全部输出 token，含推理
	Reasoning int // 其中看不见的推理部分
}

// Visible 是用户真正看到的输出 token 数。
func (u Usage) Visible() int { return u.Output - u.Reasoning }

// FromOpenAI：input_tokens 已含 cached_tokens；output_tokens 已含 reasoning_tokens。
func FromOpenAI(input, cached, output, reasoning int) Usage {
	return Usage{Input: input, CacheRead: cached, Output: output, Reasoning: reasoning}
}

// FromAnthropic：input_tokens 不含缓存，要把 cache_read 和 cache_creation 加回来；
// output_tokens 已含思考，output_tokens_details.thinking_tokens 是其中的思考部分。
func FromAnthropic(input, cacheRead, cacheCreation, output, thinking int) Usage {
	return Usage{Input: input + cacheRead + cacheCreation, CacheRead: cacheRead, Output: output, Reasoning: thinking}
}

// FromGemini：total_output_tokens 不含思考，total_thought_tokens 单独给，计费是两者之和。
func FromGemini(input, output, thought int) Usage {
	return Usage{Input: input, Output: output + thought, Reasoning: thought}
}

// Price 是每百万 token 的价格（练习用的假价格，真实价格查各家价目表）。
type Price struct{ Input, CacheRead, Output float64 }

// Cost：推理 token 按输出价计费，这是推理模型贵的根本原因。
func (u Usage) Cost(p Price) float64 {
	return (float64(u.Input-u.CacheRead)*p.Input + float64(u.CacheRead)*p.CacheRead + float64(u.Output)*p.Output) / 1e6
}

// Diagnosis 是对一次截断（stop = length / incomplete）的诊断。
type Diagnosis string

const (
	DiagOK                Diagnosis = "ok"
	DiagExhaustedThinking Diagnosis = "exhausted_in_reasoning" // 上限在推理阶段就耗光，一个可见字都没有
	DiagTruncatedAnswer   Diagnosis = "truncated_answer"       // 回答写了一半
)

// Diagnose 区分两种截断。前者最坑：输入和推理照样计费，用户什么都没看到。
func Diagnose(truncated bool, u Usage) (Diagnosis, string) {
	switch {
	case !truncated:
		return DiagOK, ""
	case u.Visible() == 0:
		return DiagExhaustedThinking, fmt.Sprintf("%d tokens spent on reasoning with no visible output: raise the output limit or lower the effort", u.Reasoning)
	default:
		return DiagTruncatedAnswer, "answer cut off: raise the output limit or ask for a shorter answer"
	}
}
