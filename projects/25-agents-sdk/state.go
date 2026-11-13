package agents

import (
	"encoding/json"
	"fmt"
	"sync"
)

// Interruption 对应 ToolApprovalItem：哪个 agent 想用哪个工具、参数是什么。
type Interruption struct {
	CallID    string `json:"call_id"`
	Agent     string `json:"agent"`
	Tool      string `json:"tool"`
	Arguments string `json:"arguments"`
}

type Decision struct {
	Approved bool   `json:"approved"`
	Message  string `json:"message,omitempty"` // 拒绝时返回给模型的文字
}

const stateVersion = 1

// RunState 是一个 run 的全部可恢复状态，可以 JSON 序列化后存库，在另一个进程里恢复。
// 注意：它只记录状态，不认证状态。只能从你自己的服务端存储里加载。
type RunState struct {
	Version   int                 `json:"version"`
	Root      string              `json:"root"`    // 顶层 agent：恢复时必须传同一个
	Current   string              `json:"current"` // 当前 agent（handoff 之后会变）
	Input     []Item              `json:"input"`   // 本次 run 的新输入（写 session 用）
	Prefix    []Item              `json:"prefix"`  // 模型在 Items[ModelFrom:] 之前看到的历史
	Items     []Item              `json:"items"`   // 本次 run 产生的所有项
	ModelFrom int                 `json:"model_from"`
	TurnStart int                 `json:"turn_start"` // 最近一次模型响应在 Items 里的起点
	Processed bool                `json:"processed"`  // 最近一次模型响应是否已处理完
	Turn      int                 `json:"turn"`
	Pending   []Interruption      `json:"pending,omitempty"`
	Decisions map[string]Decision `json:"decisions,omitempty"` // 按 call_id
	Sticky    map[string]bool     `json:"sticky,omitempty"`    // "agent/tool" → always_approve / always_reject
	Context   map[string]any      `json:"context,omitempty"`   // 应用上下文，会随状态一起落盘：别放密钥
	Guarded   bool                `json:"guarded"`             // 输入 guardrail 已经跑过
}

func (s *RunState) find(i Interruption) (Interruption, bool) {
	for _, p := range s.Pending {
		if p.CallID == i.CallID {
			return p, true
		}
	}
	return Interruption{}, false
}

// Approve 批准一个等待中的调用。always=true 时，以后同一个 agent 的同名工具都自动批准。
func (s *RunState) Approve(i Interruption, always bool) error {
	p, ok := s.find(i)
	if !ok {
		return fmt.Errorf("%w：%s", ErrUnknownInterruption, i.CallID)
	}
	s.decide(p, Decision{Approved: true}, always)
	return nil
}

// Reject 拒绝一个等待中的调用。message 为空时用默认文字。
func (s *RunState) Reject(i Interruption, message string, always bool) error {
	p, ok := s.find(i)
	if !ok {
		return fmt.Errorf("%w：%s", ErrUnknownInterruption, i.CallID)
	}
	s.decide(p, Decision{Approved: false, Message: message}, always)
	return nil
}

func (s *RunState) decide(p Interruption, d Decision, always bool) {
	if s.Decisions == nil {
		s.Decisions = map[string]Decision{}
	}
	s.Decisions[p.CallID] = d
	if always {
		if s.Sticky == nil {
			s.Sticky = map[string]bool{}
		}
		s.Sticky[p.Agent+"/"+p.Tool] = d.Approved // 按 agent 区分：别的 agent 的同名工具不继承
	}
}

func (s *RunState) ToJSON() ([]byte, error) { return json.Marshal(s) }

// StateFromJSON 对应 RunState.from_json(agent, data)：需要原始的顶层 agent 来重新找回各个 agent。
func StateFromJSON(root *Agent, b []byte) (*RunState, error) {
	var s RunState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Version != stateVersion {
		return nil, fmt.Errorf("RunState 版本 %d，代码期望 %d", s.Version, stateVersion)
	}
	if s.Root != root.Name {
		return nil, fmt.Errorf("%w：状态属于 %q，传入的是 %q", ErrStateMismatch, s.Root, root.Name)
	}
	if findAgent(root, s.Current) == nil {
		return nil, fmt.Errorf("%w：找不到 agent %q（agent 定义改过了？）", ErrStateMismatch, s.Current)
	}
	return &s, nil
}

// findAgent 沿 handoff 图找 agent（agent 定义不进 RunState，只存名字）。
func findAgent(root *Agent, name string) *Agent {
	seen := map[*Agent]bool{}
	var walk func(a *Agent) *Agent
	walk = func(a *Agent) *Agent {
		if a == nil || seen[a] {
			return nil
		}
		seen[a] = true
		if a.Name == name {
			return a
		}
		for _, h := range a.Handoffs {
			if f := walk(h.Agent); f != nil {
				return f
			}
		}
		return nil
	}
	return walk(root)
}

// ---------- Session ----------

// Session 对应 SDK 的 Session 协议：run 开始前取历史，run 结束后写入新项。
type Session interface {
	GetItems(limit int) []Item
	AddItems(items ...Item)
	PopItem() (Item, bool)
	Clear()
}

type MemSession struct {
	mu    sync.Mutex
	items []Item
}

func (m *MemSession) GetItems(limit int) []Item {
	m.mu.Lock()
	defer m.mu.Unlock()
	from := 0
	if limit > 0 && len(m.items) > limit {
		from = len(m.items) - limit
	}
	return append([]Item(nil), m.items[from:]...)
}

func (m *MemSession) AddItems(items ...Item) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items = append(m.items, items...)
}

func (m *MemSession) PopItem() (Item, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.items) == 0 {
		return Item{}, false
	}
	it := m.items[len(m.items)-1]
	m.items = m.items[:len(m.items)-1]
	return it, true
}

func (m *MemSession) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items = nil
}
