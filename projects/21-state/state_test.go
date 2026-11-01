package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// scripted 按顺序返回预设的模型回复，并记录被调用了几次。
type scripted struct {
	mu    sync.Mutex
	out   []Message
	calls int
}

func (s *scripted) Next(_ context.Context, _ []Message) (Message, Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls >= len(s.out) {
		return Message{}, Usage{}, errors.New("脚本用完")
	}
	m := s.out[s.calls]
	s.calls++
	return m, Usage{Input: 100, Output: 10}, nil
}

func weatherAgent(store EventStore, toolCalls *int) (*Agent, *scripted) {
	m := &scripted{out: []Message{
		{Calls: []ToolCall{{ID: "c1", Name: "weather", Args: `{"city":"北京"}`}}},
		{Content: "北京今天晴，18 度。"},
	}}
	return &Agent{
		Model:   m,
		Version: "v1",
		Store:   store,
		Tools: map[string]Tool{
			"weather": func(_ context.Context, args string) (string, error) {
				*toolCalls++
				return "晴 18", nil
			},
		},
	}, m
}

func TestAppendConflict(t *testing.T) {
	ctx := context.Background()
	s := &MemStore{}
	if _, err := s.Append(ctx, "r", 0, mustEvent(RunStarted, RunStartedData{Input: "hi"})); err != nil {
		t.Fatal(err)
	}
	// 另一个写入者以为流还是空的
	_, err := s.Append(ctx, "r", 0, mustEvent(RunCompleted, RunCompletedData{}))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("应当冲突，得到 %v", err)
	}
	evs, _ := s.Read(ctx, "r", 0)
	if len(evs) != 1 {
		t.Fatalf("冲突的追加不应写入，现有 %d 条", len(evs))
	}
}

func TestConcurrentAppendOnlyOneWins(t *testing.T) {
	ctx := context.Background()
	s := &MemStore{}
	var wg sync.WaitGroup
	wins := make(chan int, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Append(ctx, "r", 0, mustEvent(RunStarted, RunStartedData{Input: fmt.Sprint(i)})); err == nil {
				wins <- i
			}
		}(i)
	}
	wg.Wait()
	close(wins)
	if n := len(wins); n != 1 {
		t.Fatalf("应当只有 1 个写入成功，实际 %d", n)
	}
}

func TestRunProducesEventsAndView(t *testing.T) {
	ctx := context.Background()
	s := &MemStore{}
	n := 0
	a, _ := weatherAgent(s, &n)
	v, err := a.Run(ctx, "s1", "r1", "北京天气？")
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := s.Read(ctx, "r1", 0)
	var types []string
	for _, e := range evs {
		types = append(types, e.Type)
	}
	want := "run_started model_called tool_started tool_finished model_called run_completed"
	if got := strings.Join(types, " "); got != want {
		t.Fatalf("事件序列\n得到 %s\n期望 %s", got, want)
	}
	if v.Status != Done || v.Final != "北京今天晴，18 度。" || len(v.Steps) != 2 {
		t.Fatalf("投影不对：%+v", v)
	}
	if v.Usage.Input != 200 || v.Usage.Output != 20 {
		t.Fatalf("用量应累加：%+v", v.Usage)
	}
	if v.LastSeq != 6 {
		t.Fatalf("LastSeq=%d", v.LastSeq)
	}
}

func TestMessagesPairToolResults(t *testing.T) {
	ctx := context.Background()
	s := &MemStore{}
	n := 0
	a, _ := weatherAgent(s, &n)
	v, _ := a.Run(ctx, "s1", "r1", "北京天气？")
	roles := []Role{User, Assistant, ToolRole, Assistant}
	if len(v.Messages) != len(roles) {
		t.Fatalf("消息数 %d", len(v.Messages))
	}
	for i, r := range roles {
		if v.Messages[i].Role != r {
			t.Fatalf("第 %d 条角色 %s，期望 %s", i, v.Messages[i].Role, r)
		}
	}
	if v.Messages[2].CallID != v.Messages[1].Calls[0].ID {
		t.Fatal("工具结果必须和调用 ID 配对")
	}
}

func TestApplyRejectsGapsAndUnknown(t *testing.T) {
	var v RunView
	e := mustEvent(RunStarted, RunStartedData{Input: "x"})
	e.Seq = 2
	if err := v.Apply(e); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("跳号应拒绝：%v", err)
	}
	bad := Event{Seq: 1, Type: "model_thought", Schema: 1, Data: []byte(`{}`)}
	if err := v.Apply(bad); !errors.Is(err, ErrUnknownEvent) {
		t.Fatalf("未知事件应拒绝：%v", err)
	}
	future := mustEvent(RunStarted, RunStartedData{})
	future.Seq, future.Schema = 1, 9
	if err := v.Apply(future); !errors.Is(err, ErrUnknownEvent) {
		t.Fatalf("未来版本应拒绝：%v", err)
	}
}

func TestToolResultWithoutStartIsRejected(t *testing.T) {
	evs := []Event{
		mustEvent(RunStarted, RunStartedData{Input: "x"}),
		mustEvent(ToolFinished, ToolFinishedData{CallID: "ghost", Result: "?"}),
	}
	for i := range evs {
		evs[i].Seq = i + 1
	}
	if _, err := Fold(evs); err == nil {
		t.Fatal("没有 tool_started 的结果应当报错")
	}
}

func TestSnapshotPlusTailEqualsFullFold(t *testing.T) {
	ctx := context.Background()
	s := &MemStore{}
	n := 0
	a, _ := weatherAgent(s, &n)
	_, _ = a.Run(ctx, "s1", "r1", "北京天气？")
	all, _ := s.Read(ctx, "r1", 0)
	full, _ := Fold(all)
	snap, _ := Fold(all[:3]) // 在第 3 条处打快照
	b, _ := json.Marshal(snap)
	var stored RunView
	_ = json.Unmarshal(b, &stored) // 模拟存盘再读回
	got, err := Load(ctx, s, "r1", &stored)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := json.Marshal(full)
	y, _ := json.Marshal(got)
	if string(x) != string(y) {
		t.Fatalf("快照+尾部 != 全量\n%s\n%s", x, y)
	}
	if stored.LastSeq != 3 {
		t.Fatal("Load 不应修改传入的快照")
	}
}

func TestUpcastV1ToolFinished(t *testing.T) {
	evs := []Event{
		mustEvent(RunStarted, RunStartedData{Input: "x"}),
		mustEvent(ModelCalled, ModelCalledData{Step: 1, Response: Message{Role: Assistant, Calls: []ToolCall{{ID: "c1", Name: "w"}}}}),
		mustEvent(ToolStarted, ToolStartedData{Step: 1, Call: ToolCall{ID: "c1", Name: "w"}}),
		{Type: ToolFinished, Schema: 1, Data: []byte(`{"call_id":"c1","output":"旧格式","error":true}`)},
	}
	for i := range evs {
		evs[i].Seq = i + 1
	}
	v, err := Fold(evs)
	if err != nil {
		t.Fatal(err)
	}
	tv := v.Steps[0].Tools[0]
	if tv.Result != "旧格式" || !tv.IsError || !tv.Done {
		t.Fatalf("v1 应升级为 v2：%+v", tv)
	}
	if string(evs[3].Data) != `{"call_id":"c1","output":"旧格式","error":true}` {
		t.Fatal("升级不能改原始事件")
	}
}

func TestOpenToolCallAfterCrash(t *testing.T) {
	ctx := context.Background()
	s := &MemStore{}
	a := &Agent{
		Version: "v1", Store: s,
		Model: &scripted{out: []Message{{Calls: []ToolCall{{ID: "pay", Name: "charge", Args: `{"amt":10}`}}}}},
		Tools: map[string]Tool{"charge": func(context.Context, string) (string, error) {
			panic("进程在扣款途中崩溃")
		}},
	}
	func() {
		defer func() { _ = recover() }()
		_, _ = a.Run(ctx, "s", "r", "付款")
	}()
	v, err := Load(ctx, s, "r", nil)
	if err != nil {
		t.Fatal(err)
	}
	open := v.Open()
	if len(open) != 1 || open[0].ID != "pay" {
		t.Fatalf("应找到 1 个未完成的调用：%+v", open)
	}
	evs, _ := s.Read(ctx, "r", 0)
	var ts ToolStartedData
	_ = json.Unmarshal(evs[len(evs)-1].Data, &ts)
	if ts.IdemKey != "r/pay" {
		t.Fatalf("幂等键应为 run/call：%q", ts.IdemKey)
	}
}

func TestReplayIsExactAndCallsNothing(t *testing.T) {
	ctx := context.Background()
	s := &MemStore{}
	n := 0
	a, m := weatherAgent(s, &n)
	_, _ = a.Run(ctx, "s1", "r1", "北京天气？")
	rec, _ := s.Read(ctx, "r1", 0)
	callsBefore, toolsBefore := m.calls, n

	got, err := Replay(ctx, a, rec)
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := SameEvents(rec, got); !ok {
		t.Fatal(why)
	}
	if m.calls != callsBefore || n != toolsBefore {
		t.Fatal("重放不能真的调用模型或工具")
	}
}

func TestReplayDetectsDivergence(t *testing.T) {
	ctx := context.Background()
	s := &MemStore{}
	n := 0
	a, _ := weatherAgent(s, &n)
	_, _ = a.Run(ctx, "s1", "r1", "北京天气？")
	rec, _ := s.Read(ctx, "r1", 0)

	changed := *a
	changed.Version = "v2" // 比如改了系统提示词
	_, err := Replay(ctx, &changed, rec)
	var d *Divergence
	if !errors.As(err, &d) || !errors.Is(err, ErrDivergence) {
		t.Fatalf("应当报告分叉：%v", err)
	}
	if d.Step != 1 {
		t.Fatalf("分叉应在第 1 步：%d", d.Step)
	}

	more := *a
	more.Tools = map[string]Tool{"weather": a.Tools["weather"], "search": a.Tools["weather"]}
	if _, err := Replay(ctx, &more, rec); !errors.Is(err, ErrDivergence) {
		t.Fatalf("工具集合变化也应分叉：%v", err)
	}
}

func TestReplayFailedRunPinpointsError(t *testing.T) {
	ctx := context.Background()
	s := &MemStore{}
	a := &Agent{
		Version: "v1", Store: s, MaxSteps: 3,
		Model: &scripted{out: []Message{
			{Calls: []ToolCall{{ID: "c1", Name: "sql", Args: `DROP x`}}},
			{Calls: []ToolCall{{ID: "c2", Name: "sql", Args: `SELECT 1`}}},
			{Calls: []ToolCall{{ID: "c3", Name: "sql", Args: `SELECT 2`}}},
		}},
		Tools: map[string]Tool{"sql": func(_ context.Context, q string) (string, error) {
			if strings.HasPrefix(q, "DROP") {
				return "", errors.New("权限不足")
			}
			return "1", nil
		}},
	}
	v, err := a.Run(ctx, "s", "r", "清理表")
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != Failed || v.FailedStep != 4 {
		t.Fatalf("应在第 4 步因步数用完失败：%+v", v)
	}
	step, tv, ok := v.FirstToolError()
	if !ok || step != 1 || tv.Call.ID != "c1" || tv.Result != "权限不足" {
		t.Fatalf("第一个错误应是第 1 步的 c1：%d %+v", step, tv)
	}
	rec, _ := s.Read(ctx, "r", 0)
	got, err := Replay(ctx, a, rec)
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := SameEvents(rec, got); !ok {
		t.Fatal(why)
	}
}

func TestReplayModelFailure(t *testing.T) {
	ctx := context.Background()
	s := &MemStore{}
	a := &Agent{Version: "v1", Store: s, Model: &scripted{}, Tools: map[string]Tool{}}
	v, _ := a.Run(ctx, "s", "r", "x")
	if v.Status != Failed || v.FailedStep != 1 {
		t.Fatalf("%+v", v)
	}
	rec, _ := s.Read(ctx, "r", 0)
	got, err := Replay(ctx, a, rec)
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := SameEvents(rec, got); !ok {
		t.Fatal(why)
	}
}

func TestSessionAcrossRuns(t *testing.T) {
	ctx := context.Background()
	s := &MemStore{}
	n := 0
	a, _ := weatherAgent(s, &n)
	_, _ = a.Run(ctx, "s1", "r1", "北京天气？")
	other, _ := weatherAgent(s, &n)
	_, _ = other.Run(ctx, "s2", "rx", "无关会话")

	ix := &SessionIndex{}
	if err := ix.CatchUp(ctx, s); err != nil {
		t.Fatal(err)
	}
	prior, err := SessionMessages(ctx, s, ix, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(prior) != 4 {
		t.Fatalf("s1 应有 4 条历史：%d", len(prior))
	}

	var seen []Message
	b := &Agent{Version: "v1", Store: s, Tools: map[string]Tool{},
		Model: ModelFunc(func(_ context.Context, h []Message) (Message, Usage, error) {
			seen = h
			return Message{Content: "还是晴。"}, Usage{}, nil
		})}
	if _, err := b.RunWithHistory(ctx, "s1", "r2", prior, "明天呢？"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 5 || seen[0].Content != "北京天气？" || seen[4].Content != "明天呢？" {
		t.Fatalf("第二个 run 应看到会话历史：%+v", seen)
	}

	pos := ix.Pos
	if err := ix.CatchUp(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got := ix.Runs["s1"]; len(got) != 2 || got[1] != "r2" || ix.Pos <= pos {
		t.Fatalf("增量追赶应只加入新 run：%v", got)
	}
}

func TestRequestHashStable(t *testing.T) {
	h := []Message{{Role: User, Content: "hi"}}
	a := &Agent{Tools: map[string]Tool{"b": nil, "a": nil, "c": nil}}
	first := RequestHash("v1", a.toolNames(), h)
	for i := 0; i < 50; i++ {
		if RequestHash("v1", a.toolNames(), h) != first {
			t.Fatal("同样输入的指纹必须相同（工具名要排序）")
		}
	}
	if RequestHash("v1", a.toolNames(), append(h, Message{Role: User, Content: "x"})) == first {
		t.Fatal("历史变了指纹应变")
	}
}
