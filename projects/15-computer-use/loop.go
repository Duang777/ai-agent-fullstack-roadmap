package cua

// Step 是假模型一轮的输出：要么一组动作，要么宣布完成。
type Step struct {
	Blocks []AnthropicBlock
	Done   bool
	Answer string
}

// Model 是脚本化的假模型：根据上一轮的结果决定下一步。
type Model func(turn int, last []ToolResult) Step

type Outcome struct {
	Turns    int
	Answer   string
	Verified bool
	Reason   string
}

// RunLoop：有步数上限；模型说“完成了”不算数，用 verify 检查环境的真实状态。
func RunLoop(b *Browser, m Model, g *Guard, scale float64, maxTurns int, verify func(*Browser) error) Outcome {
	var last []ToolResult
	for t := 1; t <= maxTurns; t++ {
		s := m(t, last)
		if s.Done {
			o := Outcome{Turns: t, Answer: s.Answer, Verified: true}
			if err := verify(b); err != nil {
				o.Verified, o.Reason = false, "模型说完成了，但核验失败："+err.Error()
			}
			return o
		}
		last = RunAnthropicBatch(b, s.Blocks, scale, g)
	}
	return Outcome{Turns: maxTurns, Reason: "达到步数上限"}
}
