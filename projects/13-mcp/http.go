package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
)

// HTTPHandler 实现 Streamable HTTP 传输的核心规则（只返回 application/json，不演示 SSE 流）：
//   - 只有一个端点，只接受 POST；GET / DELETE 返回 405（2026-07-28 去掉了 GET 流和会话）；
//   - 校验 Origin，防 DNS rebinding；
//   - MCP-Protocol-Version / Mcp-Method / Mcp-Name 头必须和 body 一致，否则 400 + -32020；
//   - 通知返回 202；未知方法 404 + -32601；版本不支持 400 + -32022。
func HTTPHandler(s *Server, allowedOrigins ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !contains(allowedOrigins, o) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		var req Request
		var p struct {
			Name string
			URI  string
			Meta map[string]json.RawMessage `json:"_meta"`
		}
		if json.Unmarshal(body, &req) != nil {
			writeJSON(w, 400, errResp(nil, CodeParse, "parse error", nil))
			return
		}
		json.Unmarshal(req.Params, &p)
		if req.IsNotification() {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var ver string
		json.Unmarshal(p.Meta[metaVersion], &ver)
		if msg := checkHeaders(r.Header, req.Method, p.Name, p.URI, ver); msg != "" {
			writeJSON(w, 400, errResp(req.ID, CodeHeaderMismatch, "Header mismatch: "+msg, nil))
			return
		}
		resp := s.Handle(r.Context(), body)
		status := 200
		if resp.Error != nil {
			switch resp.Error.Code {
			case CodeMethodNotFound:
				status = 404
			case CodeUnsupportedVer, CodeMissingCap, CodeInvalidParams, CodeInvalidRequest:
				// 规范明确要求 400 的是：缺 _meta 必需字段、版本不支持、缺客户端能力、头不一致。
				// 未知工具、资源不存在也是 -32602，这里一并用 400 只是简化。
				status = 400
			}
		}
		writeJSON(w, status, resp)
	})
}

func checkHeaders(h http.Header, method, name, uri, ver string) string {
	if h.Get("MCP-Protocol-Version") != ver {
		return fmt.Sprintf("MCP-Protocol-Version 头 %q 与 body 的 %q 不一致", h.Get("MCP-Protocol-Version"), ver)
	}
	if h.Get("Mcp-Method") != method {
		return fmt.Sprintf("Mcp-Method 头 %q 与 body 的 %q 不一致", h.Get("Mcp-Method"), method)
	}
	want := ""
	switch method {
	case "tools/call", "prompts/get":
		want = name
	case "resources/read":
		want = uri
	default:
		return ""
	}
	if got := decodeHeader(h.Get("Mcp-Name")); got != want {
		return fmt.Sprintf("Mcp-Name 头 %q 与 body 的 %q 不一致", got, want)
	}
	return ""
}

// EncodeHeader：不能安全放进 HTTP 头的值（非 ASCII、控制字符、首尾空格）用 =?base64?...?= 包起来。
func EncodeHeader(v string) string {
	safe := v == strings.TrimSpace(v) && !(strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?="))
	for _, c := range []byte(v) {
		if c < 0x20 || c > 0x7e {
			safe = false
		}
	}
	if safe {
		return v
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(v)) + "?="
}

func decodeHeader(v string) string {
	if strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?=") {
		b, err := base64.StdEncoding.DecodeString(v[9 : len(v)-2])
		if err == nil {
			return string(b)
		}
	}
	return v
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// HTTPTransport 是客户端一侧：每个请求一个 POST，带上镜像头。
type HTTPTransport struct {
	URL    string
	Client *http.Client
	Origin string
	nextID atomic.Int64
}

func (t *HTTPTransport) RoundTrip(ctx context.Context, method string, params map[string]any) (Response, error) {
	id := t.nextID.Add(1)
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	meta, _ := params["_meta"].(map[string]any)
	req.Header.Set("MCP-Protocol-Version", fmt.Sprint(meta[metaVersion]))
	req.Header.Set("Mcp-Method", method)
	if n, ok := params["name"].(string); ok {
		req.Header.Set("Mcp-Name", EncodeHeader(n))
	} else if u, ok := params["uri"].(string); ok {
		req.Header.Set("Mcp-Name", EncodeHeader(u))
	}
	if t.Origin != "" {
		req.Header.Set("Origin", t.Origin)
	}
	c := t.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	var out Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Response{}, fmt.Errorf("HTTP %d：响应不是 JSON-RPC", resp.StatusCode)
	}
	return out, nil
}
