// Package llm 是一个只用标准库写成的三家模型 API 统一客户端：
// OpenAI Responses、Anthropic Messages、Gemini Interactions。
//
// 设计原则（课件 1.2 第 13 节）：
//  1. 统一的是“Agent 需要的最小公共语义”：文本、工具调用、工具结果、推理块、停止原因、用量；
//  2. 同一家来回传，原样回放（Part.Raw），一个字节都不丢；
//  3. 换一家时只用规范化字段重建，对方看不懂的东西（推理块）丢掉；
//  4. 流结束但没收到终止事件，一律当失败（0.4 课第 6 节）。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Role 只有两种。工具结果放在 user 消息里（Anthropic 的做法），
// 编码给 OpenAI / Gemini 时再拆成独立的 item / step。
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type PartType string

const (
	PartText       PartType = "text"
	PartToolCall   PartType = "tool_call"
	PartToolResult PartType = "tool_result"
	PartReasoning  PartType = "reasoning"
	// PartOpaque：厂商特有、统一层不理解的内容（服务端工具调用、搜索结果……）。
	// 发回同一家时原样回放，换一家时丢掉。
	PartOpaque PartType = "opaque"
)

// Part 是消息里的一块内容。
type Part struct {
	Type PartType `json:"type"`

	Text string `json:"text,omitempty"` // text；reasoning 的可读摘要

	CallID  string          `json:"call_id,omitempty"` // tool_call / tool_result
	Name    string          `json:"name,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`   // tool_call：一定是 JSON 对象
	Result  string          `json:"result,omitempty"` // tool_result
	IsError bool            `json:"is_error,omitempty"`

	// Provider + Raw：这块内容在原厂商协议里的原始 JSON。
	// 发回同一家时原样回放（推理签名、phase、加密推理内容都在里面）。
	Provider string          `json:"provider,omitempty"`
	Raw      json.RawMessage `json:"raw,omitempty"`
}

type Message struct {
	Role  Role   `json:"role"`
	Parts []Part `json:"parts"`
}

// Text 拼出消息里所有 text 块。
func (m Message) Text() string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Type == PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ToolCalls 返回消息里所有工具调用。
func (m Message) ToolCalls() []Part {
	var out []Part
	for _, p := range m.Parts {
		if p.Type == PartToolCall {
			out = append(out, p)
		}
	}
	return out
}

func UserText(s string) Message {
	return Message{Role: RoleUser, Parts: []Part{{Type: PartText, Text: s}}}
}

type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema，type 必须是 object
}

type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []Tool
	MaxTokens int
}

// StopReason 是规范化后的停止原因。原始值保存在 Response.RawStop。
type StopReason string

const (
	StopEnd     StopReason = "end"      // 正常说完
	StopLength  StopReason = "length"   // 到了 max tokens / 上下文上限：结果被截断
	StopToolUse StopReason = "tool_use" // 等你执行工具
	StopRefusal StopReason = "refusal"  // 拒答 / 内容过滤
	StopPause   StopReason = "pause"    // 服务端工具循环暂停，原样发回即可继续
	StopOther   StopReason = "other"
)

// Usage 统一口径：InputTokens 包含缓存命中的部分。
// Anthropic 的 input_tokens 不含缓存，要自己加回来（课件第 11 节）。
type Usage struct {
	InputTokens     int `json:"input_tokens"`
	CachedTokens    int `json:"cached_tokens"`
	OutputTokens    int `json:"output_tokens"` // 包含推理 token
	ReasoningTokens int `json:"reasoning_tokens"`
}

type Response struct {
	ID      string
	Message Message
	Stop    StopReason
	RawStop string
	Usage   Usage
}

type EventType string

const (
	EventText      EventType = "text"       // Text 是增量
	EventToolStart EventType = "tool_start" // CallID、Name 已知
	EventToolArgs  EventType = "tool_args"  // Text 是参数 JSON 的一段
	EventDone      EventType = "done"       // Response 是完整结果
)

type Event struct {
	Type     EventType
	Index    int // 这一块在本轮输出里的序号，同一个工具调用的事件序号相同
	Text     string
	CallID   string
	Name     string
	Response *Response
}

// Provider 是三家共同的接口。
type Provider interface {
	Name() string
	Generate(ctx context.Context, req *Request) (*Response, error)
	// Stream 用 Go 1.23 的 range-over-func：for ev, err := range p.Stream(ctx, req) {...}
	// 正常结束时最后一个事件一定是 EventDone；否则一定会产出一个非 nil 的 err。
	Stream(ctx context.Context, req *Request) iter.Seq2[Event, error]
}

// ---------- 错误 ----------

// ErrIncompleteStream：HTTP 流结束了，但没收到协议里的终止事件。
var ErrIncompleteStream = errors.New("llm: stream ended without a terminal event")

// APIError 是三家错误的统一形状。
type APIError struct {
	Provider   string
	Status     int    // HTTP 状态码；流中途的错误事件没有状态码，为 0
	Type       string // 厂商给的错误类型 / 状态字符串
	Message    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: HTTP %d %s: %s", e.Provider, e.Status, e.Type, e.Message)
}

// Retryable：限流、过载、服务端错误可以重试；4xx 其他的是你的请求有问题，重试没用。
func (e *APIError) Retryable() bool {
	switch {
	case e.Status == http.StatusTooManyRequests, e.Status == http.StatusRequestTimeout, e.Status == 409:
		return true
	case e.Status >= 500: // 含 Anthropic 的 529 overloaded
		return true
	case e.Status == 0: // 流中途的错误事件：按类型判断
		t := strings.ToLower(e.Type)
		return strings.Contains(t, "overloaded") || strings.Contains(t, "rate") ||
			strings.Contains(t, "unavailable") || strings.Contains(t, "server") ||
			strings.Contains(t, "exhausted") || strings.Contains(t, "internal")
	}
	return false
}

// IsRetryable 判断任意 error 是否值得重试。流断了也值得重试（但要注意已经推给用户的内容）。
func IsRetryable(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Retryable()
	}
	return errors.Is(err, ErrIncompleteStream)
}

// parseAPIError 把三家的错误体解析成 APIError。三家的形状：
//
//	OpenAI:    {"error":{"message":"...","type":"...","code":"..."}}
//	Anthropic: {"type":"error","error":{"type":"overloaded_error","message":"..."}}
//	Gemini:    {"error":{"code":429,"message":"...","status":"RESOURCE_EXHAUSTED"}}
func parseAPIError(provider string, resp *http.Response) *APIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &APIError{Provider: provider, Status: resp.StatusCode}
	var env struct {
		Error struct {
			Message string          `json:"message"`
			Type    string          `json:"type"`
			Status  string          `json:"status"`
			Code    json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error.Message != "" {
		e.Message = env.Error.Message
		e.Type = env.Error.Type
		if e.Type == "" {
			e.Type = env.Error.Status
		}
	} else {
		e.Message = strings.TrimSpace(string(body))
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return e
}

// ---------- HTTP ----------

type httpConfig struct {
	BaseURL string
	Client  *http.Client
}

func (c httpConfig) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return http.DefaultClient
}

// post 发请求，处理 0.4 课的前两层检查：HTTP 状态码、Content-Type。
func post(ctx context.Context, cfg httpConfig, provider, path string, headers map[string]string, body any, stream bool) (*http.Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.BaseURL, "/")+path, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := cfg.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 { // ① HTTP 层
		defer resp.Body.Close()
		return nil, parseAPIError(provider, resp)
	}
	if stream && !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") { // ② SSE 层
		resp.Body.Close()
		return nil, &APIError{Provider: provider, Status: resp.StatusCode, Type: "bad_content_type",
			Message: "expected text/event-stream, got " + resp.Header.Get("Content-Type")}
	}
	return resp, nil
}

func generateJSON[T any](ctx context.Context, cfg httpConfig, provider, path string, headers map[string]string, body any) (*T, error) {
	resp, err := post(ctx, cfg, provider, path, headers, body, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: decode response: %w", provider, err)
	}
	return &out, nil
}

// streamDecoder 是每家流式协议的状态机：喂一个 SSE 事件，吐出 0 个或多个统一事件。
type streamDecoder interface {
	feed(ev sseEvent) ([]Event, error)
	done() bool // 收到终止事件了吗
}

// runStream 把 HTTP 响应体接到 decoder 上。③ 业务层检查在这里：EOF 但 !done() → ErrIncompleteStream。
func runStream(ctx context.Context, open func() (*http.Response, error), dec streamDecoder) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		resp, err := open()
		if err != nil {
			yield(Event{}, err)
			return
		}
		defer resp.Body.Close()
		r := newSSEReader(resp.Body)
		for {
			ev, err := r.next()
			if err == io.EOF {
				if !dec.done() {
					yield(Event{}, ErrIncompleteStream)
				}
				return
			}
			if err != nil {
				if ctx.Err() != nil {
					err = ctx.Err()
				}
				yield(Event{}, err)
				return
			}
			out, err := dec.feed(ev)
			for _, e := range out {
				if !yield(e, nil) { // 调用方 break 了：defer 关闭连接，上游停止生成
					return
				}
			}
			if err != nil {
				yield(Event{}, err)
				return
			}
			if dec.done() {
				return
			}
		}
	}
}

// Collect 把流读完，返回最终 Response。onEvent 可以为 nil。
func Collect(seq iter.Seq2[Event, error], onEvent func(Event)) (*Response, error) {
	var final *Response
	for ev, err := range seq {
		if err != nil {
			return nil, err
		}
		if onEvent != nil {
			onEvent(ev)
		}
		if ev.Type == EventDone {
			final = ev.Response
		}
	}
	if final == nil {
		return nil, ErrIncompleteStream
	}
	return final, nil
}

// ---------- 小工具 ----------

// argsObject 保证工具参数是一个 JSON 对象：空串 → {}；不是对象 → 报错；合法的压缩成紧凑格式，
// 这样三家来源的参数字节一致，转给 OpenAI 时（它要的是字符串）也稳定。
// 流式拼出来的参数 JSON 在这里第一次被完整校验。
func argsObject(s string) (json.RawMessage, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage(`{}`), nil
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, fmt.Errorf("tool arguments are not a JSON object: %w", err)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func rawOf(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
