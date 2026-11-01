package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

type Model interface {
	Next(ctx context.Context, h []Message) (Message, Usage, error)
}

type ModelFunc func(ctx context.Context, h []Message) (Message, Usage, error)

func (f ModelFunc) Next(ctx context.Context, h []Message) (Message, Usage, error) { return f(ctx, h) }

type Tool func(ctx context.Context, args string) (string, error)

// Agent 是 3.2 的循环，只是每一步都先写事件。历史只存在事件里，内存里的 h 只是缓存。
type Agent struct {
	Model    Model
	Tools    map[string]Tool
	Version  string // 系统提示词、工具定义、模型名的版本
	Store    EventStore
	MaxSteps int
}

func (a *Agent) toolNames() []string {
	names := make([]string, 0, len(a.Tools))
	for n := range a.Tools {
		names = append(names, n)
	}
	sort.Strings(names) // map 遍历顺序随机，不排序指纹就不稳定
	return names
}

// RequestHash 是“这一步发给模型的东西”的指纹：版本、工具列表、完整历史。
// 重放时比较它，就能发现“代码改了，同一步发出去的请求已经不一样了”。
func RequestHash(version string, tools []string, h []Message) string {
	b, _ := json.Marshal(struct {
		V string    `json:"v"`
		T []string  `json:"t"`
		H []Message `json:"h"`
	}{version, tools, h})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

type recorder struct {
	a     *Agent
	runID string
	seq   int
}

func (r *recorder) emit(ctx context.Context, typ string, payload any) error {
	last, err := r.a.Store.Append(ctx, r.runID, r.seq, mustEvent(typ, payload))
	if err != nil {
		return err
	}
	r.seq = last
	return nil
}

func (a *Agent) Run(ctx context.Context, sessionID, runID, input string) (RunView, error) {
	return a.RunWithHistory(ctx, sessionID, runID, nil, input)
}

// RunWithHistory 带上会话之前的消息（由 SessionMessages 得到）开始一个新的 run。
func (a *Agent) RunWithHistory(ctx context.Context, sessionID, runID string, prior []Message, input string) (RunView, error) {
	rec := &recorder{a: a, runID: runID}
	if err := rec.emit(ctx, RunStarted, RunStartedData{SessionID: sessionID, Input: input, AgentVersion: a.Version}); err != nil {
		return RunView{}, err
	}
	h := append(append([]Message(nil), prior...), Message{Role: User, Content: input})
	max := a.MaxSteps
	if max == 0 {
		max = 10
	}
	for step := 1; ; step++ {
		if step > max {
			if err := rec.emit(ctx, RunFailed, RunFailedData{Step: step, Error: "步数用完"}); err != nil {
				return RunView{}, err
			}
			break
		}
		hash := RequestHash(a.Version, a.toolNames(), h)
		m, u, err := a.Model.Next(ctx, h)
		if err != nil {
			if e2 := rec.emit(ctx, RunFailed, RunFailedData{Step: step, Error: err.Error()}); e2 != nil {
				return RunView{}, e2
			}
			break
		}
		m.Role = Assistant
		if err := rec.emit(ctx, ModelCalled, ModelCalledData{Step: step, RequestHash: hash, Response: m, Usage: u}); err != nil {
			return RunView{}, err
		}
		h = append(h, m)
		if len(m.Calls) == 0 {
			if err := rec.emit(ctx, RunCompleted, RunCompletedData{Final: m.Content}); err != nil {
				return RunView{}, err
			}
			break
		}
		for _, c := range m.Calls {
			// 先记“开始”，再执行，再记“结束”。崩溃后只有开始没有结束的调用，就是需要核对的那一个。
			if err := rec.emit(ctx, ToolStarted, ToolStartedData{Step: step, Call: c, IdemKey: runID + "/" + c.ID}); err != nil {
				return RunView{}, err
			}
			res := ToolFinishedData{CallID: c.ID}
			if t, ok := a.Tools[c.Name]; !ok {
				res.Result, res.IsError = "未知工具："+c.Name, true
			} else if out, err := t(ctx, c.Args); err != nil {
				res.Result, res.IsError = err.Error(), true
			} else {
				res.Result = out
			}
			if err := rec.emit(ctx, ToolFinished, res); err != nil {
				return RunView{}, err
			}
			h = append(h, Message{Role: ToolRole, CallID: c.ID, Content: res.Result, IsError: res.IsError})
		}
	}
	return Load(ctx, a.Store, runID, nil)
}

// Divergence 说明重放在哪一步、因为什么和录制时不一样。
type Divergence struct {
	Step      int
	Want, Got string
}

func (d *Divergence) Error() string {
	return fmt.Sprintf("%v：第 %d 步，录制 %s，现在 %s", ErrDivergence, d.Step, d.Want, d.Got)
}
func (d *Divergence) Unwrap() error { return ErrDivergence }

type replayErr struct{ msg string }

func (e replayErr) Error() string { return e.msg }

// Replay 用录制的事件代替模型和工具，把同一个 run 在新代码上再跑一遍。
// 模型和工具一次都不会真的被调用；agent 换了 Model 和 Tools 的“录像版”，其余代码不变。
// 返回新产生的事件，可以和录制的逐条比较；请求指纹对不上时返回 *Divergence。
func Replay(ctx context.Context, live *Agent, recorded []Event) ([]Event, error) {
	var models []ModelCalledData
	var finished []ToolFinishedData
	var started []ToolStartedData
	var start RunStartedData
	var failed *RunFailedData
	var runID string
	for _, e := range recorded {
		e, err := upcast(e)
		if err != nil {
			return nil, err
		}
		switch e.Type {
		case RunStarted:
			start, _ = decode[RunStartedData](e)
			runID = e.RunID
		case ModelCalled:
			d, _ := decode[ModelCalledData](e)
			models = append(models, d)
		case ToolStarted:
			d, _ := decode[ToolStartedData](e)
			started = append(started, d)
		case ToolFinished:
			d, _ := decode[ToolFinishedData](e)
			finished = append(finished, d)
		case RunFailed:
			d, _ := decode[RunFailedData](e)
			failed = &d
		}
	}
	store := &MemStore{}
	a := &Agent{Version: live.Version, Store: store, MaxSteps: live.MaxSteps, Tools: map[string]Tool{}}
	var div *Divergence
	mi := 0
	a.Model = ModelFunc(func(_ context.Context, h []Message) (Message, Usage, error) {
		if mi >= len(models) {
			if failed != nil && failed.Step == mi+1 { // 录制时这一步模型调用就失败了，原样重现
				return Message{}, Usage{}, replayErr{failed.Error}
			}
			return Message{}, Usage{}, replayErr{"录像里没有更多模型调用"}
		}
		rec := models[mi]
		mi++
		if got := RequestHash(a.Version, a.toolNames(), h); got != rec.RequestHash {
			div = &Divergence{Step: rec.Step, Want: rec.RequestHash, Got: got}
			return Message{}, Usage{}, div
		}
		return rec.Response, rec.Usage, nil
	})
	ti := 0
	for name := range live.Tools { // 工具集合和线上一致（影响指纹），但实现换成读录像
		name := name
		a.Tools[name] = func(_ context.Context, args string) (string, error) {
			if ti >= len(finished) || ti >= len(started) {
				return "", replayErr{"录像里没有更多工具结果"}
			}
			s, f := started[ti], finished[ti]
			ti++
			if s.Call.Name != name || s.Call.Args != args {
				return "", replayErr{fmt.Sprintf("工具调用和录像不一致：%s(%s)", name, args)}
			}
			if f.IsError {
				return "", replayErr{f.Result}
			}
			return f.Result, nil
		}
	}
	if _, err := a.Run(ctx, start.SessionID, runID, start.Input); err != nil {
		return nil, err
	}
	if div != nil {
		return nil, div
	}
	return store.Read(ctx, runID, 0)
}

// SameEvents 比较两段事件流的类型、序号和内容，忽略时间戳和全局位置。
func SameEvents(a, b []Event) (bool, string) {
	if len(a) != len(b) {
		return false, fmt.Sprintf("条数不同：%d vs %d", len(a), len(b))
	}
	for i := range a {
		x, _ := upcast(a[i])
		y, _ := upcast(b[i])
		if x.Seq != y.Seq || x.Type != y.Type || string(x.Data) != string(y.Data) {
			return false, fmt.Sprintf("第 %d 条不同：%s %s vs %s %s", i+1, x.Type, x.Data, y.Type, y.Data)
		}
	}
	return true, ""
}
