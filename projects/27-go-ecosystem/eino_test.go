package goeco

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

func TestTriageGraphRoutesDeterministically(t *testing.T) {
	ctx := context.Background()
	r, err := NewTriageGraph(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		text, queue, priority, last string
	}{
		{"我要退款", "billing", "P2", "refund"},
		{"打开就崩溃", "engineering", "P1", "bug"},
		{"你们几点下班", "frontline", "P3", "human"},
	}
	for _, c := range cases {
		out, err := r.Invoke(ctx, Ticket{ID: "T1", Text: c.text})
		if err != nil {
			t.Fatal(err)
		}
		if out.Queue != c.queue || out.Priority != c.priority {
			t.Fatalf("%q: got %s/%s", c.text, out.Queue, out.Priority)
		}
		if strings.Join(out.Path, ">") != "classify>"+c.last {
			t.Fatalf("%q: path %v", c.text, out.Path)
		}
	}
}

func TestGraphToolSchemaComesFromStruct(t *testing.T) {
	ctx := context.Background()
	r, _ := NewTriageGraph(ctx)
	tt, err := TriageTool(r)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := tt.Info(ctx)
	js, err := info.ParamsOneOf.ToJSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(js)
	s := string(b)
	for _, want := range []string{`"id"`, `"text"`, "用户的原话"} {
		if !strings.Contains(s, want) {
			t.Fatalf("schema %s missing %s", s, want)
		}
	}
}

func TestAgentCallsGraphAsTool(t *testing.T) {
	ctx := context.Background()
	r, _ := NewTriageGraph(ctx)
	tt, _ := TriageTool(r)
	m := NewScriptedModel(
		CallTool("c1", "triage_ticket", `{"id":"T9","text":"申请退款"}`),
		Say("已转给账务组，优先级 P2"),
	)
	runner, err := NewEinoRunner(ctx, m, nil, AgentOptions{}, tt)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Collect(runner.Query(ctx, "工单 T9：申请退款"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Final != "已转给账务组，优先级 P2" {
		t.Fatalf("final %q", res.Final)
	}
	if !strings.Contains(res.ToolOutputs["triage_ticket"], `"queue":"billing"`) {
		t.Fatalf("tool output %v", res.ToolOutputs)
	}
	if got := m.BoundTools(); len(got) != 1 || got[0] != "triage_ticket" {
		t.Fatalf("bound tools %v", got)
	}
	// 第二次调用模型时，历史里应该有 system、user、assistant(tool call)、tool 四条。
	in := m.Inputs()
	if len(in) != 2 || len(in[1]) != 4 {
		t.Fatalf("second model input %d messages", len(in[1]))
	}
	if in[0][0].Role != "system" {
		t.Fatalf("instruction should become system message, got %s", in[0][0].Role)
	}
}

func TestStreamingEventsAreConcatenated(t *testing.T) {
	ctx := context.Background()
	m := NewScriptedModel(Say("流式输出会被拆成很多片"))
	m.ChunkRunes = 2
	runner, _ := NewEinoRunner(ctx, m, nil, AgentOptions{Streaming: true})
	res, err := Collect(runner.Query(ctx, "hi"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Final != "流式输出会被拆成很多片" || res.Streamed != 1 {
		t.Fatalf("final %q streamed %d", res.Final, res.Streamed)
	}
}

func TestMaxIterationsStopsLoop(t *testing.T) {
	ctx := context.Background()
	r, _ := NewTriageGraph(ctx)
	tt, _ := TriageTool(r)
	var script []*schema.Message
	for i := 0; i < 5; i++ {
		script = append(script, CallTool("c", "triage_ticket", `{"id":"T","text":"x"}`))
	}
	m := NewScriptedModel(script...)
	runner, _ := NewEinoRunner(ctx, m, nil, AgentOptions{MaxIterations: 2}, tt)
	_, err := Collect(runner.Query(ctx, "loop"))
	if err == nil {
		t.Fatal("want max iterations error")
	}
	if m.Calls() != 2 {
		t.Fatalf("model called %d times, want 2", m.Calls())
	}
}

func TestUnknownToolFailsUnlessHandled(t *testing.T) {
	ctx := context.Background()
	script := func() *ScriptedModel {
		return NewScriptedModel(CallTool("c1", "no_such_tool", `{}`), Say("好的"))
	}
	r, _ := NewTriageGraph(ctx)
	tt, _ := TriageTool(r)

	runner, _ := NewEinoRunner(ctx, script(), nil, AgentOptions{}, tt)
	if _, err := Collect(runner.Query(ctx, "x")); err == nil {
		t.Fatal("unknown tool should fail the run by default")
	}

	runner, _ = NewEinoRunner(ctx, script(), nil, AgentOptions{
		UnknownTool: func(_ context.Context, name, _ string) (string, error) {
			return "工具 " + name + " 不存在，可用工具：triage_ticket", nil
		},
	}, tt)
	res, err := Collect(runner.Query(ctx, "x"))
	if err != nil || res.Final != "好的" {
		t.Fatalf("handled run: %v %q", err, res.Final)
	}
}

func refundScript() *ScriptedModel {
	return NewScriptedModel(
		CallTool("c1", "refund", `{"order_id":"A100","amount":800}`),
		Say("处理完毕"),
	)
}

func TestRefundInterruptAndApprove(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	rt := &RefundTool{Threshold: 500}
	runner, _ := NewEinoRunner(ctx, refundScript(), store, AgentOptions{}, rt)

	res, err := Collect(runner.Query(ctx, "给 A100 退 800", adk.WithCheckPointID("s1")))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interrupts) != 1 {
		t.Fatalf("want 1 interrupt, got %d", len(res.Interrupts))
	}
	ic := res.Interrupts[0]
	if p, ok := ic.Info.(*RefundPending); !ok || p.Amount != 800 {
		t.Fatalf("interrupt info %#v", ic.Info)
	}
	if len(rt.Executed()) != 0 || store.Len() != 1 {
		t.Fatalf("nothing should run before approval; store=%d", store.Len())
	}

	iter, err := runner.ResumeWithParams(ctx, "s1", &adk.ResumeParams{
		Targets: map[string]any{ic.ID: &RefundApproval{Approved: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err = Collect(iter)
	if err != nil {
		t.Fatal(err)
	}
	if res.Final != "处理完毕" || len(rt.Executed()) != 1 {
		t.Fatalf("final %q executed %v", res.Final, rt.Executed())
	}
	if !strings.Contains(res.ToolOutputs["refund"], "已退款 800.00") {
		t.Fatalf("tool output %v", res.ToolOutputs)
	}
}

func TestRefundResumeInNewRunnerAndReject(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	r1, _ := NewEinoRunner(ctx, refundScript(), store, AgentOptions{}, &RefundTool{Threshold: 500})
	res, _ := Collect(r1.Query(ctx, "退 800", adk.WithCheckPointID("s2")))
	id := res.Interrupts[0].ID

	// 模拟另一台机器：新的模型实例只需要回答恢复之后的那一轮，新的工具实例，只共享 store。
	rt := &RefundTool{Threshold: 500}
	r2, _ := NewEinoRunner(ctx, NewScriptedModel(Say("已拒绝")), store, AgentOptions{}, rt)
	iter, err := r2.ResumeWithParams(ctx, "s2", &adk.ResumeParams{
		Targets: map[string]any{id: &RefundApproval{Approved: false, Note: "金额异常"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err = Collect(iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.Executed()) != 0 || res.ToolOutputs["refund"] != "退款被拒绝：金额异常" {
		t.Fatalf("executed %v outputs %v", rt.Executed(), res.ToolOutputs)
	}
}

func TestSmallRefundRunsWithoutInterrupt(t *testing.T) {
	ctx := context.Background()
	rt := &RefundTool{Threshold: 500}
	m := NewScriptedModel(CallTool("c1", "refund", `{"order_id":"A1","amount":20}`), Say("ok"))
	runner, _ := NewEinoRunner(ctx, m, NewMemoryStore(), AgentOptions{}, rt)
	res, err := Collect(runner.Query(ctx, "退 20", adk.WithCheckPointID("s3")))
	if err != nil || len(res.Interrupts) != 0 || len(rt.Executed()) != 1 {
		t.Fatalf("err %v interrupts %d executed %d", err, len(res.Interrupts), len(rt.Executed()))
	}
}

var _ tool.InvokableTool = (*RefundTool)(nil)

func TestGraphTypeMismatchCaughtWhenBuilding(t *testing.T) {
	g := compose.NewGraph[string, string]()
	_ = g.AddLambdaNode("count", compose.InvokableLambda(func(_ context.Context, s string) (int, error) { return len(s), nil }))
	_ = g.AddLambdaNode("echo", compose.InvokableLambda(func(_ context.Context, s string) (string, error) { return s, nil }))
	_ = g.AddEdge(compose.START, "count")
	edgeErr := g.AddEdge("count", "echo") // int → string，类型对不上
	_ = g.AddEdge("echo", compose.END)
	_, compileErr := g.Compile(context.Background())
	if edgeErr == nil || compileErr == nil || compileErr.Error() != edgeErr.Error() {
		t.Fatalf("edge %v compile %v", edgeErr, compileErr)
	}
	if !strings.Contains(edgeErr.Error(), "mismatch") {
		t.Fatalf("unexpected error %v", edgeErr)
	}
}
