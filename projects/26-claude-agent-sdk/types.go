// Package claudesdk 用 Go 标准库重写 Claude Agent SDK 运行时的核心：
// agent 循环与消息流、六步权限判定、hooks、子 agent、defer 与会话。
// 对应课件 notes/04-frameworks/03-claude-agent-sdk.md。
package claudesdk

import (
	"context"
	"errors"
)

// Block 是一个内容块，对应 Messages API 的 text / tool_use / tool_result。
type Block struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   string         `json:"content,omitempty"`
	IsError   bool           `json:"is_error,omitempty"`
}

func Text(s string) Block { return Block{Type: "text", Text: s} }

func ToolUse(id, name string, in map[string]any) Block {
	return Block{Type: "tool_use", ID: id, Name: name, Input: in}
}

// Message 是对话里的一条消息：user 或 assistant。
type Message struct {
	Role   string  `json:"role"`
	Blocks []Block `json:"blocks"`
}

// ModelRequest 是一次模型调用看到的全部内容。
type ModelRequest struct {
	System   string
	Tools    []string
	Messages []Message
}

// ModelResponse 是一次模型响应，CostUSD 由 API 返回的用量算出。
type ModelResponse struct {
	Blocks  []Block
	CostUSD float64
}

type Model interface {
	Respond(ctx context.Context, req ModelRequest) (ModelResponse, error)
}

// Tool 是一个工具定义。
type Tool struct {
	Name string
	// ReadOnly 的工具可以和相邻的只读调用并发执行（SDK 里是 readOnlyHint）。
	ReadOnly bool
	// EditsFiles 表示写文件的工具（Write / Edit），acceptEdits 和 plan 模式关心它。
	EditsFiles bool
	// SpecKey 是规则括号里的模式匹配哪个参数，如 Bash 是 command，Edit 是 file_path。
	SpecKey string
	// SelfApprove 是“工具自己就能批准”的调用，比如工作目录内的读文件、只读 shell 命令。
	SelfApprove func(cwd string, in map[string]any) bool
	Run         func(ctx context.Context, in map[string]any) (string, error)
}

// Event 是 SDK 吐给应用的消息流里的一条。
type Event struct {
	Type            string // system | assistant | user | result
	Subtype         string // system: init；result: success / error_*
	MessageID       string // 同一次模型响应拆出的多个 assistant 事件共享
	Block           *Block
	ParentToolUseID string // 子 agent 内部产生的事件带上发起它的 Agent 调用 ID
	SessionID       string
}

const (
	SubtypeSuccess       = "success"
	SubtypeMaxTurns      = "error_max_turns"
	SubtypeMaxBudget     = "error_max_budget_usd"
	SubtypeExecutionFail = "error_during_execution"
)

// Result 对应 ResultMessage。
type Result struct {
	Subtype         string
	Result          string // 只有 success 时有
	StopReason      string // end_turn / tool_deferred
	NumTurns        int
	TotalCostUSD    float64 // 包括子 agent
	SessionID       string
	DeferredToolUse *Block
	Events          []Event
}

var (
	ErrNoSession       = errors.New("找不到会话")
	ErrDeferredPending = errors.New("会话里有一个被 defer 的工具调用，先不带 prompt 恢复")
)
