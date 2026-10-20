package patterns

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

var ErrMaxSteps = errors.New("达到最大步数仍未给出最终回答")

type Trace struct {
	Steps   int
	Answer  string
	History []Message
}

// ReAct：想一步、做一步、看结果，循环到模型不再调用工具。
// 现在的模型原生支持工具调用，“Thought / Action / Observation”不再靠文本格式，
// 但循环的骨架和 2022 年的论文一样。
func ReAct(ctx context.Context, m Model, tools map[string]Tool, task string, maxSteps int) (Trace, error) {
	h := []Message{{Role: User, Content: task}}
	for step := 1; step <= maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return Trace{Steps: step - 1, History: h}, err
		}
		msg, err := m.Next(ctx, h)
		if err != nil {
			return Trace{Steps: step - 1, History: h}, err
		}
		h = append(h, msg)
		if len(msg.Calls) == 0 {
			return Trace{Steps: step, Answer: msg.Content, History: h}, nil
		}
		// 一条消息里的每个调用都要有结果，哪怕失败：缺一个，下一次请求会被 API 拒绝（2.1 课）
		for _, c := range msg.Calls {
			h = append(h, runTool(ctx, tools, c))
		}
	}
	return Trace{Steps: maxSteps, History: h}, ErrMaxSteps
}

// 工具错误变成观察结果交给模型，而不是让整个循环失败：模型看到错误才能换个做法。
func runTool(ctx context.Context, tools map[string]Tool, c ToolCall) Message {
	t, ok := tools[c.Name]
	if !ok {
		return Message{Role: ToolRole, CallID: c.ID, IsError: true, Content: "没有这个工具：" + c.Name + "，可用的有：" + names(tools)}
	}
	out, err := t(ctx, c.Args)
	if err != nil {
		return Message{Role: ToolRole, CallID: c.ID, IsError: true, Content: err.Error()}
	}
	return Message{Role: ToolRole, CallID: c.ID, Content: out}
}

func names(tools map[string]Tool) string {
	var ns []string
	for n := range tools {
		ns = append(ns, n)
	}
	sortStrings(ns)
	return strings.Join(ns, ", ")
}

// ---- 论文原版的文本格式 ----

// TextStep 是论文里模型每一步写出的文本：
//
//	Thought: ...
//	Action: search[阿里巴巴 总部]
//
// 或者 Action: finish[答案]。
type TextStep struct {
	Thought, Action, Input string
}

var actionRe = regexp.MustCompile(`(?m)^Action(?: \d+)?:\s*(\w+)\[(.*)\]\s*$`)
var thoughtRe = regexp.MustCompile(`(?m)^Thought(?: \d+)?:\s*(.*)$`)

// ParseText 解析一步。文本协议的毛病就在这里：少个括号、多写一行 Observation，都要你来兜底。
func ParseText(s string) (TextStep, error) {
	// 模型常常顺手把 Observation 也编出来：截断在这里，编出来的观察不能当真
	if i := strings.Index(s, "\nObservation"); i >= 0 {
		s = s[:i]
	}
	var st TextStep
	if m := thoughtRe.FindStringSubmatch(s); m != nil {
		st.Thought = strings.TrimSpace(m[1])
	}
	ms := actionRe.FindAllStringSubmatch(s, -1)
	switch len(ms) {
	case 0:
		return st, errors.New("没有找到 Action 行，格式应为 Action: 工具名[参数]")
	case 1:
		st.Action, st.Input = ms[0][1], ms[0][2]
		return st, nil
	default:
		return st, errors.New("一步只能有一个 Action")
	}
}
