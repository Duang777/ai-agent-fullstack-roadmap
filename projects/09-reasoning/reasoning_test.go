package reasoning

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

// ---------- 推理强度映射 ----------

func TestResolveExactPerProvider(t *testing.T) {
	cases := []struct {
		model string
		eff   Effort
		want  map[string]any
	}{
		{"gpt-6-astra", EffortHigh, map[string]any{"reasoning": map[string]any{"effort": "high"}}},
		{"claude-opus-5-5", EffortLow, map[string]any{"thinking": map[string]any{"type": "adaptive"}, "output_config": map[string]any{"effort": "low"}}},
		{"gemini-3.6-flash", EffortMinimal, map[string]any{"generation_config": map[string]any{"thinking_level": "minimal"}}},
		{"claude-sonnet-5-5", EffortNone, map[string]any{"thinking": map[string]any{"type": "between_tools"}}},
		{"claude-haiku-5-5", EffortNone, map[string]any{"thinking": map[string]any{"type": "disabled"}}},
	}
	for _, c := range cases {
		r, err := Resolve(c.model, c.eff, true)
		if err != nil {
			t.Fatalf("%s/%s: %v", c.model, c.eff, err)
		}
		if !reflect.DeepEqual(r.Body, c.want) || len(r.Warnings) != 0 {
			t.Errorf("%s/%s: got %v %v", c.model, c.eff, r.Body, r.Warnings)
		}
	}
}

func TestResolveStrictRejectsWhatTheServerRejects(t *testing.T) {
	for _, c := range []struct {
		model string
		eff   Effort
	}{
		{"gpt-6-astra", EffortNone},        // 文档：传 none 返回 400
		{"gpt-6.1-sol", EffortMinimal},     // 不支持 none、minimal
		{"claude-opus-5-5", EffortNone},    // 思考关不掉
		{"claude-sonnet-4-6", EffortXHigh}, // 支持 max 但不支持 xhigh
		{"gemini-3.8-flash", EffortMax},    // thinking_level 最高只到 high
	} {
		if _, err := Resolve(c.model, c.eff, true); err == nil {
			t.Errorf("%s/%s should be rejected", c.model, c.eff)
		}
	}
}

func TestResolveClampPicksNearestCheaper(t *testing.T) {
	cases := []struct {
		model string
		in    Effort
		want  Effort
	}{
		{"gpt-6-astra", EffortNone, EffortMinimal},
		{"gpt-6.1-sol", EffortNone, EffortLow},
		{"claude-sonnet-4-6", EffortXHigh, EffortHigh}, // high 和 max 一样近，取便宜的
		{"gemini-3.8-flash", EffortMax, EffortHigh},
		{"gemini-3.8-flash", EffortMinimal, EffortLow},
		{"claude-opus-5-5", EffortNone, EffortLow},
	}
	for _, c := range cases {
		r, err := Resolve(c.model, c.in, false)
		if err != nil || r.Effort != c.want || len(r.Warnings) == 0 {
			t.Errorf("%s/%s: got %s %v %v", c.model, c.in, r.Effort, r.Warnings, err)
		}
	}
}

// ---------- 用量与成本 ----------

func TestUsageNormalization(t *testing.T) {
	// OpenAI 文档示例：75 输入，1186 输出，其中 1024 是推理
	o := FromOpenAI(75, 0, 1186, 1024)
	if o.Visible() != 162 || o.Output != 1186 {
		t.Fatalf("openai %+v", o)
	}
	// Gemini 文档流式示例：62 输入，171 输出，297 思考
	g := FromGemini(62, 171, 297)
	if g.Output != 468 || g.Visible() != 171 {
		t.Fatalf("gemini %+v", g)
	}
	// Anthropic：input_tokens 不含缓存
	a := FromAnthropic(17, 1370, 0, 700, 500)
	if a.Input != 1387 || a.CacheRead != 1370 || a.Visible() != 200 {
		t.Fatalf("anthropic %+v", a)
	}
}

func TestReasoningIsBilledAsOutput(t *testing.T) {
	p := Price{Input: 2, CacheRead: 0.2, Output: 10} // 假价格，每百万 token
	short := FromOpenAI(1000, 0, 200, 0)
	long := FromOpenAI(1000, 0, 8200, 8000) // 同样 200 个可见 token，多了 8000 推理
	if short.Visible() != long.Visible() {
		t.Fatal("same visible output")
	}
	ratio := long.Cost(p) / short.Cost(p)
	if math.Abs(ratio-21) > 1e-9 {
		t.Fatalf("ratio = %v", ratio)
	}
	t.Logf("可见输出一样，多 8000 推理 token，成本是 %.0f 倍", ratio)
}

func TestDiagnoseTruncation(t *testing.T) {
	if d, _ := Diagnose(true, FromOpenAI(75, 0, 300, 300)); d != DiagExhaustedThinking {
		t.Fatal(d)
	}
	if d, _ := Diagnose(true, FromOpenAI(75, 0, 300, 120)); d != DiagTruncatedAnswer {
		t.Fatal(d)
	}
	if d, _ := Diagnose(false, FromOpenAI(75, 0, 300, 300)); d != DiagOK {
		t.Fatal(d)
	}
}

// ---------- 推理块的生命周期 ----------

const sys = "你是一个会用工具的助手。"

var tools = []string{"get_weather"}

func newServer() *Server {
	s := NewServer("test-key")
	s.Reads["claude-fable-5-1"] = []string{"claude-opus-5-5", "claude-opus-5"}
	s.Reads["claude-opus-5-5"] = []string{"claude-opus-5"}
	s.KeepAllTurns["claude-opus-5-5"] = true
	s.KeepAllTurns["claude-fable-5-1"] = true
	return s
}

// toolLoop 构造一段典型的工具循环：用户提问 → 思考 + 调工具 → 工具结果 → 思考 + 回答 → 用户追问。
func toolLoop(s *Server, model string, summarized bool) []Block {
	h := []Block{{Role: "user", Kind: KindText, Text: "北京今天要带伞吗？"}}
	h = append(h, s.Generate(model, sys, tools, h, "用户问北京天气，需要先查。", "需要查天气", summarized,
		Block{Role: "assistant", Kind: KindToolUse, Text: `get_weather{"city":"北京"}`})...)
	h = append(h, Block{Role: "user", Kind: KindToolResult, Text: "小雨，18°C"})
	h = append(h, s.Generate(model, sys, tools, h, "有小雨，建议带伞。", "有雨", summarized,
		Block{Role: "assistant", Kind: KindText, Text: "要带伞，今天有小雨。"})...)
	return append(h, Block{Role: "user", Kind: KindText, Text: "那明天呢？"})
}

func countThinking(bs []Block) int {
	n := 0
	for _, b := range bs {
		if b.Kind == KindThinking || b.Kind == KindRedacted {
			n++
		}
	}
	return n
}

func TestRoundTripUnchangedKeepsAllThinking(t *testing.T) {
	s := newServer()
	h := toolLoop(s, "claude-opus-5-5", true)
	out, dropped, err := s.Render("claude-opus-5-5", sys, tools, h, MismatchError)
	if err != nil || len(dropped) != 0 || countThinking(out) != 2 {
		t.Fatalf("err=%v dropped=%v", err, dropped)
	}
	// 服务端从签名里还原了完整思考，而不是只用摘要
	if out[1].Text != "用户问北京天气，需要先查。" {
		t.Fatalf("rendered %q", out[1].Text)
	}
}

func TestEditingThinkingSummaryIsRejected(t *testing.T) {
	s := newServer()
	h := toolLoop(s, "claude-opus-5-5", true)
	h[1].Text = "需要查天气。" // 改了一个标点
	if _, _, err := s.Render("claude-opus-5-5", sys, tools, h, MismatchError); !errors.Is(err, ErrModified) {
		t.Fatalf("got %v", err)
	}
}

func TestOmittedDisplayIgnoresClientText(t *testing.T) {
	s := newServer()
	h := toolLoop(s, "claude-opus-5-5", false) // display: omitted
	if h[1].Text != "" {
		t.Fatal("omitted thinking must have empty text")
	}
	h[1].Text = "客户端随手写的东西"
	out, dropped, err := s.Render("claude-opus-5-5", sys, tools, h, MismatchError)
	if err != nil || len(dropped) != 0 || out[1].Text != "用户问北京天气，需要先查。" {
		t.Fatalf("err=%v dropped=%v text=%q", err, dropped, out[1].Text)
	}
}

func TestEditingPrefixInvalidatesLaterThinking(t *testing.T) {
	s := newServer()
	h := toolLoop(s, "claude-opus-5-5", true)
	h[3].Text = "中雨，16°C" // 改了工具结果：它在第二个思考块之前

	if _, _, err := s.Render("claude-opus-5-5", sys, tools, h, MismatchError); !errors.Is(err, ErrModified) {
		t.Fatalf("error mode: %v", err)
	}
	out, dropped, err := s.Render("claude-opus-5-5", sys, tools, h, MismatchDrop)
	if err != nil {
		t.Fatal(err)
	}
	if countThinking(out) != 1 || len(dropped) != 1 || dropped[0] != (Dropped{4, "invalid_signature"}) {
		t.Fatalf("dropped=%v", dropped)
	}

	// 改 system prompt：所有思考块都在它之后，全部失效
	_, dropped, _ = s.Render("claude-opus-5-5", sys+"请简洁。", tools, toolLoop(s, "claude-opus-5-5", true), MismatchDrop)
	want := []Dropped{{1, "invalid_signature"}, {4, "after_invalid"}}
	if !reflect.DeepEqual(dropped, want) {
		t.Fatalf("got %v", dropped)
	}
}

func TestSwitchingModelsDropsUnreadableThinking(t *testing.T) {
	s := newServer()
	// Opus 5.5 产生的思考，Fable 5.1 读得懂
	h := toolLoop(s, "claude-opus-5-5", true)
	if _, dropped, _ := s.Render("claude-fable-5-1", sys, tools, h, MismatchError); len(dropped) != 0 {
		t.Fatalf("fable reads opus-5-5: %v", dropped)
	}
	// 反过来，Opus 5.5 读不懂 Fable 5.1 的思考：静默丢弃，不报错
	h2 := toolLoop(s, "claude-fable-5-1", true)
	out, dropped, err := s.Render("claude-opus-5-5", sys, tools, h2, MismatchError)
	if err != nil || countThinking(out) != 0 || len(dropped) != 2 || dropped[0].Reason != "unreadable_model" {
		t.Fatalf("err=%v dropped=%v", err, dropped)
	}
}

func TestLastTurnOnlyModelsStripOlderThinking(t *testing.T) {
	s := newServer()
	h := toolLoop(s, "claude-haiku-4-5", true) // 不在 KeepAllTurns 里：只保留最近一轮
	// 用户发了“那明天呢？”，之前那一轮的思考都被剥掉
	out, dropped, _ := s.Render("claude-haiku-4-5", sys, tools, h, MismatchError)
	if countThinking(out) != 0 || len(dropped) != 2 || dropped[0].Reason != "prior_turn" {
		t.Fatalf("dropped=%v", dropped)
	}
	// 但工具循环内（最后一条 user 是 tool_result）的思考必须保留
	inLoop := h[:4]
	out, dropped, _ = s.Render("claude-haiku-4-5", sys, tools, inLoop, MismatchError)
	if countThinking(out) != 1 || len(dropped) != 0 {
		t.Fatalf("in-loop thinking dropped: %v", dropped)
	}
}

func TestNaiveFilterLosesRedactedThinking(t *testing.T) {
	s := newServer()
	h := []Block{{Role: "user", Kind: KindText, Text: "q"}}
	gen := s.Generate("claude-opus-5-5", sys, tools, h, "hidden", "", false)
	gen[0].Kind = KindRedacted
	h = append(h, gen...)
	h = append(h, Block{Role: "assistant", Kind: KindToolUse, Text: "get_weather{}"}, Block{Role: "user", Kind: KindToolResult, Text: "ok"})
	if countThinking(h) != 1 || countThinking(NaiveStrip(h)) != 0 {
		t.Fatal("NaiveStrip should drop the redacted block (that is the bug)")
	}
}

// ---------- 值不值得多想 ----------

var (
	easy = []Tier{{EffortLow, 0.95, 800}, {EffortMedium, 0.97, 3000}, {EffortHigh, 0.98, 9000}}
	hard = []Tier{{EffortLow, 0.30, 1500}, {EffortMedium, 0.60, 6000}, {EffortHigh, 0.80, 20000}}
)

func TestEscalationBeatsAlwaysHigh(t *testing.T) {
	w := []float64{0.7, 0.3}
	alwaysLow := Mix(w, []Expect{Fixed(easy[0]), Fixed(hard[0])})
	alwaysHigh := Mix(w, []Expect{Fixed(easy[2]), Fixed(hard[2])})
	esc := Mix(w, []Expect{Escalate(easy), Escalate(hard)})

	if !(esc.Success > alwaysHigh.Success && esc.Tokens < alwaysHigh.Tokens) {
		t.Fatalf("esc=%+v high=%+v", esc, alwaysHigh)
	}
	if !(alwaysLow.Tokens < esc.Tokens && alwaysLow.Success < esc.Success) {
		t.Fatalf("low=%+v", alwaysLow)
	}
	t.Logf("永远 low ：成功率 %.1f%%，%5.0f token/题，%5.0f token/正确", 100*alwaysLow.Success, alwaysLow.Tokens, alwaysLow.TokensPerCorrect())
	t.Logf("永远 high：成功率 %.1f%%，%5.0f token/题，%5.0f token/正确", 100*alwaysHigh.Success, alwaysHigh.Tokens, alwaysHigh.TokensPerCorrect())
	t.Logf("逐级升级 ：成功率 %.1f%%，%5.0f token/题，%5.0f token/正确", 100*esc.Success, esc.Tokens, esc.TokensPerCorrect())
}

func TestEscalateFormula(t *testing.T) {
	e := Escalate([]Tier{{EffortLow, 0.5, 100}, {EffortHigh, 0.5, 1000}})
	if math.Abs(e.Success-0.75) > 1e-12 || math.Abs(e.Tokens-600) > 1e-12 {
		t.Fatalf("%+v", e)
	}
}

func TestMajorityVote(t *testing.T) {
	if got := MajorityAccuracy(0.6, 1); math.Abs(got-0.6) > 1e-12 {
		t.Fatal(got)
	}
	if got := MajorityAccuracy(0.6, 3); math.Abs(got-0.648) > 1e-12 {
		t.Fatal(got)
	}
	a5, a15 := MajorityAccuracy(0.6, 5), MajorityAccuracy(0.6, 15)
	if !(a15 > a5 && a5 > 0.648) {
		t.Fatal(a5, a15)
	}
	if MajorityAccuracy(0.4, 15) > 0.4 {
		t.Fatal("voting amplifies a wrong majority")
	}
	var parts []string
	for _, n := range []int{1, 3, 5, 15} {
		parts = append(parts, fmt.Sprintf("%.1f%%", 100*MajorityAccuracy(0.6, n)))
	}
	t.Logf("单次 60%%，投票 1/3/5/15 次：%s", strings.Join(parts, " / "))
}
