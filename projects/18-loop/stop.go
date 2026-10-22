// Package loop 是 3.2 课的练习：Agent Loop 的停止条件、预算和重试。
package loop

// Stop 是把各家的停止原因归一后的结果。循环只认这几种，供应商差异留在适配层。
type Stop int

const (
	StopDone        Stop = iota // 正常结束，可以用这条回答
	StopToolUse                 // 要调用工具
	StopTruncated               // 撞到 max_tokens，输出不完整
	StopPaused                  // 服务端工具循环到了上限，原样发回即可继续
	StopRefused                 // 模型拒绝
	StopContextFull             // 撑满了上下文窗口
	StopUnknown
)

func (s Stop) String() string {
	return [...]string{"done", "tool_use", "truncated", "paused", "refused", "context_full", "unknown"}[s]
}

// FromAnthropic 映射 Messages API 的 stop_reason。
func FromAnthropic(stopReason string) Stop {
	switch stopReason {
	case "end_turn", "stop_sequence":
		return StopDone
	case "tool_use":
		return StopToolUse
	case "max_tokens":
		return StopTruncated
	case "pause_turn":
		return StopPaused
	case "refusal":
		return StopRefused
	case "model_context_window_exceeded":
		return StopContextFull
	}
	return StopUnknown // 新增的值不要当成正常结束
}

// FromOpenAI 映射 Responses API：status + incomplete_details.reason，
// 工具调用不在状态里，要看 output 里有没有 function_call。
func FromOpenAI(status, incompleteReason string, hasFunctionCall bool) Stop {
	switch status {
	case "completed":
		if hasFunctionCall {
			return StopToolUse
		}
		return StopDone
	case "incomplete":
		switch incompleteReason {
		case "max_output_tokens":
			return StopTruncated
		case "content_filter":
			return StopRefused
		}
	}
	return StopUnknown
}
