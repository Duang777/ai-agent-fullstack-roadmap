// Package agents 是 OpenAI Agents SDK 核心运行时的一个最小 Go 实现，用来看清它的设计：
// Agent = 指令 + 工具 + handoff + guardrail；Runner 跑一个循环：调模型 → 有工具调用就执行再循环、
// 有 handoff 就换 agent 再循环、只有文本就是最终输出；超过 max_turns 报错。
// 审批不是“暂停一个函数”，而是把整个 run 的状态（RunState）序列化出来，人做完决定后从状态继续。
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Item 对应 Responses API 的输入/输出项。只保留三种：消息、函数调用、函数调用结果。
// handoff 对模型来说也是一个函数调用（transfer_to_<agent>），这是 SDK 最关键的设计之一。
type Item struct {
	Type      string `json:"type"` // message / function_call / function_call_output
	Role      string `json:"role,omitempty"`
	Content   string `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
	Agent     string `json:"agent,omitempty"` // 由哪个 agent 产生（输入项为空）
}

func User(s string) Item      { return Item{Type: "message", Role: "user", Content: s} }
func Assistant(s string) Item { return Item{Type: "message", Role: "assistant", Content: s} }
func Call(id, name, args string) Item {
	return Item{Type: "function_call", CallID: id, Name: name, Arguments: args}
}
func output(id, out string) Item { return Item{Type: "function_call_output", CallID: id, Output: out} }

// ModelRequest 是一次模型调用看到的全部东西：当前 agent 的指令、输入项、可用的工具名（含 handoff 工具）。
type ModelRequest struct {
	Agent        string
	Instructions string
	Input        []Item
	Tools        []string
}

type Model interface {
	Respond(ctx context.Context, req ModelRequest) ([]Item, error)
}

type ModelFunc func(ctx context.Context, req ModelRequest) ([]Item, error)

func (f ModelFunc) Respond(ctx context.Context, req ModelRequest) ([]Item, error) { return f(ctx, req) }

// Agent 是一份配置，不是一个进程：同一个 Agent 可以被很多 run 同时使用。
type Agent struct {
	Name               string
	Instructions       string
	HandoffDescription string
	Tools              []*FunctionTool
	Handoffs           []*Handoff
	InputGuardrails    []InputGuardrail  // 只在 run 的第一个 agent 上生效
	OutputGuardrails   []OutputGuardrail // 只在产生最终输出的 agent 上生效
	ToolUseBehavior    string            // "" = run_llm_again（默认）；"stop_on_first_tool"
	StopAtTools        []string          // 调用了其中任何一个工具，就把它的输出当最终输出
}

// FunctionTool：模型能调用的本地函数。
type FunctionTool struct {
	Name          string
	Description   string
	NeedsApproval bool
	// ApprovalFn 按调用决定要不要审批。参数解析不了（不是 JSON 对象）时不调用它，直接要求审批：fail closed。
	ApprovalFn       func(rc *RunContext, args map[string]any, callID string) bool
	InputGuardrails  []ToolGuardrail       // 审批通过后、执行前
	OutputGuardrails []ToolOutputGuardrail // 执行后
	Run              func(ctx context.Context, rc *RunContext, args map[string]any) (string, error)
}

// Handoff：把对话交给另一个 agent。对模型而言它就是一个叫 transfer_to_<name> 的工具。
type Handoff struct {
	Agent       *Agent
	ToolName    string
	OnHandoff   func(rc *RunContext, args map[string]any) error // 返回错误就不转交
	InputFilter func(HandoffInputData) HandoffInputData         // 改写下一个 agent 能看到的历史
}

func HandoffTo(a *Agent) *Handoff { return &Handoff{Agent: a} }

func (h *Handoff) toolName() string {
	if h.ToolName != "" {
		return h.ToolName
	}
	return "transfer_to_" + strings.ReplaceAll(strings.ToLower(h.Agent.Name), " ", "_")
}

// HandoffInputData 对应 SDK 的同名结构：原始输入历史、handoff 那一轮之前产生的项、这一轮产生的项。
type HandoffInputData struct {
	InputHistory    []Item
	PreHandoffItems []Item
	NewItems        []Item
}

// RemoveAllTools 对应 handoff_filters.remove_all_tools：去掉所有函数调用和结果，只留消息。
func RemoveAllTools(d HandoffInputData) HandoffInputData {
	keep := func(xs []Item) []Item {
		var out []Item
		for _, x := range xs {
			if x.Type == "message" {
				out = append(out, x)
			}
		}
		return out
	}
	return HandoffInputData{keep(d.InputHistory), keep(d.PreHandoffItems), keep(d.NewItems)}
}

// AsTool 把 agent 包成工具：被调用时跑一个独立的嵌套 run，只把最终输出交回。
// 和 handoff 的区别：控制权留在调用方，嵌套 agent 看不到外层对话。
func (a *Agent) AsTool(name, desc string) *FunctionTool {
	return &FunctionTool{Name: name, Description: desc,
		Run: func(ctx context.Context, rc *RunContext, args map[string]any) (string, error) {
			in, _ := args["input"].(string)
			res, err := rc.runner.Run(ctx, a, in, RunOptions{MaxTurns: rc.opts.MaxTurns, Context: rc.Context})
			if err != nil {
				return "", err
			}
			if len(res.Interruptions) > 0 {
				return "", errors.New("嵌套 agent 的审批本练习未实现")
			}
			rc.addTrace(res.Trace...)
			return res.FinalOutput, nil
		}}
}

// ---------- guardrail ----------

type InputGuardrail struct {
	Name     string
	Blocking bool // true = run_in_parallel=False：先跑完再调模型
	Fn       func(ctx context.Context, input []Item) (tripwire bool, info string, err error)
}

type OutputGuardrail struct {
	Name string
	Fn   func(ctx context.Context, output string) (tripwire bool, info string, err error)
}

type ToolGuardrailResult struct {
	Behavior string // allow / reject_content / raise
	Message  string
}

func Allow() ToolGuardrailResult                 { return ToolGuardrailResult{Behavior: "allow"} }
func RejectContent(m string) ToolGuardrailResult { return ToolGuardrailResult{"reject_content", m} }
func Raise(m string) ToolGuardrailResult         { return ToolGuardrailResult{"raise", m} }

type ToolGuardrail struct {
	Name string
	Fn   func(args map[string]any) ToolGuardrailResult
}

type ToolOutputGuardrail struct {
	Name string
	Fn   func(output string) ToolGuardrailResult
}

// ---------- 错误 ----------

var (
	ErrMaxTurns            = errors.New("超过 max_turns")
	ErrModelBehavior       = errors.New("模型输出不符合预期")
	ErrUnknownInterruption = errors.New("不是这个 RunState 里等待中的审批")
	ErrStateMismatch       = errors.New("RunState 和传入的 agent 不匹配")
)

// TripwireError 对应 Input/Output/ToolInput/ToolOutput GuardrailTripwireTriggered。
type TripwireError struct {
	Kind      string // input / output / tool_input / tool_output
	Guardrail string
	Info      string
}

func (e *TripwireError) Error() string {
	return fmt.Sprintf("%s guardrail %q 触发：%s", e.Kind, e.Guardrail, e.Info)
}

// parseArgs：参数必须是 JSON 对象，否则报错（审批规则据此 fail closed）。
func parseArgs(s string) (map[string]any, error) {
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("参数为空")
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("参数不是 JSON 对象：%T", v)
	}
	return m, nil
}
