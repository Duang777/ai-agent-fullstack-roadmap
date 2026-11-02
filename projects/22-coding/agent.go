package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Request 是一次模型调用的全部输入，形状和 Responses API 的 instructions / tools / input 对应。
type Request struct {
	Instructions string
	Tools        []string
	Input        []Message
}

type Model interface {
	Next(ctx context.Context, req Request) (Message, error)
}

type ModelFunc func(ctx context.Context, req Request) (Message, error)

func (f ModelFunc) Next(ctx context.Context, req Request) (Message, error) { return f(ctx, req) }

const baseInstructions = `你是一个在用户仓库里工作的 coding agent。
先收集上下文（grep、read），再动手（edit、write），最后验证（bash 跑测试）。
完成时调用 finish，harness 会先跑验证命令，不通过不算完成。`

// Agent 是 harness：模型之外的那一层，负责工具、上下文和规则。
type Agent struct {
	Model    Model
	WS       *Workspace
	Sh       *Shell
	Verify   string // finish 之前必须通过的命令，比如 "go test ./..."
	MaxSteps int

	// KeepLastObs>0 时，只保留最近 K 条工具输出的全文，更早的折叠成一行（SWE-agent 的做法）。
	// 代价是请求前缀每轮都会变，prompt cache 失效。默认 0 不折叠。
	KeepLastObs int

	input []Message
}

var ErrMaxSteps = errors.New("步数用完")

// Start 拼好会话开头那几条固定内容。
func (a *Agent) Start(cwd string, s Settings) error {
	in, err := InitialInput(a.WS.Root, cwd, s)
	if err != nil {
		return err
	}
	a.input = in
	return nil
}

// ChangeSettings 对话中途改了审批模式或可写目录：追加一条新的 developer 消息，
// 不去改开头那条（改了前缀就变，后面全部 cache miss）。Codex 就是这么处理的。
func (a *Agent) ChangeSettings(s Settings) {
	a.input = append(a.input, Message{Role: "developer", Content: permissionsText(s)})
}

func (a *Agent) Input() []Message { return a.input }

func toolNames() []string {
	names := []string{"bash", "edit", "finish", "grep", "read", "write"}
	sort.Strings(names) // 工具顺序一变，前缀就变。Codex 早期接 MCP 时就踩过这个坑
	return names
}

// Turn 跑一轮：从用户输入到模型给出最终回复（或 finish 验证通过）。
func (a *Agent) Turn(ctx context.Context, user string) (string, error) {
	a.input = append(a.input, Message{Role: "user", Content: user})
	max := a.MaxSteps
	if max <= 0 {
		max = 30
	}
	for step := 0; step < max; step++ {
		m, err := a.Model.Next(ctx, Request{Instructions: baseInstructions, Tools: toolNames(), Input: a.view()})
		if err != nil {
			return "", err
		}
		m.Role = "assistant"
		a.input = append(a.input, m)
		if len(m.Calls) == 0 {
			return m.Content, nil
		}
		for _, c := range m.Calls {
			out, done, err := a.call(ctx, c)
			if err != nil {
				out = strings.TrimSpace("错误：" + err.Error() + "\n" + out)
			}
			a.input = append(a.input, Message{Role: "tool", CallID: c.ID, Content: out})
			if done {
				return out, nil
			}
		}
	}
	return "", ErrMaxSteps
}

// view 生成发给模型的 input。不折叠时就是 a.input 本身的拷贝。
func (a *Agent) view() []Message {
	v := append([]Message(nil), a.input...)
	if a.KeepLastObs <= 0 {
		return v
	}
	seen := 0
	for i := len(v) - 1; i >= 0; i-- {
		if v[i].Role != "tool" {
			continue
		}
		seen++
		if seen > a.KeepLastObs {
			n := len(splitLines([]byte(v[i].Content)))
			v[i].Content = fmt.Sprintf("[较早的输出已省略，原 %d 行]", n)
		}
	}
	return v
}

func (a *Agent) call(ctx context.Context, c Call) (out string, done bool, err error) {
	var args struct {
		Path       string `json:"path"`
		Offset     int    `json:"offset"`
		Limit      int    `json:"limit"`
		Old        string `json:"old_string"`
		New        string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
		Content    string `json:"content"`
		Pattern    string `json:"pattern"`
		Glob       string `json:"glob"`
		Command    string `json:"command"`
		Summary    string `json:"summary"`
	}
	if c.Args != "" {
		if err := json.Unmarshal([]byte(c.Args), &args); err != nil {
			return "", false, fmt.Errorf("参数不是合法 JSON：%v", err)
		}
	}
	switch c.Name {
	case "read":
		out, err = a.WS.Read(args.Path, args.Offset, args.Limit)
	case "edit":
		out, err = a.WS.Edit(args.Path, args.Old, args.New, args.ReplaceAll)
	case "write":
		out, err = a.WS.Write(args.Path, args.Content)
	case "grep":
		out, err = a.WS.Grep(args.Pattern, args.Glob)
	case "bash":
		out, err = a.Sh.Run(ctx, args.Command)
	case "finish":
		if a.Verify != "" {
			// 验证命令是 harness 自己的，不走权限判断，也不受模型影响
			vout, verr := a.Sh.exec(ctx, a.Verify)
			if verr != nil {
				return vout, false, fmt.Errorf("验证没通过（%s：%v），还不能结束", a.Verify, verr)
			}
		}
		return args.Summary, true, nil
	default:
		err = fmt.Errorf("没有工具 %q", c.Name)
	}
	return out, false, err
}
