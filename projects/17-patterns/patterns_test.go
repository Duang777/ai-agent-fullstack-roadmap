package patterns

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var ctx = context.Background()

func weatherTools() map[string]Tool {
	return map[string]Tool{
		"weather": func(_ context.Context, city string) (string, error) {
			switch city {
			case "北京":
				return "晴 18°C", nil
			case "上海":
				return "小雨 21°C", nil
			}
			return "", errors.New("未知城市：" + city)
		},
	}
}

func TestReActLoop(t *testing.T) {
	m := &Script{Replies: []Message{
		{Calls: []ToolCall{call("c1", "weather", "北京"), call("c2", "weather", "上海")}},
		{Content: "北京晴 18°C，上海小雨 21°C，上海记得带伞。"},
	}}
	tr, err := ReAct(ctx, m, weatherTools(), "北京和上海天气怎样？", 5)
	if err != nil || tr.Steps != 2 || !strings.Contains(tr.Answer, "带伞") {
		t.Fatal(tr, err)
	}
	second := m.Seen[1]
	if len(second) != 4 || second[2].CallID != "c1" || second[3].Content != "小雨 21°C" {
		t.Fatal("每个调用都要有对应的结果，并按顺序放回历史", second)
	}
}

func TestReActToolErrorIsObservation(t *testing.T) {
	m := &Script{Replies: []Message{
		{Calls: []ToolCall{call("c1", "weather", "北平")}},
		{Calls: []ToolCall{call("c2", "wether", "北京")}},
		{Calls: []ToolCall{call("c3", "weather", "北京")}},
		{Content: "晴 18°C"},
	}}
	tr, err := ReAct(ctx, m, weatherTools(), "北京天气", 5)
	if err != nil || tr.Answer != "晴 18°C" {
		t.Fatal(err)
	}
	obs1, obs2 := m.Seen[1][2], m.Seen[2][4]
	if !obs1.IsError || !strings.Contains(obs1.Content, "未知城市") {
		t.Fatal("工具报错要变成观察结果交给模型", obs1)
	}
	if !obs2.IsError || !strings.Contains(obs2.Content, "可用的有：weather") {
		t.Fatal("工具名写错时告诉模型有哪些可用", obs2)
	}
}

func TestReActMaxSteps(t *testing.T) {
	var rs []Message
	for i := 0; i < 10; i++ {
		rs = append(rs, Message{Calls: []ToolCall{call(fmt.Sprint(i), "weather", "北京")}})
	}
	tr, err := ReAct(ctx, &Script{Replies: rs}, weatherTools(), "北京天气", 3)
	if !errors.Is(err, ErrMaxSteps) || tr.Steps != 3 || len(tr.History) != 7 {
		t.Fatal("模型一直调工具不收尾时，步数上限必须兜底", tr.Steps, err)
	}
}

func TestReActCancel(t *testing.T) {
	c, cancel := context.WithCancel(ctx)
	cancel()
	_, err := ReAct(c, &Script{Replies: []Message{{Content: "x"}}}, nil, "q", 3)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestParseText(t *testing.T) {
	st, err := ParseText("Thought 1: 我需要先查总部在哪。\nAction 1: search[阿里巴巴 总部]\nObservation 1: 杭州（这是模型自己编的）\nAction 2: finish[杭州]")
	if err != nil || st.Action != "search" || st.Input != "阿里巴巴 总部" || !strings.HasPrefix(st.Thought, "我需要") {
		t.Fatal("模型顺手编出来的 Observation 和后面的内容都要截掉", st, err)
	}
	if _, err := ParseText("Thought: 直接回答\n答案是杭州"); err == nil {
		t.Fatal("没有 Action 行要报错，并把正确格式告诉模型")
	}
	if _, err := ParseText("Action: search[a]\nAction: search[b]"); err == nil {
		t.Fatal("一步只能有一个 Action")
	}
}

func searchTools(calls *int32, delay time.Duration) map[string]Tool {
	return map[string]Tool{
		"search": func(_ context.Context, q string) (string, error) {
			atomic.AddInt32(calls, 1)
			time.Sleep(delay)
			if strings.Contains(q, "坏") {
				return "", errors.New("搜索超时")
			}
			return "结果(" + q + ")", nil
		},
		"llm": func(_ context.Context, p string) (string, error) { return "总结[" + p + "]", nil },
	}
}

func TestPlanWavesAndRefs(t *testing.T) {
	var n int32
	p := Plan{
		{"E1", "search", "北京 人口"},
		{"E2", "search", "上海 人口"},
		{"E3", "llm", "比较 #E1 和 #E2"},
	}
	if err := p.Validate(searchTools(&n, 0)); err != nil {
		t.Fatal(err)
	}
	w, _ := p.Waves()
	if len(w) != 2 || len(w[0]) != 2 || w[1][0].ID != "E3" {
		t.Fatal(w)
	}
	start := time.Now()
	res, err := Execute(ctx, p, searchTools(&n, 150*time.Millisecond))
	if err != nil || res["E3"] != "总结[比较 结果(北京 人口) 和 结果(上海 人口)]" {
		t.Fatal(res, err)
	}
	if time.Since(start) > 280*time.Millisecond {
		t.Fatal("E1、E2 互不依赖，应当并行执行", time.Since(start))
	}
}

func TestPlanValidate(t *testing.T) {
	var n int32
	tools := searchTools(&n, 0)
	cases := map[string]Plan{
		"重复":    {{"E1", "search", "a"}, {"E1", "search", "b"}},
		"引用不存在": {{"E1", "search", "#E9"}},
		"循环":    {{"E1", "search", "#E2"}, {"E2", "search", "#E1"}},
		"未知工具":  {{"E1", "shell", "rm -rf /"}},
	}
	for name, p := range cases {
		if p.Validate(tools) == nil {
			t.Errorf("%s：模型写的计划执行前必须检查", name)
		}
	}
}

func TestExecuteStopsOnFailure(t *testing.T) {
	var n int32
	p := Plan{{"E1", "search", "好"}, {"E2", "search", "坏"}, {"E3", "llm", "#E1 #E2"}}
	res, err := Execute(ctx, p, searchTools(&n, 0))
	var se *StepError
	if !errors.As(err, &se) || se.Step.ID != "E2" {
		t.Fatal(err)
	}
	if res["E1"] == "" || res["E3"] != "" {
		t.Fatal("失败后不再执行依赖它的步骤，已完成的结果要保留", res)
	}
}

func TestReplanKeepsDoneWork(t *testing.T) {
	var n int32
	tools := searchTools(&n, 0)
	var sawFailure error
	var sawDone string
	planner := func(_ context.Context, task string, done map[string]string, failure error) (Plan, error) {
		if failure == nil {
			return Plan{{"E1", "search", "北京 人口"}, {"E2", "search", "坏 查询"}}, nil
		}
		sawFailure, sawDone = failure, keys(done)
		// 重新规划只补没做完的部分，用新的 ID
		return Plan{{"E3", "search", "上海 人口"}}, nil
	}
	solver := func(_ context.Context, task string, r map[string]string) (string, error) {
		return r["E1"] + " / " + r["E3"], nil
	}
	run, err := PlanAndExecute(ctx, "比较人口", planner, tools, solver, 3)
	if err != nil || run.Plans != 2 || run.Answer != "结果(北京 人口) / 结果(上海 人口)" {
		t.Fatal(run, err)
	}
	if !strings.Contains(sawFailure.Error(), "E2") || sawDone != "E1" {
		t.Fatal("重新规划时要告诉规划器哪步失败、哪些已经做完", sawFailure, sawDone)
	}
	if n != 3 {
		t.Fatal("已完成的 E1 不应重跑", n)
	}
}

func TestReplanGivesUp(t *testing.T) {
	var n int32
	bad := func(context.Context, string, map[string]string, error) (Plan, error) {
		return Plan{{"E1", "shell", "x"}}, nil
	}
	run, err := PlanAndExecute(ctx, "t", bad, searchTools(&n, 0), nil, 2)
	if err == nil || run.Plans != 2 || !strings.Contains(err.Error(), "不存在的工具") {
		t.Fatal(run, err)
	}
}

func TestReflexion(t *testing.T) {
	var seen [][]string
	act := func(_ context.Context, task string, refl []string) (string, error) {
		seen = append(seen, append([]string(nil), refl...))
		switch len(seen) {
		case 1:
			return "func Abs(x int) int { return x }", nil
		case 2:
			return "func Abs(x int) int { if x < 0 { return -x }; return x } // 忘了处理溢出", nil
		}
		return "func Abs(x int) int { if x < 0 { return -x }; return x }", nil
	}
	eval := func(_ context.Context, a string) (Feedback, error) {
		if !strings.Contains(a, "-x") {
			return Feedback{Detail: "Abs(-3) = -3，期望 3"}, nil
		}
		if strings.Contains(a, "溢出") {
			return Feedback{Detail: "注释与实现不符"}, nil
		}
		return Feedback{Pass: true}, nil
	}
	reflect := func(_ context.Context, task, a string, fb Feedback) (string, error) {
		return "上次失败：" + fb.Detail, nil
	}
	run, err := Reflexion(ctx, "写 Abs", act, eval, reflect, 5, 1)
	if err != nil || !run.Pass || run.Trials != 3 {
		t.Fatal(run, err)
	}
	if len(seen[0]) != 0 || seen[1][0] != "上次失败：Abs(-3) = -3，期望 3" {
		t.Fatal("第二次要带上第一次的反思", seen)
	}
	if len(seen[2]) != 1 || seen[2][0] != "上次失败：注释与实现不符" {
		t.Fatal("记忆只保留最近 memSize 条", seen[2])
	}
}

func TestReflexionHonestFailure(t *testing.T) {
	reflections := 0
	run, err := Reflexion(ctx, "t",
		func(context.Context, string, []string) (string, error) { return "错的", nil },
		func(context.Context, string) (Feedback, error) { return Feedback{Detail: "不对"}, nil },
		func(context.Context, string, string, Feedback) (string, error) {
			reflections++
			return "再想想", nil
		},
		3, 3)
	if err != nil || run.Pass || run.Trials != 3 || run.Answer != "错的" {
		t.Fatal("用完次数仍失败，要如实返回 Pass=false", run)
	}
	if reflections != 2 {
		t.Fatal("最后一次失败后不用再反思", reflections)
	}
}
