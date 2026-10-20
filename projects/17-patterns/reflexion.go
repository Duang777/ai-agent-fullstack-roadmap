package patterns

import (
	"context"
	"strings"
)

// Reflexion：做一次 → 用外部信号评估 → 失败就写一段反思存进记忆 → 带着反思再做。
// 权重不变，“学习”全在提示词里的那几段反思。
type Feedback struct {
	Pass   bool
	Detail string // 例如没通过的测试和报错
}

type Actor func(ctx context.Context, task string, reflections []string) (string, error)
type Evaluator func(ctx context.Context, attempt string) (Feedback, error)
type Reflector func(ctx context.Context, task, attempt string, fb Feedback) (string, error)

type ReflexionRun struct {
	Answer      string
	Trials      int
	Pass        bool
	Reflections []string
}

// Reflexion 最多尝试 maxTrials 次；记忆只保留最近 memSize 条反思（论文里是 1–3 条），
// 否则提示词越来越长，旧的、已经过时的反思还会干扰模型。
func Reflexion(ctx context.Context, task string, act Actor, eval Evaluator, reflect Reflector, maxTrials, memSize int) (ReflexionRun, error) {
	var mem []string
	var last string
	for t := 1; t <= maxTrials; t++ {
		attempt, err := act(ctx, task, mem)
		if err != nil {
			return ReflexionRun{Trials: t, Reflections: mem}, err
		}
		last = attempt
		fb, err := eval(ctx, attempt)
		if err != nil {
			return ReflexionRun{Trials: t, Reflections: mem}, err
		}
		if fb.Pass {
			return ReflexionRun{Answer: attempt, Trials: t, Pass: true, Reflections: mem}, nil
		}
		if t == maxTrials {
			break // 最后一次失败后再反思也用不上
		}
		r, err := reflect(ctx, task, attempt, fb)
		if err != nil {
			return ReflexionRun{Trials: t, Reflections: mem}, err
		}
		if r = strings.TrimSpace(r); r != "" {
			mem = append(mem, r)
			if len(mem) > memSize {
				mem = mem[len(mem)-memSize:]
			}
		}
	}
	// 没通过也要如实返回：最后一次的结果，以及 Pass=false
	return ReflexionRun{Answer: last, Trials: maxTrials, Reflections: mem}, nil
}
