package goeco

import (
	"context"
	"errors"
	"sync"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ScriptedModel 是一个按脚本返回的假模型，实现 Eino 的 model.ToolCallingChatModel。
// 每次 Generate / Stream 返回脚本里的下一条消息，并记录收到的输入和绑定的工具，
// 测试用它检查框架到底把什么交给了模型。
type ScriptedModel struct {
	mu     sync.Mutex
	script []*schema.Message
	next   int
	inputs [][]*schema.Message
	tools  []string
	// ChunkRunes 控制 Stream 时每个分片的字符数，0 表示整条消息一次发出。
	ChunkRunes int
}

// NewScriptedModel 用一组预设回复创建假模型。
func NewScriptedModel(script ...*schema.Message) *ScriptedModel {
	return &ScriptedModel{script: script}
}

var errScriptExhausted = errors.New("scripted model: script exhausted")

func (m *ScriptedModel) take(input []*schema.Message, opts []model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// ChatModelAgent 不调用 WithTools，而是每次用 model.WithTools 调用选项把工具传进来，
	// 所以工具列表要从选项里读。
	if o := model.GetCommonOptions(nil, opts...); o.Tools != nil {
		m.tools = m.tools[:0]
		for _, t := range o.Tools {
			m.tools = append(m.tools, t.Name)
		}
	}
	m.inputs = append(m.inputs, append([]*schema.Message(nil), input...))
	if m.next >= len(m.script) {
		return nil, errScriptExhausted
	}
	msg := m.script[m.next]
	m.next++
	return msg, nil
}

// Generate 返回脚本里的下一条消息。
func (m *ScriptedModel) Generate(_ context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return m.take(input, opts)
}

// Stream 把下一条消息按 ChunkRunes 切成多个分片，模拟真实模型的流式输出。
// 工具调用只放在第一个分片里，这和多数厂商的流式格式不同，但足够让
// Eino 的 concat 逻辑把分片拼回完整消息。
func (m *ScriptedModel) Stream(_ context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.take(input, opts)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray(splitMessage(msg, m.ChunkRunes)), nil
}

// WithTools 按 ToolCallingChatModel 的约定应返回绑定了工具的新实例，
// 不修改自己（它取代了会修改接收者、并发不安全的 BindTools）。
// 这里为了让测试读到同一份记录，返回共享状态的同一个对象，只记下工具名。
func (m *ScriptedModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools = m.tools[:0]
	for _, t := range tools {
		m.tools = append(m.tools, t.Name)
	}
	return m, nil
}

// Inputs 返回每次调用时模型收到的消息列表。
func (m *ScriptedModel) Inputs() [][]*schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]*schema.Message(nil), m.inputs...)
}

// BoundTools 返回最近一次 WithTools 绑定的工具名。
func (m *ScriptedModel) BoundTools() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.tools...)
}

// Calls 返回模型被调用的次数。
func (m *ScriptedModel) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.inputs)
}

func splitMessage(msg *schema.Message, n int) []*schema.Message {
	if n <= 0 || utf8.RuneCountInString(msg.Content) <= n {
		return []*schema.Message{msg}
	}
	runes := []rune(msg.Content)
	var out []*schema.Message
	for i := 0; i < len(runes); i += n {
		end := min(i+n, len(runes))
		chunk := &schema.Message{Role: msg.Role, Content: string(runes[i:end])}
		if i == 0 {
			chunk.ToolCalls = msg.ToolCalls
		}
		out = append(out, chunk)
	}
	return out
}

// CallTool 构造一条只包含一个工具调用的 assistant 消息。
func CallTool(id, name, args string) *schema.Message {
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID:       id,
		Function: schema.FunctionCall{Name: name, Arguments: args},
	}})
}

// Say 构造一条纯文本 assistant 消息。
func Say(text string) *schema.Message {
	return schema.AssistantMessage(text, nil)
}
