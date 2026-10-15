package tools

import "context"

// Turn 是模型一轮的输出：要么有调用，要么是最终回答。
type Turn struct {
	Text  string
	Calls []Call
}

// Model 是假模型的接口：输入完整历史（这里用 Anthropic 格式），返回一轮输出。
type Model func(history []Message) Turn

// Loop 是最小的工具调用循环：调模型 → 有调用就执行并把结果放回 → 再调模型，直到给出回答或达到 maxSteps。
// 停止条件、预算、重试等在 3.2 课细讲。
func Loop(ctx context.Context, m Model, ex Executor, user string, maxSteps int) (answer string, history []Message, steps int) {
	history = []Message{{Role: "user", Content: []Block{{Type: "text", Text: user}}}}
	for steps = 1; steps <= maxSteps; steps++ {
		t := m(history)
		a := Message{Role: "assistant"}
		if t.Text != "" {
			a.Content = append(a.Content, Block{Type: "text", Text: t.Text})
		}
		for _, c := range t.Calls {
			a.Content = append(a.Content, Block{Type: "tool_use", ID: c.ID, Name: c.Name, Input: c.Args})
		}
		history = append(history, a)
		if len(t.Calls) == 0 {
			return t.Text, history, steps
		}
		history = append(history, AnthropicResults(ex.Run(ctx, t.Calls), ""))
	}
	return "", history, maxSteps
}
