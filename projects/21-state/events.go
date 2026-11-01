// Package state 是 3.5 课的练习：Agent 的事件日志、投影、快照、升级和完整重放。
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Role string

const (
	User      Role = "user"
	Assistant Role = "assistant"
	ToolRole  Role = "tool"
)

type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"`
}

type Message struct {
	Role    Role       `json:"role"`
	Content string     `json:"content,omitempty"`
	Calls   []ToolCall `json:"calls,omitempty"`
	CallID  string     `json:"call_id,omitempty"`
	IsError bool       `json:"is_error,omitempty"`
}

type Usage struct {
	Input  int `json:"input"`
	Output int `json:"output"`
}

// 事件类型。事件名用过去式：它描述已经发生的事实，不是命令。
const (
	RunStarted   = "run_started"
	ModelCalled  = "model_called"
	ToolStarted  = "tool_started"
	ToolFinished = "tool_finished"
	RunCompleted = "run_completed"
	RunFailed    = "run_failed"
)

// currentSchema 是每种事件当前的结构版本。改了字段就加一，并写一个升级函数。
var currentSchema = map[string]int{
	RunStarted:   1,
	ModelCalled:  1,
	ToolStarted:  1,
	ToolFinished: 2, // v1 用的是 output / error 字段
	RunCompleted: 1,
	RunFailed:    1,
}

type RunStartedData struct {
	SessionID    string `json:"session_id"`
	Input        string `json:"input"`
	AgentVersion string `json:"agent_version"`
}

type ModelCalledData struct {
	Step        int     `json:"step"`
	RequestHash string  `json:"request_hash"` // 这一步发给模型的完整请求的指纹
	Response    Message `json:"response"`
	Usage       Usage   `json:"usage"`
}

type ToolStartedData struct {
	Step    int      `json:"step"`
	Call    ToolCall `json:"call"`
	IdemKey string   `json:"idem_key"`
}

type ToolFinishedData struct {
	CallID  string `json:"call_id"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
}

type RunCompletedData struct {
	Final string `json:"final"`
}

type RunFailedData struct {
	Step  int    `json:"step"`
	Error string `json:"error"`
}

// Event 是日志里的一行。Data 保留原始 JSON，读的时候再按 Type 和 Schema 解析。
type Event struct {
	RunID  string          `json:"run_id"`
	Seq    int             `json:"seq"` // run 内序号，从 1 开始，连续
	Pos    int             `json:"pos"` // 全局位置，投影追赶用
	Type   string          `json:"type"`
	Schema int             `json:"schema"`
	At     time.Time       `json:"at"`
	Data   json.RawMessage `json:"data"`
}

func NewEvent(typ string, payload any) (Event, error) {
	v, ok := currentSchema[typ]
	if !ok {
		return Event{}, fmt.Errorf("%w：%s", ErrUnknownEvent, typ)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	return Event{Type: typ, Schema: v, Data: b}, nil
}

func mustEvent(typ string, payload any) Event {
	e, err := NewEvent(typ, payload)
	if err != nil {
		panic(err)
	}
	return e
}

var (
	ErrConflict     = errors.New("事件流已被别人追加（expect 不匹配）")
	ErrOutOfOrder   = errors.New("事件序号不连续")
	ErrUnknownEvent = errors.New("未知事件类型或版本")
	ErrDivergence   = errors.New("重放分叉：这一步的请求和录制时不一样")
)

// EventStore 只追加。Append 的 expect 是调用方看到的最后一个 seq，不一致就冲突。
type EventStore interface {
	Append(ctx context.Context, runID string, expect int, evs ...Event) (last int, err error)
	Read(ctx context.Context, runID string, after int) ([]Event, error)
	ReadAll(ctx context.Context, afterPos int, limit int) ([]Event, error)
}

type MemStore struct {
	mu   sync.Mutex
	runs map[string][]Event
	all  []Event
	Now  func() time.Time
}

func (s *MemStore) Append(_ context.Context, runID string, expect int, evs ...Event) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runs == nil {
		s.runs = map[string][]Event{}
	}
	cur := len(s.runs[runID])
	if cur != expect {
		return cur, fmt.Errorf("%w：当前 %d，期望 %d", ErrConflict, cur, expect)
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	for i, e := range evs { // 一次 Append 的多条事件要么全写要么全不写
		e.RunID, e.Seq, e.Pos, e.At = runID, cur+i+1, len(s.all)+1, now()
		e.Data = append(json.RawMessage(nil), e.Data...)
		s.runs[runID] = append(s.runs[runID], e)
		s.all = append(s.all, e)
	}
	return cur + len(evs), nil
}

func (s *MemStore) Read(_ context.Context, runID string, after int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	evs := s.runs[runID]
	if after >= len(evs) {
		return nil, nil
	}
	return append([]Event(nil), evs[after:]...), nil
}

func (s *MemStore) ReadAll(_ context.Context, afterPos, limit int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if afterPos >= len(s.all) {
		return nil, nil
	}
	out := s.all[afterPos:]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return append([]Event(nil), out...), nil
}

// upcast 把旧版本的事件转成当前版本。读的时候转，日志里的原始数据永远不改。
func upcast(e Event) (Event, error) {
	if e.Type == ToolFinished && e.Schema == 1 {
		var v1 struct {
			CallID string `json:"call_id"`
			Output string `json:"output"`
			Error  bool   `json:"error"`
		}
		if err := json.Unmarshal(e.Data, &v1); err != nil {
			return e, err
		}
		b, _ := json.Marshal(ToolFinishedData{CallID: v1.CallID, Result: v1.Output, IsError: v1.Error})
		e.Data, e.Schema = b, 2
	}
	if want, ok := currentSchema[e.Type]; !ok || e.Schema != want {
		return e, fmt.Errorf("%w：%s v%d", ErrUnknownEvent, e.Type, e.Schema)
	}
	return e, nil
}
