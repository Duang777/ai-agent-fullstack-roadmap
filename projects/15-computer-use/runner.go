package cua

import "errors"

// HaltText 是 Anthropic 规定的原文：批量动作里前一个失败后，后面的每一个都用它回复。
const HaltText = "Not executed: an earlier computer action in this turn failed."

type ToolResult struct {
	ToolUseID   string `json:"tool_use_id"`
	ToolsetName string `json:"toolset_name"` // 必须回显，缺了或写错整个请求被拒
	Content     string `json:"content,omitempty"`
	Image       *Shot  `json:"-"`
	IsError     bool   `json:"is_error,omitempty"`
}

// RunAnthropicBatch：一轮里的多个成员调用按顺序执行（不是并发），遇到第一个失败就停，
// 但**每个** tool_use 都要有一个 tool_result；安全检查在每个动作执行前单独做。
func RunAnthropicBatch(b *Browser, blocks []AnthropicBlock, scale float64, g *Guard) []ToolResult {
	var out []ToolResult
	failed := false
	for _, blk := range blocks {
		if blk.Type != "tool_use" || blk.ToolsetName != "computer" {
			continue // 其他工具（bash、自定义函数）由别的分发器处理
		}
		r := ToolResult{ToolUseID: blk.ID, ToolsetName: "computer"}
		if failed {
			r.Content, r.IsError = HaltText, true
			out = append(out, r)
			continue
		}
		a := FromAnthropic(blk, scale)
		if err := g.Before(b, a); err != nil {
			r.Content, r.IsError, failed = "Error: "+err.Error(), true, true
			out = append(out, r)
			continue
		}
		res, err := b.Do(a)
		if err != nil {
			r.Content, r.IsError, failed = "Error: "+err.Error(), true, true
		} else {
			r.Content, r.Image = res.Text, res.Image
		}
		out = append(out, r)
	}
	// 批次没有以截图结尾：在最后一个结果上附一张，省一个来回
	if n := len(out); n > 0 && out[n-1].Image == nil {
		s := b.Screenshot()
		out[n-1].Image = &s
	}
	return out
}

// RunOpenAICall：一个 computer_call 的 actions 按顺序执行，然后回一个 computer_call_output（截图）。
func RunOpenAICall(b *Browser, c OpenAICall, scale float64, g *Guard) (Shot, error) {
	for _, a := range FromOpenAI(c, scale) {
		if err := g.Before(b, a); err != nil {
			return b.Screenshot(), err
		}
		if _, err := b.Do(a); err != nil {
			return b.Screenshot(), err
		}
	}
	return b.Screenshot(), nil
}

type GeminiResult struct {
	Name, CallID string
	URL          string
	Error        string
	SafetyAck    bool // 用户确认过 require_confirmation 的动作，回传时带 safety_acknowledgement
	Image        Shot
}

var ErrTerminated = errors.New("用户拒绝，任务终止")

// RunGemini：每个 function_call 一个 function_result（带当前 URL 和截图）。
// require_confirmation 的动作先问用户；拒绝就终止整个任务。
func RunGemini(b *Browser, steps []GeminiStep, g *Guard) ([]GeminiResult, error) {
	var out []GeminiResult
	for _, s := range steps {
		if s.Type != "function_call" {
			continue
		}
		a := FromGemini(s, b.W, b.H)
		r := GeminiResult{Name: s.Name, CallID: s.ID}
		if err := g.Before(b, a); err != nil {
			if a.NeedsConfirm && errors.Is(err, ErrNeedsConfirmation) {
				return out, ErrTerminated
			}
			r.Error = err.Error()
		} else {
			r.SafetyAck = a.NeedsConfirm
			if _, err := b.Do(a); err != nil {
				r.Error = err.Error() // Gemini 的写法：错误放进结果，不中止后面的动作
			}
		}
		r.URL, r.Image = b.Page.URL, b.Screenshot()
		out = append(out, r)
	}
	return out, nil
}
