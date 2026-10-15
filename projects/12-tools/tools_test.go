package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var weather = Tool{
	Name:        "get_weather",
	Description: "查询某个城市当前的天气。用户问到天气、气温、要不要带伞时使用；不提供未来几天的预报。返回温度和天气描述。",
	Props: map[string]Prop{
		"city": {Type: "string", Description: "城市名，例如 北京、上海"},
		"unit": {Type: "string", Enum: []string{"celsius", "fahrenheit"}, Nullable: true},
	},
	Required: []string{"city", "unit"},
	ReadOnly: true,
}

func TestLint(t *testing.T) {
	if w := weather.Lint(true); len(w) != 0 {
		t.Fatal(w)
	}
	bad := Tool{Name: "get weather", Description: "查天气", Props: map[string]Prop{"city": {Type: "string"}}}
	w := bad.Lint(true)
	t.Logf("%v", w)
	if len(w) != 4 {
		t.Fatalf("期望 4 条：名字、描述、参数说明、strict 必填，得到 %d", len(w))
	}
}

func TestRenderTwoVendors(t *testing.T) {
	oa, _ := json.Marshal(weather.OpenAI())
	an, _ := json.Marshal(weather.Anthropic(false))
	for _, want := range []string{`"type":"function"`, `"parameters"`, `"strict":true`, `"additionalProperties":false`, `"type":["string","null"]`} {
		if !strings.Contains(string(oa), want) {
			t.Fatalf("OpenAI 定义缺 %s：%s", want, oa)
		}
	}
	if !strings.Contains(string(an), `"input_schema"`) || strings.Contains(string(an), `"parameters"`) {
		t.Fatalf("Anthropic 用 input_schema：%s", an)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		args, err string
	}{
		{`{"city":"北京","unit":"celsius"}`, ""},
		{`{"city":"北京","unit":null}`, ""},
		{`{"unit":"celsius"}`, `缺少必填参数 "city"`},
		{`{"city":"北京","unit":"kelvin"}`, `只能是 celsius / fahrenheit 之一`},
		{`{"city":1,"unit":null}`, `应为 string`},
		{`{"city":"北京","unit":null,"days":3}`, `未知参数 "days"`},
		{`{"city":"北京",`, `不是合法的 JSON`},
	}
	for _, c := range cases {
		_, err := weather.Validate(json.RawMessage(c.args))
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s → %v，期望含 %q", c.args, err, c.err)
		}
	}
}

func TestParseBothVendors(t *testing.T) {
	oa := `[{"type":"reasoning","id":"rs_1"},
	 {"type":"function_call","id":"fc_1","call_id":"call_a","name":"get_weather","arguments":"{\"city\":\"北京\",\"unit\":null}"},
	 {"type":"function_call","id":"fc_2","call_id":"call_b","name":"get_weather","arguments":"{\"city\":\"上海\",\"unit\":null}"}]`
	an := `[{"type":"text","text":"我查一下两地天气。"},
	 {"type":"tool_use","id":"toolu_a","name":"get_weather","input":{"city":"北京","unit":null}},
	 {"type":"tool_use","id":"toolu_b","name":"get_weather","input":{"city":"上海","unit":null}}]`
	c1, err := ParseOpenAI([]byte(oa))
	if err != nil {
		t.Fatal(err)
	}
	c2, err := ParseAnthropic([]byte(an))
	if err != nil {
		t.Fatal(err)
	}
	if len(c1) != 2 || len(c2) != 2 || c1[0].ID != "call_a" || c2[1].ID != "toolu_b" {
		t.Fatal(c1, c2)
	}
	// 归一化之后参数是同一个 JSON
	if string(canon(c1[1].Args)) != string(canon(c2[1].Args)) {
		t.Fatal(string(c1[1].Args), string(c2[1].Args))
	}
}

func TestResultFormats(t *testing.T) {
	rs := []Result{{CallID: "a", Content: "晴 18°C"}, {CallID: "b", Content: "服务不可用", IsError: true}}
	oa := OpenAIOutputs(rs)
	if len(oa) != 2 || oa[1]["output"] != "ERROR: 服务不可用" || oa[0]["call_id"] != "a" {
		t.Fatal(oa)
	}
	m := AnthropicResults(rs, "还需要别的吗")
	if len(m.Content) != 3 || m.Content[0].Type != "tool_result" || m.Content[2].Type != "text" || !m.Content[1].IsError {
		t.Fatal(m)
	}
}

func TestCheckHistory(t *testing.T) {
	asst := Message{Role: "assistant", Content: []Block{{Type: "tool_use", ID: "1"}, {Type: "tool_use", ID: "2"}}}
	r1 := Block{Type: "tool_result", ToolUseID: "1"}
	r2 := Block{Type: "tool_result", ToolUseID: "2"}
	txt := Block{Type: "text", Text: "结果如下"}
	cases := []struct {
		name string
		msgs []Message
		want error
	}{
		{"正确", []Message{asst, {Role: "user", Content: []Block{r1, r2, txt}}}, nil},
		{"文字在前", []Message{asst, {Role: "user", Content: []Block{txt, r1, r2}}}, ErrTextFirst},
		{"拆成两条", []Message{asst, {Role: "user", Content: []Block{r1}}, {Role: "user", Content: []Block{r2}}}, ErrSplitResults},
		{"漏了一个", []Message{asst, {Role: "user", Content: []Block{r1}}}, ErrMissingResult},
		{"对不上", []Message{asst, {Role: "user", Content: []Block{r1, r2, {Type: "tool_result", ToolUseID: "9"}}}}, ErrOrphanResult},
	}
	for _, c := range cases {
		if err := CheckHistory(c.msgs); !errors.Is(err, c.want) {
			t.Errorf("%s：%v，期望 %v", c.name, err, c.want)
		}
	}
}

func TestStreamInterleaved(t *testing.T) {
	// 两个并行调用的参数片段交错到达
	evs := []Event{
		{Kind: "start", Index: 0, ID: "call_a", Name: "get_weather"},
		{Kind: "delta", Index: 0, Delta: `{"city":"北`},
		{Kind: "start", Index: 1, ID: "call_b", Name: "get_weather"},
		{Kind: "delta", Index: 1, Delta: `{"city":"上海",`},
		{Kind: "delta", Index: 0, Delta: `京","unit":null}`},
		{Kind: "done", Index: 0},
		{Kind: "delta", Index: 1, Delta: `"unit":"celsius"}`},
		{Kind: "done", Index: 1},
	}
	a := NewAccumulator()
	for i, e := range evs {
		if err := a.Add(e); err != nil {
			t.Fatal(err)
		}
		if i == 3 && a.Partial(0) != `{"city":"北` {
			t.Fatal("中途的片段不是合法 JSON，只能显示，不能执行：", a.Partial(0))
		}
	}
	cs := a.Calls()
	if len(cs) != 2 || string(cs[0].Args) != `{"city":"北京","unit":null}` || string(cs[1].Args) != `{"city":"上海","unit":"celsius"}` {
		t.Fatal(cs)
	}
}

func TestStreamErrors(t *testing.T) {
	a := NewAccumulator()
	if err := a.Add(Event{Kind: "delta", Index: 3, Delta: "{"}); err == nil {
		t.Fatal("start 之前的片段应报错")
	}
	a.Add(Event{Kind: "start", Index: 0, ID: "x", Name: "ping"})
	if err := a.Add(Event{Kind: "done", Index: 0}); err != nil || string(a.Calls()[0].Args) != "{}" {
		t.Fatal("无参数工具应得到 {}")
	}
	a.Add(Event{Kind: "start", Index: 1, ID: "y", Name: "get_weather"})
	a.Add(Event{Kind: "delta", Index: 1, Delta: `{"city":"北京"`})
	if err := a.Add(Event{Kind: "done", Index: 1}); err == nil {
		t.Fatal("被截断的参数应报错")
	}
}

func slowWeather(d time.Duration, n *atomic.Int32) Func {
	return func(ctx context.Context, args map[string]any) (string, error) {
		n.Add(1)
		select {
		case <-time.After(d):
			return fmt.Sprintf("%s：晴 18°C", args["city"]), nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func call(id, name, args string) Call { return Call{ID: id, Name: name, Args: json.RawMessage(args)} }

func TestParallelReadOnly(t *testing.T) {
	var n atomic.Int32
	reg := NewRegistry()
	reg.Register(weather, slowWeather(50*time.Millisecond, &n))
	ex := Executor{Reg: reg, Timeout: time.Second}
	calls := []Call{
		call("a", "get_weather", `{"city":"北京","unit":null}`),
		call("b", "get_weather", `{"city":"上海","unit":null}`),
		call("c", "get_weather", `{"city":"广州","unit":null}`),
		call("d", "get_weather", `{"city":"深圳","unit":null}`),
	}
	start := time.Now()
	rs := ex.Run(context.Background(), calls)
	el := time.Since(start)
	t.Logf("4 个 50ms 的只读调用，并行耗时 %v（串行约 200ms）", el.Round(time.Millisecond))
	if el > 150*time.Millisecond {
		t.Fatal("应该并行")
	}
	for i, r := range rs {
		if r.CallID != calls[i].ID || r.IsError {
			t.Fatal("结果顺序要和调用顺序一致", rs)
		}
	}
	ex.MaxParallel = 1
	start = time.Now()
	ex.Run(context.Background(), calls)
	if time.Since(start) < 190*time.Millisecond {
		t.Fatal("MaxParallel=1 时应退化为串行")
	}
}

func TestDedupe(t *testing.T) {
	var n atomic.Int32
	reg := NewRegistry()
	reg.Register(weather, slowWeather(time.Millisecond, &n))
	rs := Executor{Reg: reg}.Run(context.Background(), []Call{
		call("a", "get_weather", `{"city":"北京","unit":null}`),
		call("b", "get_weather", `{"unit":null,"city":"北京"}`),
	})
	if n.Load() != 1 || rs[0].Content != rs[1].Content || rs[1].CallID != "b" {
		t.Fatalf("相同的只读调用只执行一次，但两个 call_id 都要有结果；执行了 %d 次", n.Load())
	}
}

func TestErrorsBecomeResults(t *testing.T) {
	reg := NewRegistry()
	reg.Register(weather, slowWeather(time.Second, new(atomic.Int32)))
	reg.Register(Tool{Name: "boom", Description: "一个会 panic 的工具，用来测试执行器不会因为单个工具崩掉。", ReadOnly: true},
		func(context.Context, map[string]any) (string, error) { panic("nil map") })
	rs := Executor{Reg: reg, Timeout: 30 * time.Millisecond}.Run(context.Background(), []Call{
		call("1", "get_wether", `{}`),
		call("2", "get_weather", `{"city":"北京"}`),
		call("3", "get_weather", `{"city":"北京","unit":null}`),
		call("4", "boom", `{}`),
	})
	want := []string{"未知工具 \"get_wether\"。可用工具：boom, get_weather", "缺少必填参数 \"unit\"", "超时", "工具内部错误：nil map"}
	for i, r := range rs {
		t.Logf("%s → %s", r.CallID, r.Content)
		if !r.IsError || !strings.Contains(r.Content, want[i]) {
			t.Errorf("结果 %d：%q 应含 %q", i, r.Content, want[i])
		}
	}
}

func TestSideEffectsStopOnFailure(t *testing.T) {
	var log []string
	reg := NewRegistry()
	mk := func(name string, fail bool) {
		reg.Register(Tool{Name: name, Description: "一个有副作用的测试工具，会记录自己被调用的顺序。"},
			func(context.Context, map[string]any) (string, error) {
				log = append(log, name)
				if fail {
					return "", errors.New("磁盘已满")
				}
				return "ok", nil
			})
	}
	mk("create_branch", false)
	mk("write_file", true)
	mk("commit", false)
	rs := Executor{Reg: reg}.Run(context.Background(), []Call{
		call("1", "create_branch", `{}`), call("2", "write_file", `{}`), call("3", "commit", `{}`),
	})
	if strings.Join(log, ",") != "create_branch,write_file" {
		t.Fatal(log)
	}
	if !rs[2].IsError || !strings.Contains(rs[2].Content, "未执行：前面的 write_file 调用失败了") {
		t.Fatal(rs[2])
	}
}

func TestLoop(t *testing.T) {
	var n atomic.Int32
	reg := NewRegistry()
	reg.Register(weather, slowWeather(time.Millisecond, &n))
	ex := Executor{Reg: reg}
	model := func(h []Message) Turn {
		last := h[len(h)-1]
		if last.Content[0].Type == "tool_result" {
			return Turn{Text: "北京和上海今天都是晴天，18°C。"}
		}
		return Turn{Calls: []Call{
			call("t1", "get_weather", `{"city":"北京","unit":null}`),
			call("t2", "get_weather", `{"city":"上海","unit":null}`),
		}}
	}
	ans, hist, steps := Loop(context.Background(), model, ex, "北京和上海天气怎么样？", 5)
	if steps != 2 || ans == "" || len(hist) != 4 {
		t.Fatal(steps, ans, len(hist))
	}
	if err := CheckHistory(hist); err != nil {
		t.Fatal("循环生成的历史必须合法：", err)
	}
	// 一个永远在调工具的模型：maxSteps 是兜底
	loopy := func([]Message) Turn {
		return Turn{Calls: []Call{call("x", "get_weather", `{"city":"北京","unit":null}`)}}
	}
	if ans, _, steps := Loop(context.Background(), loopy, ex, "?", 3); ans != "" || steps != 3 {
		t.Fatal(ans, steps)
	}
}
