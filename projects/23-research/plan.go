package research

import (
	"errors"
	"fmt"
)

// Kind 是问题的类型，决定投入多少子代理和调用。
type Kind int

const (
	Fact       Kind = iota // 查一个事实
	Comparison             // 直接对比几样东西
	Broad                  // 开放的、面很广的调研
)

// Effort 是预算。Anthropic 把这条规则直接写进主研究员的提示词：
// 查事实 1 个 agent、3–10 次工具调用；对比 2–4 个子代理、每个 10–15 次；
// 复杂调研 10 个以上子代理，分工明确。没有这条规则，早期版本会为简单问题开 50 个子代理。
type Effort struct {
	Subagents     int
	CallsPerAgent int
}

func (e Effort) Total() int { return e.Subagents * e.CallsPerAgent }

func Scale(k Kind, aspects int) Effort {
	switch k {
	case Fact:
		return Effort{Subagents: 1, CallsPerAgent: 10}
	case Comparison:
		return Effort{Subagents: clamp(aspects, 2, 4), CallsPerAgent: 15}
	default:
		return Effort{Subagents: clamp(aspects, 10, 20), CallsPerAgent: 15}
	}
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }

// Aspect 是问题的一个方面：一个子代理负责一个，边界清楚，避免两个子代理搜同样的东西。
type Aspect struct {
	Name       string
	Keywords   []string // 第一个词用来“先宽”，全部用来“后窄”
	Alternates []string // 这一方面第一轮没找到时，换一组说法再试一次
}

type Task struct {
	ID          string
	Aspect      string
	Objective   string
	Keywords    []string
	MaxCalls    int
	MinEvidence int // 找到这么多条证据就停，不要在已经够了的时候继续搜
}

type Plan struct {
	Question   string
	Kind       Kind
	Effort     Effort
	Required   []string
	Tasks      []Task
	Alternates map[string][]string
}

var ErrTooManyAspects = errors.New("方面数超过预算允许的子代理数")

// NewPlan 把问题拆成任务。每个任务写明目标、关键词、调用上限和停止条件。
// Anthropic 的教训：只给一句“研究一下半导体短缺”，子代理会误解任务或者互相重复。
func NewPlan(question string, k Kind, aspects []Aspect) (Plan, error) {
	e := Scale(k, len(aspects))
	if len(aspects) == 0 || len(aspects) > e.Subagents {
		return Plan{}, fmt.Errorf("%w：%d 个方面，%d 个子代理", ErrTooManyAspects, len(aspects), e.Subagents)
	}
	p := Plan{Question: question, Kind: k, Effort: e, Alternates: map[string][]string{}}
	for _, a := range aspects {
		p.Required = append(p.Required, a.Name)
		p.Tasks = append(p.Tasks, Task{
			ID:          a.Name,
			Aspect:      a.Name,
			Objective:   fmt.Sprintf("只调查“%s”这一方面：%s。不要调查其他方面。", a.Name, question),
			Keywords:    a.Keywords,
			MaxCalls:    e.CallsPerAgent,
			MinEvidence: 3,
		})
		if len(a.Alternates) > 0 {
			p.Alternates[a.Name] = a.Alternates
		}
	}
	return p, nil
}
