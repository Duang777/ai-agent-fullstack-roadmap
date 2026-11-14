package claudesdk

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- 测试工具 ----------

// fake 是按调用次数返回预设响应的假模型，并记录每次请求。
type fake struct {
	mu   sync.Mutex
	reqs []ModelRequest
	fn   func(n int, req ModelRequest) ModelResponse
}

func (f *fake) Respond(_ context.Context, req ModelRequest) (ModelResponse, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	n := len(f.reqs)
	f.mu.Unlock()
	return f.fn(n, req), nil
}

func (f *fake) calls() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.reqs) }

// router 按 system prompt 把请求分给不同的假模型（主 agent / 子 agent）。
type router map[string]*fake

func (r router) Respond(ctx context.Context, req ModelRequest) (ModelResponse, error) {
	return r[req.System].Respond(ctx, req)
}

func resp(bs ...Block) ModelResponse { return ModelResponse{Blocks: bs} }

func kv(xs ...string) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(xs); i += 2 {
		m[xs[i]] = xs[i+1]
	}
	return m
}

type rec struct {
	mu sync.Mutex
	xs []string
}

func (r *rec) add(s string) { r.mu.Lock(); r.xs = append(r.xs, s); r.mu.Unlock() }
func (r *rec) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.xs...)
}

func stdTools(rc *rec) []*Tool {
	return []*Tool{
		{Name: "Read", ReadOnly: true, SpecKey: "file_path",
			SelfApprove: func(cwd string, in map[string]any) bool { p, _ := in["file_path"].(string); return inside(cwd, p) },
			Run: func(_ context.Context, in map[string]any) (string, error) {
				rc.add(fmt.Sprint("Read ", in["file_path"]))
				return fmt.Sprint("content of ", in["file_path"]), nil
			}},
		{Name: "Write", EditsFiles: true, SpecKey: "file_path",
			Run: func(_ context.Context, in map[string]any) (string, error) {
				rc.add(fmt.Sprint("Write ", in["file_path"]))
				return "ok", nil
			}},
		{Name: "Bash", SpecKey: "command",
			Run: func(_ context.Context, in map[string]any) (string, error) {
				rc.add(fmt.Sprint("Bash ", in["command"]))
				return "ok", nil
			}},
	}
}

// lastResults 取请求里最后一条 user 消息的 tool_result。
func lastResults(req ModelRequest) []Block {
	m := req.Messages[len(req.Messages)-1]
	return m.Blocks
}

func once(calls ...Block) func(int, ModelRequest) ModelResponse {
	return func(n int, _ ModelRequest) ModelResponse {
		if n == 1 {
			return resp(calls...)
		}
		return resp(Text("完成"))
	}
}

type approver struct {
	rc     rec
	reason rec
	allow  bool
}

func (a *approver) fn(_ context.Context, r PermissionRequest) PermissionResult {
	a.rc.add(r.ToolName)
	a.reason.add(r.Reason)
	if a.allow {
		return PermissionResult{Behavior: "allow"}
	}
	return PermissionResult{Behavior: "deny", Message: "用户拒绝了 " + r.ToolName}
}

var ctx = context.Background()

// ---------- 循环与消息流 ----------

func TestLoopTurnsAndEvents(t *testing.T) {
	rc := &rec{}
	f := &fake{fn: func(n int, _ ModelRequest) ModelResponse {
		if n == 1 {
			return resp(Text("先看看"), ToolUse("t1", "Read", kv("file_path", "/work/a.go")))
		}
		return resp(Text("完成"))
	}}
	res, err := Query(ctx, "读 a.go", Options{Model: f, Tools: stdTools(rc)})
	if err != nil || res.Subtype != SubtypeSuccess || res.Result != "完成" || res.NumTurns != 2 {
		t.Fatal(res, err)
	}
	if !reflect.DeepEqual(rc.list(), []string{"Read /work/a.go"}) {
		t.Fatal(rc.list())
	}
	var types []string
	for _, e := range res.Events {
		types = append(types, e.Type)
	}
	if strings.Join(types, ",") != "system,assistant,assistant,user,assistant,result" {
		t.Fatal(types)
	}
	ev := res.Events
	if ev[1].MessageID != ev[2].MessageID || ev[1].MessageID == ev[4].MessageID {
		t.Fatal("同一次响应的内容块应共享消息 ID")
	}
	got := lastResults(f.reqs[1])
	if len(f.reqs[1].Messages) != 3 || got[0].Content != "content of /work/a.go" || got[0].ToolUseID != "t1" {
		t.Fatalf("%+v", f.reqs[1].Messages)
	}
}

func TestMaxTurnsCountsFinalTurn(t *testing.T) {
	mk := func() *fake {
		return &fake{fn: func(n int, _ ModelRequest) ModelResponse {
			if n <= 2 {
				return resp(ToolUse(fmt.Sprint("t", n), "Read", kv("file_path", "/work/a")))
			}
			return resp(Text("done"))
		}}
	}
	res, _ := Query(ctx, "x", Options{Model: mk(), Tools: stdTools(&rec{}), MaxTurns: 2})
	if res.Subtype != SubtypeMaxTurns || res.Result != "" || res.NumTurns != 2 {
		t.Fatal(res)
	}
	res, _ = Query(ctx, "x", Options{Model: mk(), Tools: stdTools(&rec{}), MaxTurns: 3})
	if res.Subtype != SubtypeSuccess || res.NumTurns != 3 {
		t.Fatal(res)
	}
}

func TestBudgetLetsCrossingResponseFinish(t *testing.T) {
	rc := &rec{}
	f := &fake{fn: func(n int, _ ModelRequest) ModelResponse {
		return ModelResponse{Blocks: []Block{ToolUse(fmt.Sprint("t", n), "Read", kv("file_path", "/work/a"))}, CostUSD: 0.4}
	}}
	res, _ := Query(ctx, "x", Options{Model: f, Tools: stdTools(rc), MaxBudgetUSD: 1.0})
	// 第 3 次响应把花费推到 1.2，它照样完成（工具也执行了），下一次调模型前才停
	if res.Subtype != SubtypeMaxBudget || res.NumTurns != 3 || len(rc.list()) != 3 || math.Abs(res.TotalCostUSD-1.2) > 1e-9 {
		t.Fatal(res, rc.list())
	}
}

func TestReadOnlyConcurrentMutatingSequential(t *testing.T) {
	var mu sync.Mutex
	arrivals, active, wActive := 0, 0, -1
	var order []string
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	r := &Tool{Name: "R", ReadOnly: true, SelfApprove: func(string, map[string]any) bool { return true },
		Run: func(c context.Context, _ map[string]any) (string, error) {
			id := ToolUseID(c)
			mu.Lock()
			arrivals++
			active++
			target := ((arrivals-1)/2 + 1) * 2 // 同一组的两个调用都到了才返回
			mu.Unlock()
			note("start " + id)
			deadline := time.Now().Add(2 * time.Second)
			for {
				mu.Lock()
				done := arrivals >= target
				mu.Unlock()
				if done {
					break
				}
				if time.Now().After(deadline) {
					return "", errors.New("只读调用没有并发执行")
				}
				time.Sleep(time.Millisecond)
			}
			mu.Lock()
			active--
			mu.Unlock()
			note("end " + id)
			return "r:" + id, nil
		}}
	w := &Tool{Name: "W", SelfApprove: func(string, map[string]any) bool { return true },
		Run: func(c context.Context, _ map[string]any) (string, error) {
			mu.Lock()
			wActive = active
			mu.Unlock()
			note("W")
			return "w", nil
		}}
	f := &fake{fn: once(ToolUse("r1", "R", nil), ToolUse("r2", "R", nil), ToolUse("w", "W", nil), ToolUse("r3", "R", nil), ToolUse("r4", "R", nil))}
	if _, err := Query(ctx, "x", Options{Model: f, Tools: []*Tool{r, w}}); err != nil {
		t.Fatal(err)
	}
	var ids, outs []string
	for _, b := range lastResults(f.reqs[1]) {
		ids = append(ids, b.ToolUseID)
		outs = append(outs, b.Content)
	}
	if strings.Join(ids, ",") != "r1,r2,w,r3,r4" || strings.Join(outs, ",") != "r:r1,r:r2,w,r:r3,r:r4" {
		t.Fatal(ids, outs)
	}
	pos := map[string]int{}
	for i, s := range order {
		pos[s] = i
	}
	if wActive != 0 || pos["W"] < pos["end r1"] || pos["W"] < pos["end r2"] || pos["W"] > pos["start r3"] || pos["W"] > pos["start r4"] {
		t.Fatal(order, wActive)
	}
}

// ---------- 六步权限判定 ----------

func TestDenyRuleBeatsHookAllowAndBypass(t *testing.T) {
	rc, ap := &rec{}, &approver{allow: true}
	allowAll := func(context.Context, HookInput) (*HookOutput, error) {
		return &HookOutput{PermissionDecision: "allow"}, nil
	}
	f := &fake{fn: once(
		ToolUse("t1", "Bash", kv("command", "rm -rf build")),
		ToolUse("t2", "Bash", kv("command", "/bin/rm -rf build")), // 规则按写法匹配，换个写法就漏了
		ToolUse("t3", "Bash", kv("command", "ls")),
	)}
	_, _ = Query(ctx, "x", Options{
		Model: f, Tools: stdTools(rc), PermissionMode: ModeBypass, CanUseTool: ap.fn,
		DisallowedTools: []string{"Bash(rm *)"},
		Hooks:           map[string][]HookMatcher{"PreToolUse": {{Hooks: []HookFunc{allowAll}}}},
	})
	if !reflect.DeepEqual(rc.list(), []string{"Bash /bin/rm -rf build", "Bash ls"}) || len(ap.rc.list()) != 0 {
		t.Fatal(rc.list(), ap.rc.list())
	}
	if r := lastResults(f.reqs[1])[0]; !r.IsError || !strings.Contains(r.Content, "denied") {
		t.Fatalf("%+v", r)
	}
}

func TestAskRuleEvenInBypassButDeniedInDontAsk(t *testing.T) {
	calls := []Block{ToolUse("t1", "Bash", kv("command", "git push origin main")), ToolUse("t2", "Bash", kv("command", "git status"))}
	rc, ap := &rec{}, &approver{allow: true}
	_, _ = Query(ctx, "x", Options{Model: &fake{fn: once(calls...)}, Tools: stdTools(rc), PermissionMode: ModeBypass,
		AskRules: []string{"Bash(git push*)"}, CanUseTool: ap.fn})
	if !reflect.DeepEqual(ap.reason.list(), []string{"ask_rule"}) || len(rc.list()) != 2 {
		t.Fatal(ap.reason.list(), rc.list())
	}

	rc, ap = &rec{}, &approver{allow: true}
	_, _ = Query(ctx, "x", Options{Model: &fake{fn: once(calls...)}, Tools: stdTools(rc), PermissionMode: ModeDontAsk,
		AskRules: []string{"Bash(git push*)"}, AllowedTools: []string{"Bash(git status)"}, CanUseTool: ap.fn})
	if !reflect.DeepEqual(rc.list(), []string{"Bash git status"}) || len(ap.rc.list()) != 0 {
		t.Fatal(rc.list(), ap.rc.list())
	}
}

func TestAllowedToolsSkipCallbackAndNoCallbackDenies(t *testing.T) {
	calls := []Block{ToolUse("t1", "Read", kv("file_path", "/etc/hosts")), ToolUse("t2", "Write", kv("file_path", "/work/a"))}
	rc, ap := &rec{}, &approver{allow: true}
	_, _ = Query(ctx, "x", Options{Model: &fake{fn: once(calls...)}, Tools: stdTools(rc), AllowedTools: []string{"Read"}, CanUseTool: ap.fn})
	if !reflect.DeepEqual(ap.rc.list(), []string{"Write"}) || len(rc.list()) != 2 {
		t.Fatal(ap.rc.list(), rc.list())
	}

	rc = &rec{}
	f := &fake{fn: once(calls...)}
	_, _ = Query(ctx, "x", Options{Model: f, Tools: stdTools(rc), AllowedTools: []string{"Read"}})
	r := lastResults(f.reqs[1])
	if !reflect.DeepEqual(rc.list(), []string{"Read /etc/hosts"}) || !r[1].IsError || !strings.Contains(r[1].Content, "no approver") {
		t.Fatal(rc.list(), r)
	}
}

func TestToolNameGlobs(t *testing.T) {
	rc := &rec{}
	mcp := func(name string) *Tool {
		return &Tool{Name: name, Run: func(context.Context, map[string]any) (string, error) { rc.add(name); return "ok", nil }}
	}
	tools := append(stdTools(rc), mcp("mcp__github__get_issue"), mcp("mcp__github__create_issue"))
	ap := &approver{}
	f := &fake{fn: once(ToolUse("t1", "mcp__github__get_issue", nil), ToolUse("t2", "mcp__github__create_issue", nil))}
	_, _ = Query(ctx, "x", Options{Model: f, Tools: tools, CanUseTool: ap.fn,
		AllowedTools: []string{"*", "mcp__*", "mcp__github__get_*"}}) // 前两条没有锚定到具体 server，被忽略
	if !reflect.DeepEqual(rc.list(), []string{"mcp__github__get_issue"}) || !reflect.DeepEqual(ap.rc.list(), []string{"mcp__github__create_issue"}) {
		t.Fatal(rc.list(), ap.rc.list())
	}

	f = &fake{fn: once()}
	_, _ = Query(ctx, "x", Options{Model: f, Tools: tools, DisallowedTools: []string{"mcp__*", "Bash"}})
	if !reflect.DeepEqual(f.reqs[0].Tools, []string{"Read", "Write"}) {
		t.Fatal(f.reqs[0].Tools)
	}
}

func TestPathRules(t *testing.T) {
	w := &Tool{Name: "Write", EditsFiles: true, SpecKey: "file_path"}
	cases := []struct {
		rule, path string
		want       bool
	}{
		{"Write(//etc/**)", "/etc/passwd", false}, // 写文件的路径规则要写成 Edit(...)
		{"Edit(//etc/**)", "/etc/passwd", true},
		{"Edit(//etc/**)", "/work/../etc/x", true}, // 先规范化路径
		{"Edit(/secrets/**)", "/work/secrets/k", true},
		{"Edit(/secrets/**)", "/secrets/k", false}, // 单斜杠是工作目录内
		{"Write", "/anything", true},
	}
	for _, c := range cases {
		if got := parseRule(c.rule).matches(w, kv("file_path", c.path), "/work", false); got != c.want {
			t.Errorf("%s %s = %v", c.rule, c.path, got)
		}
	}
}

func TestAcceptEditsScopedToCwd(t *testing.T) {
	rc, ap := &rec{}, &approver{}
	f := &fake{fn: once(
		ToolUse("t1", "Write", kv("file_path", "/work/a.go")),
		ToolUse("t2", "Write", kv("file_path", "/work/../etc/x")),
		ToolUse("t3", "Bash", kv("command", "make")),
	)}
	_, _ = Query(ctx, "x", Options{Model: f, Tools: stdTools(rc), PermissionMode: ModeAcceptEdits, CanUseTool: ap.fn})
	if !reflect.DeepEqual(rc.list(), []string{"Write /work/a.go"}) || !reflect.DeepEqual(ap.rc.list(), []string{"Write", "Bash"}) {
		t.Fatal(rc.list(), ap.rc.list())
	}
}

func TestPlanModeEditsReachCallback(t *testing.T) {
	rc, ap := &rec{}, &approver{allow: true}
	allow := func(context.Context, HookInput) (*HookOutput, error) {
		return &HookOutput{PermissionDecision: "allow"}, nil
	}
	f := &fake{fn: once(ToolUse("t1", "Write", kv("file_path", "/work/a")), ToolUse("t2", "Read", kv("file_path", "/work/b")))}
	_, _ = Query(ctx, "x", Options{Model: f, Tools: stdTools(rc), PermissionMode: ModePlan, CanUseTool: ap.fn,
		AllowedTools: []string{"Write", "Read"},
		Hooks:        map[string][]HookMatcher{"PreToolUse": {{Matcher: "Write", Hooks: []HookFunc{allow}}}}})
	if !reflect.DeepEqual(ap.rc.list(), []string{"Write"}) || !reflect.DeepEqual(ap.reason.list(), []string{"plan"}) || len(rc.list()) != 2 {
		t.Fatal(ap.rc.list(), ap.reason.list(), rc.list())
	}
}

// ---------- Hooks ----------

func TestHookAggregationAndUpdatedInput(t *testing.T) {
	rc, ap := &rec{}, &approver{allow: true}
	h := func(o HookOutput) HookFunc {
		return func(context.Context, HookInput) (*HookOutput, error) { oo := o; return &oo, nil }
	}
	db := &Tool{Name: "mcp__db__query", Run: func(context.Context, map[string]any) (string, error) { rc.add("db"); return "", nil }}
	f := &fake{fn: once(
		ToolUse("t1", "Write", kv("file_path", "/work/x")),
		ToolUse("t2", "mcp__db__query", nil),
		ToolUse("t3", "Bash", kv("command", "ls")),
	)}
	_, _ = Query(ctx, "x", Options{Model: f, Tools: append(stdTools(rc), db), CanUseTool: ap.fn,
		Hooks: map[string][]HookMatcher{"PreToolUse": {
			{Matcher: "Write|Edit", Hooks: []HookFunc{h(HookOutput{PermissionDecision: "allow", UpdatedInput: kv("file_path", "/work/sandbox/x")}), h(HookOutput{})}},
			{Matcher: "^mcp__", Hooks: []HookFunc{h(HookOutput{PermissionDecision: "deny", Reason: "MCP 已禁用"}), h(HookOutput{PermissionDecision: "allow"})}},
			{Matcher: "Bash", Hooks: []HookFunc{h(HookOutput{PermissionDecision: "allow"}), h(HookOutput{PermissionDecision: "ask"})}},
		}}})
	r := lastResults(f.reqs[1])
	if !reflect.DeepEqual(rc.list(), []string{"Write /work/sandbox/x", "Bash ls"}) || r[1].Content != "MCP 已禁用" || !r[1].IsError {
		t.Fatal(rc.list(), r)
	}
	if !reflect.DeepEqual(ap.rc.list(), []string{"Bash"}) { // ask 压过 allow，交给回调
		t.Fatal(ap.rc.list())
	}
}

func TestHookFailure(t *testing.T) {
	broken := func(context.Context, HookInput) (*HookOutput, error) { return nil, errors.New("policy service down") }
	silent := func(context.Context, HookInput) (*HookOutput, error) { return nil, nil }
	run := func(f HookFunc, onFailure string) ([]string, Block) {
		rc := &rec{}
		m := &fake{fn: once(ToolUse("t1", "Bash", kv("command", "ls")))}
		_, _ = Query(ctx, "x", Options{Model: m, Tools: stdTools(rc), AllowedTools: []string{"Bash"},
			Hooks: map[string][]HookMatcher{"PreToolUse": {{Matcher: "Bash", Hooks: []HookFunc{f}, OnFailure: onFailure}}}})
		return rc.list(), lastResults(m.reqs[1])[0]
	}
	if ran, _ := run(broken, ""); len(ran) != 1 { // 默认：守卫崩了，调用照常进入后面的权限流程
		t.Fatal("默认应放行到后续步骤", ran)
	}
	if ran, r := run(broken, "block"); len(ran) != 0 || !strings.Contains(r.Content, `onFailure is "block"`) {
		t.Fatal(ran, r)
	}
	if ran, _ := run(silent, "block"); len(ran) != 0 { // 什么都不返回也算失败
		t.Fatal(ran)
	}
}

func TestPostToolUseRewritesOutput(t *testing.T) {
	rc, post, fail := &rec{}, &rec{}, &rec{}
	redacted := "[已脱敏]"
	boom := &Tool{Name: "Boom", SelfApprove: func(string, map[string]any) bool { return true },
		Run: func(context.Context, map[string]any) (string, error) { return "", errors.New("磁盘满了") }}
	f := &fake{fn: once(ToolUse("t1", "Read", kv("file_path", "/work/.env")), ToolUse("t2", "Boom", nil))}
	_, _ = Query(ctx, "x", Options{Model: f, Tools: append(stdTools(rc), boom),
		Hooks: map[string][]HookMatcher{
			"PostToolUse": {{Hooks: []HookFunc{
				func(_ context.Context, in HookInput) (*HookOutput, error) {
					post.add(in.ToolName)
					if in.ToolName == "Read" {
						return &HookOutput{UpdatedToolOutput: &redacted}, nil
					}
					return &HookOutput{}, nil
				},
				func(_ context.Context, in HookInput) (*HookOutput, error) {
					return &HookOutput{AdditionalContext: "注意：这是密钥文件"}, nil
				},
			}}},
			"PostToolUseFailure": {{Hooks: []HookFunc{func(_ context.Context, in HookInput) (*HookOutput, error) {
				fail.add(in.ToolName + ":" + in.ToolOutput)
				return &HookOutput{}, nil
			}}}},
		}})
	r := lastResults(f.reqs[1])
	if r[0].Content != "[已脱敏]\n\n注意：这是密钥文件" || !r[1].IsError {
		t.Fatal(r)
	}
	if !reflect.DeepEqual(post.list(), []string{"Read"}) || !reflect.DeepEqual(fail.list(), []string{"Boom:磁盘满了"}) {
		t.Fatal(post.list(), fail.list())
	}
}

func TestCanUseToolUpdatedInputAndRemember(t *testing.T) {
	rc := &rec{}
	n := 0
	cb := func(_ context.Context, r PermissionRequest) PermissionResult {
		n++
		cmd := r.Input["command"].(string)
		return PermissionResult{Behavior: "allow", UpdatedInput: kv("command", cmd+" --silent"), UpdatedPermissions: r.Suggestions}
	}
	f := &fake{fn: func(i int, _ ModelRequest) ModelResponse {
		if i <= 2 {
			return resp(ToolUse(fmt.Sprint("t", i), "Bash", kv("command", "npm test")))
		}
		return resp(Text("测试通过"))
	}}
	_, _ = Query(ctx, "x", Options{Model: f, Tools: stdTools(rc), CanUseTool: cb})
	// 第二次命中“记住”的规则 Bash(npm test)，不再问，也就没有被改写
	if n != 1 || !reflect.DeepEqual(rc.list(), []string{"Bash npm test --silent", "Bash npm test"}) {
		t.Fatal(n, rc.list())
	}
}

// ---------- defer ----------

func TestDeferAndResume(t *testing.T) {
	var mu sync.Mutex
	answer := ""
	ask := &Tool{Name: "AskUserQuestion", Run: func(_ context.Context, in map[string]any) (string, error) {
		return fmt.Sprint("用户选了 ", in["answer"]), nil
	}}
	hook := func(_ context.Context, in HookInput) (*HookOutput, error) {
		mu.Lock()
		defer mu.Unlock()
		if answer == "" {
			return &HookOutput{PermissionDecision: "defer"}, nil
		}
		return &HookOutput{PermissionDecision: "allow", UpdatedInput: kv("answer", answer)}, nil
	}
	f := &fake{fn: func(n int, _ ModelRequest) ModelResponse {
		if n == 1 {
			return resp(ToolUse("q1", "AskUserQuestion", kv("question", "用什么语言？")))
		}
		return resp(Text("好，用 Go"))
	}}
	store := NewSessionStore()
	opts := Options{Model: f, Tools: []*Tool{ask}, Store: store,
		Hooks: map[string][]HookMatcher{"PreToolUse": {{Matcher: "AskUserQuestion", Hooks: []HookFunc{hook}}}}}

	r1, _ := Query(ctx, "建个项目", opts)
	if r1.StopReason != "tool_deferred" || r1.DeferredToolUse == nil || r1.DeferredToolUse.ID != "q1" || f.calls() != 1 {
		t.Fatal(r1)
	}
	opts.Resume = r1.SessionID
	if _, err := Query(ctx, "继续", opts); !errors.Is(err, ErrDeferredPending) {
		t.Fatal(err)
	}
	mu.Lock()
	answer = "Go"
	mu.Unlock()
	r2, err := Query(ctx, "", opts)
	if err != nil || r2.Result != "好，用 Go" || r2.SessionID != r1.SessionID || f.calls() != 2 {
		t.Fatal(r2, err)
	}
	if got := lastResults(f.reqs[1])[0].Content; got != "用户选了 Go" {
		t.Fatal(got)
	}

	// 一轮里有两个调用：defer 被忽略，走正常流程（没有回调 → 拒绝）
	mu.Lock()
	answer = ""
	mu.Unlock()
	f2 := &fake{fn: once(ToolUse("q1", "AskUserQuestion", nil), ToolUse("q2", "AskUserQuestion", nil))}
	opts2 := opts
	opts2.Model, opts2.Resume = f2, ""
	r3, _ := Query(ctx, "x", opts2)
	if r3.StopReason != "end_turn" || !lastResults(f2.reqs[1])[0].IsError {
		t.Fatal(r3)
	}
}

// ---------- 子 agent ----------

func TestSubagentContextIsolation(t *testing.T) {
	rc, starts, readBy := &rec{}, &rec{}, &rec{}
	main := &fake{fn: func(n int, _ ModelRequest) ModelResponse {
		if n == 1 {
			return resp(Text("交给审查员"), ToolUse("a1", "Agent", kv("subagent_type", "reviewer", "prompt", "审查 /work/a.go")))
		}
		return resp(Text("审查完了"))
	}}
	sub := &fake{fn: func(n int, _ ModelRequest) ModelResponse {
		if n == 1 {
			return resp(ToolUse("s1", "Read", kv("file_path", "/work/a.go")))
		}
		return resp(Text("发现 2 个问题"))
	}}
	record := func(r *rec) HookFunc {
		return func(_ context.Context, in HookInput) (*HookOutput, error) {
			r.add(in.Event + ":" + in.AgentType)
			return &HookOutput{}, nil
		}
	}
	res, _ := Query(ctx, "审查一下", Options{
		Model: router{"主": main, "你是代码审查员": sub}, System: "主", Tools: stdTools(rc),
		Agents: map[string]AgentDefinition{"reviewer": {Description: "代码审查", Prompt: "你是代码审查员", Tools: []string{"Read"}}},
		Hooks: map[string][]HookMatcher{
			"SubagentStart": {{Hooks: []HookFunc{record(starts)}}},
			"SubagentStop":  {{Hooks: []HookFunc{record(starts)}}},
			"PreToolUse":    {{Matcher: "Read", Hooks: []HookFunc{record(readBy)}}},
		}})
	if res.Result != "审查完了" {
		t.Fatal(res)
	}
	want := []Message{{Role: "user", Blocks: []Block{Text("审查 /work/a.go")}}}
	if !reflect.DeepEqual(sub.reqs[0].Messages, want) || !reflect.DeepEqual(sub.reqs[0].Tools, []string{"Read"}) {
		t.Fatalf("子 agent 应从空白上下文开始：%+v %v", sub.reqs[0].Messages, sub.reqs[0].Tools)
	}
	if !contains(main.reqs[0].Tools, "Agent") {
		t.Fatal(main.reqs[0].Tools)
	}
	if got := lastResults(main.reqs[1])[0].Content; got != "发现 2 个问题" {
		t.Fatal(got)
	}
	for _, m := range main.reqs[1].Messages {
		for _, b := range m.Blocks {
			if strings.Contains(b.Content, "content of") {
				t.Fatal("子 agent 的工具结果不应进入主 agent 的上下文")
			}
		}
	}
	nested := 0
	for _, e := range res.Events {
		if e.ParentToolUseID == "a1" {
			nested++
		}
	}
	if nested != 3 || !reflect.DeepEqual(starts.list(), []string{"SubagentStart:reviewer", "SubagentStop:reviewer"}) ||
		!reflect.DeepEqual(readBy.list(), []string{"PreToolUse:reviewer"}) {
		t.Fatal(nested, starts.list(), readBy.list())
	}
}

func TestSubagentDepth(t *testing.T) {
	for _, c := range []struct {
		depth   int
		wantSub bool
	}{{1, false}, {0, true}} { // 0 = 默认 3 层
		w := &fake{fn: func(int, ModelRequest) ModelResponse { return resp(Text("ok")) }}
		main := &fake{fn: once(ToolUse("a1", "Agent", kv("subagent_type", "worker", "prompt", "干活")))}
		_, _ = Query(ctx, "x", Options{Model: router{"": main, "w": w}, MaxSubagentDepth: c.depth,
			Agents: map[string]AgentDefinition{"worker": {Prompt: "w"}}})
		if got := contains(w.reqs[0].Tools, "Agent"); got != c.wantSub {
			t.Fatal(c.depth, w.reqs[0].Tools)
		}
	}
}

func TestSubagentConcurrencyLimit(t *testing.T) {
	release := make(chan struct{})
	var closeOnce sync.Once
	w := &fake{fn: func(int, ModelRequest) ModelResponse { <-release; return resp(Text("ok")) }}
	main := &fake{fn: once(
		ToolUse("a1", "Agent", kv("subagent_type", "worker", "prompt", "1")),
		ToolUse("a2", "Agent", kv("subagent_type", "worker", "prompt", "2")),
		ToolUse("a3", "Agent", kv("subagent_type", "worker", "prompt", "3")),
	)}
	// 被拒绝的那个会马上失败；收到失败再放行另外两个，保证它们拒绝时仍在运行
	onFail := func(_ context.Context, in HookInput) (*HookOutput, error) {
		if strings.Contains(in.ToolOutput, "Concurrent subagent limit reached") {
			closeOnce.Do(func() { close(release) })
		}
		return &HookOutput{}, nil
	}
	_, _ = Query(ctx, "x", Options{Model: router{"": main, "w": w}, MaxConcurrentSubagents: 2,
		Agents: map[string]AgentDefinition{"worker": {Prompt: "w"}},
		Hooks:  map[string][]HookMatcher{"PostToolUseFailure": {{Matcher: "Agent", Hooks: []HookFunc{onFail}}}}})
	refused, okN := 0, 0
	for _, b := range lastResults(main.reqs[1]) {
		if b.IsError && strings.Contains(b.Content, "Concurrent subagent limit reached") {
			refused++
		} else if b.Content == "ok" {
			okN++
		}
	}
	if refused != 1 || okN != 2 {
		t.Fatal(lastResults(main.reqs[1]))
	}
}

func TestSubagentPermissionMode(t *testing.T) {
	cases := []struct{ parent, own, want Mode }{
		{ModePlan, ModeAcceptEdits, ModePlan},
		{ModeBypass, ModeDefault, ModeBypass},
		{ModeDefault, ModeAcceptEdits, ModeAcceptEdits},
		{ModeDefault, ModeBypass, ModeDefault}, // 子 agent 自己写的 bypassPermissions 永远不生效
		{ModeDontAsk, ModeBypass, ModeDontAsk},
		{ModeDefault, "", ModeDefault},
	}
	for _, c := range cases {
		if got := childMode(c.parent, c.own); got != c.want {
			t.Errorf("childMode(%s,%s)=%s", c.parent, c.own, got)
		}
	}
	// 集成：父会话在 plan 模式，子 agent 声明 acceptEdits，它的写操作仍然走到回调
	ap := &approver{}
	editor := &fake{fn: once(ToolUse("e1", "Write", kv("file_path", "/work/a.go")))}
	main := &fake{fn: once(ToolUse("a1", "Agent", kv("subagent_type", "editor", "prompt", "改")))}
	var agentType string
	cb := func(c context.Context, r PermissionRequest) PermissionResult {
		agentType = r.AgentType
		return ap.fn(c, r)
	}
	_, _ = Query(ctx, "x", Options{Model: router{"": main, "e": editor}, Tools: stdTools(&rec{}), PermissionMode: ModePlan,
		CanUseTool: cb, Agents: map[string]AgentDefinition{"editor": {Prompt: "e", PermissionMode: ModeAcceptEdits}}})
	if !reflect.DeepEqual(ap.reason.list(), []string{"plan"}) || agentType != "editor" {
		t.Fatal(ap.reason.list(), agentType)
	}
}

func TestSubagentPartialAndSharedBudget(t *testing.T) {
	digger := &fake{fn: func(n int, _ ModelRequest) ModelResponse {
		return resp(Text("查到一半"), ToolUse(fmt.Sprint("d", n), "Read", kv("file_path", "/work/a")))
	}}
	main := &fake{fn: once(ToolUse("a1", "Agent", kv("subagent_type", "digger", "prompt", "查")))}
	_, _ = Query(ctx, "x", Options{Model: router{"": main, "d": digger}, Tools: stdTools(&rec{}),
		Agents: map[string]AgentDefinition{"digger": {Prompt: "d", MaxTurns: 1}}})
	if got := lastResults(main.reqs[1])[0].Content; !strings.HasPrefix(got, "[partial") || !strings.Contains(got, "查到一半") {
		t.Fatal(got)
	}

	// 预算覆盖整棵树：子 agent 花掉 1.0 之后，再派子 agent 被拒绝，主循环也停下
	w := &fake{fn: func(int, ModelRequest) ModelResponse { return ModelResponse{Blocks: []Block{Text("ok")}, CostUSD: 1.0} }}
	stepped := false
	step := &Tool{Name: "Step", SelfApprove: func(string, map[string]any) bool { return true },
		Run: func(context.Context, map[string]any) (string, error) { stepped = true; return "", nil }}
	main = &fake{fn: once(
		ToolUse("a1", "Agent", kv("subagent_type", "worker", "prompt", "1")),
		ToolUse("s", "Step", nil), // 非只读，把两个 Agent 调用隔成两批
		ToolUse("a2", "Agent", kv("subagent_type", "worker", "prompt", "2")),
	)}
	res, _ := Query(ctx, "x", Options{Model: router{"": main, "w": w}, Tools: []*Tool{step}, MaxBudgetUSD: 1.0,
		Agents: map[string]AgentDefinition{"worker": {Prompt: "w"}}})
	if main.calls() != 1 {
		t.Fatal("主循环应在下一次调模型前停下")
	}
	if res.Subtype != SubtypeMaxBudget || res.TotalCostUSD != 1.0 || !stepped || w.calls() != 1 {
		t.Fatal(res.Subtype, res.TotalCostUSD, stepped, w.calls())
	}
	var refused bool
	for _, e := range res.Events {
		if e.Type == "user" && e.Block.ToolUseID == "a2" && e.Block.Content == "Budget limit reached" {
			refused = true
		}
	}
	if !refused {
		t.Fatal("第二个子 agent 应因预算被拒绝")
	}
}

// ---------- 会话 ----------

func TestSessionsResumeForkContinue(t *testing.T) {
	store := NewSessionStore()
	f := &fake{fn: func(n int, req ModelRequest) ModelResponse { return resp(Text(fmt.Sprint(len(req.Messages)))) }}
	o := Options{Model: f, Store: store}

	r1, _ := Query(ctx, "A", o)
	o.Resume = r1.SessionID
	r2, _ := Query(ctx, "B", o)
	if r2.Result != "3" || r2.SessionID != r1.SessionID {
		t.Fatal(r2)
	}
	o.ForkSession = true
	r3, _ := Query(ctx, "C", o)
	if r3.Result != "5" || r3.SessionID == r1.SessionID {
		t.Fatal(r3)
	}
	if orig, _ := store.Load(r1.SessionID); len(orig) != 4 {
		t.Fatal("fork 不应修改原会话", len(orig))
	}
	r4, _ := Query(ctx, "D", Options{Model: f, Store: store, Continue: true}) // 最近写入的是 fork
	if r4.SessionID != r3.SessionID || r4.Result != "7" {
		t.Fatal(r4)
	}
	if _, err := Query(ctx, "E", Options{Model: f, Store: store, Resume: "nope"}); !errors.Is(err, ErrNoSession) {
		t.Fatal(err)
	}
}
