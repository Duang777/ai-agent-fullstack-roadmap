package longrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Model 根据历史返回下一条 assistant 消息。
type Model interface {
	Next(ctx context.Context, h []Message) (Message, error)
}

type ModelFunc func(ctx context.Context, h []Message) (Message, error)

func (f ModelFunc) Next(ctx context.Context, h []Message) (Message, error) { return f(ctx, h) }

// Tool 收到幂等键：外部系统靠它去重，崩溃后重放同一个调用也只生效一次。
type Tool struct {
	Fn func(ctx context.Context, args, idempotencyKey string) (string, error)
	// NeedsApproval 拿到解析后的参数，返回是否要人审批。nil 表示从不需要。
	// 参数解析不了时不调用它，直接要求审批（fail closed）。
	NeedsApproval func(args map[string]any) bool
	Editable      bool // 是否允许人改参数后执行
}

type Runner struct {
	Model       Model
	Tools       map[string]Tool
	Store       Store
	Ledger      *Ledger
	Version     string
	MaxSteps    int           // 0 表示 20
	ApprovalTTL time.Duration // 0 表示审批不过期
	Now         func() time.Time

	// AfterTool 在工具执行完、结果写进检查点之前调用。测试用它模拟“刚执行完工具进程就崩了”。
	AfterTool func(c ToolCall) error
}

type Result struct {
	RunID   string
	Status  Status
	Final   string
	Pending []Pending
	Seq     int
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// save 用当前 seq 做比较并交换，成功后本地 seq 跟着 +1。
func (r *Runner) save(ctx context.Context, cp *Checkpoint) error {
	if err := r.Store.Save(ctx, *cp, cp.Seq); err != nil {
		return err
	}
	cp.Seq++
	return nil
}

func (r *Runner) load(ctx context.Context, runID string) (Checkpoint, error) {
	cp, err := r.Store.Load(ctx, runID)
	if err != nil {
		return cp, err
	}
	if cp.Version != r.Version {
		return cp, fmt.Errorf("%w：检查点 %q，当前 %q", ErrVersion, cp.Version, r.Version)
	}
	return cp, nil
}

// Start 新建一个 run 并开始执行。
func (r *Runner) Start(ctx context.Context, runID, input string) (Result, error) {
	cp := Checkpoint{
		RunID:    runID,
		Version:  r.Version,
		Status:   Running,
		Messages: []Message{{Role: User, Content: input}},
	}
	if err := r.save(ctx, &cp); err != nil {
		return Result{}, err
	}
	return r.loop(ctx, &cp)
}

// Continue 从最近的检查点继续一个 Running 状态的 run（例如进程崩溃重启后）。
func (r *Runner) Continue(ctx context.Context, runID string) (Result, error) {
	cp, err := r.load(ctx, runID)
	if err != nil {
		return Result{}, err
	}
	if cp.Status != Running {
		return result(cp), nil
	}
	// 先写一次占住这个 run：两个 worker 同时 Continue，只有一个能写成功。
	if err := r.save(ctx, &cp); err != nil {
		return Result{}, err
	}
	return r.loop(ctx, &cp)
}

// Resume 带着人的决定恢复一个等待审批的 run。没决定的调用会让 run 再次暂停。
func (r *Runner) Resume(ctx context.Context, runID string, decisions map[string]Decision) (Result, error) {
	cp, err := r.load(ctx, runID)
	if err != nil {
		return Result{}, err
	}
	if cp.Status != Interrupted {
		return Result{}, fmt.Errorf("%w：当前状态 %s", ErrNotInterrupted, cp.Status)
	}
	pending := map[string]Pending{}
	for _, p := range cp.Pending {
		pending[p.Call.ID] = p
	}
	for id, d := range decisions {
		p, ok := pending[id]
		if !ok {
			return Result{}, fmt.Errorf("%w：%s", ErrUnknownCall, id)
		}
		if err := r.checkDecision(p.Call, d); err != nil {
			return Result{}, err
		}
	}
	if cp.Decisions == nil {
		cp.Decisions = map[string]Decision{}
	}
	for id, d := range decisions {
		cp.Decisions[id] = d
	}
	// 过期的审批按拒绝处理，而不是一直挂着或者默认通过。
	now := r.now()
	for id, p := range pending {
		if _, ok := cp.Decisions[id]; !ok && !p.Deadline.IsZero() && now.After(p.Deadline) {
			cp.Decisions[id] = Decision{Kind: Reject, Message: "审批超时，操作未执行", By: "system"}
		}
	}
	cp.Status = Running
	// 这次写入就是“认领”：并发或重复提交的 Resume 只有一个能通过 seq 检查。
	if err := r.save(ctx, &cp); err != nil {
		return Result{}, err
	}
	return r.loop(ctx, &cp)
}

func (r *Runner) checkDecision(c ToolCall, d Decision) error {
	switch d.Kind {
	case Approve, Reject:
		return nil
	case Edit:
		if !r.Tools[c.Name].Editable {
			return fmt.Errorf("%w：工具 %s 不允许改参数", ErrBadDecision, c.Name)
		}
		var m map[string]any
		if json.Unmarshal([]byte(d.Args), &m) != nil || m == nil {
			return fmt.Errorf("%w：修改后的参数不是 JSON 对象", ErrBadDecision)
		}
		return nil
	}
	return fmt.Errorf("%w：未知类型 %q", ErrBadDecision, d.Kind)
}

func result(cp Checkpoint) Result {
	res := Result{RunID: cp.RunID, Status: cp.Status, Pending: cp.Pending, Seq: cp.Seq}
	if cp.Status == Done && len(cp.Messages) > 0 {
		res.Final = cp.Messages[len(cp.Messages)-1].Content
	}
	return res
}

// unresolved 返回最后一条 assistant 消息里还没有结果的工具调用（按原顺序）。
func unresolved(h []Message) []ToolCall {
	i := len(h) - 1
	for i >= 0 && h[i].Role == ToolRole {
		i--
	}
	if i < 0 || h[i].Role != Assistant {
		return nil
	}
	done := map[string]bool{}
	for _, m := range h[i+1:] {
		done[m.CallID] = true
	}
	var out []ToolCall
	for _, c := range h[i].Calls {
		if !done[c.ID] {
			out = append(out, c)
		}
	}
	return out
}

func (r *Runner) needsApproval(c ToolCall) (bool, string) {
	t := r.Tools[c.Name]
	if t.NeedsApproval == nil {
		return false, ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(c.Args), &m); err != nil || m == nil {
		return true, "参数无法解析，按需要审批处理"
	}
	if t.NeedsApproval(m) {
		return true, "命中审批规则"
	}
	return false, ""
}

func (r *Runner) loop(ctx context.Context, cp *Checkpoint) (Result, error) {
	max := r.MaxSteps
	if max == 0 {
		max = 20
	}
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if calls := unresolved(cp.Messages); len(calls) > 0 {
			var waiting []Pending
			old := map[string]Pending{}
			for _, p := range cp.Pending {
				old[p.Call.ID] = p
			}
			for _, c := range calls {
				d, decided := cp.Decisions[c.ID]
				if !decided {
					if need, why := r.needsApproval(c); need {
						p, ok := old[c.ID] // 已经在等的保留原来的截止时间
						if !ok {
							p = Pending{Call: c, Reason: why}
							if r.ApprovalTTL > 0 {
								p.Deadline = r.now().Add(r.ApprovalTTL)
							}
						}
						waiting = append(waiting, p)
						continue
					}
					d = Decision{Kind: Approve}
				}
				msg, err := r.execute(ctx, cp.RunID, c, d)
				if err != nil {
					return Result{}, err // 模拟崩溃：结果没进检查点，下次从上一个检查点恢复
				}
				cp.Messages = append(cp.Messages, msg)
				cp.Pending = waiting
				if err := r.save(ctx, cp); err != nil { // 每个工具结果都单独落一次检查点
					return Result{}, err
				}
			}
			if len(waiting) > 0 {
				cp.Pending = waiting
				cp.Status = Interrupted
				if err := r.save(ctx, cp); err != nil {
					return Result{}, err
				}
				return result(*cp), nil
			}
			cp.Pending = nil
			continue
		}
		if last := cp.Messages[len(cp.Messages)-1]; last.Role == Assistant {
			cp.Status = Done
			cp.Pending = nil
			if err := r.save(ctx, cp); err != nil {
				return Result{}, err
			}
			return result(*cp), nil
		}
		if cp.Step >= max {
			return Result{}, ErrMaxSteps
		}
		m, err := r.Model.Next(ctx, cp.Messages)
		if err != nil {
			return Result{}, err
		}
		m.Role = Assistant
		cp.Messages = append(cp.Messages, m)
		cp.Step++
		if err := r.save(ctx, cp); err != nil {
			return Result{}, err
		}
	}
}

func (r *Runner) execute(ctx context.Context, runID string, c ToolCall, d Decision) (Message, error) {
	key := runID + "/" + c.ID
	if m, ok := r.Ledger.Get(key); ok {
		return m, nil // 执行过了：崩溃前已经记账，直接用记下的结果
	}
	msg := Message{Role: ToolRole, CallID: c.ID}
	t, ok := r.Tools[c.Name]
	switch {
	case !ok:
		msg.Content, msg.IsError = "未知工具："+c.Name, true
	case d.Kind == Reject:
		msg.Content, msg.IsError = "用户拒绝了这个操作", true
		if d.Message != "" {
			msg.Content = "用户拒绝了这个操作：" + d.Message
		}
	default:
		args := c.Args
		if d.Kind == Edit {
			args = d.Args
		}
		out, err := t.Fn(ctx, args, key)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return Message{}, err
			}
			msg.Content, msg.IsError = "工具出错："+err.Error(), true
		} else {
			msg.Content = out
		}
	}
	if r.AfterTool != nil {
		if err := r.AfterTool(c); err != nil {
			return Message{}, err // 崩在执行之后、记账之前：只能靠幂等键去重
		}
	}
	r.Ledger.Put(key, msg)
	return msg, nil
}
