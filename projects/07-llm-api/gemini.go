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

// Gemini 对接 Interactions API：POST /v1beta/interactions（2026 年 6 月 GA，官方推荐的新接口；
// 老的 generateContent 仍然支持，课件第 7 节有对照）。
//
// 形状要点：
//   - 一次调用叫一个 interaction，结果是一条 steps 时间线：thought / function_call / model_output……
//   - 默认 store=true，可以用 previous_interaction_id 续聊；本客户端用 store=false 无状态模式，
//     每次把完整历史（包括带 signature 的 thought step）放进 input；
//   - 函数调用的 arguments 是对象，结果是 function_result step，用 call_id 配对，还要带 name；
//   - 需要你执行工具时，status 是 requires_action。
type Gemini struct {
	APIKey  string
	BaseURL string // 默认 https://generativelanguage.googleapis.com
	HTTP    httpConfig
}

func (p *Gemini) Name() string { return "gemini" }

func (p *Gemini) cfg() httpConfig {
	c := p.HTTP
	c.BaseURL = p.BaseURL
	if c.BaseURL == "" {
		c.BaseURL = "https://generativelanguage.googleapis.com"
	}
	return c
}

func (p *Gemini) headers() map[string]string {
	return map[string]string{"x-goog-api-key": p.APIKey}
}

type gmTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type gmRequest struct {
	Model             string            `json:"model"`
	SystemInstruction string            `json:"system_instruction,omitempty"`
	Input             []json.RawMessage `json:"input"`
	Tools             []gmTool          `json:"tools,omitempty"`
	GenerationConfig  *struct {
		MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	} `json:"generation_config,omitempty"`
	Store  bool `json:"store"`
	Stream bool `json:"stream,omitempty"`
}

func textContent(s string) []map[string]string {
	return []map[string]string{{"type": "text", "text": s}}
}

func (p *Gemini) encode(req *Request, stream bool) gmRequest {
	out := gmRequest{Model: req.Model, SystemInstruction: req.System, Stream: stream}
	if req.MaxTokens > 0 {
		out.GenerationConfig = &struct {
			MaxOutputTokens int `json:"max_output_tokens,omitempty"`
		}{req.MaxTokens}
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, gmTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Schema})
	}
	for _, m := range req.Messages {
		for _, part := range m.Parts {
			if part.Provider == "gemini" && len(part.Raw) > 0 {
				out.Input = append(out.Input, part.Raw)
				continue
			}
			switch part.Type {
			case PartText:
				typ := "user_input"
				if m.Role == RoleAssistant {
					typ = "model_output"
				}
				out.Input = append(out.Input, rawOf(map[string]any{"type": typ, "content": textContent(part.Text)}))
			case PartToolCall:
				out.Input = append(out.Input, rawOf(map[string]any{
					"type": "function_call", "id": part.CallID, "name": part.Name, "arguments": part.Args,
				}))
			case PartToolResult:
				step := map[string]any{"type": "function_result", "call_id": part.CallID, "name": part.Name, "result": textContent(part.Result)}
				if part.IsError {
					step["is_error"] = true
				}
				out.Input = append(out.Input, rawOf(step))
			}
		}
	}
	return out
}

type gmStep struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Signature string          `json:"signature"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary"`
}

type gmUsage struct {
	TotalInputTokens   int `json:"total_input_tokens"`
	TotalCachedTokens  int `json:"total_cached_tokens"`
	TotalOutputTokens  int `json:"total_output_tokens"`
	TotalThoughtTokens int `json:"total_thought_tokens"`
}

type gmInteraction struct {
	ID     string            `json:"id"`
	Status string            `json:"status"`
	Steps  []json.RawMessage `json:"steps"`
	Usage  *gmUsage          `json:"usage"`
}

func (r *gmInteraction) toResponse() (*Response, error) {
	if r.Status == "failed" {
		return nil, &APIError{Provider: "gemini", Type: "failed", Message: "interaction failed"}
	}
	out := &Response{ID: r.ID, Message: Message{Role: RoleAssistant}, RawStop: r.Status}
	for _, raw := range r.Steps {
		var st gmStep
		if err := json.Unmarshal(raw, &st); err != nil {
			return nil, fmt.Errorf("gemini: bad step: %w", err)
		}
		p := Part{Provider: "gemini", Raw: raw}
		switch st.Type {
		case "model_output":
			var sb strings.Builder
			for _, c := range st.Content {
				if c.Type == "text" {
					sb.WriteString(c.Text)
				}
			}
			p.Type, p.Text = PartText, sb.String()
		case "function_call":
			args, err := argsObject(string(st.Arguments))
			if err != nil {
				return nil, fmt.Errorf("gemini: %s: %w", st.Name, err)
			}
			p.Type, p.CallID, p.Name, p.Args = PartToolCall, st.ID, st.Name, args
		case "thought":
			var sb strings.Builder
			for _, c := range st.Summary {
				sb.WriteString(c.Text)
			}
			p.Type, p.Text = PartReasoning, sb.String()
		case "user_input", "function_result":
			continue // GET 返回的完整时间线里会有，POST 只返回模型产出的 step
		default:
			p.Type = PartOpaque
		}
		out.Message.Parts = append(out.Message.Parts, p)
	}
	switch r.Status {
	case "requires_action":
		out.Stop = StopToolUse
	case "completed":
		out.Stop = StopEnd
		if len(out.Message.ToolCalls()) > 0 {
			out.Stop = StopToolUse
		}
	case "incomplete":
		out.Stop = StopLength
	default:
		out.Stop = StopOther
	}
	if r.Usage != nil {
		u := r.Usage
		// 官方示例：total_tokens = input + output + thought，说明 output 不含思考 token；
		// 统一口径里 OutputTokens 包含推理，所以要加起来。
		out.Usage = Usage{
			InputTokens:     u.TotalInputTokens,
			CachedTokens:    u.TotalCachedTokens,
			OutputTokens:    u.TotalOutputTokens + u.TotalThoughtTokens,
			ReasoningTokens: u.TotalThoughtTokens,
		}
	}
	return out, nil
}

func (p *Gemini) Generate(ctx context.Context, req *Request) (*Response, error) {
	r, err := generateJSON[gmInteraction](ctx, p.cfg(), "gemini", "/v1beta/interactions", p.headers(), p.encode(req, false))
	if err != nil {
		return nil, err
	}
	return r.toResponse()
}

func (p *Gemini) Stream(ctx context.Context, req *Request) iter.Seq2[Event, error] {
	open := func() (*http.Response, error) {
		return post(ctx, p.cfg(), "gemini", "/v1beta/interactions", p.headers(), p.encode(req, true), true)
	}
	return runStream(ctx, open, &gmStream{steps: map[int]*gmStreamStep{}})
}

// gmStream：interaction.created → (step.start → step.delta* → step.stop)* → interaction.completed / requires_action。
// 终止事件里的 interaction 可能只有 id、status、usage，没有 steps，所以 steps 要自己拼。
type gmStream struct {
	id       string
	steps    map[int]*gmStreamStep
	finished bool
}

type gmStreamStep struct {
	start     json.RawMessage
	st        gmStep
	text      strings.Builder // model_output 的文本 / thought 的摘要
	args      strings.Builder
	signature string
}

func (s *gmStream) done() bool { return s.finished }

func (s *gmStream) feed(ev sseEvent) ([]Event, error) {
	var d struct {
		EventType   string          `json:"event_type"`
		Type        string          `json:"type"` // 文档里有的示例用 type，参考手册用 event_type，两个都认
		Index       int             `json:"index"`
		Step        json.RawMessage `json:"step"`
		Interaction json.RawMessage `json:"interaction"`
		Status      string          `json:"status"`
		Delta       struct {
			Type             string `json:"type"`
			Text             string `json:"text"`
			Arguments        string `json:"arguments"`         // 参考手册：type=arguments_delta
			PartialArguments string `json:"partial_arguments"` // 迁移指南示例：type=arguments
			Signature        string `json:"signature"`
			Content          struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"delta"`
		Error struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &d); err != nil {
		return nil, fmt.Errorf("gemini: bad event %q: %w", ev.Event, err)
	}
	kind := d.EventType
	if kind == "" {
		kind = d.Type
	}
	if kind == "" {
		kind = ev.Event
	}
	switch kind {
	case "interaction.created":
		var it gmInteraction
		_ = json.Unmarshal(d.Interaction, &it)
		s.id = it.ID
	case "step.start":
		blk := &gmStreamStep{start: d.Step}
		if err := json.Unmarshal(d.Step, &blk.st); err != nil {
			return nil, fmt.Errorf("gemini: bad step.start: %w", err)
		}
		blk.signature = blk.st.Signature
		s.steps[d.Index] = blk
		var out []Event
		switch blk.st.Type {
		case "function_call":
			out = append(out, Event{Type: EventToolStart, Index: d.Index, CallID: blk.st.ID, Name: blk.st.Name})
		case "model_output": // step.start 可能已经带了第一段文本
			for _, c := range blk.st.Content {
				if c.Type == "text" && c.Text != "" {
					blk.text.WriteString(c.Text)
					out = append(out, Event{Type: EventText, Index: d.Index, Text: c.Text})
				}
			}
		case "thought":
			for _, c := range blk.st.Summary {
				blk.text.WriteString(c.Text)
			}
		}
		return out, nil
	case "step.delta":
		blk := s.steps[d.Index]
		if blk == nil {
			return nil, fmt.Errorf("gemini: delta for unknown step %d", d.Index)
		}
		switch d.Delta.Type {
		case "text":
			blk.text.WriteString(d.Delta.Text)
			if blk.st.Type == "model_output" {
				return []Event{{Type: EventText, Index: d.Index, Text: d.Delta.Text}}, nil
			}
		case "arguments_delta", "arguments":
			chunk := d.Delta.Arguments + d.Delta.PartialArguments
			blk.args.WriteString(chunk)
			return []Event{{Type: EventToolArgs, Index: d.Index, Text: chunk}}, nil
		case "thought", "thought_summary":
			blk.text.WriteString(d.Delta.Text + d.Delta.Content.Text)
		case "thought_signature":
			blk.signature = d.Delta.Signature
		}
	case "interaction.completed", "interaction.requires_action", "interaction.incomplete", "interaction.status_update":
		var it gmInteraction
		_ = json.Unmarshal(d.Interaction, &it)
		status := it.Status
		if status == "" {
			status = d.Status
		}
		if kind == "interaction.requires_action" {
			status = "requires_action"
		}
		switch status {
		case "completed", "requires_action", "incomplete", "failed", "cancelled":
		default:
			return nil, nil // in_progress 之类的中间状态
		}
		s.finished = true
		resp, err := s.build(status, it.Usage)
		if err != nil {
			return nil, err
		}
		return []Event{{Type: EventDone, Response: resp}}, nil
	case "error":
		s.finished = true
		return nil, &APIError{Provider: "gemini", Type: fmt.Sprint(d.Error.Code), Message: d.Error.Message}
	}
	return nil, nil
}

func (s *gmStream) build(status string, usage *gmUsage) (*Response, error) {
	idx := make([]int, 0, len(s.steps))
	for i := range s.steps {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	it := gmInteraction{ID: s.id, Status: status, Usage: usage}
	for _, i := range idx {
		b := s.steps[i]
		var raw json.RawMessage
		switch b.st.Type {
		case "model_output":
			raw = rawOf(map[string]any{"type": "model_output", "content": textContent(b.text.String())})
		case "function_call":
			argStr := b.args.String()
			if argStr == "" && len(b.st.Arguments) > 0 { // 没有增量，参数在 step.start 里一次给全
				argStr = string(b.st.Arguments)
			}
			args, err := argsObject(argStr)
			if err != nil {
				return nil, fmt.Errorf("gemini: %s: %w", b.st.Name, err)
			}
			raw = rawOf(map[string]any{"type": "function_call", "id": b.st.ID, "name": b.st.Name, "arguments": args})
		case "thought":
			step := map[string]any{"type": "thought", "signature": b.signature}
			if b.text.Len() > 0 {
				step["summary"] = textContent(b.text.String())
			}
			raw = rawOf(step)
		default:
			raw = b.start
		}
		it.Steps = append(it.Steps, raw)
	}
	return it.toResponse()
}
