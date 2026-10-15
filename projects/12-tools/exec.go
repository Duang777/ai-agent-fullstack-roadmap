package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Func func(ctx context.Context, args map[string]any) (string, error)

type Registry struct {
	tools map[string]Tool
	funcs map[string]Func
}

func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}, funcs: map[string]Func{}} }

func (r *Registry) Register(t Tool, f Func) { r.tools[t.Name] = t; r.funcs[t.Name] = f }

func (r *Registry) names() string { return strings.Join(sortedKeys(r.tools), ", ") }

type Executor struct {
	Reg         *Registry
	Timeout     time.Duration // 每个调用的超时
	MaxParallel int           // 只读调用的并发上限
}

// Run 执行模型在一轮里发出的一批调用：
//   - 只读工具并发执行（受 MaxParallel 限制），完全相同的只读调用只执行一次；
//   - 有副作用的工具按出现顺序串行执行；一旦有一个失败，后面有副作用的调用不再执行，返回“未执行”；
//   - 未知工具、参数不合法、超时、panic 都变成 IsError 结果，而不是让整个循环崩掉；
//   - 每个调用都恰好返回一个结果，顺序和调用顺序一致。
func (e Executor) Run(ctx context.Context, calls []Call) []Result {
	res := make([]Result, len(calls))
	type job struct {
		i    int
		args map[string]any
	}
	var readOnly, sideEffect []job
	for i, c := range calls {
		res[i].CallID = c.ID
		t, ok := e.Reg.tools[c.Name]
		if !ok {
			res[i].Content, res[i].IsError = fmt.Sprintf("未知工具 %q。可用工具：%s", c.Name, e.Reg.names()), true
			continue
		}
		args, err := t.Validate(c.Args)
		if err != nil {
			res[i].Content, res[i].IsError = err.Error()+"。请按工具定义修正参数后重试。", true
			continue
		}
		if t.ReadOnly {
			readOnly = append(readOnly, job{i, args})
		} else {
			sideEffect = append(sideEffect, job{i, args})
		}
	}

	// 只读：去重 + 并发
	groups := map[string][]int{}
	var order []string
	for _, j := range readOnly {
		key := calls[j.i].Name + string(canon(calls[j.i].Args))
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], j.i)
	}
	argsOf := map[int]map[string]any{}
	for _, j := range readOnly {
		argsOf[j.i] = j.args
	}
	limit := e.MaxParallel
	if limit <= 0 {
		limit = 8
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for _, key := range order {
		idx := groups[key]
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			first := idx[0]
			content, isErr := e.call(ctx, calls[first].Name, argsOf[first])
			for _, i := range idx {
				res[i].Content, res[i].IsError = content, isErr
			}
		}()
	}
	wg.Wait()

	// 有副作用：串行，失败即停
	var failed string
	for _, j := range sideEffect {
		c := calls[j.i]
		if failed != "" {
			res[j.i].Content, res[j.i].IsError = fmt.Sprintf("未执行：前面的 %s 调用失败了。", failed), true
			continue
		}
		res[j.i].Content, res[j.i].IsError = e.call(ctx, c.Name, j.args)
		if res[j.i].IsError {
			failed = c.Name
		}
	}
	return res
}

func (e Executor) call(ctx context.Context, name string, args map[string]any) (content string, isErr bool) {
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type out struct {
		s   string
		err error
	}
	ch := make(chan out, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				ch <- out{err: fmt.Errorf("工具内部错误：%v", p)}
			}
		}()
		s, err := e.Reg.funcs[name](ctx, args)
		ch <- out{s, err}
	}()
	select {
	case o := <-ch:
		if o.err != nil {
			return o.err.Error(), true
		}
		return o.s, false
	case <-ctx.Done():
		return fmt.Sprintf("超时：%s 在 %v 内没有返回。可以缩小范围后重试。", name, timeout), true
	}
}

// canon 把参数 JSON 规范化（键排序），让 {"a":1,"b":2} 和 {"b":2,"a":1} 被视为同一个调用。
func canon(raw json.RawMessage) []byte {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	b, _ := json.Marshal(v) // encoding/json 对 map 的键按字典序输出
	return b
}
