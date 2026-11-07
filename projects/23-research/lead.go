package research

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// State 是研究过程的全部状态，每轮结束写一次检查点。
// 长任务出错不能从头再来：既贵又让用户等。
type State struct {
	Plan     Plan               `json:"plan"`
	Round    int                `json:"round"`
	Done     map[string]Finding `json:"done"`
	Pending  []Task             `json:"pending"`
	UsedAlts map[string]bool    `json:"used_alts"`
}

// Lead 是主研究员（orchestrator）。它不搜索，只负责拆任务、派子代理、看缺口、决定何时停止。
type Lead struct {
	Web         *Web
	Store       Store
	MaxParallel int // 一次并行几个子代理（Anthropic：3–5 个）
	MaxRounds   int

	CrashAfterRound int // 测试用：第 N 轮写完检查点后模拟进程崩溃
}

var ErrCrash = errors.New("模拟崩溃")

const (
	keyPlan       = "plan.json"
	keyCheckpoint = "checkpoint.json"
)

// Run 先把计划写进存储。Anthropic 的主研究员一开始就把计划存进 Memory：
// 上下文超过 200K token 会被截断，计划不能跟着丢。
func (l *Lead) Run(ctx context.Context, p Plan) (*State, error) {
	b, _ := json.Marshal(p)
	if err := l.Store.Put(keyPlan, b); err != nil {
		return nil, err
	}
	st := &State{Plan: p, Done: map[string]Finding{}, Pending: p.Tasks, UsedAlts: map[string]bool{}}
	return l.loop(ctx, st)
}

// Resume 从最近一次检查点继续，已完成的任务不再重跑。
func (l *Lead) Resume(ctx context.Context) (*State, error) {
	b, ok := l.Store.Get(keyCheckpoint)
	if !ok {
		return nil, errors.New("没有检查点")
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return l.loop(ctx, &st)
}

func (l *Lead) loop(ctx context.Context, st *State) (*State, error) {
	rounds := l.MaxRounds
	if rounds <= 0 {
		rounds = 3
	}
	for st.Round < rounds && len(st.Pending) > 0 {
		for _, f := range l.runRound(ctx, st.Pending, st.Done) {
			st.Done[f.TaskID] = f
		}
		if err := ctx.Err(); err != nil {
			return st, err
		}
		st.Round++
		st.Pending = l.gaps(st)
		b, _ := json.Marshal(st)
		if err := l.Store.Put(keyCheckpoint, b); err != nil {
			return st, err
		}
		if l.CrashAfterRound == st.Round {
			return st, ErrCrash
		}
	}
	return st, nil
}

// runRound 并行跑一轮子代理，用信号量限制并发。
// 早期 agent 串行搜索，复杂问题慢得难以忍受；并行后研究时间最多缩短 90%。
// 这一轮是同步的：等所有子代理结束再进入下一步（Anthropic 目前也是这样，代价是被最慢的那个拖住）。
func (l *Lead) runRound(ctx context.Context, tasks []Task, done map[string]Finding) []Finding {
	par := l.MaxParallel
	if par <= 0 {
		par = 3
	}
	sem := make(chan struct{}, par)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var out []Finding
	for _, t := range tasks {
		if _, ok := done[t.ID]; ok {
			continue
		}
		wg.Add(1)
		go func(t Task) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := &Researcher{Web: l.Web, Store: l.Store}
			f, err := r.Run(ctx, t)
			if err != nil {
				f.Errors = append(f.Errors, err.Error())
			}
			mu.Lock()
			out = append(out, f)
			mu.Unlock()
		}(t)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].TaskID < out[j].TaskID })
	return out
}

// gaps 找出还没有任何证据的必需方面，用备选说法派一个新任务。每个方面只补一次。
func (l *Lead) gaps(st *State) []Task {
	covered := map[string]bool{}
	for _, f := range st.Done {
		if f.Evidence > 0 {
			covered[f.Aspect] = true
		}
	}
	var next []Task
	for _, a := range st.Plan.Required {
		alts := st.Plan.Alternates[a]
		if covered[a] || len(alts) == 0 || st.UsedAlts[a] {
			continue
		}
		st.UsedAlts[a] = true
		next = append(next, Task{
			ID:          fmt.Sprintf("%s-r%d", a, st.Round),
			Aspect:      a,
			Objective:   "上一轮没有找到证据，换一组说法：" + strings.Join(alts, " "),
			Keywords:    alts,
			MaxCalls:    st.Plan.Effort.CallsPerAgent,
			MinEvidence: 3,
		})
	}
	return next
}

// Evidence 从工件存储里取回全部证据并去重（同一来源、同一句话只算一次）。
func (l *Lead) Evidence(st *State) ([]Evidence, error) {
	ids := make([]string, 0, len(st.Done))
	for id := range st.Done {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	seen := map[string]bool{}
	var all []Evidence
	for _, id := range ids {
		f := st.Done[id]
		if f.Ref == "" {
			continue
		}
		b, ok := l.Store.Get(f.Ref)
		if !ok {
			return nil, fmt.Errorf("工件 %s 不存在", f.Ref)
		}
		var ev []Evidence
		if err := json.Unmarshal(b, &ev); err != nil {
			return nil, err
		}
		for _, e := range ev {
			k := Canonical(e.URL) + "|" + strings.ToLower(e.Claim)
			if !seen[k] {
				seen[k] = true
				all = append(all, e)
			}
		}
	}
	return all, nil
}

// Sources 汇总所有不重复的来源。
func Sources(ev []Evidence) map[string]Quality {
	m := map[string]Quality{}
	for _, e := range ev {
		m[Canonical(e.URL)] = e.Quality
	}
	return m
}
