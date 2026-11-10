package graph

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

var (
	ErrEmptyInput       = errors.New("thread 没有任何 checkpoint，无法继续")
	ErrRecursion        = errors.New("超过递归上限（super-step 数）")
	ErrInvalidUpdate    = errors.New("LastValue 通道在同一个 super-step 收到多个写入")
	ErrNoInterrupt      = errors.New("没有等待中的 interrupt")
	ErrAmbiguousResume  = errors.New("有多个等待中的 interrupt，必须按 ID 给出恢复值")
	ErrUnknownInterrupt = errors.New("不存在这个 interrupt ID")
	ErrStaleResume      = errors.New("这个 interrupt 已经回答过了")
	ErrNoCheckpoint     = errors.New("checkpoint 不存在")
)

// NodeError 表示某一步里有节点失败。成功节点的写入已经作为 pending writes 保存。
type NodeError struct {
	Task string
	Msg  string
}

func (e *NodeError) Error() string { return fmt.Sprintf("任务 %s 失败：%s", e.Task, e.Msg) }

// Config 对应 LangGraph 的 config：thread_id 是持久化的“游标”。
type Config struct {
	Thread         string
	RecursionLimit int // 一次运行最多多少个 super-step；0 表示默认值 10007（LangGraph 1.1.4 起的默认）
}

type Result struct {
	Values     State
	Interrupts []Interrupt
	Done       bool
}

// ResumeMap 按 interrupt ID 给出恢复值，用于同时有多个 interrupt 等待时。
type ResumeMap map[string]any

// Runtime 是节点拿到的运行时句柄。
type Runtime struct {
	task    Task
	resumes []any
	idx     int
}

func (rt *Runtime) TaskID() string { return rt.task.ID }

type interruptSignal struct{ in Interrupt }

// Interrupt 暂停图并把 payload 交给调用方。恢复时节点从头重跑，
// 第 i 次调用 Interrupt 直接返回第 i 个恢复值——严格按序号，所以调用顺序必须每次都一样。
// 和 Python 一样用“抛出”实现：调用之后的代码这一次不会执行。
func (rt *Runtime) Interrupt(payload any) any {
	i := rt.idx
	rt.idx++
	if i < len(rt.resumes) {
		return rt.resumes[i]
	}
	panic(interruptSignal{Interrupt{ID: fmt.Sprintf("%s/%d", rt.task.ID, i), Task: rt.task.ID, Index: i, Value: payload}})
}

// Invoke 提交新输入：输入经 reducer 合并进这个 thread 的现有状态，从 START 开始。
// 新输入不会清空 thread；要从空状态开始就换一个 thread。
func (g *Graph) Invoke(cfg Config, input State) (Result, error) {
	if input == nil {
		return Result{}, ErrEmptyInput
	}
	cp := Checkpoint{Step: -1, Source: "input", Values: State{}}
	if prev, ok := g.cp.Get(cfg.Thread, 0); ok {
		cp.Parent, cp.Step, cp.Values = prev.ID, prev.Step+1, prev.Values
	}
	cp.Next = []Task{{ID: taskID(cp.Step+1, START, 0), Node: START, Arg: input}}
	id, err := g.cp.Put(cfg.Thread, cp)
	if err != nil {
		return Result{}, err
	}
	cp.ID = id
	return g.execute(cfg, Tuple{Checkpoint: cp})
}

// Continue 从最新的 checkpoint 继续：用于节点失败之后重试（对应 invoke(None, config)）。
func (g *Graph) Continue(cfg Config) (Result, error) {
	t, ok := g.cp.Get(cfg.Thread, 0)
	if !ok {
		return Result{}, ErrEmptyInput
	}
	return g.execute(cfg, t)
}

// Resume 回答等待中的 interrupt（对应 Command(resume=...)）。
func (g *Graph) Resume(cfg Config, value any) (Result, error) {
	t, ok := g.cp.Get(cfg.Thread, 0)
	if !ok {
		return Result{}, ErrEmptyInput
	}
	pending := pendingInterrupts(t)
	if len(pending) == 0 {
		return Result{}, ErrNoInterrupt
	}
	var ws []Write
	if rm, ok := value.(ResumeMap); ok {
		ids := make([]string, 0, len(rm))
		for id := range rm {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			in, found := findInterrupt(pending, id)
			if !found {
				if _, old := findInterrupt(allInterrupts(t), id); old {
					return Result{}, fmt.Errorf("%w：%s", ErrStaleResume, id)
				}
				return Result{}, fmt.Errorf("%w：%s", ErrUnknownInterrupt, id)
			}
			ws = append(ws, Write{TaskID: in.Task, Kind: "resume", Resume: rm[id]})
		}
	} else {
		if len(pending) != 1 {
			return Result{}, fmt.Errorf("%w（%d 个）", ErrAmbiguousResume, len(pending))
		}
		ws = append(ws, Write{TaskID: pending[0].Task, Kind: "resume", Resume: value})
	}
	if err := g.cp.PutWrites(cfg.Thread, t.ID, ws...); err != nil {
		return Result{}, err
	}
	t, _ = g.cp.Get(cfg.Thread, t.ID)
	return g.execute(cfg, t)
}

// Replay 从历史上的某个 checkpoint 重新执行之后的步骤（时间旅行）。
// 之前的步骤不重跑；新产生的 checkpoint 以它为父节点，原来的历史保留不动。
func (g *Graph) Replay(cfg Config, checkpointID int) (Result, error) {
	t, ok := g.cp.Get(cfg.Thread, checkpointID)
	if !ok {
		return Result{}, ErrNoCheckpoint
	}
	t.Writes = nil // 重放就是重新执行，不复用旧的 pending writes
	return g.execute(cfg, t)
}

// UpdateState 像某个节点产生了这次更新一样修改状态：值经过 reducer，
// 下一步由 asNode 的出边决定。会新建一个 source=update 的 checkpoint，不改旧的。
func (g *Graph) UpdateState(cfg Config, update State, asNode string) error {
	t, ok := g.cp.Get(cfg.Thread, 0)
	if !ok {
		return ErrEmptyInput
	}
	if _, ok := g.b.nodes[asNode]; !ok {
		return fmt.Errorf("%w：%s", ErrUnknownNode, asNode)
	}
	w := Write{TaskID: "update", Kind: "result", Update: update}
	vals, err := g.apply(t.Values, []Write{w})
	if err != nil {
		return err
	}
	cp := Checkpoint{Parent: t.ID, Step: t.Step + 1, Source: "update", Values: vals,
		Next: g.nextTasks(t.Step+1, []Task{{Node: asNode}}, []Write{w}, vals)}
	_, err = g.cp.Put(cfg.Thread, cp)
	return err
}

type Snapshot struct {
	Checkpoint
	Interrupts []Interrupt
}

func (s Snapshot) NextNodes() []string {
	var out []string
	for _, t := range s.Next {
		out = append(out, t.Node)
	}
	return out
}

func (g *Graph) GetState(cfg Config) (Snapshot, bool) {
	t, ok := g.cp.Get(cfg.Thread, 0)
	if !ok {
		return Snapshot{}, false
	}
	return Snapshot{Checkpoint: t.Checkpoint, Interrupts: pendingInterrupts(t)}, true
}

func (g *Graph) History(cfg Config) []Checkpoint { return g.cp.List(cfg.Thread) }

// execute 是 Pregel 主循环：一次一个 super-step，直到没有下一步任务。
func (g *Graph) execute(cfg Config, t Tuple) (Result, error) {
	limit := cfg.RecursionLimit
	if limit <= 0 {
		limit = 10007
	}
	cp, writes := t.Checkpoint, t.Writes
	for steps := 0; len(cp.Next) > 0; steps++ {
		if steps >= limit {
			return Result{Values: cp.Values}, fmt.Errorf("%w：%d", ErrRecursion, limit)
		}
		done := map[string]Write{}
		resumes := map[string][]any{}
		for _, w := range writes {
			switch w.Kind {
			case "result":
				done[w.TaskID] = w
			case "resume":
				resumes[w.TaskID] = append(resumes[w.TaskID], w.Resume)
			}
		}

		// 1. 并行运行这一步的所有任务。每个任务拿到同一份快照的独立拷贝，看不到同一步里别人的写入。
		results := make([]Write, len(cp.Next))
		fresh := make([]bool, len(cp.Next))
		var wg sync.WaitGroup
		for i, task := range cp.Next {
			if w, ok := done[task.ID]; ok {
				results[i] = w // pending write：上次已经成功，不重跑
				continue
			}
			fresh[i] = true
			view := cp.Values
			if task.Arg != nil {
				view = task.Arg // Send：只看自己的输入
			}
			var own State
			if err := jsonCopy(view, &own); err != nil {
				return Result{}, err
			}
			wg.Add(1)
			go func(i int, task Task, view State) {
				defer wg.Done()
				results[i] = g.runTask(task, view, resumes[task.ID])
			}(i, task, own)
		}
		wg.Wait()

		// 2. 新结果立刻作为写入挂到当前 checkpoint 上（per-task 持久化）
		var newWrites []Write
		for i := range results {
			if fresh[i] {
				newWrites = append(newWrites, results[i])
			}
		}
		if err := g.cp.PutWrites(cfg.Thread, cp.ID, newWrites...); err != nil {
			return Result{}, err
		}

		// 3. 有失败就停在这一步；有 interrupt 就暂停。checkpoint 都不前进。
		var ins []Interrupt
		for i, w := range results {
			if w.Kind == "error" {
				return Result{Values: cp.Values}, &NodeError{Task: cp.Next[i].ID, Msg: w.Err}
			}
			if w.Kind == "interrupt" {
				ins = append(ins, *w.Interrupt)
			}
		}
		if len(ins) > 0 {
			sort.Slice(ins, func(i, j int) bool { return ins[i].ID < ins[j].ID })
			return Result{Values: cp.Values, Interrupts: ins}, nil
		}

		// 4. 步末统一合并写入（按任务 ID 排序，结果与 goroutine 调度无关），算出下一步，写 checkpoint
		order := make([]int, len(results))
		for i := range order {
			order[i] = i
		}
		sort.Slice(order, func(a, b int) bool { return cp.Next[order[a]].ID < cp.Next[order[b]].ID })
		tasks := make([]Task, len(order))
		sorted := make([]Write, len(order))
		for k, i := range order {
			tasks[k], sorted[k] = cp.Next[i], results[i]
		}
		vals, err := g.apply(cp.Values, sorted)
		if err != nil {
			return Result{Values: cp.Values}, err
		}
		next := Checkpoint{Parent: cp.ID, Step: cp.Step + 1, Source: "loop", Values: vals,
			Next: g.nextTasks(cp.Step+1, tasks, sorted, vals)}
		id, err := g.cp.Put(cfg.Thread, next)
		if err != nil {
			return Result{}, err
		}
		next.ID = id
		cp, writes = next, nil
	}
	return Result{Values: cp.Values, Done: true}, nil
}

func (g *Graph) runTask(task Task, view State, resumes []any) (w Write) {
	w.TaskID = task.ID
	defer func() {
		if r := recover(); r != nil {
			if s, ok := r.(interruptSignal); ok {
				in := s.in
				w = Write{TaskID: task.ID, Kind: "interrupt", Interrupt: &in}
			} else {
				w = Write{TaskID: task.ID, Kind: "error", Err: fmt.Sprint("panic：", r)}
			}
		}
		// 结果要落盘，先确认能序列化；不能的话当作这个任务失败
		var norm Write
		if err := jsonCopy(w, &norm); err != nil {
			norm = Write{TaskID: task.ID, Kind: "error", Err: err.Error()}
		}
		w = norm
	}()
	var out any
	var err error
	if task.Node == START {
		out = task.Arg
	} else {
		out, err = g.b.nodes[task.Node].fn(&Runtime{task: task, resumes: resumes}, view)
	}
	if err != nil {
		return Write{TaskID: task.ID, Kind: "error", Err: err.Error()}
	}
	w.Kind = "result"
	switch o := out.(type) {
	case nil:
	case State:
		w.Update = o
	case Command:
		w.Update, w.Goto = o.Update, o.Goto
	default:
		return Write{TaskID: task.ID, Kind: "error", Err: fmt.Sprintf("节点返回了不支持的类型 %T", out)}
	}
	return w
}

// apply 把一步里所有写入合并进状态。
func (g *Graph) apply(vals State, ws []Write) (State, error) {
	var out State
	if err := jsonCopy(vals, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = State{}
	}
	lastWriter := map[string]string{}
	for _, w := range ws {
		keys := make([]string, 0, len(w.Update))
		for k := range w.Update {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := w.Update[k]
			r, declared := g.b.channels[k]
			if !declared {
				return nil, fmt.Errorf("%w：%s", ErrUnknownChannel, k)
			}
			if ov, ok := asOverwrite(v); ok {
				out[k] = ov
				continue
			}
			if r == nil {
				if prev, ok := lastWriter[k]; ok {
					return nil, fmt.Errorf("%w：通道 %s 被 %s 和 %s 同时写入，需要给它一个 reducer", ErrInvalidUpdate, k, prev, w.TaskID)
				}
				lastWriter[k] = w.TaskID
				out[k] = v
				continue
			}
			nv, err := r(out[k], v)
			if err != nil {
				return nil, fmt.Errorf("通道 %s：%w", k, err)
			}
			out[k] = nv
		}
	}
	return out, nil
}

// nextTasks：每个完成的任务，按 静态边 + 条件边（读合并后的状态）+ Command.Goto 产生下一步。
// 普通目标去重（一个节点被多个前驱触发也只跑一次）；Send 不去重，每个都是独立实例。
// 注意静态边和 Goto 是叠加的，所以同一个节点不要两种方式混用。
func (g *Graph) nextTasks(step int, tasks []Task, ws []Write, vals State) []Task {
	var out []Task
	seen := map[string]bool{}
	count := map[string]int{}
	add := func(t Target) {
		if t.Node == END {
			return
		}
		if t.Arg == nil {
			if seen[t.Node] {
				return
			}
			seen[t.Node] = true
		}
		out = append(out, Task{ID: taskID(step+1, t.Node, count[t.Node]), Node: t.Node, Arg: t.Arg})
		count[t.Node]++
	}
	for i, task := range tasks {
		for _, to := range g.b.edges[task.Node] {
			add(To(to))
		}
		if r := g.b.routers[task.Node]; r != nil {
			for _, t := range r(vals) {
				add(t)
			}
		}
		for _, t := range ws[i].Goto {
			add(t)
		}
	}
	return out
}

func taskID(step int, node string, i int) string { return fmt.Sprintf("s%d:%s:%03d", step, node, i) }

// pendingInterrupts：还没有结果、而且还没被回答的那个 interrupt。
// 判断“没被回答”：interrupt 的序号等于这个任务已收到的恢复值个数。
func pendingInterrupts(t Tuple) []Interrupt {
	resumes := map[string]int{}
	hasResult := map[string]bool{}
	last := map[string]Interrupt{}
	for _, w := range t.Writes {
		switch w.Kind {
		case "resume":
			resumes[w.TaskID]++
		case "result":
			hasResult[w.TaskID] = true
		case "interrupt":
			last[w.TaskID] = *w.Interrupt
		}
	}
	var out []Interrupt
	for task, in := range last {
		if !hasResult[task] && in.Index == resumes[task] {
			out = append(out, in)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func allInterrupts(t Tuple) []Interrupt {
	var out []Interrupt
	for _, w := range t.Writes {
		if w.Interrupt != nil {
			out = append(out, *w.Interrupt)
		}
	}
	return out
}

func findInterrupt(ins []Interrupt, id string) (Interrupt, bool) {
	for _, in := range ins {
		if in.ID == id {
			return in, true
		}
	}
	return Interrupt{}, false
}
