package agents

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

const DefaultMaxTurns = 10

const defaultRejection = "工具调用被审批人拒绝。"

type RunOptions struct {
	MaxTurns           int // 0 → DefaultMaxTurns；<0 不限（对应 max_turns=None）
	Session            Session
	ToolNotFound       string // "" = 抛 ErrModelBehavior；"return_error_to_model" = 把错误作为工具结果交给模型
	MaxToolConcurrency int    // 0 = 不限：同一轮的工具全部并发
	OnMaxTurns         func(r *Result) (final string, ok bool)
	Context            map[string]any
}

// RunContext 是工具、guardrail、handoff 回调拿到的上下文（对应 RunContextWrapper）。
type RunContext struct {
	Context map[string]any
	runner  *Runner
	opts    RunOptions
	mu      sync.Mutex
	trace   []string
}

func (rc *RunContext) addTrace(s ...string) {
	rc.mu.Lock()
	rc.trace = append(rc.trace, s...)
	rc.mu.Unlock()
}

type Result struct {
	FinalOutput   string
	LastAgent     *Agent
	NewItems      []Item
	Interruptions []Interruption
	Turns         int
	Trace         []string // 简化的 trace：agent / generation / function / handoff / guardrail span
	state         *RunState
}

// ToState 对应 result.to_state()：拿到可以审批、序列化、恢复的状态。
func (r *Result) ToState() *RunState { return r.state }

type Runner struct{ Model Model }

// Run 开始一次新的 run：一个“逻辑上的用户轮次”，里面可能有多次模型调用、多个 agent。
func (r *Runner) Run(ctx context.Context, agent *Agent, input string, opts RunOptions) (*Result, error) {
	in := []Item{User(input)}
	var prefix []Item
	if opts.Session != nil {
		prefix = opts.Session.GetItems(0)
	}
	st := &RunState{Version: stateVersion, Root: agent.Name, Current: agent.Name, Input: in,
		Prefix: append(prefix, in...), Processed: true, Context: opts.Context}
	return r.loop(ctx, agent, st, opts)
}

// Resume 对应 Runner.run(agent, state)：agent 必须是这个 run 原来的顶层 agent。
func (r *Runner) Resume(ctx context.Context, root *Agent, st *RunState, opts RunOptions) (*Result, error) {
	if st.Root != root.Name {
		return nil, fmt.Errorf("%w：状态属于 %q", ErrStateMismatch, st.Root)
	}
	return r.loop(ctx, root, st, opts)
}

func (r *Runner) loop(ctx context.Context, root *Agent, st *RunState, opts RunOptions) (*Result, error) {
	rc := &RunContext{Context: st.Context, runner: r, opts: opts}
	maxTurns := opts.MaxTurns
	if maxTurns == 0 {
		maxTurns = DefaultMaxTurns
	}
	result := func(final string) *Result {
		return &Result{FinalOutput: final, LastAgent: findAgent(root, st.Current), NewItems: st.Items,
			Interruptions: st.Pending, Turns: st.Turn, Trace: rc.trace, state: st}
	}
	for {
		cur := findAgent(root, st.Current)
		if cur == nil {
			return nil, fmt.Errorf("%w：%s", ErrStateMismatch, st.Current)
		}

		// 1. 处理最近一次模型响应：执行工具 / 审批 / handoff / 判断最终输出
		if !st.Processed {
			final, paused, err := r.processTurn(ctx, st, cur, rc, opts)
			if err != nil {
				return result(""), err
			}
			if paused {
				return result(""), nil // 有等待审批的调用：返回 interruptions，状态可以序列化
			}
			if final != nil {
				if err := r.finish(ctx, st, cur, rc, opts, *final); err != nil {
					return result(""), err
				}
				return result(*final), nil
			}
			continue // handoff 之后或工具执行之后，再调一次模型
		}

		// 2. 轮数上限
		if maxTurns > 0 && st.Turn >= maxTurns {
			if opts.OnMaxTurns != nil {
				if final, ok := opts.OnMaxTurns(result("")); ok {
					return result(final), nil
				}
			}
			return result(""), fmt.Errorf("%w：%d", ErrMaxTurns, maxTurns)
		}

		// 3. 调模型（第一个 agent 的第一轮同时跑输入 guardrail）
		input := append(append([]Item(nil), st.Prefix...), st.Items[st.ModelFrom:]...)
		req := ModelRequest{Agent: cur.Name, Instructions: cur.Instructions, Input: input, Tools: toolNames(cur)}
		var items []Item
		var err error
		if !st.Guarded && cur.Name == st.Root {
			items, err = r.callWithInputGuardrails(ctx, cur, req, st, rc)
		} else {
			rc.addTrace("generation:" + cur.Name)
			items, err = r.Model.Respond(ctx, req)
		}
		st.Guarded = true
		if err != nil {
			return result(""), err
		}
		st.Turn++
		st.TurnStart = len(st.Items)
		for _, it := range items {
			it.Agent = cur.Name
			st.Items = append(st.Items, it)
		}
		st.Processed = false
	}
}

// callWithInputGuardrails：阻塞型 guardrail 先跑完再调模型；并行型和模型调用同时跑。
// 简化：并行模式下这里会等 guardrail 结束再执行工具；真实 SDK 里 tripwire 触发前工具可能已经执行。
func (r *Runner) callWithInputGuardrails(ctx context.Context, a *Agent, req ModelRequest, st *RunState, rc *RunContext) ([]Item, error) {
	check := func(g InputGuardrail) error {
		rc.addTrace("guardrail:" + g.Name)
		trip, info, err := g.Fn(ctx, req.Input)
		if err != nil {
			return err
		}
		if trip {
			return &TripwireError{Kind: "input", Guardrail: g.Name, Info: info}
		}
		return nil
	}
	var parallel []InputGuardrail
	for _, g := range a.InputGuardrails {
		if g.Blocking {
			if err := check(g); err != nil {
				return nil, err
			}
		} else {
			parallel = append(parallel, g)
		}
	}
	errs := make([]error, len(parallel))
	var wg sync.WaitGroup
	for i, g := range parallel {
		wg.Add(1)
		go func(i int, g InputGuardrail) { defer wg.Done(); errs[i] = check(g) }(i, g)
	}
	rc.addTrace("generation:" + a.Name)
	items, merr := r.Model.Respond(ctx, req)
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return nil, e // guardrail 结果优先：模型已经调用（花了 token），但它的输出被丢弃
		}
	}
	return items, merr
}

func toolNames(a *Agent) []string {
	var out []string
	for _, t := range a.Tools {
		out = append(out, t.Name)
	}
	for _, h := range a.Handoffs {
		out = append(out, h.toolName())
	}
	return out
}

type job struct {
	call Item
	tool *FunctionTool
	args map[string]any
}

// processTurn 处理最近一次模型响应。可能被调用多次（恢复时），所以只处理还没有结果的调用。
func (r *Runner) processTurn(ctx context.Context, st *RunState, cur *Agent, rc *RunContext, opts RunOptions) (*string, bool, error) {
	resp := st.Items[st.TurnStart:]
	done := map[string]bool{}
	for _, it := range resp {
		if it.Type == "function_call_output" {
			done[it.CallID] = true
		}
	}
	var calls, handoffs []Item
	var allCalls []Item
	for _, it := range resp {
		if it.Type != "function_call" {
			continue
		}
		if handoffFor(cur, it.Name) != nil {
			if !done[it.CallID] {
				handoffs = append(handoffs, it)
			}
			continue
		}
		allCalls = append(allCalls, it)
		if !done[it.CallID] {
			calls = append(calls, it)
		}
	}

	// 1. 逐个判断：找不到工具 / 需要审批但没决定 / 被拒绝 / 可以执行
	outs := map[string]string{}
	var jobs []job
	var pending []Interruption
	for _, c := range calls {
		t := toolFor(cur, c.Name)
		if t == nil {
			if opts.ToolNotFound != "return_error_to_model" {
				return nil, false, fmt.Errorf("%w：%s 没有工具 %q", ErrModelBehavior, cur.Name, c.Name)
			}
			outs[c.CallID] = fmt.Sprintf("工具 %q 不存在，可用的工具：%v", c.Name, toolNames(cur))
			continue
		}
		args, perr := parseArgs(c.Arguments)
		need := t.NeedsApproval
		if !need && t.ApprovalFn != nil {
			need = perr != nil || t.ApprovalFn(rc, args, c.CallID) // 解析失败：不调用回调，直接要求审批
		}
		if need {
			d, ok := st.Decisions[c.CallID]
			if !ok {
				if v, sticky := st.Sticky[cur.Name+"/"+t.Name]; sticky {
					d, ok = Decision{Approved: v}, true
				}
			}
			if !ok {
				pending = append(pending, Interruption{CallID: c.CallID, Agent: cur.Name, Tool: t.Name, Arguments: c.Arguments})
				continue
			}
			if !d.Approved {
				msg := d.Message
				if msg == "" {
					msg = defaultRejection
				}
				outs[c.CallID] = msg
				continue
			}
		}
		if perr != nil {
			outs[c.CallID] = "参数无效：" + perr.Error()
			continue
		}
		jobs = append(jobs, job{c, t, args})
	}

	// 2. 并发执行（可限流），结果按调用顺序写回
	if err := r.runTools(ctx, jobs, rc, opts, outs); err != nil {
		return nil, false, err
	}
	for _, c := range calls {
		if o, ok := outs[c.CallID]; ok {
			st.Items = append(st.Items, Item{Type: "function_call_output", CallID: c.CallID, Output: o, Agent: cur.Name})
			done[c.CallID] = true
		}
	}
	st.Pending = pending
	if len(pending) > 0 {
		return nil, true, nil // 已经能执行的先执行了；剩下的等决定
	}

	// 3. tool_use_behavior：工具结果直接当最终输出
	for _, c := range allCalls {
		stop := cur.ToolUseBehavior == "stop_on_first_tool"
		for _, n := range cur.StopAtTools {
			stop = stop || n == c.Name
		}
		if stop {
			o := outputOf(st.Items[st.TurnStart:], c.CallID)
			st.Processed = true
			return &o, false, nil
		}
	}

	// 4. handoff：只采用第一个
	if len(handoffs) > 0 {
		if err := r.doHandoff(st, cur, handoffs, rc); err != nil {
			return nil, false, err
		}
		st.Processed = true
		return nil, false, nil
	}
	st.Processed = true
	if len(allCalls) > 0 {
		return nil, false, nil // 有工具调用：把结果交给模型再来一轮
	}

	// 5. 没有任何调用：最后一条 assistant 消息就是最终输出
	for i := len(resp) - 1; i >= 0; i-- {
		if resp[i].Type == "message" && resp[i].Role == "assistant" {
			s := resp[i].Content
			return &s, false, nil
		}
	}
	return nil, false, nil // 什么都没有：再来一轮（计入 max_turns）
}

func (r *Runner) runTools(ctx context.Context, jobs []job, rc *RunContext, opts RunOptions, outs map[string]string) error {
	type res struct {
		out string
		err error
	}
	results := make([]res, len(jobs))
	limit := opts.MaxToolConcurrency
	if limit <= 0 {
		limit = len(jobs) + 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out, err := runOne(ctx, j, rc)
			results[i] = res{out, err}
		}(i, j)
	}
	wg.Wait()
	for i, j := range jobs {
		if results[i].err != nil {
			return results[i].err
		}
		outs[j.call.CallID] = results[i].out
	}
	return nil
}

// runOne：工具输入 guardrail → 执行 → 工具输出 guardrail。工具自身的错误变成给模型看的文字。
func runOne(ctx context.Context, j job, rc *RunContext) (string, error) {
	for _, g := range j.tool.InputGuardrails {
		switch v := g.Fn(j.args); v.Behavior {
		case "reject_content":
			return v.Message, nil // 不执行工具
		case "raise":
			return "", &TripwireError{Kind: "tool_input", Guardrail: g.Name, Info: v.Message}
		}
	}
	rc.addTrace("function:" + j.tool.Name)
	out, err := j.tool.Run(ctx, rc, j.args)
	if err != nil {
		var tw *TripwireError
		if errors.As(err, &tw) {
			return "", err
		}
		out = "运行工具时出错：" + err.Error()
	}
	for _, g := range j.tool.OutputGuardrails {
		switch v := g.Fn(out); v.Behavior {
		case "reject_content":
			out = v.Message
		case "raise":
			return "", &TripwireError{Kind: "tool_output", Guardrail: g.Name, Info: v.Message}
		}
	}
	return out, nil
}

func (r *Runner) doHandoff(st *RunState, cur *Agent, calls []Item, rc *RunContext) error {
	first := calls[0]
	h := handoffFor(cur, first.Name)
	if h.OnHandoff != nil {
		args, err := parseArgs(first.Arguments)
		if err != nil {
			args = map[string]any{}
		}
		if err := h.OnHandoff(rc, args); err != nil {
			return err // 回调失败就不转交（授权检查应该放在这里，先于任何副作用）
		}
	}
	st.Items = append(st.Items, Item{Type: "function_call_output", CallID: first.CallID,
		Output: fmt.Sprintf(`{"assistant": %q}`, h.Agent.Name), Agent: cur.Name})
	for _, c := range calls[1:] {
		st.Items = append(st.Items, Item{Type: "function_call_output", CallID: c.CallID,
			Output: "检测到多个 handoff，忽略这一个。", Agent: cur.Name})
	}
	if h.InputFilter != nil {
		d := h.InputFilter(HandoffInputData{
			InputHistory:    st.Prefix,
			PreHandoffItems: st.Items[st.ModelFrom:st.TurnStart],
			NewItems:        st.Items[st.TurnStart:],
		})
		var p []Item
		p = append(p, d.InputHistory...)
		p = append(p, d.PreHandoffItems...)
		p = append(p, d.NewItems...)
		st.Prefix, st.ModelFrom = p, len(st.Items) // 模型以后看到过滤后的历史；Items 本身完整保留
	}
	rc.addTrace("handoff:" + cur.Name + "->" + h.Agent.Name)
	st.Current = h.Agent.Name
	return nil
}

// finish：输出 guardrail（只看产生最终输出的 agent），然后写 session。
func (r *Runner) finish(ctx context.Context, st *RunState, cur *Agent, rc *RunContext, opts RunOptions, final string) error {
	for _, g := range cur.OutputGuardrails {
		rc.addTrace("guardrail:" + g.Name)
		trip, info, err := g.Fn(ctx, final)
		if err != nil {
			return err
		}
		if trip {
			// 被拒绝的候选输出不写进 session，已经完成的工具调用照常保存
			if opts.Session != nil {
				items := st.Items
				if n := len(items); n > 0 && items[n-1].Type == "message" && items[n-1].Role == "assistant" {
					items = items[:n-1]
				}
				opts.Session.AddItems(append(append([]Item(nil), st.Input...), items...)...)
			}
			return &TripwireError{Kind: "output", Guardrail: g.Name, Info: info}
		}
	}
	if opts.Session != nil {
		opts.Session.AddItems(append(append([]Item(nil), st.Input...), st.Items...)...)
	}
	return nil
}

func toolFor(a *Agent, name string) *FunctionTool {
	for _, t := range a.Tools {
		if t.Name == name {
			return t
		}
	}
	return nil
}

func handoffFor(a *Agent, name string) *Handoff {
	for _, h := range a.Handoffs {
		if h.toolName() == name {
			return h
		}
	}
	return nil
}

func outputOf(items []Item, callID string) string {
	for _, it := range items {
		if it.Type == "function_call_output" && it.CallID == callID {
			return it.Output
		}
	}
	return ""
}
