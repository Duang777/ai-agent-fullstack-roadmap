// Package context11 是 1.6 课的练习：上下文窗口预算、长上下文退化、上下文组装、压缩策略与提示词回归评测。
// 纯 Go 标准库，不调真实 API。
package context11

import "unicode/utf8"

// Window 描述一个模型的上下文窗口。
type Window struct{ Size int }

type Outcome int

const (
	Fits        Outcome = iota // 输入 + max_tokens 都放得下
	MayTruncate                // 输入放得下，但输入 + max_tokens 超了：可能在生成中途停下
	Rejected                   // 输入本身就超了：400 prompt is too long
)

func (o Outcome) String() string {
	return [...]string{"放得下", "可能中途截断", "直接 400"}[o]
}

// Check 按 Anthropic 4.5+ 的行为：输入超窗口直接 400；
// 输入 + max_tokens 超窗口时请求被接受，生成碰到上限以 model_context_window_exceeded 停止。
func (w Window) Check(input, maxTokens int) Outcome {
	switch {
	case input > w.Size:
		return Rejected
	case input+maxTokens > w.Size:
		return MayTruncate
	default:
		return Fits
	}
}

// EstimateTokens 是粗估：ASCII 约 4 字符 1 token，其余字符（中文等）约 1 字符 1 token。
// 只用于预算；精确数字要用各家的 token counting 接口。
func EstimateTokens(s string) int {
	ascii, other := 0, 0
	for _, r := range s {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}
