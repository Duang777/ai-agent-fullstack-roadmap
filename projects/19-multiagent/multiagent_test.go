package multiagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var ctx = context.Background()

// fake：每个 agent 一个脚本函数，根据历史决定回什么。并发安全。
type seen struct {
	agent string
	tools []string
	h     []Message
}

type fake struct {
	mu      sync.Mutex
	scripts map[string]func(h []Message) Message
	calls   []seen
	usage   Usage
}

func (f *fake) Next(_ context.Context, a *Agent, tools []string, h []Message) (Message, Usage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, seen{a.Name, tools, append([]Message(nil), h...)})
	fn := f.scripts[a.Name]
	f.mu.Unlock()
	if fn == nil {
		return Message{}, Usage{}, fmt.Errorf("没有 %s 的脚本", a.Name)
	}
	return fn(h), f.usage, nil
}

func (f *fake) callsOf(agent string) []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []seen
	for _, c := range f.calls {
		if c.agent == agent {
			out = append(out, c)
		}
	}
	return out
}

func say(s string) Message { return Message{Content: s} }
func call(id, name, args string) Message {
	return Message{Calls: []ToolCall{{id, name, args}}}
}
func last(h []Message) Message { return h[len(h)-1] }

// 统计历史里某角色的消息数
func count(h []Message, r Role) int {
	n := 0
	for _, m := range h {
		if m.Role == r {
			n++
		}
	}
	return n
}

func TestHandoffToolName(t *testing.T) {
	if HandoffTool("Refund Agent") != "transfer_to_refund_agent" {
		t.Fatal(HandoffTool("Refund Agent"))
	}
}

func triageSetup(f *fake) (*Runtime, *Session) {
	rt := &Runtime{Model: f, Meter: &Meter{}, MaxSteps: 10, MaxHandoffs: 3, Agents: map[string]*Agent{
		"Triage": {Name: "Triage", Handoffs: []string{"Refund", "FAQ"}},
		"Refund": {Name: "Refund", Handoffs: []string{"Triage"}, Tools: map[string]Tool{
			"lookup_order": func(_ context.Context, a string) (string, error) { return "订单 " + a + "：已付款 299 元", nil },
		}},
		"FAQ": {Name: "FAQ", Handoffs: []string{"Triage"}},
	}}
	return rt, &Session{Active: "Triage"}
}

func TestHandoffSwitchesActiveAgent(t *testing.T) {
	f := &fake{scripts: map[string]func([]Message) Message{
		"Triage": func(h []Message) Message { return call("h1", "transfer_to_refund", "") },
		"Refund": func(h []Message) Message {
			if last(h).Role == ToolRole && last(h).Agent == "Refund" {
				return say("已经给你退了 299 元")
			}
			return call("t1", "lookup_order", "A42")
		},
	}}
	rt, s := triageSetup(f)
	r, err := rt.Run(ctx, s, "我要退款，订单 A42")
	if err != nil || r.Agent != "Refund" || r.Answer != "已经给你退了 299 元" {
		t.Fatal(r, err)
	}
	if strings.Join(r.Path, ">") != "Triage>Refund" || s.Active != "Refund" {
		t.Fatal("转交后活跃 agent 换成 Refund", r.Path, s.Active)
	}
	got := f.callsOf("Refund")[0]
	if strings.Join(got.tools, ",") != "lookup_order,transfer_to_triage" {
		t.Fatal("每个 agent 只看到自己的工具和转交目标", got.tools)
	}
	if count(got.h, User) != 1 || last(got.h).Content != "已转交给 Refund" {
		t.Fatal("接手的 agent 默认看到完整对话，包括转交调用的结果", got.h)
	}
}

func TestSwarmKeepsActiveAcrossTurns(t *testing.T) {
	f := &fake{scripts: map[string]func([]Message) Message{
		"Triage": func(h []Message) Message { return call("h1", "transfer_to_faq", "") },
		"FAQ":    func(h []Message) Message { return say("回答：" + lastUser(h)) },
	}}
	rt, s := triageSetup(f)
	rt.Run(ctx, s, "营业时间？")
	r, err := rt.Run(ctx, s, "周末呢？")
	if err != nil || r.Agent != "FAQ" || len(r.Path) != 1 || r.Answer != "回答：周末呢？" {
		t.Fatal("第二轮直接从 FAQ 开始，不再经过分诊", r, err)
	}
	if len(f.callsOf("Triage")) != 1 {
		t.Fatal("分诊只在第一轮调用一次")
	}
}

func lastUser(h []Message) string {
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == User {
			return h[i].Content
		}
	}
	return ""
}

func TestHandoffFilterRemovesTools(t *testing.T) {
	f := &fake{scripts: map[string]func([]Message) Message{
		"Triage": func(h []Message) Message {
			if last(h).Role == ToolRole {
				return Message{Content: "转给 FAQ", Calls: []ToolCall{{"h1", "transfer_to_faq", ""}}}
			}
			return call("s1", "search", "")
		},
		"FAQ": func(h []Message) Message { return say("ok") },
	}}
	rt, s := triageSetup(f)
	rt.Agents["Triage"].Tools = map[string]Tool{"search": func(context.Context, string) (string, error) { return "内部数据", nil }}
	rt.Agents["FAQ"].Filter = RemoveTools
	if _, err := rt.Run(ctx, s, "问题"); err != nil {
		t.Fatal(err)
	}
	h := f.callsOf("FAQ")[0].h
	for _, m := range h {
		if m.Role == ToolRole || len(m.Calls) > 0 {
			t.Fatal("过滤后不应再有工具调用和结果", h)
		}
	}
	if len(h) != 2 || h[1].Content != "转给 FAQ" {
		t.Fatal("带文字的 assistant 消息保留文字，去掉调用", h)
	}
}

func TestOnlyFirstHandoffWins(t *testing.T) {
	f := &fake{scripts: map[string]func([]Message) Message{
		"Triage": func(h []Message) Message {
			return Message{Calls: []ToolCall{{"a", "transfer_to_refund", ""}, {"b", "transfer_to_faq", ""}, {"c", "transfer_to_billing", ""}}}
		},
		"Refund": func(h []Message) Message { return say("refund") },
	}}
	rt, s := triageSetup(f)
	r, err := rt.Run(ctx, s, "x")
	if err != nil || r.Agent != "Refund" {
		t.Fatal(r, err)
	}
	h := f.callsOf("Refund")[0].h
	if count(h, ToolRole) != 3 {
		t.Fatal("每个调用都要有结果，否则下一次请求会被拒", h)
	}
	if !h[3].IsError || !strings.Contains(h[3].Content, "已经转交给 Refund") || !h[4].IsError || !strings.Contains(h[4].Content, "不在可转交列表") {
		t.Fatal(h[3], h[4])
	}
}

func TestPingPongLimited(t *testing.T) {
	f := &fake{scripts: map[string]func([]Message) Message{
		"Triage": func(h []Message) Message { return call("x", "transfer_to_refund", "") },
		"Refund": func(h []Message) Message { return call("y", "transfer_to_triage", "") },
	}}
	rt, s := triageSetup(f)
	r, err := rt.Run(ctx, s, "x")
	if !errors.Is(err, ErrTooManyHandoffs) || len(r.Path) != 4 {
		t.Fatal("两个 agent 来回转交要有上限", r.Path, err)
	}
}

func TestHandoffSkipsOtherTools(t *testing.T) {
	var ran atomic.Int32
	f := &fake{scripts: map[string]func([]Message) Message{
		"Triage": func(h []Message) Message {
			return Message{Calls: []ToolCall{{"t", "delete_all", ""}, {"h", "transfer_to_faq", ""}}}
		},
		"FAQ": func(h []Message) Message { return say("ok") },
	}}
	rt, s := triageSetup(f)
	rt.Agents["Triage"].Tools = map[string]Tool{"delete_all": func(context.Context, string) (string, error) { ran.Add(1); return "", nil }}
	rt.Run(ctx, s, "x")
	h := f.callsOf("FAQ")[0].h
	if ran.Load() != 0 || !h[2].IsError || !strings.Contains(h[2].Content, "未执行") {
		t.Fatal("同一条消息里有转交时，其他工具不执行", ran.Load(), h)
	}
}

func supervisorSetup(f *fake) *Runtime {
	rt := &Runtime{Model: f, Meter: &Meter{}, MaxSteps: 8, MaxDepth: 1, Parallel: 4, Agents: map[string]*Agent{
		"Lead":       {Name: "Lead", Handoffs: []string{"Researcher"}},
		"Researcher": {Name: "Researcher", Handoffs: []string{"Lead"}},
	}}
	rt.Agents["Lead"].Tools = map[string]Tool{"ask_researcher": rt.AsTool("Researcher")}
	rt.Agents["Lead"].Handoffs = nil
	return rt
}

func TestSupervisorIsolatesContext(t *testing.T) {
	f := &fake{usage: Usage{Input: 100, Output: 10}, scripts: map[string]func([]Message) Message{
		"Lead": func(h []Message) Message {
			if last(h).Role == ToolRole {
				return say("综合：" + last(h).Content)
			}
			return call("1", "ask_researcher", `{"task":"查 A 公司董事会成员，返回名单"}`)
		},
		"Researcher": func(h []Message) Message { return say("张三、李四") },
	}}
	rt := supervisorSetup(f)
	s := &Session{Active: "Lead"}
	r, err := rt.Run(ctx, s, "用户的长问题……包含很多背景")
	if err != nil || r.Agent != "Lead" || r.Answer != "综合：张三、李四" {
		t.Fatal("主管始终掌控对话，最终回答来自主管", r, err)
	}
	sub := f.callsOf("Researcher")[0]
	if len(sub.h) != 1 || sub.h[0].Content != "查 A 公司董事会成员，返回名单" {
		t.Fatal("子 agent 只看到任务描述，看不到主管的历史", sub.h)
	}
	if len(sub.tools) != 0 {
		t.Fatal("作为工具运行的子 agent 不能转交", sub.tools)
	}
	if rt.Meter.Of("Lead") != 220 || rt.Meter.Of("Researcher") != 110 || rt.Meter.Total() != 330 {
		t.Fatal("共享预算按 agent 记账", rt.Meter.Of("Lead"), rt.Meter.Of("Researcher"))
	}
}

func TestSupervisorParallelSubagents(t *testing.T) {
	var running, peak atomic.Int32
	f := &fake{scripts: map[string]func([]Message) Message{
		"Lead": func(h []Message) Message {
			if last(h).Role == ToolRole {
				var parts []string
				for _, m := range h {
					if m.Role == ToolRole {
						parts = append(parts, m.Content)
					}
				}
				return say(strings.Join(parts, "|"))
			}
			var cs []ToolCall
			for i := 1; i <= 4; i++ {
				cs = append(cs, ToolCall{fmt.Sprint(i), "ask_researcher", fmt.Sprintf(`{"task":"子任务%d"}`, i)})
			}
			return Message{Calls: cs}
		},
		"Researcher": func(h []Message) Message {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			running.Add(-1)
			return say("结果:" + h[0].Content)
		},
	}}
	rt := supervisorSetup(f)
	start := time.Now()
	r, err := rt.Run(ctx, &Session{Active: "Lead"}, "比较四家公司")
	if err != nil || r.Answer != "结果:子任务1|结果:子任务2|结果:子任务3|结果:子任务4" {
		t.Fatal("并行执行，结果按调用顺序", r.Answer, err)
	}
	if peak.Load() < 2 || time.Since(start) > 100*time.Millisecond {
		t.Fatal("子 agent 应并行", peak.Load(), time.Since(start))
	}
}

func TestSupervisorDepthAndBadArgs(t *testing.T) {
	f := &fake{scripts: map[string]func([]Message) Message{
		"Lead": func(h []Message) Message {
			if last(h).Role == ToolRole {
				return say(last(h).Content)
			}
			return call("1", "ask_researcher", `{"task":"x"}`)
		},
		"Researcher": func(h []Message) Message {
			if last(h).Role == ToolRole {
				return say("子结果：" + last(h).Content)
			}
			return call("2", "ask_researcher", `{"task":"再派一个"}`)
		},
	}}
	rt := supervisorSetup(f)
	rt.Agents["Researcher"].Tools = map[string]Tool{"ask_researcher": rt.AsTool("Researcher")}
	r, err := rt.Run(ctx, &Session{Active: "Lead"}, "q")
	if err != nil || !strings.Contains(r.Answer, ErrTooDeep.Error()) {
		t.Fatal("超过嵌套深度变成工具错误，交给上层决定", r.Answer, err)
	}
	if _, err := rt.AsTool("Researcher")(ctx, `{"q":1}`); err == nil {
		t.Fatal("参数不对要报错")
	}
}

func TestSharedBudgetStopsEveryone(t *testing.T) {
	f := &fake{usage: Usage{Input: 400, Output: 100}, scripts: map[string]func([]Message) Message{
		"Lead": func(h []Message) Message {
			if last(h).Role == ToolRole {
				return say("done")
			}
			return Message{Calls: []ToolCall{{"1", "ask_researcher", `{"task":"a"}`}, {"2", "ask_researcher", `{"task":"b"}`}}}
		},
		"Researcher": func(h []Message) Message { return say("r") },
	}}
	rt := supervisorSetup(f)
	rt.Meter.Max = 1200
	_, err := rt.Run(ctx, &Session{Active: "Lead"}, "q")
	if !errors.Is(err, ErrBudget) || rt.Meter.Total() != 2000 {
		t.Fatal("主管 500 + 两个子 agent 各 500 = 1500 已超，主管下一次调用记账后停止", err, rt.Meter.Total())
	}
}

func TestMaxStepsAndToolPanic(t *testing.T) {
	f := &fake{scripts: map[string]func([]Message) Message{
		"Triage": func(h []Message) Message { return call("p", "boom", "") },
	}}
	rt, s := triageSetup(f)
	rt.Agents["Triage"].Tools = map[string]Tool{"boom": func(context.Context, string) (string, error) { panic("x") }}
	rt.MaxSteps = 3
	_, err := rt.Run(ctx, s, "x")
	if !errors.Is(err, ErrMaxSteps) || !strings.Contains(last(s.History).Content, "崩溃") {
		t.Fatal(err, last(s.History))
	}
}
