package multiagent

import (
	"context"
	"sort"
	"strings"
	"sync"
)

type Runtime struct {
	Model       Model
	Agents      map[string]*Agent
	Meter       *Meter
	MaxSteps    int // 每次 Run 的模型调用上限（含所有转交）
	MaxHandoffs int // 每次 Run 的转交上限
	MaxDepth    int // agents-as-tools 的嵌套上限
	Parallel    int // 同一条消息里工具调用的并发上限，<=1 串行
}

// Session 保存多轮对话的状态。Swarm 的关键是 Active 跨轮保留：
// 用户下一句话直接发给上次接手的 agent，而不是每次都从分诊开始。
type Session struct {
	Active  string
	History []Message
}

type Result struct {
	Answer string
	Agent  string   // 最后回答的 agent
	Path   []string // 这次经过的 agent
}

func (rt *Runtime) tools(a *Agent) []string {
	var ns []string
	for n := range a.Tools {
		ns = append(ns, n)
	}
	for _, h := range a.Handoffs {
		ns = append(ns, HandoffTool(h))
	}
	sort.Strings(ns)
	return ns
}

func (rt *Runtime) handoffTarget(a *Agent, tool string) (string, bool) {
	for _, h := range a.Handoffs {
		if HandoffTool(h) == tool {
			return h, true
		}
	}
	return "", false
}

// Run 处理用户的一句话：从 s.Active 开始，按需转交，直到某个 agent 不再调用工具。
func (rt *Runtime) Run(ctx context.Context, s *Session, input string) (Result, error) {
	s.History = append(s.History, Message{Role: User, Content: input})
	active := rt.Agents[s.Active]
	res := Result{Path: []string{active.Name}}
	handoffs := 0

	for step := 0; ; step++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if rt.MaxSteps > 0 && step >= rt.MaxSteps {
			return res, ErrMaxSteps
		}
		msg, u, err := rt.Model.Next(ctx, active, rt.tools(active), s.History)
		if err != nil {
			return res, err
		}
		msg.Role, msg.Agent = Assistant, active.Name
		s.History = append(s.History, msg)
		if err := rt.Meter.Add(active.Name, u); err != nil {
			return res, err
		}
		if len(msg.Calls) == 0 {
			res.Answer, res.Agent = msg.Content, active.Name
			return res, nil
		}

		// 一条消息里只认第一个转交；其余调用一律不执行，但都要有结果，保持配对
		var next string
		var normal []ToolCall
		results := make(map[string]Message)
		for _, c := range msg.Calls {
			if !strings.HasPrefix(c.Name, handoffPrefix) {
				normal = append(normal, c)
				continue
			}
			target, ok := rt.handoffTarget(active, c.Name)
			switch {
			case !ok:
				results[c.ID] = toolMsg(c, active, "不能转交："+c.Name+" 不在可转交列表里", true)
			case next != "":
				results[c.ID] = toolMsg(c, active, "本轮已经转交给 "+next+"，忽略这次转交", true)
			default:
				next = target
				results[c.ID] = toolMsg(c, active, "已转交给 "+target, false)
			}
		}
		if next != "" {
			for _, c := range normal {
				results[c.ID] = toolMsg(c, active, "未执行：本轮已转交给 "+next, true)
			}
		} else {
			for i, m := range rt.runTools(ctx, active, normal) {
				results[normal[i].ID] = m
			}
		}
		for _, c := range msg.Calls {
			s.History = append(s.History, results[c.ID])
		}

		if next != "" {
			handoffs++
			if rt.MaxHandoffs > 0 && handoffs > rt.MaxHandoffs {
				return res, ErrTooManyHandoffs
			}
			active = rt.Agents[next]
			s.Active = next
			res.Path = append(res.Path, next)
			if active.Filter != nil {
				s.History = active.Filter(s.History)
			}
		}
	}
}

func toolMsg(c ToolCall, a *Agent, content string, isErr bool) Message {
	return Message{Role: ToolRole, CallID: c.ID, Content: content, IsError: isErr, Agent: a.Name}
}

// runTools 并发执行，结果按调用顺序返回；panic 变成错误结果。
func (rt *Runtime) runTools(ctx context.Context, a *Agent, calls []ToolCall) []Message {
	out := make([]Message, len(calls))
	limit := rt.Parallel
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, c ToolCall) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					out[i] = toolMsg(c, a, "工具崩溃", true)
				}
			}()
			t, ok := a.Tools[c.Name]
			if !ok {
				out[i] = toolMsg(c, a, "没有这个工具："+c.Name, true)
				return
			}
			s, err := t(ctx, c.Args)
			if err != nil {
				out[i] = toolMsg(c, a, err.Error(), true)
				return
			}
			out[i] = toolMsg(c, a, s, false)
		}(i, c)
	}
	wg.Wait()
	return out
}
