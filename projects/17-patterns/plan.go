package patterns

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Step 是计划里的一步。参数里可以写 #E1、#E2 引用前面步骤的结果（ReWOO 的写法），
// 依赖关系从引用里推出来，没有依赖的步骤可以并行（LLMCompiler 的思路）。
type Step struct {
	ID   string // "E1"
	Tool string
	Args string
}

type Plan []Step

var refRe = regexp.MustCompile(`#(E\d+)`)

func (s Step) Deps() []string {
	var ds []string
	for _, m := range refRe.FindAllStringSubmatch(s.Args, -1) {
		ds = append(ds, m[1])
	}
	return ds
}

// Validate：ID 不重复、引用的步骤存在、没有环、工具存在。计划是模型写的，执行前必须检查。
func (p Plan) Validate(tools map[string]Tool) error {
	ids := map[string]bool{}
	for _, s := range p {
		if ids[s.ID] {
			return errf("步骤 %s 重复", s.ID)
		}
		ids[s.ID] = true
		if _, ok := tools[s.Tool]; !ok {
			return errf("步骤 %s 用了不存在的工具 %s", s.ID, s.Tool)
		}
	}
	for _, s := range p {
		for _, d := range s.Deps() {
			if !ids[d] {
				return errf("步骤 %s 引用了不存在的 #%s", s.ID, d)
			}
		}
	}
	if _, err := p.Waves(); err != nil {
		return err
	}
	return nil
}

// Waves 把计划分成若干波：同一波里的步骤互不依赖，可以并行。
func (p Plan) Waves() ([][]Step, error) {
	done := map[string]bool{}
	left := append(Plan(nil), p...)
	var waves [][]Step
	for len(left) > 0 {
		var wave, rest []Step
		for _, s := range left {
			ready := true
			for _, d := range s.Deps() {
				if !done[d] {
					ready = false
				}
			}
			if ready {
				wave = append(wave, s)
			} else {
				rest = append(rest, s)
			}
		}
		if len(wave) == 0 {
			return nil, errf("计划里有循环依赖")
		}
		for _, s := range wave {
			done[s.ID] = true
		}
		waves, left = append(waves, wave), rest
	}
	return waves, nil
}

type StepError struct {
	Step Step
	Err  error
}

func (e *StepError) Error() string { return "步骤 " + e.Step.ID + " 失败：" + e.Err.Error() }

// Execute 一波一波地执行，同一波并行；把 #En 替换成结果。任何一步失败就停下，返回已有结果。
func Execute(ctx context.Context, p Plan, tools map[string]Tool) (map[string]string, error) {
	waves, err := p.Waves()
	if err != nil {
		return nil, err
	}
	results := map[string]string{}
	for _, wave := range waves {
		var mu sync.Mutex
		var wg sync.WaitGroup
		var first *StepError
		for _, s := range wave {
			args := refRe.ReplaceAllStringFunc(s.Args, func(r string) string { return results[r[1:]] })
			wg.Add(1)
			go func(s Step, args string) {
				defer wg.Done()
				out, err := tools[s.Tool](ctx, args)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					if first == nil || s.ID < first.Step.ID {
						first = &StepError{s, err}
					}
					return
				}
				results[s.ID] = out
			}(s, args)
		}
		wg.Wait()
		if first != nil {
			return results, first
		}
	}
	return results, nil
}

// Planner 产出计划；replan 时会拿到已完成的结果和失败原因。
type Planner func(ctx context.Context, task string, done map[string]string, failure error) (Plan, error)

// Solver 根据全部结果写最终回答（ReWOO 的 Solver）。
type Solver func(ctx context.Context, task string, results map[string]string) (string, error)

type PlanRun struct {
	Answer  string
	Plans   int // 一共规划了几次（1 = 没有重新规划）
	Results map[string]string
}

// PlanAndExecute：先规划、再执行；失败时带着已完成的结果和错误重新规划，最多 maxPlans 次。
func PlanAndExecute(ctx context.Context, task string, plan Planner, tools map[string]Tool, solve Solver, maxPlans int) (PlanRun, error) {
	done := map[string]string{}
	var failure error
	for i := 1; i <= maxPlans; i++ {
		p, err := plan(ctx, task, done, failure)
		if err != nil {
			return PlanRun{Plans: i}, err
		}
		if err := p.Validate(tools); err != nil {
			failure = err // 计划本身不合法，也算失败，交回给规划器修
			continue
		}
		res, err := Execute(ctx, p, tools)
		for k, v := range res {
			done[k] = v
		}
		if err != nil {
			failure = err
			continue
		}
		ans, err := solve(ctx, task, done)
		return PlanRun{Answer: ans, Plans: i, Results: done}, err
	}
	return PlanRun{Plans: maxPlans, Results: done}, errf("重新规划 %d 次仍失败：%v", maxPlans, failure)
}

func sortStrings(s []string) { sort.Strings(s) }

func keys(m map[string]string) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}
