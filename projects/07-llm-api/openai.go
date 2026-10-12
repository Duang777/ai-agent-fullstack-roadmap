package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"strings"
)

// OpenAI 对接 Responses API：POST /v1/responses。
//
// 形状要点：
//   - 系统提示放顶层 instructions；对话是一个扁平的 input 数组，元素叫 item；
//   - 工具调用和工具结果都是独立的 item（function_call / function_call_output），用 call_id 配对；
//   - arguments 是 JSON 字符串，不是对象；
//   - store=false 时推理 item 自带 encrypted_content，必须原样回传（课件第 5 节）。
type OpenAI struct {
	APIKey  string
	BaseURL string // 默认 https://api.openai.com
	HTTP    httpConfig
}

func (p *OpenAI) Name() string { return "openai" }

func (p *OpenAI) cfg() httpConfig {
	c := p.HTTP
	c.BaseURL = p.BaseURL
	if c.BaseURL == "" {
		c.BaseURL = "https://api.openai.com"
	}
	return c
}

func (p *OpenAI) headers() map[string]string {
	return map[string]string{"Authorization": "Bearer " + p.APIKey}
}

type oaTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type oaRequest struct {
	Model           string            `json:"model"`
	Instructions    string            `json:"instructions,omitempty"`
	Input           []json.RawMessage `json:"input"`
	Tools           []oaTool          `json:"tools,omitempty"`
	MaxOutputTokens int               `json:"max_output_tokens,omitempty"`
	Store           bool              `json:"store"` // 不加 omitempty：false 也要发出去
	Stream          bool              `json:"stream,omitempty"`
}

func (p *OpenAI) encode(req *Request, stream bool) oaRequest {
	out := oaRequest{Model: req.Model, Instructions: req.System, MaxOutputTokens: req.MaxTokens, Stream: stream}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, oaTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Schema})
	}
	for _, m := range req.Messages {
		for _, part := range m.Parts {
			if part.Provider == "openai" && len(part.Raw) > 0 { // 同一家：原样回放
				out.Input = append(out.Input, part.Raw)
				continue
			}
			switch part.Type {
			case PartText:
				out.Input = append(out.Input, rawOf(map[string]string{"role": string(m.Role), "content": part.Text}))
			case PartToolCall:
				out.Input = append(out.Input, rawOf(map[string]string{
					"type": "function_call", "call_id": part.CallID, "name": part.Name, "arguments": string(part.Args),
				}))
			case PartToolResult:
				output := part.Result
				if part.IsError { // Responses 没有 is_error 字段，只能写进文本
					output = "ERROR: " + output
				}
				out.Input = append(out.Input, rawOf(map[string]string{
					"type": "function_call_output", "call_id": part.CallID, "output": output,
				}))
			case PartReasoning, PartOpaque:
				// 别家的推理块、内置工具记录，OpenAI 看不懂，丢掉
			}
		}
	}
	return out
}

type oaItem struct {
	Type      string `json:"type"`
	Role      string `json:"role"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
}

type oaResponse struct {
	ID                string            `json:"id"`
	Status            string            `json:"status"`
	Output            []json.RawMessage `json:"output"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Usage struct {
		InputTokens        int `json:"input_tokens"`
		InputTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		OutputTokens        int `json:"output_tokens"`
		OutputTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

func (r *oaResponse) toResponse() (*Response, error) {
	if r.Status == "failed" {
		e := &APIError{Provider: "openai", Type: "response_failed", Message: "response failed"}
		if r.Error != nil {
			e.Type, e.Message = r.Error.Code, r.Error.Message
		}
		return nil, e
	}
	out := &Response{ID: r.ID, Message: Message{Role: RoleAssistant}, RawStop: r.Status}
	refused := false
	for _, raw := range r.Output {
		var it oaItem
		if err := json.Unmarshal(raw, &it); err != nil {
			return nil, fmt.Errorf("openai: bad output item: %w", err)
		}
		switch it.Type {
		case "reasoning":
			var sb strings.Builder
			for _, s := range it.Summary {
				sb.WriteString(s.Text)
			}
			out.Message.Parts = append(out.Message.Parts, Part{Type: PartReasoning, Text: sb.String(), Provider: "openai", Raw: raw})
		case "message":
			var sb strings.Builder
			for _, c := range it.Content {
				switch c.Type {
				case "output_text":
					sb.WriteString(c.Text)
				case "refusal":
					sb.WriteString(c.Refusal)
					refused = true
				}
			}
			out.Message.Parts = append(out.Message.Parts, Part{Type: PartText, Text: sb.String(), Provider: "openai", Raw: raw})
		case "function_call":
			args, err := argsObject(it.Arguments)
			if err != nil {
				return nil, fmt.Errorf("openai: %s: %w", it.Name, err)
			}
			out.Message.Parts = append(out.Message.Parts, Part{Type: PartToolCall, CallID: it.CallID, Name: it.Name, Args: args, Provider: "openai", Raw: raw})
		default: // web_search_call 等内置工具的 item
			out.Message.Parts = append(out.Message.Parts, Part{Type: PartOpaque, Provider: "openai", Raw: raw})
		}
	}
	switch {
	case r.Status == "incomplete":
		reason := ""
		if r.IncompleteDetails != nil {
			reason = r.IncompleteDetails.Reason
		}
		out.RawStop = "incomplete:" + reason
		switch reason {
		case "max_output_tokens":
			out.Stop = StopLength
		case "content_filter":
			out.Stop = StopRefusal
		default:
			out.Stop = StopOther
		}
	case refused:
		out.Stop = StopRefusal
	case len(out.Message.ToolCalls()) > 0:
		out.Stop = StopToolUse
	case r.Status == "completed":
		out.Stop = StopEnd
	default:
		out.Stop = StopOther
	}
	u := r.Usage
	out.Usage = Usage{
		InputTokens:     u.InputTokens, // OpenAI 的 input_tokens 已经包含缓存命中
		CachedTokens:    u.InputTokensDetails.CachedTokens,
		OutputTokens:    u.OutputTokens,
		ReasoningTokens: u.OutputTokensDetails.ReasoningTokens,
	}
	return out, nil
}

func (p *OpenAI) Generate(ctx context.Context, req *Request) (*Response, error) {
	r, err := generateJSON[oaResponse](ctx, p.cfg(), "openai", "/v1/responses", p.headers(), p.encode(req, false))
	if err != nil {
		return nil, err
	}
	return r.toResponse()
}

func (p *OpenAI) Stream(ctx context.Context, req *Request) iter.Seq2[Event, error] {
	open := func() (*http.Response, error) {
		return post(ctx, p.cfg(), "openai", "/v1/responses", p.headers(), p.encode(req, true), true)
	}
	return runStream(ctx, open, &oaStream{})
}

// oaStream：Responses 的流是“语义事件”，每个事件的 data 里都有 type。
// 终止事件是 response.completed / response.incomplete / response.failed，
// 前两个携带完整的 response 对象，所以最终结果直接用它，不用自己拼。
type oaStream struct{ finished bool }

func (s *oaStream) done() bool { return s.finished }

func (s *oaStream) feed(ev sseEvent) ([]Event, error) {
	var d struct {
		Type        string          `json:"type"`
		OutputIndex int             `json:"output_index"`
		Delta       string          `json:"delta"`
		Item        oaItem          `json:"item"`
		Response    json.RawMessage `json:"response"`
		Code        string          `json:"code"`
		Message     string          `json:"message"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &d); err != nil {
		return nil, fmt.Errorf("openai: bad event %q: %w", ev.Event, err)
	}
	switch d.Type {
	case "response.output_item.added":
		if d.Item.Type == "function_call" {
			return []Event{{Type: EventToolStart, Index: d.OutputIndex, CallID: d.Item.CallID, Name: d.Item.Name}}, nil
		}
	case "response.output_text.delta":
		return []Event{{Type: EventText, Index: d.OutputIndex, Text: d.Delta}}, nil
	case "response.function_call_arguments.delta":
		return []Event{{Type: EventToolArgs, Index: d.OutputIndex, Text: d.Delta}}, nil
	case "response.completed", "response.incomplete", "response.failed":
		s.finished = true
		var r oaResponse
		if err := json.Unmarshal(d.Response, &r); err != nil {
			return nil, fmt.Errorf("openai: bad %s: %w", d.Type, err)
		}
		resp, err := r.toResponse()
		if err != nil {
			return nil, err
		}
		return []Event{{Type: EventDone, Response: resp}}, nil
	case "error":
		s.finished = true
		return nil, &APIError{Provider: "openai", Type: d.Code, Message: d.Message}
	}
	return nil, nil // 其余事件（created、in_progress、content_part.added……）忽略
}
