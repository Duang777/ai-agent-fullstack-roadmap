package state

import (
	"context"
	"encoding/json"
	"fmt"
)

type Status string

const (
	Running Status = "running"
	Done    Status = "done"
	Failed  Status = "failed"
)

type ToolView struct {
	Call    ToolCall `json:"call"`
	Result  string   `json:"result,omitempty"`
	IsError bool     `json:"is_error,omitempty"`
	Done    bool     `json:"done"`
}

type StepView struct {
	N           int        `json:"n"`
	RequestHash string     `json:"request_hash"`
	Response    Message    `json:"response"`
	Usage       Usage      `json:"usage"`
	Tools       []ToolView `json:"tools,omitempty"`
}

// RunView 是从事件折叠出来的读模型。它可以随时丢掉重建，所以本身也能当快照存。
type RunView struct {
	RunID      string     `json:"run_id"`
	SessionID  string     `json:"session_id"`
	Version    string     `json:"version"`
	Status     Status     `json:"status"`
	Steps      []StepView `json:"steps"`
	Usage      Usage      `json:"usage"`
	Final      string     `json:"final,omitempty"`
	Error      string     `json:"error,omitempty"`
	FailedStep int        `json:"failed_step,omitempty"`
	Messages   []Message  `json:"messages"` // 发给模型的历史，也是从事件推出来的
	LastSeq    int        `json:"last_seq"`
}

func decode[T any](e Event) (T, error) {
	var v T
	err := json.Unmarshal(e.Data, &v)
	return v, err
}

// Apply 应用一条事件。只做确定性的计算：不读时钟、不调外部服务、不生成随机数。
func (v *RunView) Apply(e Event) error {
	if e.Seq != v.LastSeq+1 {
		return fmt.Errorf("%w：期望 %d，收到 %d", ErrOutOfOrder, v.LastSeq+1, e.Seq)
	}
	e, err := upcast(e)
	if err != nil {
		return err
	}
	switch e.Type {
	case RunStarted:
		d, err := decode[RunStartedData](e)
		if err != nil {
			return err
		}
		v.RunID, v.SessionID, v.Version, v.Status = e.RunID, d.SessionID, d.AgentVersion, Running
		v.Messages = append(v.Messages, Message{Role: User, Content: d.Input})
	case ModelCalled:
		d, err := decode[ModelCalledData](e)
		if err != nil {
			return err
		}
		v.Steps = append(v.Steps, StepView{N: d.Step, RequestHash: d.RequestHash, Response: d.Response, Usage: d.Usage})
		v.Usage.Input += d.Usage.Input
		v.Usage.Output += d.Usage.Output
		v.Messages = append(v.Messages, d.Response)
	case ToolStarted:
		d, err := decode[ToolStartedData](e)
		if err != nil {
			return err
		}
		st := v.step(d.Step)
		if st == nil {
			return fmt.Errorf("工具调用 %s 引用了不存在的步骤 %d", d.Call.ID, d.Step)
		}
		st.Tools = append(st.Tools, ToolView{Call: d.Call})
	case ToolFinished:
		d, err := decode[ToolFinishedData](e)
		if err != nil {
			return err
		}
		tv := v.tool(d.CallID)
		if tv == nil {
			return fmt.Errorf("工具结果 %s 没有对应的 tool_started", d.CallID)
		}
		tv.Result, tv.IsError, tv.Done = d.Result, d.IsError, true
		v.Messages = append(v.Messages, Message{Role: ToolRole, CallID: d.CallID, Content: d.Result, IsError: d.IsError})
	case RunCompleted:
		d, err := decode[RunCompletedData](e)
		if err != nil {
			return err
		}
		v.Status, v.Final = Done, d.Final
	case RunFailed:
		d, err := decode[RunFailedData](e)
		if err != nil {
			return err
		}
		v.Status, v.Error, v.FailedStep = Failed, d.Error, d.Step
	}
	v.LastSeq = e.Seq
	return nil
}

func (v *RunView) step(n int) *StepView {
	for i := range v.Steps {
		if v.Steps[i].N == n {
			return &v.Steps[i]
		}
	}
	return nil
}

func (v *RunView) tool(id string) *ToolView {
	for i := range v.Steps {
		for j := range v.Steps[i].Tools {
			if v.Steps[i].Tools[j].Call.ID == id {
				return &v.Steps[i].Tools[j]
			}
		}
	}
	return nil
}

// Open 返回开始了但没有结果的工具调用：崩溃或中断后要先处理它们。
func (v *RunView) Open() []ToolCall {
	var out []ToolCall
	for _, s := range v.Steps {
		for _, t := range s.Tools {
			if !t.Done {
				out = append(out, t.Call)
			}
		}
	}
	return out
}

// FirstToolError 返回第一个出错的工具调用和它所在的步骤，排查失败从这里开始。
func (v *RunView) FirstToolError() (step int, tv ToolView, ok bool) {
	for _, s := range v.Steps {
		for _, t := range s.Tools {
			if t.IsError {
				return s.N, t, true
			}
		}
	}
	return 0, ToolView{}, false
}

func Fold(evs []Event) (RunView, error) {
	var v RunView
	for _, e := range evs {
		if err := v.Apply(e); err != nil {
			return v, err
		}
	}
	return v, nil
}

// Load 从快照（可以为 nil）开始，只读快照之后的事件。
func Load(ctx context.Context, s EventStore, runID string, snap *RunView) (RunView, error) {
	var v RunView
	if snap != nil {
		b, _ := json.Marshal(snap) // 深拷贝，避免改到调用方的快照
		if err := json.Unmarshal(b, &v); err != nil {
			return v, err
		}
	}
	evs, err := s.Read(ctx, runID, v.LastSeq)
	if err != nil {
		return v, err
	}
	for _, e := range evs {
		if err := v.Apply(e); err != nil {
			return v, err
		}
	}
	return v, nil
}

// SessionIndex 是跨 run 的投影：会话 -> run 列表。它从全局流追赶，并记住读到哪里。
type SessionIndex struct {
	Pos  int                 `json:"pos"`
	Runs map[string][]string `json:"runs"`
}

func (ix *SessionIndex) CatchUp(ctx context.Context, s EventStore) error {
	for {
		evs, err := s.ReadAll(ctx, ix.Pos, 100)
		if err != nil {
			return err
		}
		if len(evs) == 0 {
			return nil
		}
		for _, e := range evs {
			if e.Type == RunStarted {
				d, err := decode[RunStartedData](e)
				if err != nil {
					return err
				}
				if ix.Runs == nil {
					ix.Runs = map[string][]string{}
				}
				ix.Runs[d.SessionID] = append(ix.Runs[d.SessionID], e.RunID)
			}
			ix.Pos = e.Pos // 位置要和投影结果一起保存，否则崩溃重启后会重复处理
		}
	}
}

// SessionMessages 把一个会话里所有 run 的消息按顺序接起来，作为下一个 run 的历史。
func SessionMessages(ctx context.Context, s EventStore, ix *SessionIndex, sessionID string) ([]Message, error) {
	var out []Message
	for _, id := range ix.Runs[sessionID] {
		v, err := Load(ctx, s, id, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, v.Messages...)
	}
	return out, nil
}
