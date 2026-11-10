package graph

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func ret(s State) NodeFunc { return func(*Runtime, State) (any, error) { return s, nil } }

func build(t *testing.T, b *Builder) *Graph {
	t.Helper()
	g, err := b.Compile(NewMemSaver())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func list(xs ...any) []any {
	if xs == nil {
		return []any{}
	}
	return xs
}

func TestCompileValidates(t *testing.T) {
	_, err := New().AddNode("a", ret(nil)).AddEdge(START, "a").AddEdge("a", "ghost").Compile(NewMemSaver())
	if !errors.Is(err, ErrUnknownNode) {
		t.Fatalf("边指向不存在的节点：%v", err)
	}
	_, err = New().AddNode("a", ret(nil)).Compile(NewMemSaver())
	if !errors.Is(err, ErrNoEntry) {
		t.Fatalf("没有入口：%v", err)
	}
	_, err = New().AddNode("a", ret(nil)).AddNode("lonely", ret(nil)).AddEdge(START, "a").Compile(NewMemSaver())
	if !errors.Is(err, ErrOrphan) || !strings.Contains(err.Error(), "lonely") {
		t.Fatalf("孤立节点：%v", err)
	}
	_, err = New().AddNode("a", ret(nil)).AddNode("a", ret(nil)).AddEdge(START, "a").Compile(NewMemSaver())
	if !errors.Is(err, ErrDuplicateNode) {
		t.Fatalf("重名：%v", err)
	}
	// 通过 Command 声明的目标算可达
	_, err = New().AddNode("a", ret(nil), "b").AddNode("b", ret(nil)).AddEdge(START, "a").Compile(NewMemSaver())
	if err != nil {
		t.Fatal(err)
	}
}

// 对应官方文档的例子：START → node_a → node_b → END 正好产生 4 个 checkpoint
func TestCheckpointPerSuperStep(t *testing.T) {
	g := build(t, New().Channel("foo", nil).Channel("bar", Append).
		AddNode("node_a", ret(State{"foo": "a", "bar": list("a")})).
		AddNode("node_b", ret(State{"foo": "b", "bar": list("b")})).
		AddEdge(START, "node_a").AddEdge("node_a", "node_b").AddEdge("node_b", END))
	cfg := Config{Thread: "1"}
	res, err := g.Invoke(cfg, State{"foo": "", "bar": list()})
	if err != nil || !res.Done {
		t.Fatal(res, err)
	}
	if !reflect.DeepEqual(res.Values, State{"foo": "b", "bar": list("a", "b")}) {
		t.Fatalf("foo 覆盖、bar 追加：%v", res.Values)
	}
	h := g.History(cfg)
	type row struct {
		step   int
		source string
		next   string
	}
	var got []row
	for _, c := range h {
		var n []string
		for _, t := range c.Next {
			n = append(n, t.Node)
		}
		got = append(got, row{c.Step, c.Source, strings.Join(n, ",")})
	}
	want := []row{{2, "loop", ""}, {1, "loop", "node_b"}, {0, "loop", "node_a"}, {-1, "input", START}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checkpoint 历史：\n%v\n期望\n%v", got, want)
	}
	if h[0].Parent != h[1].ID || h[3].Parent != 0 {
		t.Fatal("parent 链不对")
	}
}

func TestParallelNodesShareOneSuperStep(t *testing.T) {
	var cRuns atomic.Int32
	g := build(t, New().Channel("a", nil).Channel("b", nil).Channel("log", Append).
		AddNode("a", func(_ *Runtime, s State) (any, error) {
			return State{"a": "A", "log": fmt.Sprintf("a 看到 b=%v", s["b"])}, nil
		}).
		AddNode("b", func(_ *Runtime, s State) (any, error) {
			return State{"b": "B", "log": fmt.Sprintf("b 看到 a=%v", s["a"])}, nil
		}).
		AddNode("c", func(_ *Runtime, s State) (any, error) {
			cRuns.Add(1)
			return State{"log": fmt.Sprintf("c 看到 %v%v", s["a"], s["b"])}, nil
		}).
		AddEdge(START, "a").AddEdge(START, "b").AddEdge("a", "c").AddEdge("b", "c"))
	cfg := Config{Thread: "p"}
	res, err := g.Invoke(cfg, State{"log": list()})
	if err != nil {
		t.Fatal(err)
	}
	if want := list("a 看到 b=<nil>", "b 看到 a=<nil>", "c 看到 AB"); !reflect.DeepEqual(res.Values["log"], want) {
		t.Fatalf("同一步的节点看到同一份快照：%v", res.Values["log"])
	}
	if cRuns.Load() != 1 || len(g.History(cfg)) != 4 {
		t.Fatalf("c 被两个前驱触发也只在下一步跑一次：%d 次，%d 个 checkpoint", cRuns.Load(), len(g.History(cfg)))
	}
}

func TestLastValueConflictNeedsReducer(t *testing.T) {
	g := build(t, New().Channel("x", nil).
		AddNode("a", ret(State{"x": 1})).AddNode("b", ret(State{"x": 2})).
		AddEdge(START, "a").AddEdge(START, "b"))
	_, err := g.Invoke(Config{Thread: "c"}, State{})
	if !errors.Is(err, ErrInvalidUpdate) || !strings.Contains(err.Error(), "reducer") {
		t.Fatalf("并行写同一个 LastValue 通道应当报错：%v", err)
	}
	_, err = g.Invoke(Config{Thread: "d"}, State{"nope": 1})
	if !errors.Is(err, ErrUnknownChannel) {
		t.Fatalf("未声明的键：%v", err)
	}
}

func TestSendFanOut(t *testing.T) {
	g := build(t, New().Channel("subjects", nil).Channel("jokes", Append).
		AddNode("plan", ret(nil)).
		AddNode("joke", func(_ *Runtime, s State) (any, error) {
			if _, leak := s["subjects"]; leak {
				return nil, errors.New("Send 的实例不该看到共享状态")
			}
			return State{"jokes": "关于" + s["subject"].(string) + "的笑话"}, nil
		}).
		AddEdge(START, "plan").
		AddConditionalEdges("plan", func(s State) []Target {
			var out []Target
			for _, x := range s["subjects"].([]any) {
				out = append(out, SendTo("joke", State{"subject": x}))
			}
			return out
		}, "joke"))
	res, err := g.Invoke(Config{Thread: "s"}, State{"subjects": list("猫", "狗", "猫头鹰")})
	if err != nil {
		t.Fatal(err)
	}
	if want := list("关于猫的笑话", "关于狗的笑话", "关于猫头鹰的笑话"); !reflect.DeepEqual(res.Values["jokes"], want) {
		t.Fatalf("%v", res.Values["jokes"])
	}
	if h := g.History(Config{Thread: "s"}); len(h[1].Next) != 3 {
		t.Fatalf("三个 Send 在同一步：%v", h[1].Next)
	}
}

func TestCommandUpdatesAndRoutes(t *testing.T) {
	g := build(t, New().Channel("route", nil).Channel("handled", nil).
		AddNode("triage", func(*Runtime, State) (any, error) {
			return Command{Update: State{"route": "billing"}, Goto: []Target{To("billing")}}, nil
		}, "billing", "tech").
		AddNode("billing", ret(State{"handled": "billing"})).
		AddNode("tech", ret(State{"handled": "tech"})).
		AddEdge(START, "triage"))
	res, err := g.Invoke(Config{Thread: "c"}, State{})
	if err != nil || res.Values["route"] != "billing" || res.Values["handled"] != "billing" {
		t.Fatal(res, err)
	}
}

func approvalGraph(t *testing.T, before *atomic.Int32) *Graph {
	return build(t, New().Channel("details", nil).Channel("status", nil).
		AddNode("approval", func(rt *Runtime, s State) (any, error) {
			before.Add(1) // interrupt 之前的代码：恢复时会再执行一次
			ok := rt.Interrupt(map[string]any{"question": "批准这个操作吗？", "details": s["details"]})
			if ok == true {
				return Command{Goto: []Target{To("proceed")}}, nil
			}
			return Command{Goto: []Target{To("cancel")}}, nil
		}, "proceed", "cancel").
		AddNode("proceed", ret(State{"status": "approved"})).
		AddNode("cancel", ret(State{"status": "rejected"})).
		AddEdge(START, "approval"))
}

func TestInterruptAndResume(t *testing.T) {
	var before atomic.Int32
	g := approvalGraph(t, &before)
	cfg := Config{Thread: "approval-123"}
	res, err := g.Invoke(cfg, State{"details": "转账 500 元", "status": "pending"})
	if err != nil || res.Done || len(res.Interrupts) != 1 {
		t.Fatal(res, err)
	}
	if p := res.Interrupts[0].Value.(map[string]any); p["details"] != "转账 500 元" {
		t.Fatal(p)
	}
	snap, _ := g.GetState(cfg)
	if !reflect.DeepEqual(snap.NextNodes(), []string{"approval"}) || len(snap.Interrupts) != 1 {
		t.Fatalf("暂停时状态停在 approval 之前：%v", snap.NextNodes())
	}
	res, err = g.Resume(cfg, true)
	if err != nil || !res.Done || res.Values["status"] != "approved" {
		t.Fatal(res, err)
	}
	if before.Load() != 2 {
		t.Fatalf("恢复时节点从头重跑，interrupt 之前的代码执行了 %d 次", before.Load())
	}
	if _, err := g.Resume(cfg, true); !errors.Is(err, ErrNoInterrupt) {
		t.Fatal(err)
	}
	// 另一个 thread 互不影响
	cfg2 := Config{Thread: "approval-456"}
	_, _ = g.Invoke(cfg2, State{"details": "删库"})
	res, _ = g.Resume(cfg2, false)
	if res.Values["status"] != "rejected" {
		t.Fatal(res)
	}
}

func TestMultipleInterruptsMatchByIndex(t *testing.T) {
	var runs atomic.Int32
	g := build(t, New().Channel("name", nil).Channel("city", nil).
		AddNode("form", func(rt *Runtime, _ State) (any, error) {
			runs.Add(1)
			name := rt.Interrupt("你叫什么？")
			city := rt.Interrupt("你在哪个城市？")
			return State{"name": name, "city": city}, nil
		}).AddEdge(START, "form"))
	cfg := Config{Thread: "f"}
	r1, _ := g.Invoke(cfg, State{})
	r2, _ := g.Resume(cfg, "Alice")
	r3, err := g.Resume(cfg, "北京")
	if err != nil || !r3.Done {
		t.Fatal(r3, err)
	}
	if r1.Interrupts[0].Index != 0 || r2.Interrupts[0].Index != 1 || r2.Interrupts[0].Value != "你在哪个城市？" {
		t.Fatalf("%v %v", r1.Interrupts, r2.Interrupts)
	}
	if r3.Values["name"] != "Alice" || r3.Values["city"] != "北京" || runs.Load() != 3 {
		t.Fatalf("%v，运行 %d 次", r3.Values, runs.Load())
	}
}

func TestParallelInterruptsResumeByID(t *testing.T) {
	var aRuns, bRuns atomic.Int32
	g := build(t, New().Channel("vals", Append).
		AddNode("a", func(rt *Runtime, _ State) (any, error) {
			aRuns.Add(1)
			return State{"vals": fmt.Sprint("a:", rt.Interrupt("question_a"))}, nil
		}).
		AddNode("b", func(rt *Runtime, _ State) (any, error) {
			bRuns.Add(1)
			return State{"vals": fmt.Sprint("b:", rt.Interrupt("question_b"))}, nil
		}).
		AddEdge(START, "a").AddEdge(START, "b"))
	cfg := Config{Thread: "m"}
	res, _ := g.Invoke(cfg, State{})
	if len(res.Interrupts) != 2 {
		t.Fatal(res)
	}
	ia, ib := res.Interrupts[0].ID, res.Interrupts[1].ID
	if _, err := g.Resume(cfg, "x"); !errors.Is(err, ErrAmbiguousResume) {
		t.Fatal(err)
	}
	if _, err := g.Resume(cfg, ResumeMap{"bogus": 1}); !errors.Is(err, ErrUnknownInterrupt) {
		t.Fatal(err)
	}
	res, err := g.Resume(cfg, ResumeMap{ia: "A"}) // 只回答 a
	if err != nil || len(res.Interrupts) != 1 || res.Interrupts[0].ID != ib {
		t.Fatal(res, err)
	}
	if _, err := g.Resume(cfg, ResumeMap{ia: "again"}); !errors.Is(err, ErrStaleResume) {
		t.Fatalf("重复回答应当拒绝：%v", err)
	}
	res, err = g.Resume(cfg, "B") // 现在只剩一个，可以不带 ID
	if err != nil || !res.Done {
		t.Fatal(res, err)
	}
	if !reflect.DeepEqual(res.Values["vals"], list("a:A", "b:B")) {
		t.Fatal(res.Values)
	}
	if aRuns.Load() != 2 || bRuns.Load() != 3 {
		t.Fatalf("a 的结果作为 pending write 保存后不再重跑：a=%d b=%d", aRuns.Load(), bRuns.Load())
	}
}

func TestPendingWritesAfterFailure(t *testing.T) {
	var aRuns, bRuns atomic.Int32
	g := build(t, New().Channel("a", nil).Channel("b", nil).
		AddNode("a", func(*Runtime, State) (any, error) { aRuns.Add(1); return State{"a": "ok"}, nil }).
		AddNode("b", func(*Runtime, State) (any, error) {
			if bRuns.Add(1) == 1 {
				return nil, errors.New("上游 503")
			}
			return State{"b": "ok"}, nil
		}).
		AddEdge(START, "a").AddEdge(START, "b"))
	cfg := Config{Thread: "e"}
	_, err := g.Invoke(cfg, State{})
	var ne *NodeError
	if !errors.As(err, &ne) || !strings.Contains(ne.Task, ":b:") {
		t.Fatalf("%v", err)
	}
	snap, _ := g.GetState(cfg)
	if !reflect.DeepEqual(snap.NextNodes(), []string{"a", "b"}) {
		t.Fatalf("失败后停在这一步：%v", snap.NextNodes())
	}
	res, err := g.Continue(cfg)
	if err != nil || res.Values["a"] != "ok" || res.Values["b"] != "ok" {
		t.Fatal(res, err)
	}
	if aRuns.Load() != 1 || bRuns.Load() != 2 {
		t.Fatalf("成功的 a 不重跑：a=%d b=%d", aRuns.Load(), bRuns.Load())
	}
	if _, err := g.Continue(Config{Thread: "never"}); !errors.Is(err, ErrEmptyInput) {
		t.Fatal(err)
	}
}

func TestThreadMemoryAndReducers(t *testing.T) {
	g := build(t, New().Channel("messages", Append).
		AddNode("echo", func(_ *Runtime, s State) (any, error) {
			m := s["messages"].([]any)
			return State{"messages": "echo:" + m[len(m)-1].(string)}, nil
		}).AddEdge(START, "echo"))
	t1, t2 := Config{Thread: "t1"}, Config{Thread: "t2"}
	_, _ = g.Invoke(t1, State{"messages": "hi"})
	r1, _ := g.Invoke(t1, State{"messages": "again"})
	r2, _ := g.Invoke(t2, State{"messages": "yo"})
	if !reflect.DeepEqual(r1.Values["messages"], list("hi", "echo:hi", "again", "echo:again")) {
		t.Fatalf("同一个 thread 的新输入经 reducer 合并：%v", r1.Values)
	}
	if !reflect.DeepEqual(r2.Values["messages"], list("yo", "echo:yo")) {
		t.Fatalf("新 thread 从空状态开始：%v", r2.Values)
	}
	r3, _ := g.Invoke(t2, State{"messages": Overwrite(list("reset"))})
	if !reflect.DeepEqual(r3.Values["messages"], list("reset", "echo:reset")) {
		t.Fatalf("Overwrite 绕过 reducer：%v", r3.Values)
	}
}

func linear(t *testing.T, bRuns *atomic.Int32) *Graph {
	return build(t, New().Channel("log", Append).
		AddNode("a", ret(State{"log": "a"})).
		AddNode("b", func(*Runtime, State) (any, error) { bRuns.Add(1); return State{"log": "b"}, nil }).
		AddEdge(START, "a").AddEdge("a", "b"))
}

func TestReplayForksHistory(t *testing.T) {
	var bRuns atomic.Int32
	g := linear(t, &bRuns)
	cfg := Config{Thread: "r"}
	_, _ = g.Invoke(cfg, State{})
	h := g.History(cfg)
	var beforeB Checkpoint
	for _, c := range h {
		if len(c.Next) == 1 && c.Next[0].Node == "b" {
			beforeB = c
		}
	}
	res, err := g.Replay(cfg, beforeB.ID)
	if err != nil || !reflect.DeepEqual(res.Values["log"], list("a", "b")) || bRuns.Load() != 2 {
		t.Fatal(res, err, bRuns.Load())
	}
	h2 := g.History(cfg)
	if len(h2) != len(h)+1 || h2[0].Parent != beforeB.ID || h2[1].ID != h[0].ID {
		t.Fatalf("重放产生一个新分支，旧历史保留：%d → %d", len(h), len(h2))
	}
	if _, err := g.Replay(cfg, 999); !errors.Is(err, ErrNoCheckpoint) {
		t.Fatal(err)
	}
}

func TestUpdateStateAsNode(t *testing.T) {
	var bRuns atomic.Int32
	g := linear(t, &bRuns)
	cfg := Config{Thread: "u"}
	_, _ = g.Invoke(cfg, State{})
	if err := g.UpdateState(cfg, State{"log": "human"}, "a"); err != nil {
		t.Fatal(err)
	}
	snap, _ := g.GetState(cfg)
	if snap.Source != "update" || !reflect.DeepEqual(snap.NextNodes(), []string{"b"}) ||
		!reflect.DeepEqual(snap.Values["log"], list("a", "b", "human")) {
		t.Fatalf("以 a 的身份更新：下一步是 b，值经过 reducer：%+v", snap)
	}
	res, _ := g.Continue(cfg)
	if !reflect.DeepEqual(res.Values["log"], list("a", "b", "human", "b")) {
		t.Fatal(res.Values)
	}
}

func TestRecursionLimit(t *testing.T) {
	var loops atomic.Int32
	g := build(t, New().Channel("n", nil).
		AddNode("loop", func(*Runtime, State) (any, error) { loops.Add(1); return nil, nil }).
		AddEdge(START, "loop").
		AddConditionalEdges("loop", func(State) []Target { return []Target{To("loop")} }, "loop"))
	_, err := g.Invoke(Config{Thread: "x", RecursionLimit: 5}, State{})
	if !errors.Is(err, ErrRecursion) || loops.Load() != 4 {
		t.Fatalf("START 占一步，loop 跑 4 步后停：%v，%d", err, loops.Load())
	}
}

func TestInterruptPayloadMustSerialize(t *testing.T) {
	g := build(t, New().Channel("x", nil).
		AddNode("bad", func(rt *Runtime, _ State) (any, error) {
			rt.Interrupt(map[string]any{"validator": func(string) bool { return true }})
			return nil, nil
		}).AddEdge(START, "bad"))
	_, err := g.Invoke(Config{Thread: "s"}, State{})
	var ne *NodeError
	if !errors.As(err, &ne) || !strings.Contains(ne.Msg, "序列化") {
		t.Fatalf("函数不能放进 interrupt 载荷：%v", err)
	}
}
