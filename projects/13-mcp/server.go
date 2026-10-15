package mcp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ToolResult 是工具执行的结果。IsError 表示“工具执行错误”（模型可以据此自我纠正），
// 和 JSON-RPC 协议错误（未知工具、请求格式错误）是两回事。
type ToolResult struct {
	Text       string
	Structured any
	IsError    bool
}

// CallCtx 是工具执行时能拿到的东西：参数、客户端声明的能力、上一轮多轮请求的回答。
type CallCtx struct {
	Args           map[string]any
	ClientCaps     map[string]any
	InputResponses map[string]json.RawMessage
}

// NeedInput 由工具返回，表示需要客户端（通常是用户）补充信息：对应规范里的 InputRequiredResult。
type NeedInput struct {
	Requests map[string]any // key → {method, params}
	Cap      string         // 这些请求需要的客户端能力，例如 "elicitation"
}

type Tool struct {
	Name        string
	Title       string
	Description string
	InputSchema map[string]any
	Handler     func(ctx context.Context, c CallCtx) (*ToolResult, *NeedInput)
}

type Resource struct {
	URI, Name, MimeType, Text string
}

type PromptArg struct {
	Name, Description string
	Required          bool
}

type Prompt struct {
	Name, Description string
	Args              []PromptArg
	Render            func(args map[string]string) string
}

type Server struct {
	Info      Implementation
	Versions  []string
	Secret    []byte // 给 requestState 做 HMAC
	PageSize  int
	Now       func() time.Time
	tools     map[string]Tool
	resources map[string]Resource
	prompts   map[string]Prompt
}

func NewServer(name string) *Server {
	return &Server{
		Info: Implementation{name, "0.1.0"}, Versions: []string{Version},
		Secret: []byte("dev-secret-" + name), PageSize: 50, Now: time.Now,
		tools: map[string]Tool{}, resources: map[string]Resource{}, prompts: map[string]Prompt{},
	}
}

func (s *Server) AddTool(t Tool)         { s.tools[t.Name] = t }
func (s *Server) AddResource(r Resource) { s.resources[r.URI] = r }
func (s *Server) AddPrompt(p Prompt)     { s.prompts[p.Name] = p }

// Handle 处理一条 JSON-RPC 消息。通知返回 nil（接收方不能回复通知）。
// 注意它不看任何“连接”或“会话”：每个请求自带版本、能力，处理完就结束。
func (s *Server) Handle(ctx context.Context, raw []byte) *Response {
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		r := errResp(nil, CodeParse, "parse error", nil)
		return &r
	}
	if req.IsNotification() {
		return nil
	}
	if req.JSONRPC != "2.0" || string(req.ID) == "null" {
		r := errResp(req.ID, CodeInvalidRequest, "invalid request: jsonrpc 必须是 2.0，id 不能是 null", nil)
		return &r
	}
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	json.Unmarshal(req.Params, &p)
	var ver string
	json.Unmarshal(p.Meta[metaVersion], &ver)
	var caps map[string]any
	if err := json.Unmarshal(p.Meta[metaCaps], &caps); err != nil || caps == nil {
		r := errResp(req.ID, CodeInvalidParams, "缺少必需的 _meta 字段 "+metaCaps, nil)
		if ver == "" {
			r.Error.Message = "缺少必需的 _meta 字段 " + metaVersion
		}
		return &r
	}
	if ver == "" {
		r := errResp(req.ID, CodeInvalidParams, "缺少必需的 _meta 字段 "+metaVersion, nil)
		return &r
	}
	if !contains(s.Versions, ver) {
		r := errResp(req.ID, CodeUnsupportedVer, "unsupported protocol version "+ver,
			map[string]any{"supported": s.Versions, "requested": ver})
		return &r
	}
	res, e := s.dispatch(ctx, req, caps)
	if e != nil {
		r := Response{JSONRPC: "2.0", ID: req.ID, Error: e}
		return &r
	}
	if _, ok := res["resultType"]; !ok {
		res["resultType"] = "complete"
	}
	res["_meta"] = map[string]any{metaServer: s.Info}
	r := okResp(req.ID, res)
	return &r
}

func (s *Server) caps() map[string]any {
	c := map[string]any{}
	if len(s.tools) > 0 {
		c["tools"] = map[string]any{"listChanged": false}
	}
	if len(s.resources) > 0 {
		c["resources"] = map[string]any{}
	}
	if len(s.prompts) > 0 {
		c["prompts"] = map[string]any{"listChanged": false}
	}
	return c
}

func (s *Server) dispatch(ctx context.Context, req Request, caps map[string]any) (map[string]any, *Error) {
	switch req.Method {
	case "server/discover":
		return map[string]any{"supportedVersions": s.Versions, "capabilities": s.caps(),
			"instructions": "示例 Server：天气、笔记和需要确认的危险操作。", "ttlMs": 3600000, "cacheScope": "public"}, nil
	case "tools/list":
		return s.listTools(req.Params)
	case "tools/call":
		return s.callTool(ctx, req.Params, caps)
	case "resources/list":
		var out []map[string]any
		for _, k := range sortedKeys(s.resources) {
			r := s.resources[k]
			out = append(out, map[string]any{"uri": r.URI, "name": r.Name, "mimeType": r.MimeType})
		}
		return map[string]any{"resources": out, "ttlMs": 60000, "cacheScope": "private"}, nil
	case "resources/read":
		var p struct{ URI string }
		json.Unmarshal(req.Params, &p)
		r, ok := s.resources[p.URI]
		if !ok {
			// 规范：不存在要报错，不能返回空 contents（空数组有歧义）
			return nil, &Error{Code: CodeInvalidParams, Message: "Resource not found", Data: map[string]any{"uri": p.URI}}
		}
		return map[string]any{"contents": []any{map[string]any{"uri": r.URI, "mimeType": r.MimeType, "text": r.Text}},
			"ttlMs": 60000, "cacheScope": "private"}, nil
	case "prompts/list":
		var out []map[string]any
		for _, k := range sortedKeys(s.prompts) {
			p := s.prompts[k]
			var args []map[string]any
			for _, a := range p.Args {
				args = append(args, map[string]any{"name": a.Name, "description": a.Description, "required": a.Required})
			}
			out = append(out, map[string]any{"name": p.Name, "description": p.Description, "arguments": args})
		}
		return map[string]any{"prompts": out, "ttlMs": 600000, "cacheScope": "public"}, nil
	case "prompts/get":
		var p struct {
			Name      string
			Arguments map[string]string
		}
		json.Unmarshal(req.Params, &p)
		pr, ok := s.prompts[p.Name]
		if !ok {
			return nil, &Error{Code: CodeInvalidParams, Message: "unknown prompt: " + p.Name}
		}
		for _, a := range pr.Args {
			if _, ok := p.Arguments[a.Name]; a.Required && !ok {
				return nil, &Error{Code: CodeInvalidParams, Message: "missing required argument: " + a.Name}
			}
		}
		return map[string]any{"description": pr.Description, "messages": []any{
			map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": pr.Render(p.Arguments)}},
		}}, nil
	}
	return nil, &Error{Code: CodeMethodNotFound, Message: "method not found: " + req.Method}
}

// listTools：顺序确定（按名字排序），支持分页。顺序确定能让客户端缓存工具列表，也能提高 Prompt Cache 命中（1.5 课）。
func (s *Server) listTools(params json.RawMessage) (map[string]any, *Error) {
	var p struct{ Cursor string }
	json.Unmarshal(params, &p)
	names := sortedKeys(s.tools)
	start := 0
	if p.Cursor != "" {
		n, err := strconv.Atoi(p.Cursor)
		if err != nil || n < 0 || n > len(names) {
			return nil, &Error{Code: CodeInvalidParams, Message: "invalid cursor"}
		}
		start = n
	}
	end := min(start+s.PageSize, len(names))
	var out []map[string]any
	for _, n := range names[start:end] {
		t := s.tools[n]
		out = append(out, map[string]any{"name": t.Name, "title": t.Title, "description": t.Description, "inputSchema": t.InputSchema})
	}
	res := map[string]any{"tools": out, "ttlMs": 300000, "cacheScope": "public"}
	if end < len(names) {
		res["nextCursor"] = strconv.Itoa(end)
	}
	return res, nil
}

func (s *Server) callTool(ctx context.Context, params json.RawMessage, caps map[string]any) (map[string]any, *Error) {
	var p struct {
		Name           string
		Arguments      map[string]any
		InputResponses map[string]json.RawMessage `json:"inputResponses"`
		RequestState   string                     `json:"requestState"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &Error{Code: CodeInvalidParams, Message: "malformed tools/call params"}
	}
	t, ok := s.tools[p.Name]
	if !ok {
		// 协议错误：模型不太可能自己修好
		return nil, &Error{Code: CodeInvalidParams, Message: "Unknown tool: " + p.Name}
	}
	if p.RequestState != "" {
		if err := s.verifyState(p.RequestState, p.Name, p.Arguments); err != nil {
			return nil, &Error{Code: CodeInvalidParams, Message: "invalid requestState: " + err.Error()}
		}
	} else if len(p.InputResponses) > 0 {
		return nil, &Error{Code: CodeInvalidParams, Message: "inputResponses 必须和服务端签发的 requestState 一起出现"}
	}
	// 必填参数检查：这是“工具执行错误”，用 isError 结果返回，模型可以自己补上
	if req, ok := t.InputSchema["required"].([]string); ok {
		for _, r := range req {
			if _, ok := p.Arguments[r]; !ok {
				return toolOut(&ToolResult{Text: fmt.Sprintf("缺少必填参数 %q。", r), IsError: true}), nil
			}
		}
	}
	res, need := t.Handler(ctx, CallCtx{Args: p.Arguments, ClientCaps: caps, InputResponses: p.InputResponses})
	if need != nil {
		if _, ok := caps[need.Cap]; !ok {
			return nil, &Error{Code: CodeMissingCap, Message: "client capability required: " + need.Cap,
				Data: map[string]any{"requiredCapabilities": map[string]any{need.Cap: map[string]any{}}}}
		}
		return map[string]any{"resultType": "input_required", "inputRequests": need.Requests,
			"requestState": s.signState(p.Name, p.Arguments)}, nil
	}
	return toolOut(res), nil
}

func toolOut(r *ToolResult) map[string]any {
	out := map[string]any{"content": []any{map[string]any{"type": "text", "text": r.Text}}, "isError": r.IsError}
	if r.Structured != nil {
		out["structuredContent"] = r.Structured
	}
	return out
}

// requestState：客户端只负责原样带回，服务端必须把它当成攻击者可控的输入。
// 这里签进去：工具名、参数摘要、过期时间，然后 HMAC。
type state struct {
	Tool   string `json:"t"`
	Digest string `json:"d"`
	Exp    int64  `json:"e"`
}

func digest(args map[string]any) string {
	b, _ := json.Marshal(args) // map 的键按字典序输出，结果稳定
	h := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(h[:8])
}

func (s *Server) signState(tool string, args map[string]any) string {
	b, _ := json.Marshal(state{tool, digest(args), s.Now().Add(5 * time.Minute).Unix()})
	m := hmac.New(sha256.New, s.Secret)
	m.Write(b)
	return base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Server) verifyState(tok, tool string, args map[string]any) error {
	body, sig, ok := strings.Cut(tok, ".")
	if !ok {
		return fmt.Errorf("格式错误")
	}
	b, err1 := base64.RawURLEncoding.DecodeString(body)
	got, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil {
		return fmt.Errorf("格式错误")
	}
	m := hmac.New(sha256.New, s.Secret)
	m.Write(b)
	if !hmac.Equal(got, m.Sum(nil)) {
		return fmt.Errorf("签名不对，可能被篡改")
	}
	var st state
	json.Unmarshal(b, &st)
	if st.Tool != tool || st.Digest != digest(args) {
		return fmt.Errorf("和原始请求不匹配")
	}
	if s.Now().Unix() > st.Exp {
		return fmt.Errorf("已过期")
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
