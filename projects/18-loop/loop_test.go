package loop

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"
)

var ctx = context.Background()

// script 按顺序返回预设的结果；err 不为 nil 时返回错误。
type step struct {
	r   Response
	err error
}

type script struct {
	steps []step
	seen  [][]Message
	opts  []CallOpts
}

func (s *script) Next(_ context.Context, h []Message, o CallOpts) (Response, error) {
	s.seen = append(s.seen, append([]Message(nil), h...))
	s.opts = append(s.opts, o)
	if len(s.steps) == 0 {
		return Response{}, errors.New("脚本用完了")
	}
	st := s.steps[0]
	s.steps = s.steps[1:]
	st.r.Message.Role = Assistant
	return st.r, st.err
}

func noSleep(context.Context, time.Duration) error { return nil }

func testRetry() Retry {
	return Retry{MaxAttempts: 3, Base: 100 * time.Millisecond, Cap: 2 * time.Second, Sleep: noSleep, Rand: rand.New(rand.NewSource(1))}
}

func done(text string, u Usage) step {
	return step{r: Response{Message: Message{Content: text}, Stop: StopDone, Usage: u}}
}

func tool(id, name, args string, u Usage) step {
	return step{r: Response{Message: Message{Calls: []ToolCall{{id, name, args}}}, Stop: StopToolUse, Usage: u}}
}

var echo = map[string]Tool{"echo": func(_ context.Context, a string) (string, error) { return a, nil }}

func TestStopMapping(t *testing.T) {
	cases := map[string]Stop{
		"end_turn": StopDone, "stop_sequence": StopDone, "tool_use": StopToolUse, "max_tokens": StopTruncated,
		"pause_turn": StopPaused, "refusal": StopRefused, "model_context_window_exceeded": StopContextFull, "something_new": StopUnknown,
	}
	for in, want := range cases {
		if got := FromAnthropic(in); got != want {
			t.Errorf("Anthropic %s = %v，期望 %v", in, got, want)
		}
	}
	if FromOpenAI("completed", "", true) != StopToolUse || FromOpenAI("completed", "", false) != StopDone {
		t.Fatal("OpenAI 的工具调用要看 output 里有没有 function_call")
	}
	if FromOpenAI("incomplete", "max_output_tokens", false) != StopTruncated || FromOpenAI("incomplete", "content_filter", false) != StopRefused {
		t.Fatal("incomplete 要看 incomplete_details.reason")
	}
}

func TestCostRoundsUp(t *testing.T) {
	p := Price{Input: 3_000_000, CachedInput: 300_000, Output: 15_000_000} // 每百万：$3 / $0.3 / $15
	if c := p.Cost(Usage{Input: 1_000_000, CachedInput: 1_000_000, Output: 100_000}); c != 4_800_000 {
		t.Fatal(c)
	}
	if c := p.Cost(Usage{Input: 1}); c != 3 {
		t.Fatal("1 个 token 也要计费，向上取整", c)
	}
}

func TestRunHappyPath(t *testing.T) {
	m := &script{steps: []step{tool("c1", "echo", "hi", Usage{Input: 100, Output: 10}), done("好了", Usage{Input: 120, Output: 5})}}
	r, err := Run(ctx, Config{Model: m, Tools: echo, Retry: testRetry(), Price: Price{Input: 1_000_000, Output: 1_000_000}}, "t")
	if err != nil || r.Reason != "done" || r.Answer != "好了" {
		t.Fatal(r, err)
	}
	if r.Spent.Steps != 2 || r.Spent.ToolCalls != 1 || r.Spent.Usage.Total() != 235 || r.Spent.Cost != 235 {
		t.Fatal("步数、工具次数、用量、费用都要累计", r.Spent)
	}
}

func TestBudgetDims(t *testing.T) {
	loopForever := func() *script {
		var ss []step
		for i := 0; i < 50; i++ {
			ss = append(ss, tool("c", "echo", "x", Usage{Input: 1000, Output: 100}))
		}
		return &script{steps: ss}
	}
	cases := []struct {
		b    Budget
		dim  string
		step int
	}{
		{Budget{MaxSteps: 3}, "steps", 3},
		{Budget{MaxToolCalls: 2}, "tool_calls", 2},
		{Budget{MaxTokens: 5000}, "tokens", 5}, // 每步 1100，第 5 步后 5500 ≥ 5000
		{Budget{MaxCost: 2_000}, "cost", 2},    // 每步 $0.0011 = 1100 微美元
	}
	for _, c := range cases {
		r, err := Run(ctx, Config{Model: loopForever(), Tools: echo, Retry: testRetry(), Budget: c.b, Price: Price{Input: 1_000_000, Output: 1_000_000}}, "t")
		var be *BudgetError
		if !errors.As(err, &be) || be.Dim != c.dim || r.Reason != "budget:"+c.dim || r.Spent.Steps != c.step {
			t.Errorf("%s：%v %v steps=%d", c.dim, r.Reason, err, r.Spent.Steps)
		}
	}
}

func TestBudgetWallClock(t *testing.T) {
	clock := time.Unix(0, 0)
	slowTools := map[string]Tool{"echo": func(_ context.Context, a string) (string, error) {
		clock = clock.Add(40 * time.Second)
		return a, nil
	}}
	m := &script{steps: []step{tool("1", "echo", "", Usage{}), tool("2", "echo", "", Usage{}), tool("3", "echo", "", Usage{})}}
	r, err := Run(ctx, Config{Model: m, Tools: slowTools, Retry: testRetry(), Budget: Budget{MaxWall: time.Minute}, Now: func() time.Time { return clock }}, "t")
	var be *BudgetError
	if !errors.As(err, &be) || be.Dim != "wall" || r.Spent.Steps != 2 {
		t.Fatal(r.Reason, err)
	}
}

func TestFinalizeOnBudget(t *testing.T) {
	m := &script{steps: []step{tool("1", "echo", "a", Usage{}), tool("2", "echo", "b", Usage{}), done("只查到 a 和 b，c 没来得及查", Usage{})}}
	r, err := Run(ctx, Config{Model: m, Tools: echo, Retry: testRetry(), Budget: Budget{MaxSteps: 2}, Finalize: true}, "t")
	var be *BudgetError
	if !errors.As(err, &be) || r.Reason != "finalized:steps" || !strings.Contains(r.Answer, "没来得及") {
		t.Fatal("预算用完时给模型一次收尾机会，但仍然要把预算错误报出来", r, err)
	}
	if !m.opts[2].NoTools || m.opts[0].NoTools {
		t.Fatal("只有收尾调用禁用工具", m.opts)
	}
	last := m.seen[2][len(m.seen[2])-1]
	if last.Role != User || !strings.Contains(last.Content, "预算已用完") {
		t.Fatal("要明确告诉模型为什么停", last)
	}
}

func TestPausedAndTruncated(t *testing.T) {
	m := &script{steps: []step{
		{r: Response{Message: Message{Content: "搜索中…"}, Stop: StopPaused}},
		{r: Response{Message: Message{Content: "第一段"}, Stop: StopTruncated}},
		done("第二段", Usage{}),
	}}
	r, err := Run(ctx, Config{Model: m, Retry: testRetry(), MaxContinuations: 2}, "t")
	if err != nil || r.Answer != "第二段" || r.Spent.Steps != 3 {
		t.Fatal(r, err)
	}
	if got := m.seen[1]; len(got) != 2 || got[1].Role != Assistant {
		t.Fatal("pause_turn 原样发回，不加 user 消息", got)
	}
	if got := m.seen[2]; got[len(got)-1].Role != User || !strings.Contains(got[len(got)-1].Content, "截断") {
		t.Fatal("截断后要求续写", got)
	}
}

func TestTruncatedLimits(t *testing.T) {
	cut := step{r: Response{Message: Message{Content: "…"}, Stop: StopTruncated}}
	_, err := Run(ctx, Config{Model: &script{steps: []step{cut, cut, cut}}, Retry: testRetry(), MaxContinuations: 1}, "t")
	if !errors.Is(err, ErrTruncated) {
		t.Fatal("续写次数要有上限", err)
	}
	half := step{r: Response{Message: Message{Calls: []ToolCall{{"1", "echo", `{"q":"abc`}}}, Stop: StopTruncated}}
	_, err = Run(ctx, Config{Model: &script{steps: []step{half}}, Tools: echo, Retry: testRetry(), MaxContinuations: 3}, "t")
	if !errors.Is(err, ErrTruncated) {
		t.Fatal("带半截工具调用的截断不能执行，也不能续写", err)
	}
}

func TestRefusedContextUnknown(t *testing.T) {
	for stop, want := range map[Stop]error{StopRefused: ErrRefused, StopContextFull: ErrContextFull, StopUnknown: ErrUnknownStop} {
		_, err := Run(ctx, Config{Model: &script{steps: []step{{r: Response{Stop: stop}}}}, Retry: testRetry()}, "t")
		if !errors.Is(err, want) {
			t.Errorf("%v：%v", stop, err)
		}
	}
}

func TestRetryable(t *testing.T) {
	yes := []error{
		errors.New("connection reset"),
		&APIError{Status: 429}, &APIError{Status: 500}, &APIError{Status: 503}, &APIError{Status: 529}, &APIError{Status: 504},
	}
	no := []error{
		&APIError{Status: 400}, &APIError{Status: 401}, &APIError{Status: 413},
		&APIError{Status: 429, Code: "enforced_spend_limit_reached"}, &APIError{Status: 429, Code: "credit_balance_exhausted"},
		context.Canceled,
	}
	for _, e := range yes {
		if !Retryable(e) {
			t.Errorf("应重试：%v", e)
		}
	}
	for _, e := range no {
		if Retryable(e) {
			t.Errorf("不应重试：%v", e)
		}
	}
}

func TestRetryDelay(t *testing.T) {
	r := testRetry()
	for i := 0; i < 200; i++ {
		d := r.Delay(i%8, errors.New("x"))
		if d < 0 || d > r.Cap {
			t.Fatal("全抖动的等待落在 [0, Cap] 里", d)
		}
	}
	if d := r.Delay(0, &APIError{Status: 429, RetryAfter: 5 * time.Second}); d < 5*time.Second || d > 5*time.Second+r.Base {
		t.Fatal("retry-after 是下限", d)
	}
	if d := r.Delay(70, errors.New("x")); d > r.Cap {
		t.Fatal("移位溢出也要封顶", d)
	}
}

func TestRetryInLoop(t *testing.T) {
	var slept []time.Duration
	rt := testRetry()
	rt.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	m := &script{steps: []step{
		{err: &APIError{Status: 529}},
		{err: &APIError{Status: 429, RetryAfter: 3 * time.Second}},
		done("ok", Usage{Input: 10}),
	}}
	r, err := Run(ctx, Config{Model: m, Retry: rt}, "t")
	if err != nil || r.Answer != "ok" || r.Spent.Steps != 1 || len(slept) != 2 || slept[1] < 3*time.Second {
		t.Fatal("失败的请求重试，不计入步数", r.Spent, slept, err)
	}

	m = &script{steps: []step{{err: &APIError{Status: 429, Code: "enforced_spend_limit_reached"}}, done("不该到这里", Usage{})}}
	_, err = Run(ctx, Config{Model: m, Retry: testRetry()}, "t")
	var ae *APIError
	if !errors.As(err, &ae) || len(m.seen) != 1 {
		t.Fatal("额度用完不重试", err, len(m.seen))
	}

	m = &script{steps: []step{{err: &APIError{Status: 503}}, {err: &APIError{Status: 503}}, {err: &APIError{Status: 503}}}}
	_, err = Run(ctx, Config{Model: m, Retry: testRetry()}, "t")
	var re *RetryError
	if !errors.As(err, &re) || re.Attempts != 3 || !errors.As(err, &ae) {
		t.Fatal("次数用完返回 RetryError，并能拿到最后一次的原始错误", err)
	}
}

func TestRetryMaxTotalAndCancel(t *testing.T) {
	rt := testRetry()
	rt.MaxAttempts = 10
	rt.MaxTotal = 10 * time.Second
	calls := 0
	err := rt.Do(ctx, func(context.Context) error { calls++; return &APIError{Status: 429, RetryAfter: 4 * time.Second} })
	var re *RetryError
	if !errors.As(err, &re) || calls != 3 {
		t.Fatal("4s+4s 之后再等 4s 会超过 10s 总上限，不再等", calls, err)
	}

	c, cancel := context.WithCancel(ctx)
	rt.Sleep = SleepCtx
	rt.MaxTotal = 0
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	start := time.Now()
	err = rt.Do(c, func(context.Context) error { return &APIError{Status: 503, RetryAfter: time.Hour} })
	if !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
		t.Fatal("等待中被取消要立即返回", err)
	}
}

func TestToolTimeout(t *testing.T) {
	tools := map[string]Tool{"slow": func(c context.Context, _ string) (string, error) {
		select {
		case <-time.After(time.Second):
			return "太慢了", nil
		case <-c.Done():
			return "", c.Err()
		}
	}}
	m := &script{steps: []step{tool("1", "slow", "", Usage{}), done("换个办法", Usage{})}}
	r, err := Run(ctx, Config{Model: m, Tools: tools, Retry: testRetry(), ToolTimeout: 30 * time.Millisecond}, "t")
	if err != nil || r.Answer != "换个办法" {
		t.Fatal(r, err)
	}
	obs := m.seen[1][2]
	if !obs.IsError || !strings.Contains(obs.Content, "超时") {
		t.Fatal("工具超时是观察结果，不是整个循环失败", obs)
	}
}
