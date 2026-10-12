package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"sort"
	"strings"
)

// Anthropic 对接 Messages API：POST /v1/messages。
//
// 形状要点：
//   - system 在顶层；messages 只有 user / assistant 两种角色，content 是 block 数组；
//   - 工具调用是 assistant 消息里的 tool_use block（input 是对象），
//     工具结果是下一条 user 消息里的 tool_result block，必须排在最前面；
//   - max_tokens 必填；
//   - 开启 thinking 时，带工具调用的那一轮的 thinking block（含 signature）必须原样回传。
type Anthropic struct {
	APIKey  string
	BaseURL string // 默认 https://api.anthropic.com
	Version string // anthropic-version 头，默认 2023-06-01
	HTTP    httpConfig
}

func (p *Anthropic) Name() string { return "anthropic" }

func (p *Anthropic) cfg() httpConfig {
	c := p.HTTP
	c.BaseURL = p.BaseURL
	if c.BaseURL == "" {
		c.BaseURL = "https://api.anthropic.com"
	}
	return c
}

func (p *Anthropic) headers() map[string]string {
	v := p.Version
	if v == "" {
		v = "2023-06-01"
	}
	return map[string]string{"x-api-key": p.APIKey, "anthropic-version": v}
}

type anTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anMessage struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

type anRequest struct {
	Model     string      `json:"model"`
	MaxTokens int         `json:"max_tokens"`
	System    string      `json:"system,omitempty"`
	Messages  []anMessage `json:"messages"`
	Tools     []anTool    `json:"tools,omitempty"`
	Stream    bool        `json:"stream,omitempty"`
}

const defaultMaxTokens = 4096

func (p *Anthropic) encode(req *Request, stream bool) anRequest {
	out := anRequest{Model: req.Model, MaxTokens: req.MaxTokens, System: req.System, Stream: stream}
	if out.MaxTokens <= 0 {
		out.MaxTokens = defaultMaxTokens // Anthropic 要求必填，另外两家可以不填
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, anTool{Name: t.Name, Description: t.Description, InputSchema: t.Schema})
	}
	for _, m := range req.Messages {
		am := anMessage{Role: string(m.Role)}
		for _, part := range m.Parts {
			if part.Provider == "anthropic" && len(part.Raw) > 0 {
				am.Content = append(am.Content, part.Raw)
				continue
			}
			switch part.Type {
			case PartText:
				if part.Text == "" { // 空 text block 会被拒绝
					continue
				}
				am.Content = append(am.Content, rawOf(map[string]string{"type": "text", "text": part.Text}))
			case PartToolCall:
				am.Content = append(am.Content, rawOf(map[string]any{
					"type": "tool_use", "id": part.CallID, "name": part.Name, "input": part.Args,
				}))
			case PartToolResult:
				block := map[string]any{"type": "tool_result", "tool_use_id": part.CallID, "content": part.Result}
				if part.IsError {
					block["is_error"] = true
				}
				am.Content = append(am.Content, rawOf(block))
			}
		}
		if len(am.Content) > 0 { // 全被丢掉的消息不能发空 content
			out.Messages = append(out.Messages, am)
		}
	}
	return out
}

type anUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// Anthropic 的 input_tokens 只算“缓存断点之后”的部分，总输入要把缓存读写加回来。
func (u anUsage) normalize() Usage {
	return Usage{
		InputTokens:  u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens,
		CachedTokens: u.CacheReadInputTokens,
		OutputTokens: u.OutputTokens,
	}
}

type anBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
}

func anPart(raw json.RawMessage) (Part, error) {
	var b anBlock
	if err := json.Unmarshal(raw, &b); err != nil {
		return Part{}, fmt.Errorf("anthropic: bad content block: %w", err)
	}
	p := Part{Provider: "anthropic", Raw: raw}
	switch b.Type {
	case "text":
		p.Type, p.Text = PartText, b.Text
	case "tool_use":
		args, err := argsObject(string(b.Input))
		if err != nil {
			return Part{}, fmt.Errorf("anthropic: %s: %w", b.Name, err)
		}
		p.Type, p.CallID, p.Name, p.Args = PartToolCall, b.ID, b.Name, args
	case "thinking", "redacted_thinking":
		p.Type, p.Text = PartReasoning, b.Thinking
	default: // server_tool_use、web_search_tool_result……
		p.Type = PartOpaque
	}
	return p, nil
}

func anStop(s string) StopReason {
	switch s {
	case "end_turn", "stop_sequence":
		return StopEnd
	case "max_tokens", "model_context_window_exceeded":
		return StopLength
	case "tool_use":
		return StopToolUse
	case "refusal":
		return StopRefusal
	case "pause_turn":
		return StopPause
	}
	return StopOther
}

type anResponse struct {
	ID         string            `json:"id"`
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      anUsage           `json:"usage"`
}

func (r *anResponse) toResponse() (*Response, error) {
	out := &Response{ID: r.ID, Message: Message{Role: RoleAssistant}, RawStop: r.StopReason, Stop: anStop(r.StopReason), Usage: r.Usage.normalize()}
	for _, raw := range r.Content {
		p, err := anPart(raw)
		if err != nil {
			return nil, err
		}
		out.Message.Parts = append(out.Message.Parts, p)
	}
	return out, nil
}

func (p *Anthropic) Generate(ctx context.Context, req *Request) (*Response, error) {
	r, err := generateJSON[anResponse](ctx, p.cfg(), "anthropic", "/v1/messages", p.headers(), p.encode(req, false))
	if err != nil {
		return nil, err
	}
	return r.toResponse()
}

func (p *Anthropic) Stream(ctx context.Context, req *Request) iter.Seq2[Event, error] {
	open := func() (*http.Response, error) {
		return post(ctx, p.cfg(), "anthropic", "/v1/messages", p.headers(), p.encode(req, true), true)
	}
	return runStream(ctx, open, &anStream{blocks: map[int]*anStreamBlock{}})
}

// anStream：Anthropic 的流要自己拼。
// message_start → (content_block_start → content_block_delta* → content_block_stop)* → message_delta → message_stop
type anStream struct {
	id       string
	usage    anUsage
	stop     string
	blocks   map[int]*anStreamBlock
	finished bool
}

type anStreamBlock struct {
	start     json.RawMessage // content_block_start 里的 content_block
	typ       string
	id, name  string
	text      strings.Builder // text / thinking
	json      strings.Builder // tool_use 的 input_json_delta
	signature string
}

func (s *anStream) done() bool { return s.finished }

func (s *anStream) feed(ev sseEvent) ([]Event, error) {
	var d struct {
		Type         string          `json:"type"`
		Index        int             `json:"index"`
		Message      anResponse      `json:"message"`
		ContentBlock json.RawMessage `json:"content_block"`
		Delta        struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			Thinking    string `json:"thinking"`
			Signature   string `json:"signature"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage anUsage `json:"usage"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &d); err != nil {
		return nil, fmt.Errorf("anthropic: bad event %q: %w", ev.Event, err)
	}
	switch d.Type {
	case "message_start":
		s.id = d.Message.ID
		s.usage = d.Message.Usage
	case "content_block_start":
		var b anBlock
		if err := json.Unmarshal(d.ContentBlock, &b); err != nil {
			return nil, fmt.Errorf("anthropic: bad content_block_start: %w", err)
		}
		blk := &anStreamBlock{start: d.ContentBlock, typ: b.Type, id: b.ID, name: b.Name}
		blk.text.WriteString(b.Text)
		s.blocks[d.Index] = blk
		if b.Type == "tool_use" {
			return []Event{{Type: EventToolStart, Index: d.Index, CallID: b.ID, Name: b.Name}}, nil
		}
	case "content_block_delta":
		blk := s.blocks[d.Index]
		if blk == nil {
			return nil, fmt.Errorf("anthropic: delta for unknown block %d", d.Index)
		}
		switch d.Delta.Type {
		case "text_delta":
			blk.text.WriteString(d.Delta.Text)
			return []Event{{Type: EventText, Index: d.Index, Text: d.Delta.Text}}, nil
		case "input_json_delta":
			blk.json.WriteString(d.Delta.PartialJSON)
			if blk.typ == "tool_use" {
				return []Event{{Type: EventToolArgs, Index: d.Index, Text: d.Delta.PartialJSON}}, nil
			}
		case "thinking_delta":
			blk.text.WriteString(d.Delta.Thinking)
		case "signature_delta":
			blk.signature = d.Delta.Signature
		}
	case "message_delta":
		if d.Delta.StopReason != "" {
			s.stop = d.Delta.StopReason
		}
		// message_delta 里的 usage 是累计值：有就覆盖
		if d.Usage.OutputTokens > 0 {
			s.usage.OutputTokens = d.Usage.OutputTokens
		}
		if d.Usage.InputTokens > 0 {
			s.usage.InputTokens = d.Usage.InputTokens
		}
	case "message_stop":
		s.finished = true
		resp, err := s.build()
		if err != nil {
			return nil, err
		}
		return []Event{{Type: EventDone, Response: resp}}, nil
	case "error":
		s.finished = true
		return nil, &APIError{Provider: "anthropic", Type: d.Error.Type, Message: d.Error.Message}
	}
	return nil, nil // ping、content_block_stop 和未来新增的事件类型：忽略
}

// build 按 index 顺序把块拼回和非流式一模一样的 content，再复用 anPart 解析。
func (s *anStream) build() (*Response, error) {
	idx := make([]int, 0, len(s.blocks))
	for i := range s.blocks {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	r := anResponse{ID: s.id, StopReason: s.stop, Usage: s.usage}
	for _, i := range idx {
		b := s.blocks[i]
		var raw json.RawMessage
		switch b.typ {
		case "text":
			raw = rawOf(map[string]string{"type": "text", "text": b.text.String()})
		case "thinking":
			raw = rawOf(map[string]string{"type": "thinking", "thinking": b.text.String(), "signature": b.signature})
		case "tool_use", "server_tool_use":
			input, err := argsObject(b.json.String())
			if err != nil {
				return nil, fmt.Errorf("anthropic: %s: %w", b.name, err)
			}
			raw = rawOf(map[string]any{"type": b.typ, "id": b.id, "name": b.name, "input": input})
		default: // redacted_thinking、各种 *_tool_result：start 里就是完整内容
			raw = b.start
		}
		r.Content = append(r.Content, raw)
	}
	return r.toResponse()
}
