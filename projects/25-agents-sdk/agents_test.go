package agents

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// script 是一个按调用次数返回预设响应的假模型，并记录每次请求。
type script struct {
	mu   sync.Mutex
	reqs []ModelRequest
	fn   func(n int, req ModelRequest) []Item
}

func (s *script) Respond(_ context.Context, req ModelRequest) ([]Item, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	n := len(s.reqs)
	s.mu.Unlock()
	return s.fn(n, req), nil
}

func (s *script) calls() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.reqs) }

func echoTool(name string, runs *atomic.Int32) *FunctionTool {
	return &FunctionTool{Name: name, Run: func(_ context.Context, _ *RunContext, a map[string]any) (string, error) {
		if runs != nil {
			runs.Add(1)
		}
		return fmt.Sprintf("%s(%v)", name, a["x"]), nil
	}}
}

func lastOutputs(items []Item) []string {
	var out []string
	for _, it := range items {
		if it.Type == "function_call_output" {
			out = append(out, it.Output)
		}
	}
	return out
}

func TestFinalOutputWhenNoToolCalls(t *testing.T) {
	m := &script{fn: func(int, ModelRequest) []Item { return []Item{Assistant("你好")} }}
	res, err := (&Runner{m}).Run(context.Background(), &Agent{Name: "A", Instructions: "简短"}, "hi", RunOptions{})
	if err != nil || res.FinalOutput != "你好" || res.Turns != 1 || res.LastAgent.Name != "A" {
		t.Fatal(res, err)
	}
	if m.reqs[0].Instructions != "简短" || !reflect.DeepEqual(m.reqs[0].Input, []Item{User("hi")}) {
		t.Fatalf("%+v", m.reqs[0])
	}
}

func TestToolLoopConcurrencyAndOrder(t *testing.T) {
	var active, peak atomic.Int32
	slow := func(name string, d time.Duration) *FunctionTool {
		return &FunctionTool{Name: name, Run: func(context.Context, *RunContext, map[string]any) (string, error) {
			n := active.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(d)
			active.Add(-1)
			return name + " 完成", nil
		}}
	}
	a := &Agent{Name: "A", Tools: []*FunctionTool{slow("t1", 30*time.Millisecond), slow("t2", 1*time.Millisecond),
		slow("t3", 20*time.Millisecond), slow("t4", 1*time.Millisecond)}}
	m := &script{fn: func(n int, req ModelRequest) []Item {
		if n == 1 {
			return []Item{Call("c1", "t1", "{}"), Call("c2", "t2", "{}"), Call("c3", "t3", "{}"), Call("c4", "t4", "{}")}
		}
		return []Item{Assistant(fmt.Sprint(len(req.Input)))}
	}}
	res, err := (&Runner{m}).Run(context.Background(), a, "go", RunOptions{MaxToolConcurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"t1 完成", "t2 完成", "t3 完成", "t4 完成"}; !reflect.DeepEqual(lastOutputs(res.NewItems), want) {
		t.Fatalf("结果按调用顺序写回，与完成顺序无关：%v", lastOutputs(res.NewItems))
	}
	if peak.Load() != 2 {
		t.Fatalf("最多同时执行 2 个：%d", peak.Load())
	}
	if res.FinalOutput != "9" || res.Turns != 2 {
		t.Fatalf("第二轮模型看到 1 条输入 + 4 个调用 + 4 个结果：%v，%d 轮", res.FinalOutput, res.Turns)
	}
}

func TestHandoffSwitchesAgentAndFilters(t *testing.T) {
	var got map[string]any
	refund := &Agent{Name: "Refund agent", Instructions: "处理退款"}
	h := HandoffTo(refund)
	h.OnHandoff = func(_ *RunContext, a map[string]any) error { got = a; return nil }
	h.InputFilter = RemoveAllTools
	triage := &Agent{Name: "Triage", Tools: []*FunctionTool{echoTool("lookup", nil)}, Handoffs: []*Handoff{h}}
	m := &script{fn: func(n int, req ModelRequest) []Item {
		switch n {
		case 1:
			return []Item{Call("c1", "lookup", `{"x":1}`)}
		case 2:
			return []Item{Call("h1", "transfer_to_refund_agent", `{"reason":"重复扣款"}`), Call("h2", "transfer_to_refund_agent", `{}`)}
		}
		return []Item{Assistant("已退款")}
	}}
	res, err := (&Runner{m}).Run(context.Background(), triage, "我被扣了两次", RunOptions{})
	if err != nil || res.FinalOutput != "已退款" || res.LastAgent != refund {
		t.Fatal(res, err)
	}
	if !reflect.DeepEqual(m.reqs[0].Tools, []string{"lookup", "transfer_to_refund_agent"}) {
		t.Fatalf("handoff 对模型就是一个工具：%v", m.reqs[0].Tools)
	}
	if got["reason"] != "重复扣款" {
		t.Fatal(got)
	}
	last := m.reqs[2]
	if last.Agent != "Refund agent" || last.Instructions != "处理退款" {
		t.Fatalf("%+v", last)
	}
	for _, it := range last.Input {
		if it.Type != "message" {
			t.Fatalf("RemoveAllTools 之后新 agent 只看到消息：%+v", last.Input)
		}
	}
	if outs := lastOutputs(res.NewItems); !strings.Contains(outs[2], "忽略") {
		t.Fatalf("只采用第一个 handoff：%v", outs)
	}
	if !reflect.DeepEqual(res.Trace, []string{"generation:Triage", "function:lookup", "generation:Triage",
		"handoff:Triage->Refund agent", "generation:Refund agent"}) {
		t.Fatal(res.Trace)
	}
}

func TestOnHandoffErrorBlocksTransfer(t *testing.T) {
	b := &Agent{Name: "B"}
	h := HandoffTo(b)
	h.OnHandoff = func(*RunContext, map[string]any) error { return errors.New("无权限") }
	m := &script{fn: func(int, ModelRequest) []Item { return []Item{Call("h", "transfer_to_b", "{}")} }}
	_, err := (&Runner{m}).Run(context.Background(), &Agent{Name: "A", Handoffs: []*Handoff{h}}, "x", RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "无权限") {
		t.Fatal(err)
	}
}

func TestAgentAsToolKeepsControl(t *testing.T) {
	m := &script{fn: func(_ int, req ModelRequest) []Item {
		switch {
		case req.Agent == "翻译":
			if len(req.Input) != 1 {
				return []Item{Assistant("嵌套 agent 不该看到外层对话")}
			}
			return []Item{Assistant("hello")}
		case len(req.Input) == 1:
			return []Item{Call("c1", "translate", `{"input":"你好"}`)}
		}
		return []Item{Assistant("译文：" + req.Input[len(req.Input)-1].Output)}
	}}
	tr := &Agent{Name: "翻译"}
	boss := &Agent{Name: "经理", Tools: []*FunctionTool{tr.AsTool("translate", "翻译成英文")}}
	res, err := (&Runner{m}).Run(context.Background(), boss, "把你好翻成英文", RunOptions{})
	if err != nil || res.FinalOutput != "译文：hello" || res.LastAgent != boss {
		t.Fatal(res, err)
	}
}

func TestInputGuardrailBlockingVsParallel(t *testing.T) {
	trip := func(blocking bool) InputGuardrail {
		return InputGuardrail{Name: "math", Blocking: blocking, Fn: func(_ context.Context, in []Item) (bool, string, error) {
			return strings.Contains(in[len(in)-1].Content, "2x"), "数学作业", nil
		}}
	}
	for _, blocking := range []bool{true, false} {
		m := &script{fn: func(int, ModelRequest) []Item { return []Item{Assistant("x=4")} }}
		a := &Agent{Name: "客服", InputGuardrails: []InputGuardrail{trip(blocking)}}
		_, err := (&Runner{m}).Run(context.Background(), a, "解 2x+3=11", RunOptions{})
		var tw *TripwireError
		if !errors.As(err, &tw) || tw.Kind != "input" {
			t.Fatal(err)
		}
		want := 1
		if blocking {
			want = 0
		}
		if m.calls() != want {
			t.Fatalf("blocking=%v：模型被调用 %d 次（阻塞模式不该调用，并行模式已经花了 token）", blocking, m.calls())
		}
	}
}

func TestGuardrailBoundaries(t *testing.T) {
	var inputRuns atomic.Int32
	b := &Agent{Name: "B",
		InputGuardrails: []InputGuardrail{{Name: "b-in", Blocking: true, Fn: func(context.Context, []Item) (bool, string, error) {
			inputRuns.Add(1)
			return true, "", nil
		}}},
		OutputGuardrails: []OutputGuardrail{{Name: "no-secret", Fn: func(_ context.Context, out string) (bool, string, error) {
			return strings.Contains(out, "sk-"), "包含密钥", nil
		}}}}
	a := &Agent{Name: "A", Handoffs: []*Handoff{HandoffTo(b)},
		OutputGuardrails: []OutputGuardrail{{Name: "a-out", Fn: func(context.Context, string) (bool, string, error) {
			return true, "不该运行", nil
		}}}}
	m := &script{fn: func(n int, _ ModelRequest) []Item {
		if n == 1 {
			return []Item{Call("h", "transfer_to_b", "{}")}
		}
		return []Item{Assistant("密钥是 sk-123")}
	}}
	sess := &MemSession{}
	_, err := (&Runner{m}).Run(context.Background(), a, "hi", RunOptions{Session: sess})
	var tw *TripwireError
	if !errors.As(err, &tw) || tw.Guardrail != "no-secret" {
		t.Fatalf("输出 guardrail 只看最后一个 agent（B），不看 A：%v", err)
	}
	if inputRuns.Load() != 0 {
		t.Fatal("输入 guardrail 只在第一个 agent 上运行，B 的不跑")
	}
	for _, it := range sess.GetItems(0) {
		if strings.Contains(it.Content, "sk-") {
			t.Fatal("被拒绝的候选输出不能写进 session")
		}
	}
	if n := len(sess.GetItems(0)); n != 3 {
		t.Fatalf("已完成的 handoff 调用和结果照常保存：%d", n)
	}
}

func approvalAgent(runs *atomic.Int32) *Agent {
	cancel := echoTool("cancel_order", runs)
	cancel.NeedsApproval = true
	return &Agent{Name: "客服", Tools: []*FunctionTool{cancel, echoTool("lookup", nil)}}
}

func TestApprovalPauseSerializeResume(t *testing.T) {
	var runs atomic.Int32
	a := approvalAgent(&runs)
	m := &script{fn: func(n int, req ModelRequest) []Item {
		if n == 1 {
			return []Item{Call("c1", "lookup", `{"x":7}`), Call("c2", "cancel_order", `{"x":7}`)}
		}
		return []Item{Assistant("完成：" + strings.Join(lastOutputs(req.Input), "，"))}
	}}
	r := &Runner{m}
	res, err := r.Run(context.Background(), a, "取消订单 7", RunOptions{})
	if err != nil || len(res.Interruptions) != 1 || res.Interruptions[0].Tool != "cancel_order" || res.FinalOutput != "" {
		t.Fatal(res, err)
	}
	if runs.Load() != 0 || len(lastOutputs(res.NewItems)) != 1 {
		t.Fatal("不需要审批的 lookup 先执行，cancel_order 等决定")
	}
	// 序列化 → 存储 → 另一个进程里恢复
	b, _ := res.ToState().ToJSON()
	st, err := StateFromJSON(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Approve(Interruption{CallID: "nope"}, false); !errors.Is(err, ErrUnknownInterruption) {
		t.Fatal(err)
	}
	if err := st.Approve(res.Interruptions[0], false); err != nil {
		t.Fatal(err)
	}
	res, err = r.Resume(context.Background(), a, st, RunOptions{})
	if err != nil || res.FinalOutput != "完成：lookup(7)，cancel_order(7)" {
		t.Fatal(res, err)
	}
	if runs.Load() != 1 || m.calls() != 2 {
		t.Fatalf("恢复时不重新调用模型，只执行被批准的调用：工具 %d 次，模型 %d 次", runs.Load(), m.calls())
	}
	if _, err := StateFromJSON(&Agent{Name: "别的"}, b); !errors.Is(err, ErrStateMismatch) {
		t.Fatal(err)
	}
}

func TestRejectReturnsMessageToModel(t *testing.T) {
	var runs atomic.Int32
	a := approvalAgent(&runs)
	m := &script{fn: func(n int, req ModelRequest) []Item {
		if n == 1 {
			return []Item{Call("c1", "cancel_order", `{"x":7}`)}
		}
		return []Item{Assistant(req.Input[len(req.Input)-1].Output)}
	}}
	r := &Runner{m}
	res, _ := r.Run(context.Background(), a, "取消", RunOptions{})
	st := res.ToState()
	_ = st.Reject(res.Interruptions[0], "主管拒绝：订单已发货", false)
	res, err := r.Resume(context.Background(), a, st, RunOptions{})
	if err != nil || res.FinalOutput != "主管拒绝：订单已发货" || runs.Load() != 0 {
		t.Fatalf("拒绝不是报错，而是变成工具结果交给模型：%v %v", res.FinalOutput, err)
	}
}

func TestStickyApprovalScopedToAgent(t *testing.T) {
	var runs atomic.Int32
	other := approvalAgent(&runs)
	other.Name = "另一个"
	a := approvalAgent(&runs)
	a.Handoffs = []*Handoff{HandoffTo(other)}
	m := &script{fn: func(n int, _ ModelRequest) []Item {
		switch n {
		case 1:
			return []Item{Call("c1", "cancel_order", `{"x":1}`)}
		case 2:
			return []Item{Call("c2", "cancel_order", `{"x":2}`)}
		case 3:
			return []Item{Call("h", "transfer_to_另一个", "{}")}
		}
		return []Item{Call("c3", "cancel_order", `{"x":3}`)}
	}}
	r := &Runner{m}
	res, _ := r.Run(context.Background(), a, "批量取消", RunOptions{})
	st := res.ToState()
	_ = st.Approve(res.Interruptions[0], true) // always_approve
	res, err := r.Resume(context.Background(), a, st, RunOptions{})
	if err != nil || len(res.Interruptions) != 1 || res.Interruptions[0].CallID != "c3" || res.Interruptions[0].Agent != "另一个" {
		t.Fatalf("c2 自动批准；handoff 后另一个 agent 的同名工具不继承：%+v %v", res.Interruptions, err)
	}
	if runs.Load() != 2 {
		t.Fatal(runs.Load())
	}
}

func TestPartialDecisions(t *testing.T) {
	var runs atomic.Int32
	a := approvalAgent(&runs)
	m := &script{fn: func(n int, _ ModelRequest) []Item {
		if n == 1 {
			return []Item{Call("c1", "cancel_order", `{"x":1}`), Call("c2", "cancel_order", `{"x":2}`)}
		}
		return []Item{Assistant("ok")}
	}}
	r := &Runner{m}
	res, _ := r.Run(context.Background(), a, "x", RunOptions{})
	st := res.ToState()
	_ = st.Approve(res.Interruptions[0], false)
	res, _ = r.Resume(context.Background(), a, st, RunOptions{})
	if len(res.Interruptions) != 1 || res.Interruptions[0].CallID != "c2" || runs.Load() != 1 || m.calls() != 1 {
		t.Fatalf("已决定的先执行，未决定的继续等，不调模型：%+v runs=%d calls=%d", res.Interruptions, runs.Load(), m.calls())
	}
	_ = st.Approve(res.Interruptions[0], false)
	res, err := r.Resume(context.Background(), a, st, RunOptions{})
	if err != nil || res.FinalOutput != "ok" || runs.Load() != 2 {
		t.Fatal(res, err)
	}
}

func TestApprovalCallableFailsClosed(t *testing.T) {
	var asked atomic.Int32
	send := echoTool("send_email", nil)
	send.ApprovalFn = func(_ *RunContext, a map[string]any, _ string) bool {
		asked.Add(1)
		return strings.Contains(fmt.Sprint(a["subject"]), "退款")
	}
	a := &Agent{Name: "A", Tools: []*FunctionTool{send}}
	for _, tc := range []struct {
		args  string
		pause bool
		asked int32
	}{
		{`{"subject":"周报"}`, false, 1},
		{`{"subject":"退款通知"}`, true, 1},
		{`not json`, true, 0},
		{`[1,2]`, true, 0},
		{`null`, true, 0},
		{``, true, 0},
	} {
		asked.Store(0)
		m := &script{fn: func(n int, _ ModelRequest) []Item {
			if n == 1 {
				return []Item{Call("c", "send_email", tc.args)}
			}
			return []Item{Assistant("done")}
		}}
		res, err := (&Runner{m}).Run(context.Background(), a, "x", RunOptions{})
		if err != nil || (len(res.Interruptions) == 1) != tc.pause || asked.Load() != tc.asked {
			t.Fatalf("%q：暂停=%v 回调=%d %v", tc.args, len(res.Interruptions) == 1, asked.Load(), err)
		}
	}
}

func TestToolGuardrails(t *testing.T) {
	var runs atomic.Int32
	tool := echoTool("classify", &runs)
	tool.NeedsApproval = true
	var order []string
	tool.InputGuardrails = []ToolGuardrail{{Name: "secrets", Fn: func(a map[string]any) ToolGuardrailResult {
		order = append(order, "guard")
		if strings.Contains(fmt.Sprint(a["x"]), "sk-") {
			return RejectContent("调用前请去掉密钥")
		}
		if a["x"] == "boom" {
			return Raise("危险输入")
		}
		return Allow()
	}}}
	tool.OutputGuardrails = []ToolOutputGuardrail{{Name: "redact", Fn: func(out string) ToolGuardrailResult {
		if strings.Contains(out, "42") {
			return RejectContent("[已脱敏]")
		}
		return Allow()
	}}}
	a := &Agent{Name: "A", Tools: []*FunctionTool{tool}, ToolUseBehavior: "stop_on_first_tool"}
	run := func(x string) (*Result, error) {
		m := &script{fn: func(int, ModelRequest) []Item { return []Item{Call("c", "classify", fmt.Sprintf(`{"x":%q}`, x))} }}
		r := &Runner{m}
		before := len(order)
		res, _ := r.Run(context.Background(), a, "x", RunOptions{})
		if len(order) != before {
			t.Fatal("审批通过之前不跑工具输入 guardrail")
		}
		st := res.ToState()
		_ = st.Approve(res.Interruptions[0], false)
		return r.Resume(context.Background(), a, st, RunOptions{})
	}
	res, _ := run("sk-abc")
	if res.FinalOutput != "调用前请去掉密钥" || runs.Load() != 0 || len(order) != 1 {
		t.Fatal(res.FinalOutput, runs.Load())
	}
	res, _ = run("42")
	if res.FinalOutput != "[已脱敏]" || runs.Load() != 1 {
		t.Fatal(res.FinalOutput)
	}
	_, err := run("boom")
	var tw *TripwireError
	if !errors.As(err, &tw) || tw.Kind != "tool_input" {
		t.Fatal(err)
	}
}

func TestToolUseBehavior(t *testing.T) {
	m := func() *script {
		return &script{fn: func(n int, _ ModelRequest) []Item {
			if n == 1 {
				return []Item{Call("c1", "weather", `{"x":"北京"}`)}
			}
			return []Item{Assistant("北京晴")}
		}}
	}
	base := &Agent{Name: "A", Tools: []*FunctionTool{echoTool("weather", nil)}}
	res, _ := (&Runner{m()}).Run(context.Background(), base, "天气", RunOptions{})
	if res.FinalOutput != "北京晴" || res.Turns != 2 {
		t.Fatal("默认 run_llm_again：工具结果交回模型", res.FinalOutput)
	}
	stop := *base
	stop.ToolUseBehavior = "stop_on_first_tool"
	res, _ = (&Runner{m()}).Run(context.Background(), &stop, "天气", RunOptions{})
	if res.FinalOutput != "weather(北京)" || res.Turns != 1 {
		t.Fatal(res.FinalOutput)
	}
	at := *base
	at.StopAtTools = []string{"weather"}
	res, _ = (&Runner{m()}).Run(context.Background(), &at, "天气", RunOptions{})
	if res.FinalOutput != "weather(北京)" {
		t.Fatal(res.FinalOutput)
	}
}

func TestMaxTurnsAndHandler(t *testing.T) {
	loop := &script{fn: func(n int, _ ModelRequest) []Item { return []Item{Call(fmt.Sprint(n), "lookup", "{}")} }}
	a := &Agent{Name: "A", Tools: []*FunctionTool{echoTool("lookup", nil)}}
	res, err := (&Runner{loop}).Run(context.Background(), a, "x", RunOptions{})
	if !errors.Is(err, ErrMaxTurns) || res.Turns != DefaultMaxTurns {
		t.Fatal(err, res.Turns)
	}
	res, err = (&Runner{loop}).Run(context.Background(), a, "x", RunOptions{MaxTurns: 3,
		OnMaxTurns: func(r *Result) (string, bool) {
			return fmt.Sprintf("%d 轮内没做完，请缩小范围", r.Turns), true
		}})
	if err != nil || res.FinalOutput != "3 轮内没做完，请缩小范围" {
		t.Fatal(res, err)
	}
}

func TestToolNotFound(t *testing.T) {
	m := func() *script {
		return &script{fn: func(n int, req ModelRequest) []Item {
			if n == 1 {
				return []Item{Call("c", "delete_db", "{}")}
			}
			return []Item{Assistant(req.Input[len(req.Input)-1].Output)}
		}}
	}
	a := &Agent{Name: "A", Tools: []*FunctionTool{echoTool("lookup", nil)}}
	if _, err := (&Runner{m()}).Run(context.Background(), a, "x", RunOptions{}); !errors.Is(err, ErrModelBehavior) {
		t.Fatal(err)
	}
	res, err := (&Runner{m()}).Run(context.Background(), a, "x", RunOptions{ToolNotFound: "return_error_to_model"})
	if err != nil || !strings.Contains(res.FinalOutput, "lookup") {
		t.Fatal(res, err)
	}
}

func TestToolErrorBecomesOutput(t *testing.T) {
	bad := &FunctionTool{Name: "fetch", Run: func(context.Context, *RunContext, map[string]any) (string, error) {
		return "", errors.New("上游 503")
	}}
	m := &script{fn: func(n int, req ModelRequest) []Item {
		if n == 1 {
			return []Item{Call("c", "fetch", "{}")}
		}
		return []Item{Assistant(req.Input[len(req.Input)-1].Output)}
	}}
	res, err := (&Runner{m}).Run(context.Background(), &Agent{Name: "A", Tools: []*FunctionTool{bad}}, "x", RunOptions{})
	if err != nil || !strings.Contains(res.FinalOutput, "上游 503") {
		t.Fatal(res, err)
	}
}

func TestSessionAccumulates(t *testing.T) {
	sess := &MemSession{}
	m := &script{fn: func(_ int, req ModelRequest) []Item {
		return []Item{Assistant(fmt.Sprintf("看到 %d 条", len(req.Input)))}
	}}
	r := &Runner{m}
	a := &Agent{Name: "A"}
	r1, _ := r.Run(context.Background(), a, "金门大桥在哪个城市？", RunOptions{Session: sess})
	r2, _ := r.Run(context.Background(), a, "在哪个州？", RunOptions{Session: sess})
	if r1.FinalOutput != "看到 1 条" || r2.FinalOutput != "看到 3 条" || len(sess.GetItems(0)) != 4 {
		t.Fatal(r1.FinalOutput, r2.FinalOutput, len(sess.GetItems(0)))
	}
	last, _ := sess.PopItem()
	if last.Content != "看到 3 条" || len(sess.GetItems(2)) != 2 {
		t.Fatal(last)
	}
	sess.Clear()
	r3, _ := r.Run(context.Background(), a, "新话题", RunOptions{Session: sess})
	if r3.FinalOutput != "看到 1 条" {
		t.Fatal(r3.FinalOutput)
	}
}
