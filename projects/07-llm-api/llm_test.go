package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- 测试工具 ----------

// fakeServer 依次返回 replies 里的文件（.sse 用 text/event-stream，其余用 JSON），并记录每个请求。
type fakeServer struct {
	t       *testing.T
	mu      sync.Mutex
	replies []string
	bodies  []map[string]any
	headers []http.Header
	paths   []string
}

func newFake(t *testing.T, replies ...string) (*fakeServer, *httptest.Server) {
	f := &fakeServer{t: t, replies: replies}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	var body map[string]any
	b, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(b, &body); err != nil {
		f.t.Errorf("request body is not JSON: %v", err)
	}
	f.bodies = append(f.bodies, body)
	f.headers = append(f.headers, r.Header.Clone())
	f.paths = append(f.paths, r.URL.Path)
	n := len(f.bodies) - 1
	f.mu.Unlock()
	if n >= len(f.replies) {
		http.Error(w, "no more replies", 500)
		return
	}
	reply := f.replies[n]
	var data []byte
	if strings.HasPrefix(reply, "{") { // 内联 JSON
		data = []byte(reply)
	} else {
		var err error
		if data, err = os.ReadFile("testdata/" + reply); err != nil {
			f.t.Fatal(err)
		}
	}
	if strings.HasSuffix(reply, ".sse") {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.Write(data)
}

func (f *fakeServer) body(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[i]
}

func providers(base string) map[string]Provider {
	return map[string]Provider{
		"openai":    &OpenAI{APIKey: "sk-test", BaseURL: base},
		"anthropic": &Anthropic{APIKey: "ak-test", BaseURL: base},
		"gemini":    &Gemini{APIKey: "gk-test", BaseURL: base},
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// jsonEqual 比较两段 JSON 的语义（忽略空白和 key 顺序）。
func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("bad json %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("bad json %s: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}

var weatherTool = Tool{
	Name:        "get_weather",
	Description: "查询城市天气",
	Schema:      json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
}

// 一段“跨三家通用”的历史：用户提问 → 助手调用工具 → 工具结果。
func sampleHistory() []Message {
	return []Message{
		UserText("北京天气怎么样？"),
		{Role: RoleAssistant, Parts: []Part{
			{Type: PartReasoning, Text: "别家的推理", Provider: "elsewhere", Raw: json.RawMessage(`{"secret":1}`)},
			{Type: PartText, Text: "我查一下。"},
			{Type: PartToolCall, CallID: "call_1", Name: "get_weather", Args: json.RawMessage(`{"city":"北京"}`)},
		}},
		{Role: RoleUser, Parts: []Part{{Type: PartToolResult, CallID: "call_1", Name: "get_weather", Result: "城市不存在", IsError: true}}},
	}
}

// ---------- SSE 解析 ----------

func TestSSEReader(t *testing.T) {
	raw := ": comment\r\n" +
		"event: a\r\nid: 7\r\ndata: line1\r\ndata: line2\r\n\r\n" +
		"event: only-event\n\n" + // 没有 data：不派发
		"data:no-space\n\n" +
		"data: 中文不会被劈开\r\r" + // 单独的 \r 也是行尾
		"data: last-without-blank-line" // 缺了结尾空行：不派发
	r := newSSEReader(strings.NewReader(raw))
	var got []sseEvent
	for {
		ev, err := r.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ev)
	}
	want := []sseEvent{
		{Event: "a", Data: "line1\nline2", ID: "7"},
		{Event: "message", Data: "no-space", ID: "7"},
		{Event: "message", Data: "中文不会被劈开", ID: "7"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// ---------- OpenAI ----------

func TestOpenAIEncode(t *testing.T) {
	p := &OpenAI{}
	req := &Request{Model: "m", System: "你是助手", Messages: sampleHistory(), Tools: []Tool{weatherTool}}
	got := mustJSON(t, p.encode(req, false))
	want := `{"model":"m","instructions":"你是助手","input":[` +
		`{"content":"北京天气怎么样？","role":"user"},` +
		`{"content":"我查一下。","role":"assistant"},` +
		`{"arguments":"{\"city\":\"北京\"}","call_id":"call_1","name":"get_weather","type":"function_call"},` +
		`{"call_id":"call_1","output":"ERROR: 城市不存在","type":"function_call_output"}],` +
		`"tools":[{"type":"function","name":"get_weather","description":"查询城市天气","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],` +
		`"store":false}`
	if !jsonEqual(t, json.RawMessage(got), json.RawMessage(want)) {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if !strings.Contains(got, `"store":false`) {
		t.Fatal("store=false must be sent explicitly")
	}
}

func TestOpenAIDecodeToolCallKeepsEncryptedReasoning(t *testing.T) {
	f, srv := newFake(t, "openai_tool.json")
	p := &OpenAI{APIKey: "sk-test", BaseURL: srv.URL}
	resp, err := p.Generate(context.Background(), &Request{Model: "m", Messages: []Message{UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	if f.paths[0] != "/v1/responses" || f.headers[0].Get("Authorization") != "Bearer sk-test" {
		t.Fatalf("path/auth: %s %q", f.paths[0], f.headers[0].Get("Authorization"))
	}
	if resp.Stop != StopToolUse {
		t.Fatalf("stop = %s", resp.Stop)
	}
	parts := resp.Message.Parts
	if len(parts) != 2 || parts[0].Type != PartReasoning || parts[1].Type != PartToolCall {
		t.Fatalf("parts: %+v", parts)
	}
	if !strings.Contains(string(parts[0].Raw), "gAAAA-ENCRYPTED-REASONING") {
		t.Fatal("reasoning item must keep encrypted_content for replay")
	}
	if parts[0].Text != "需要先查天气。" {
		t.Fatalf("summary: %q", parts[0].Text)
	}
	c := parts[1]
	if c.CallID != "call_abc" || c.Name != "get_weather" || string(c.Args) != `{"city":"北京"}` {
		t.Fatalf("call: %+v", c)
	}
	want := Usage{InputTokens: 120, CachedTokens: 64, OutputTokens: 90, ReasoningTokens: 70}
	if resp.Usage != want {
		t.Fatalf("usage %+v", resp.Usage)
	}
}

func TestOpenAITextAndIncomplete(t *testing.T) {
	_, srv := newFake(t, "openai_text.json", "openai_incomplete.json")
	p := &OpenAI{BaseURL: srv.URL}
	r1, err := p.Generate(context.Background(), &Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Message.Text() != "北京今天晴，25°C。" || r1.Stop != StopEnd {
		t.Fatalf("text=%q stop=%s", r1.Message.Text(), r1.Stop)
	}
	if !strings.Contains(string(r1.Message.Parts[0].Raw), `final_answer`) {
		t.Fatal("assistant message raw must keep phase")
	}
	r2, err := p.Generate(context.Background(), &Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Stop != StopLength || r2.RawStop != "incomplete:max_output_tokens" || r2.Message.Text() != "" {
		t.Fatalf("stop=%s raw=%s text=%q", r2.Stop, r2.RawStop, r2.Message.Text())
	}
	t.Logf("推理把 %d 个输出 token 全用完了，一个可见字都没有", r2.Usage.ReasoningTokens)
}

func TestOpenAIStream(t *testing.T) {
	_, srv := newFake(t, "openai_stream_tool.sse", "openai_stream_text.sse")
	p := &OpenAI{BaseURL: srv.URL}

	var args strings.Builder
	var started []string
	resp, err := Collect(p.Stream(context.Background(), &Request{Model: "m"}), func(ev Event) {
		switch ev.Type {
		case EventToolStart:
			started = append(started, ev.Name+"#"+ev.CallID)
		case EventToolArgs:
			args.WriteString(ev.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(started, []string{"get_weather#call_s1"}) {
		t.Fatalf("started %v", started)
	}
	if args.String() != `{"city":"上海"}` {
		t.Fatalf("args deltas joined = %q", args.String())
	}
	call := resp.Message.ToolCalls()[0]
	if string(call.Args) != args.String() || resp.Stop != StopToolUse {
		t.Fatalf("final call %s stop %s", call.Args, resp.Stop)
	}

	var text strings.Builder
	resp, err = Collect(p.Stream(context.Background(), &Request{Model: "m"}), func(ev Event) {
		if ev.Type == EventText {
			text.WriteString(ev.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "你好，世界" || resp.Message.Text() != text.String() {
		t.Fatalf("stream text %q final %q", text.String(), resp.Message.Text())
	}
}

// ---------- Anthropic ----------

func TestAnthropicEncode(t *testing.T) {
	p := &Anthropic{}
	msgs := append(sampleHistory(), Message{Role: RoleAssistant, Parts: []Part{
		{Type: PartText, Text: ""}, // 空文本：跳过
		{Type: PartReasoning, Provider: "openai", Raw: json.RawMessage(`{"type":"reasoning"}`)}, // 别家的：丢掉
	}})
	req := &Request{Model: "m", System: "你是助手", Messages: msgs, Tools: []Tool{weatherTool}}
	got := mustJSON(t, p.encode(req, true))
	want := `{"model":"m","max_tokens":4096,"system":"你是助手","stream":true,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"北京天气怎么样？"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"我查一下。"},{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"北京"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"城市不存在","is_error":true}]}],` +
		`"tools":[{"name":"get_weather","description":"查询城市天气","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]}`
	if !jsonEqual(t, json.RawMessage(got), json.RawMessage(want)) {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestAnthropicGenerateUsageIncludesCache(t *testing.T) {
	f, srv := newFake(t, "anthropic_tool.json")
	p := &Anthropic{APIKey: "ak-test", BaseURL: srv.URL}
	resp, err := p.Generate(context.Background(), &Request{Model: "m", Messages: []Message{UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	h := f.headers[0]
	if f.paths[0] != "/v1/messages" || h.Get("x-api-key") != "ak-test" || h.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("path/headers: %s %v", f.paths[0], h)
	}
	if resp.Usage.InputTokens != 1030 || resp.Usage.CachedTokens != 1000 {
		t.Fatalf("usage %+v: input must include cache reads", resp.Usage)
	}
	t.Logf("input_tokens 字段是 30，真实输入是 %d，命中缓存 %d", resp.Usage.InputTokens, resp.Usage.CachedTokens)
}

func TestAnthropicStreamMatchesGenerate(t *testing.T) {
	_, srv := newFake(t, "anthropic_stream_tool.sse", "anthropic_tool.json")
	p := &Anthropic{BaseURL: srv.URL}

	var kinds []EventType
	var text, args strings.Builder
	streamed, err := Collect(p.Stream(context.Background(), &Request{Model: "m"}), func(ev Event) {
		kinds = append(kinds, ev.Type)
		switch ev.Type {
		case EventText:
			text.WriteString(ev.Text)
		case EventToolArgs:
			args.WriteString(ev.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "我查一下。" || args.String() != `{"city": "北京"}` {
		t.Fatalf("text %q args %q", text.String(), args.String())
	}
	if kinds[len(kinds)-1] != EventDone {
		t.Fatal("last event must be done")
	}
	whole, err := p.Generate(context.Background(), &Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	// 流式拼出来的结果和非流式必须一致：类型、文本、参数、Raw（签名在里面）、停止原因、用量
	if len(streamed.Message.Parts) != len(whole.Message.Parts) {
		t.Fatalf("parts %d vs %d", len(streamed.Message.Parts), len(whole.Message.Parts))
	}
	for i := range whole.Message.Parts {
		a, b := streamed.Message.Parts[i], whole.Message.Parts[i]
		if a.Type != b.Type || a.Text != b.Text || a.CallID != b.CallID || !jsonEqual(t, a.Raw, b.Raw) {
			t.Fatalf("part %d differs:\n stream %+v %s\n whole  %+v %s", i, a, a.Raw, b, b.Raw)
		}
		if a.Type == PartToolCall && !jsonEqual(t, a.Args, b.Args) {
			t.Fatalf("args differ: %s vs %s", a.Args, b.Args)
		}
	}
	if streamed.Stop != whole.Stop || streamed.Usage != whole.Usage {
		t.Fatalf("stop/usage: %s %+v vs %s %+v", streamed.Stop, streamed.Usage, whole.Stop, whole.Usage)
	}
	if !strings.Contains(string(streamed.Message.Parts[0].Raw), "EqQBSIG==") {
		t.Fatal("thinking signature lost")
	}
}

func TestAnthropicStreamTruncatedIsError(t *testing.T) {
	_, srv := newFake(t, "anthropic_stream_truncated.sse")
	p := &Anthropic{BaseURL: srv.URL}
	var sawText bool
	_, err := Collect(p.Stream(context.Background(), &Request{Model: "m"}), func(ev Event) {
		if ev.Type == EventText {
			sawText = true
		}
	})
	if !errors.Is(err, ErrIncompleteStream) {
		t.Fatalf("want ErrIncompleteStream, got %v", err)
	}
	if !IsRetryable(err) {
		t.Fatal("a cut stream should be retryable")
	}
	t.Logf("用户已经看到了文字 (text=%v)，工具参数只到一半，没有 message_stop：整体判失败", sawText)
}

func TestAnthropicStreamErrorEvent(t *testing.T) {
	_, srv := newFake(t, "anthropic_stream_error.sse")
	p := &Anthropic{BaseURL: srv.URL}
	_, err := Collect(p.Stream(context.Background(), &Request{Model: "m"}), nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Type != "overloaded_error" || ae.Status != 0 {
		t.Fatalf("got %v", err)
	}
	if !ae.Retryable() {
		t.Fatal("overloaded_error in stream must be retryable")
	}
}

// ---------- Gemini ----------

func TestGeminiEncode(t *testing.T) {
	p := &Gemini{}
	req := &Request{Model: "m", System: "你是助手", Messages: sampleHistory(), Tools: []Tool{weatherTool}, MaxTokens: 256}
	got := mustJSON(t, p.encode(req, false))
	want := `{"model":"m","system_instruction":"你是助手","input":[` +
		`{"type":"user_input","content":[{"type":"text","text":"北京天气怎么样？"}]},` +
		`{"type":"model_output","content":[{"type":"text","text":"我查一下。"}]},` +
		`{"type":"function_call","id":"call_1","name":"get_weather","arguments":{"city":"北京"}},` +
		`{"type":"function_result","call_id":"call_1","name":"get_weather","result":[{"type":"text","text":"城市不存在"}],"is_error":true}],` +
		`"tools":[{"type":"function","name":"get_weather","description":"查询城市天气","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],` +
		`"generation_config":{"max_output_tokens":256},"store":false}`
	if !jsonEqual(t, json.RawMessage(got), json.RawMessage(want)) {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestGeminiGenerateRequiresAction(t *testing.T) {
	f, srv := newFake(t, "gemini_tool.json")
	p := &Gemini{APIKey: "gk-test", BaseURL: srv.URL}
	resp, err := p.Generate(context.Background(), &Request{Model: "m", Messages: []Message{UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	if f.paths[0] != "/v1beta/interactions" || f.headers[0].Get("x-goog-api-key") != "gk-test" {
		t.Fatalf("path/key: %s", f.paths[0])
	}
	if resp.Stop != StopToolUse || resp.RawStop != "requires_action" {
		t.Fatalf("stop %s/%s", resp.Stop, resp.RawStop)
	}
	if !strings.Contains(string(resp.Message.Parts[0].Raw), "thought_sig_abcd") {
		t.Fatal("thought signature lost")
	}
	want := Usage{InputTokens: 100, CachedTokens: 40, OutputTokens: 55, ReasoningTokens: 30}
	if resp.Usage != want {
		t.Fatalf("usage %+v", resp.Usage)
	}
}

func TestGeminiStream(t *testing.T) {
	_, srv := newFake(t, "gemini_stream_tool.sse", "gemini_stream_text.sse")
	p := &Gemini{BaseURL: srv.URL}

	var args strings.Builder
	resp, err := Collect(p.Stream(context.Background(), &Request{Model: "m"}), func(ev Event) {
		if ev.Type == EventToolArgs {
			args.WriteString(ev.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	// 参考手册的 arguments_delta/arguments 和迁移指南的 arguments/partial_arguments 两种写法都要认
	if args.String() != `{"city": "广州"}` {
		t.Fatalf("args %q", args.String())
	}
	if resp.Stop != StopToolUse || resp.ID != "int_gs1" {
		t.Fatalf("stop %s id %s", resp.Stop, resp.ID)
	}
	th := resp.Message.Parts[0]
	if th.Type != PartReasoning || th.Text != "需要查天气。" || !strings.Contains(string(th.Raw), "sig_stream_1") {
		t.Fatalf("thought %+v %s", th, th.Raw)
	}
	if resp.Usage.OutputTokens != 35 {
		t.Fatalf("usage %+v", resp.Usage)
	}

	var text strings.Builder
	resp, err = Collect(p.Stream(context.Background(), &Request{Model: "m"}), func(ev Event) {
		if ev.Type == EventText {
			text.WriteString(ev.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "从前有座山。" || resp.Message.Text() != text.String() || resp.Stop != StopEnd {
		t.Fatalf("text %q final %q stop %s", text.String(), resp.Message.Text(), resp.Stop)
	}
}

// ---------- 错误处理 ----------

func TestHTTPErrors(t *testing.T) {
	cases := []struct {
		provider  string
		status    int
		body      string
		retryAt   string
		wantType  string
		retryable bool
	}{
		{"openai", 429, `{"error":{"message":"Rate limit reached","type":"requests","code":"rate_limit_exceeded"}}`, "7", "requests", true},
		{"openai", 400, `{"error":{"message":"Unsupported parameter: 'temperature'","type":"invalid_request_error"}}`, "", "invalid_request_error", false},
		{"anthropic", 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, "", "overloaded_error", true},
		{"anthropic", 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, "", "authentication_error", false},
		{"gemini", 429, `{"error":{"code":429,"message":"Resource has been exhausted","status":"RESOURCE_EXHAUSTED"}}`, "", "RESOURCE_EXHAUSTED", true},
		{"gemini", 503, `upstream connect error`, "", "", true},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s_%d", c.provider, c.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.retryAt != "" {
					w.Header().Set("Retry-After", c.retryAt)
				}
				w.WriteHeader(c.status)
				io.WriteString(w, c.body)
			}))
			defer srv.Close()
			_, err := providers(srv.URL)[c.provider].Generate(context.Background(), &Request{Model: "m"})
			var ae *APIError
			if !errors.As(err, &ae) {
				t.Fatalf("want APIError, got %v", err)
			}
			if ae.Status != c.status || ae.Type != c.wantType || ae.Retryable() != c.retryable {
				t.Fatalf("got %+v retryable=%v", ae, ae.Retryable())
			}
			if c.retryAt == "7" && ae.RetryAfter != 7*time.Second {
				t.Fatalf("retry-after %v", ae.RetryAfter)
			}
		})
	}
}

func TestStreamRejectsNonSSE(t *testing.T) {
	_, srv := newFake(t, "openai_text.json") // 服务端回了 JSON
	p := &OpenAI{BaseURL: srv.URL}
	_, err := Collect(p.Stream(context.Background(), &Request{Model: "m"}), nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Type != "bad_content_type" {
		t.Fatalf("got %v", err)
	}
}

func TestInvalidToolArguments(t *testing.T) {
	_, srv := newFake(t, `{"id":"r","status":"completed","output":[{"type":"function_call","call_id":"c","name":"f","arguments":"{\"a\":"}]}`)
	_, err := (&OpenAI{BaseURL: srv.URL}).Generate(context.Background(), &Request{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "not a JSON object") {
		t.Fatalf("got %v", err)
	}
}

// 调用方 break 之后，连接要被关掉，服务端的 r.Context() 要被取消。
func TestStreamBreakClosesConnection(t *testing.T) {
	cancelled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; ; i++ {
			fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"t%d\"}\n\n", i)
			fl.Flush()
			select {
			case <-r.Context().Done():
				close(cancelled)
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}))
	defer srv.Close()
	p := &OpenAI{BaseURL: srv.URL}
	n := 0
	for ev, err := range p.Stream(context.Background(), &Request{Model: "m"}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == EventText {
			n++
			if n == 3 {
				break
			}
		}
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw the client go away")
	}
}

// ---------- Agent 循环 ----------

// 同一个 RunTools，三家各跑一遍：第一轮调工具，第二轮回答。
// 断言：第二个请求里，第一轮的推理块原样回传了，工具调用和结果按各家格式配好了对。
func TestRunToolsAllProviders(t *testing.T) {
	cases := []struct {
		name, first, final string
		check              func(t *testing.T, second map[string]any)
	}{
		{"openai", "openai_tool.json",
			`{"id":"r2","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"北京晴。"}]}],"usage":{}}`,
			func(t *testing.T, b map[string]any) {
				in := mustJSON(t, b["input"])
				for _, s := range []string{"gAAAA-ENCRYPTED-REASONING", `"type":"function_call"`, `"type":"function_call_output"`, `"call_id":"call_abc"`, `"output":"晴，25°C"`} {
					if !strings.Contains(in, s) {
						t.Errorf("second request input missing %s:\n%s", s, in)
					}
				}
			}},
		{"anthropic", "anthropic_tool.json",
			`{"id":"m2","content":[{"type":"text","text":"北京晴。"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			func(t *testing.T, b map[string]any) {
				msgs := b["messages"].([]any)
				if len(msgs) != 3 {
					t.Fatalf("want 3 messages, got %d", len(msgs))
				}
				asst := mustJSON(t, msgs[1])
				if !strings.Contains(asst, `"signature":"EqQBSIG=="`) {
					t.Errorf("thinking block must be replayed: %s", asst)
				}
				res := mustJSON(t, msgs[2])
				if !strings.Contains(res, `"tool_use_id":"toolu_1"`) || !strings.Contains(res, `"role":"user"`) {
					t.Errorf("tool_result: %s", res)
				}
			}},
		{"gemini", "gemini_tool.json",
			`{"id":"int2","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"北京晴。"}]}]}`,
			func(t *testing.T, b map[string]any) {
				in := mustJSON(t, b["input"])
				for _, s := range []string{`"signature":"thought_sig_abcd"`, `"type":"function_result"`, `"call_id":"fc_g1"`, `"name":"get_weather"`} {
					if !strings.Contains(in, s) {
						t.Errorf("second request input missing %s:\n%s", s, in)
					}
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, srv := newFake(t, c.first, c.final)
			p := providers(srv.URL)[c.name]
			var gotCity string
			tools := map[string]ToolFunc{"get_weather": func(_ context.Context, args json.RawMessage) (string, error) {
				var a struct{ City string }
				if err := json.Unmarshal(args, &a); err != nil {
					return "", err
				}
				gotCity = a.City
				return "晴，25°C", nil
			}}
			resp, hist, err := RunTools(context.Background(), p, Request{Model: "m", Messages: []Message{UserText("北京天气？")}, Tools: []Tool{weatherTool}}, tools, 5)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Message.Text() != "北京晴。" || gotCity != "北京" || len(hist) != 4 {
				t.Fatalf("text=%q city=%q hist=%d", resp.Message.Text(), gotCity, len(hist))
			}
			c.check(t, f.body(1))
		})
	}
}

func TestRunToolsUnknownToolAndMaxSteps(t *testing.T) {
	// 模型一直要调一个不存在的工具：每轮都给它 is_error 结果，最后撞上步数上限
	_, srv := newFake(t, "anthropic_tool.json", "anthropic_tool.json")
	p := &Anthropic{BaseURL: srv.URL}
	_, hist, err := RunTools(context.Background(), p, Request{Model: "m", Messages: []Message{UserText("hi")}}, nil, 2)
	if !errors.Is(err, ErrMaxSteps) {
		t.Fatalf("want ErrMaxSteps, got %v", err)
	}
	res := hist[2].Parts[0]
	if !res.IsError || res.Result != "unknown tool: get_weather" {
		t.Fatalf("result %+v", res)
	}
}

// 换模型：Anthropic 产生的历史（带 thinking）发给 OpenAI 和 Gemini。
// 推理块必须丢掉，工具调用和结果要转换成对方的格式。
func TestCrossProviderHistory(t *testing.T) {
	_, srv := newFake(t, "anthropic_tool.json")
	first, err := (&Anthropic{BaseURL: srv.URL}).Generate(context.Background(), &Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	hist := []Message{UserText("北京天气？"), first.Message,
		{Role: RoleUser, Parts: []Part{{Type: PartToolResult, CallID: "toolu_1", Name: "get_weather", Result: "晴"}}}}

	oa := mustJSON(t, (&OpenAI{}).encode(&Request{Model: "m", Messages: hist}, false))
	if strings.Contains(oa, "EqQBSIG") || strings.Contains(oa, "thinking") {
		t.Fatalf("anthropic thinking leaked into openai request: %s", oa)
	}
	for _, s := range []string{`"arguments":"{\"city\":\"北京\"}"`, `"call_id":"toolu_1"`, `"type":"function_call_output"`} {
		if !strings.Contains(oa, s) {
			t.Errorf("openai request missing %s: %s", s, oa)
		}
	}
	gm := mustJSON(t, (&Gemini{}).encode(&Request{Model: "m", Messages: hist}, false))
	if strings.Contains(gm, "EqQBSIG") {
		t.Fatalf("thinking leaked into gemini request: %s", gm)
	}
	for _, s := range []string{`"arguments":{"city":"北京"}`, `"type":"function_result"`, `"type":"model_output"`} {
		if !strings.Contains(gm, s) {
			t.Errorf("gemini request missing %s: %s", s, gm)
		}
	}
}
