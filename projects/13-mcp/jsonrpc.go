// Package mcp 是 2.2 课的练习：按 MCP 2026-07-28 规范实现一个最小的 Server 和 Client。
// 只用 Go 标准库，覆盖：无状态的每请求 _meta、server/discover、tools / resources / prompts、
// 多轮请求（MRTR）、stdio 与 Streamable HTTP 两种传输、多 Server 聚合。
package mcp

import "encoding/json"

const Version = "2026-07-28"

// 规范里用到的错误码
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602 // 2026-07-28 起“资源不存在”也用它（以前是 -32002）
	CodeInternal       = -32603
	CodeHeaderMismatch = -32020
	CodeMissingCap     = -32021
	CodeUnsupportedVer = -32022
)

const (
	metaVersion = "io.modelcontextprotocol/protocolVersion"
	metaCaps    = "io.modelcontextprotocol/clientCapabilities"
	metaClient  = "io.modelcontextprotocol/clientInfo"
	metaServer  = "io.modelcontextprotocol/serverInfo"
)

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // 没有 id 的是通知
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r Request) IsNotification() bool { return len(r.ID) == 0 }

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Meta 是每个请求都要带的协议字段。2026-07-28 去掉了 initialize 握手：版本和能力每次都带。
type Meta struct {
	ProtocolVersion string          `json:"io.modelcontextprotocol/protocolVersion,omitempty"`
	ClientInfo      *Implementation `json:"io.modelcontextprotocol/clientInfo,omitempty"`
	ClientCaps      map[string]any  `json:"io.modelcontextprotocol/clientCapabilities,omitempty"`
}

func errResp(id json.RawMessage, code int, msg string, data any) Response {
	return Response{JSONRPC: "2.0", ID: id, Error: &Error{Code: code, Message: msg, Data: data}}
}

func okResp(id json.RawMessage, result map[string]any) Response {
	b, _ := json.Marshal(result)
	return Response{JSONRPC: "2.0", ID: id, Result: b}
}
