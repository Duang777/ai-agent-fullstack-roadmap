package tools

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Call 是两家格式归一化之后的一次工具调用。
type Call struct {
	ID   string // OpenAI 的 call_id / Anthropic 的 tool_use.id
	Name string
	Args json.RawMessage
}

// Result 是一次调用的结果。
type Result struct {
	CallID  string
	Content string
	IsError bool
}

// ---------- OpenAI Responses ----------

type oaItem struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"` // 注意：是 JSON 字符串，不是对象
}

// ParseOpenAI 从 response.output 里取出所有 function_call。
// output 里还可能有 reasoning、message 等项：它们要原样放回下一轮的 input（推理模型尤其如此），但不是调用。
func ParseOpenAI(output []byte) ([]Call, error) {
	var items []oaItem
	if err := json.Unmarshal(output, &items); err != nil {
		return nil, err
	}
	var calls []Call
	for _, it := range items {
		if it.Type == "function_call" {
			calls = append(calls, Call{ID: it.CallID, Name: it.Name, Args: json.RawMessage(it.Arguments)})
		}
	}
	return calls, nil
}

// OpenAIOutputs 生成下一轮要追加到 input 的 function_call_output 项，每个调用一项，用 call_id 对应。
// OpenAI 的结果没有 is_error 字段：错误写进 output 字符串里。
func OpenAIOutputs(rs []Result) []map[string]any {
	out := make([]map[string]any, 0, len(rs))
	for _, r := range rs {
		s := r.Content
		if r.IsError {
			s = "ERROR: " + s
		}
		out = append(out, map[string]any{"type": "function_call_output", "call_id": r.CallID, "output": s})
	}
	return out
}

// ---------- Anthropic Messages ----------

type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"` // 是对象，不是字符串
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type Message struct {
	Role    string  `json:"role"`
	Content []Block `json:"content"`
}

func ParseAnthropic(content []byte) ([]Call, error) {
	var blocks []Block
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, err
	}
	var calls []Call
	for _, b := range blocks {
		if b.Type == "tool_use" {
			calls = append(calls, Call{ID: b.ID, Name: b.Name, Args: b.Input})
		}
	}
	return calls, nil
}

// AnthropicResults 把所有结果放进同一条 user 消息，tool_result 在前，可选的文字在后。
func AnthropicResults(rs []Result, text string) Message {
	m := Message{Role: "user"}
	for _, r := range rs {
		m.Content = append(m.Content, Block{Type: "tool_result", ToolUseID: r.CallID, Content: r.Content, IsError: r.IsError})
	}
	if text != "" {
		m.Content = append(m.Content, Block{Type: "text", Text: text})
	}
	return m
}

var (
	ErrMissingResult = errors.New("有 tool_use 没有在紧接着的 user 消息里得到 tool_result")
	ErrTextFirst     = errors.New("user 消息里 tool_result 之前不能有文字")
	ErrSplitResults  = errors.New("同一轮的结果被拆成了多条 user 消息")
	ErrOrphanResult  = errors.New("tool_result 对应不到上一条 assistant 的 tool_use")
)

// CheckHistory 检查 Anthropic 消息历史是否满足工具调用的格式要求（违反了会 400，或者教坏模型不再并行）。
func CheckHistory(msgs []Message) error {
	for i, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		want := map[string]bool{}
		for _, b := range m.Content {
			if b.Type == "tool_use" {
				want[b.ID] = true
			}
		}
		if len(want) == 0 {
			continue
		}
		if i+1 >= len(msgs) || msgs[i+1].Role != "user" {
			return ErrMissingResult
		}
		seenText := false
		for _, b := range msgs[i+1].Content {
			switch b.Type {
			case "text":
				seenText = true
			case "tool_result":
				if seenText {
					return ErrTextFirst
				}
				if !want[b.ToolUseID] {
					return fmt.Errorf("%w：%s", ErrOrphanResult, b.ToolUseID)
				}
				delete(want, b.ToolUseID)
			}
		}
		if len(want) > 0 {
			if i+2 < len(msgs) && msgs[i+2].Role == "user" {
				return ErrSplitResults
			}
			return ErrMissingResult
		}
	}
	return nil
}
