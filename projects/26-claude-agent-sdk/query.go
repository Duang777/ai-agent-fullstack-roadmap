package claudesdk

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// AgentDefinition 对应 SDK 的 AgentDefinition（子 agent）。
type AgentDefinition struct {
	Description     string
	Prompt          string   // 子 agent 的 system prompt
	Tools           []string // nil：继承所有可用工具；否则只有这些
	DisallowedTools []string
	MaxTurns        int
	PermissionMode  Mode
	Model           Model // nil：沿用父 agent 的模型
}

// Options 对应 ClaudeAgentOptions / Options。
type Options struct {
	Model           Model
	System          string
	Tools           []*Tool
	AllowedTools    []string
	DisallowedTools []string
	AskRules        []string // settings.json 里的 ask 规则
	PermissionMode  Mode
	CanUseTool      CanUseTool
	Hooks           map[string][]HookMatcher
	MaxTurns        int     // 0：不限
	MaxBudgetUSD    float64 // 0：不限
	Cwd             string

	Agents                 map[string]AgentDefinition
	MaxSubagentDepth       int // 默认 3（CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH）
	MaxConcurrentSubagents int // 默认 20（CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS）

	Store       *SessionStore
	Resume      string
	Continue    bool
	ForkSession bool
}

// shared 是一次 query 里整棵 agent 树共享的状态。
type shared struct {
	mu         sync.Mutex
	opts       *Options
	sessionID  string
	cost       float64
	running    int
	events     []Event
	remembered []string // “批准并记住”加进来的 allow 规则
	msgSeq     int
}

func (s *shared) emit(e Event) {
	s.mu.Lock()
	e.SessionID = s.sessionID
	s.events = append(s.events, e)
	s.mu.Unlock()
}

// agentRun 是一个 agent（主 agent 或子 agent）的循环。
type agentRun struct {
	sh              *shared
	depth           int
	agentType       string
	system          string
	model           Model
	mode            Mode
	maxTurns        int
	tools           []*Tool
	parentToolUseID string
	msgs            []Message
	turns           int
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func Query(ctx context.Context, prompt string, opts Options) (*Result, error) {
	o := opts
	if o.Cwd == "" {
		o.Cwd = "/work"
	}
	if o.PermissionMode == "" {
		o.PermissionMode = ModeDefault
	}
	if o.MaxSubagentDepth == 0 {
		o.MaxSubagentDepth = 3
	}
	if o.MaxConcurrentSubagents == 0 {
		o.MaxConcurrentSubagents = 20
	}
	if o.Store == nil {
		o.Store = NewSessionStore()
	}

	// 会话：resume 按 ID，continue 取最近一个，fork 复制一份新 ID
	var msgs []Message
	sid := ""
	switch {
	case o.Resume != "":
		m, ok := o.Store.Load(o.Resume)
		if !ok {
			return nil, ErrNoSession
		}
		msgs, sid = m, o.Resume
	case o.Continue:
		if id, ok := o.Store.Latest(); ok {
			msgs, _ = o.Store.Load(id)
			sid = id
		}
	}
	if sid != "" && o.ForkSession {
		sid = ""
	}
	if sid == "" {
		sid = newID()
	}

	sh := &shared{opts: &o, sessionID: sid}
	main := &agentRun{sh: sh, system: o.System, model: o.Model, mode: o.PermissionMode, maxTurns: o.MaxTurns, msgs: msgs}
	main.tools = main.visibleTools(o.Tools, nil, nil)
	sh.emit(Event{Type: "system", Subtype: "init"})

	pending := len(pendingCalls(msgs)) > 0
	if pending && prompt != "" {
		return nil, ErrDeferredPending
	}
	if !pending {
		main.msgs = append(main.msgs, Message{Role: "user", Blocks: []Block{Text(prompt)}})
	}

	res := main.loop(ctx, pending)
	o.Store.Save(sid, main.msgs)

	sh.mu.Lock()
	res.TotalCostUSD = sh.cost
	sh.mu.Unlock()
	res.SessionID = sid
	sh.emit(Event{Type: "result", Subtype: res.Subtype})
	sh.mu.Lock()
	res.Events = append([]Event(nil), sh.events...)
	sh.mu.Unlock()
	return res, nil
}

// pendingCalls：最后一条是 assistant 且带 tool_use，说明有调用还没有结果（被 defer 了）。
func pendingCalls(msgs []Message) []Block {
	if len(msgs) == 0 || msgs[len(msgs)-1].Role != "assistant" {
		return nil
	}
	return toolUses(msgs[len(msgs)-1].Blocks)
}

func toolUses(bs []Block) []Block {
	var out []Block
	for _, b := range bs {
		if b.Type == "tool_use" {
			out = append(out, b)
		}
	}
	return out
}

func (r *agentRun) toolNames() []string {
	var n []string
	for _, t := range r.tools {
		n = append(n, t.Name)
	}
	return n
}

func (r *agentRun) loop(ctx context.Context, pending bool) *Result {
	o := r.sh.opts
	for {
		if !pending {
			// 每次调模型前检查轮数和预算。越过预算的那次响应会完整执行完。
			if r.maxTurns > 0 && r.turns >= r.maxTurns {
				return &Result{Subtype: SubtypeMaxTurns, NumTurns: r.turns}
			}
			r.sh.mu.Lock()
			over := o.MaxBudgetUSD > 0 && r.sh.cost >= o.MaxBudgetUSD
			r.sh.mu.Unlock()
			if over {
				return &Result{Subtype: SubtypeMaxBudget, NumTurns: r.turns}
			}
			resp, err := r.model.Respond(ctx, ModelRequest{
				System: r.system, Tools: r.toolNames(),
				Messages: append([]Message(nil), r.msgs...),
			})
			if err != nil {
				return &Result{Subtype: SubtypeExecutionFail, NumTurns: r.turns}
			}
			r.turns++
			r.sh.mu.Lock()
			r.sh.cost += resp.CostUSD
			r.sh.msgSeq++
			mid := fmt.Sprintf("msg_%d", r.sh.msgSeq)
			r.sh.mu.Unlock()
			r.msgs = append(r.msgs, Message{Role: "assistant", Blocks: resp.Blocks})
			for i := range resp.Blocks { // 每个内容块一个 AssistantMessage，共享消息 ID
				b := resp.Blocks[i]
				r.sh.emit(Event{Type: "assistant", MessageID: mid, Block: &b, ParentToolUseID: r.parentToolUseID})
			}
			if len(toolUses(resp.Blocks)) == 0 {
				var sb strings.Builder
				for _, b := range resp.Blocks {
					sb.WriteString(b.Text)
				}
				return &Result{Subtype: SubtypeSuccess, Result: sb.String(), StopReason: "end_turn", NumTurns: r.turns}
			}
		}
		pending = false

		calls := toolUses(r.msgs[len(r.msgs)-1].Blocks)
		results, deferred := r.handleCalls(ctx, calls)
		if deferred != nil {
			return &Result{Subtype: SubtypeSuccess, StopReason: "tool_deferred", DeferredToolUse: deferred, NumTurns: r.turns}
		}
		r.msgs = append(r.msgs, Message{Role: "user", Blocks: results})
		for i := range results {
			b := results[i]
			r.sh.emit(Event{Type: "user", Block: &b, ParentToolUseID: r.parentToolUseID})
		}
	}
}

func (r *agentRun) findTool(name string) *Tool {
	for _, t := range r.tools {
		if t.Name == name {
			return t
		}
	}
	return nil
}

type ctxKey struct{}

// ToolUseID 在工具的 Run 里取当前调用的 ID。
func ToolUseID(ctx context.Context) string { s, _ := ctx.Value(ctxKey{}).(string); return s }

// handleCalls 先按调用顺序逐个做权限判定，再执行：
// 相邻的只读调用并发，其他调用单独顺序执行；结果按调用顺序写回。
func (r *agentRun) handleCalls(ctx context.Context, calls []Block) ([]Block, *Block) {
	n := len(calls)
	results := make([]Block, n)
	tools := make([]*Tool, n)
	inputs := make([]map[string]any, n)
	ok := make([]bool, n)
	for i, c := range calls {
		results[i] = Block{Type: "tool_result", ToolUseID: c.ID}
		t := r.findTool(c.Name)
		if t == nil {
			results[i].Content, results[i].IsError = "No such tool available: "+c.Name, true
			continue
		}
		d := r.decide(ctx, t, c, r.depth == 0 && n == 1)
		if d.Deferred {
			cc := c
			return nil, &cc
		}
		if !d.Allow {
			results[i].Content, results[i].IsError = d.Message, true
			continue
		}
		tools[i], inputs[i], ok[i] = t, d.Input, true
	}

	run := func(i int) {
		t, c := tools[i], calls[i]
		out, err := t.Run(context.WithValue(ctx, ctxKey{}, c.ID), inputs[i])
		hin := HookInput{SessionID: r.sh.sessionID, AgentType: r.agentType, ToolName: t.Name, ToolUseID: c.ID, ToolInput: inputs[i]}
		if err != nil {
			hin.ToolOutput = err.Error()
			runHooks(ctx, r.sh.opts.Hooks, "PostToolUseFailure", t.Name, hin)
			results[i].Content, results[i].IsError = err.Error(), true
			return
		}
		hin.ToolOutput = out
		for _, h := range runHooks(ctx, r.sh.opts.Hooks, "PostToolUse", t.Name, hin) {
			if h.failed {
				continue
			}
			if h.out.UpdatedToolOutput != nil {
				out = *h.out.UpdatedToolOutput
			}
			if h.out.AdditionalContext != "" {
				out += "\n\n" + h.out.AdditionalContext
			}
		}
		results[i].Content = out
	}

	for i := 0; i < n; {
		if !ok[i] {
			i++
			continue
		}
		if !tools[i].ReadOnly {
			run(i)
			i++
			continue
		}
		j := i
		var wg sync.WaitGroup
		for ; j < n && (!ok[j] || tools[j].ReadOnly); j++ {
			if ok[j] {
				wg.Add(1)
				go func(k int) { defer wg.Done(); run(k) }(j)
			}
		}
		wg.Wait()
		i = j
	}
	return results, nil
}

// visibleTools：去掉被不带括号的 deny 规则拿掉的工具，按子 agent 的 tools 过滤，
// 深度允许时加上 Agent 工具。
func (r *agentRun) visibleTools(base []*Tool, only, without []string) []*Tool {
	o := r.sh.opts
	var out []*Tool
	for _, t := range base {
		if t.Name == "Agent" || removedByDeny(t.Name, o.DisallowedTools) || removedByDeny(t.Name, without) {
			continue
		}
		if only != nil && !contains(only, t.Name) {
			continue
		}
		out = append(out, t)
	}
	if len(o.Agents) > 0 && r.depth < o.MaxSubagentDepth && !removedByDeny("Agent", o.DisallowedTools) &&
		!removedByDeny("Agent", without) && (only == nil || contains(only, "Agent")) {
		out = append(out, r.agentTool())
	}
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func (r *agentRun) agentTool() *Tool {
	return &Tool{
		Name: "Agent", ReadOnly: true, // 不需要批准，可以并发
		SelfApprove: func(string, map[string]any) bool { return true },
		Run:         func(ctx context.Context, in map[string]any) (string, error) { return r.spawn(ctx, in) },
	}
}

// childMode：父会话是 plan / acceptEdits / bypassPermissions 时子 agent 继承父的模式；
// 父是 default / dontAsk 时用子 agent 自己的模式，但 bypassPermissions 永远不生效。
func childMode(parent, own Mode) Mode {
	switch parent {
	case ModePlan, ModeAcceptEdits, ModeBypass:
		return parent
	}
	if own == "" || own == ModeBypass {
		return parent
	}
	return own
}

func (r *agentRun) spawn(ctx context.Context, in map[string]any) (string, error) {
	o := r.sh.opts
	typ, _ := in["subagent_type"].(string)
	prompt, _ := in["prompt"].(string)
	def, found := o.Agents[typ]
	if !found {
		return "", fmt.Errorf("unknown subagent_type: %q", typ)
	}
	r.sh.mu.Lock()
	if o.MaxBudgetUSD > 0 && r.sh.cost >= o.MaxBudgetUSD {
		r.sh.mu.Unlock()
		return "", errors.New("Budget limit reached")
	}
	if r.sh.running >= o.MaxConcurrentSubagents {
		r.sh.mu.Unlock()
		return "", errors.New("Concurrent subagent limit reached")
	}
	r.sh.running++
	r.sh.mu.Unlock()
	defer func() { r.sh.mu.Lock(); r.sh.running--; r.sh.mu.Unlock() }()

	callID := ToolUseID(ctx)
	hin := HookInput{SessionID: r.sh.sessionID, AgentType: typ, ToolUseID: callID}
	runHooks(ctx, o.Hooks, "SubagentStart", typ, hin)

	m := def.Model
	if m == nil {
		m = r.model
	}
	child := &agentRun{
		sh: r.sh, depth: r.depth + 1, agentType: typ, system: def.Prompt, model: m,
		mode: childMode(r.mode, def.PermissionMode), maxTurns: def.MaxTurns, parentToolUseID: callID,
		// 全新的上下文：只有 Agent 工具的 prompt 字符串，没有父 agent 的历史
		msgs: []Message{{Role: "user", Blocks: []Block{Text(prompt)}}},
	}
	child.tools = child.visibleTools(o.Tools, def.Tools, def.DisallowedTools)
	res := child.loop(ctx, false)

	hin.ToolOutput = res.Result
	runHooks(ctx, o.Hooks, "SubagentStop", typ, hin)
	switch res.Subtype {
	case SubtypeSuccess:
		return res.Result, nil
	case SubtypeMaxTurns:
		return "[partial: subagent stopped at maxTurns] " + lastText(child.msgs), nil
	default:
		return "", fmt.Errorf("subagent %s ended: %s", typ, res.Subtype)
	}
}

func lastText(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		var sb strings.Builder
		for _, b := range msgs[i].Blocks {
			sb.WriteString(b.Text)
		}
		if sb.Len() > 0 {
			return sb.String()
		}
	}
	return ""
}
