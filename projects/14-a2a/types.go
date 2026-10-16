// Package a2a 是 2.3 课的练习：按 A2A 1.0 规范，用 Go 标准库实现一个最小的 Agent（Server）和 Client。
// 覆盖：Agent Card 发现、JSON-RPC 绑定的 SendMessage / SendStreamingMessage / GetTask / ListTasks / CancelTask、
// 任务生命周期、contextId 与 taskId 规则、input-required 多轮、阻塞与 returnImmediately、SSE 流、版本头、
// 以及把远程 Agent 包装成本地 Agent 的一个工具。
// 生产里用官方 SDK：github.com/a2aproject/a2a-go/v2。
package a2a

import (
	"encoding/json"
	"errors"
)

const Version = "1.0"

type TaskState string

const (
	StateSubmitted     TaskState = "TASK_STATE_SUBMITTED"
	StateWorking       TaskState = "TASK_STATE_WORKING"
	StateCompleted     TaskState = "TASK_STATE_COMPLETED"
	StateFailed        TaskState = "TASK_STATE_FAILED"
	StateCanceled      TaskState = "TASK_STATE_CANCELED"
	StateRejected      TaskState = "TASK_STATE_REJECTED"
	StateInputRequired TaskState = "TASK_STATE_INPUT_REQUIRED"
	StateAuthRequired  TaskState = "TASK_STATE_AUTH_REQUIRED"
)

// Terminal：终态，任务结束，不能再继续。
func (s TaskState) Terminal() bool {
	return s == StateCompleted || s == StateFailed || s == StateCanceled || s == StateRejected
}

// Interrupted：中断态，等客户端补充信息（或认证）后继续。
func (s TaskState) Interrupted() bool { return s == StateInputRequired || s == StateAuthRequired }

const (
	RoleUser  = "ROLE_USER"
	RoleAgent = "ROLE_AGENT"
)

// Part 必须且只能有 text / raw / url / data 之一。
type Part struct {
	Text      string `json:"text,omitempty"`
	Raw       []byte `json:"raw,omitempty"` // JSON 里是 base64
	URL       string `json:"url,omitempty"`
	Data      any    `json:"data,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
	Filename  string `json:"filename,omitempty"`
}

func (p Part) Validate() error {
	n := 0
	if p.Text != "" {
		n++
	}
	if len(p.Raw) > 0 {
		n++
	}
	if p.URL != "" {
		n++
	}
	if p.Data != nil {
		n++
	}
	if n != 1 {
		return errors.New("part 必须且只能包含 text、raw、url、data 之一")
	}
	return nil
}

type Message struct {
	MessageID        string         `json:"messageId"`
	ContextID        string         `json:"contextId,omitempty"`
	TaskID           string         `json:"taskId,omitempty"`
	Role             string         `json:"role"`
	Parts            []Part         `json:"parts"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	ReferenceTaskIDs []string       `json:"referenceTaskIds,omitempty"`
}

type TaskStatus struct {
	State     TaskState `json:"state"`
	Message   *Message  `json:"message,omitempty"`
	Timestamp string    `json:"timestamp,omitempty"`
}

type Artifact struct {
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name,omitempty"`
	Parts      []Part `json:"parts"`
}

type Task struct {
	ID        string     `json:"id"`
	ContextID string     `json:"contextId,omitempty"`
	Status    TaskStatus `json:"status"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
	History   []Message  `json:"history,omitempty"`
}

type StatusUpdate struct {
	TaskID    string     `json:"taskId"`
	ContextID string     `json:"contextId"`
	Status    TaskStatus `json:"status"`
}

type ArtifactUpdate struct {
	TaskID    string   `json:"taskId"`
	ContextID string   `json:"contextId"`
	Artifact  Artifact `json:"artifact"`
	Append    bool     `json:"append,omitempty"`
	LastChunk bool     `json:"lastChunk,omitempty"`
}

// StreamResponse：流里的每个事件，必须且只能有一个字段。SendMessage 的结果也是 task 或 message 之一。
type StreamResponse struct {
	Task           *Task           `json:"task,omitempty"`
	Message        *Message        `json:"message,omitempty"`
	StatusUpdate   *StatusUpdate   `json:"statusUpdate,omitempty"`
	ArtifactUpdate *ArtifactUpdate `json:"artifactUpdate,omitempty"`
}

type SendConfig struct {
	HistoryLength     *int `json:"historyLength,omitempty"`
	ReturnImmediately bool `json:"returnImmediately,omitempty"`
}

type SendMessageRequest struct {
	Message       Message     `json:"message"`
	Configuration *SendConfig `json:"configuration,omitempty"`
}

// ---- Agent Card ----

type AgentInterface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"` // JSONRPC / GRPC / HTTP+JSON
	ProtocolVersion string `json:"protocolVersion"`
}

type AgentCapabilities struct {
	Streaming         bool `json:"streaming,omitempty"`
	PushNotifications bool `json:"pushNotifications,omitempty"`
	ExtendedAgentCard bool `json:"extendedAgentCard,omitempty"`
}

type AgentSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Examples    []string `json:"examples,omitempty"`
}

type AgentCard struct {
	Name                string            `json:"name"`
	Description         string            `json:"description"`
	SupportedInterfaces []AgentInterface  `json:"supportedInterfaces"`
	Version             string            `json:"version"`
	Capabilities        AgentCapabilities `json:"capabilities"`
	DefaultInputModes   []string          `json:"defaultInputModes"`
	DefaultOutputModes  []string          `json:"defaultOutputModes"`
	Skills              []AgentSkill      `json:"skills"`
}

// ---- JSON-RPC ----

// A2A 1.0 在 JSON-RPC 绑定下的错误码（规范 5.4 节）
const (
	CodeInvalidParams      = -32602
	CodeMethodNotFound     = -32601
	CodeTaskNotFound       = -32001
	CodeTaskNotCancelable  = -32002
	CodePushNotSupported   = -32003
	CodeUnsupportedOp      = -32004
	CodeContentTypeNotSupp = -32005
	CodeVersionNotSupp     = -32009
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }
