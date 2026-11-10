// Package graph 是 LangGraph 核心运行时的一个最小 Go 实现，用来看清它的几个关键机制：
// 状态 = 一组通道（channel），每个通道可以有 reducer；节点返回部分更新；
// 执行按 Pregel 风格的 super-step 推进，同一步里的节点并行、看到同一份快照、写入在步末统一合并；
// 每个 super-step 结束写一个 checkpoint，按 thread 组织；节点失败时成功节点的写入作为 pending writes 保存；
// interrupt 通过“抛出 + 恢复时整个节点重跑”实现，恢复值按序号匹配。
package graph

import (
	"errors"
	"fmt"
	"sort"
)

const (
	START = "__start__"
	END   = "__end__"
)

// State 是一份状态快照：通道名 → 值。所有值必须能 JSON 序列化（checkpoint 要落盘）。
type State map[string]any

// Reducer 把节点的更新合并进旧值。nil 表示 LastValue：直接覆盖，且同一步只允许一个写入。
type Reducer func(old, update any) (any, error)

// Append 是最常用的 reducer：列表追加（对应 Python 里的 operator.add）。
func Append(old, update any) (any, error) {
	var out []any
	if old != nil {
		o, ok := old.([]any)
		if !ok {
			return nil, fmt.Errorf("append：旧值不是列表（%T）", old)
		}
		out = append(out, o...)
	}
	if update == nil { // 空更新（JSON 里的 null）什么都不追加
		return out, nil
	}
	if u, ok := update.([]any); ok {
		return append(out, u...), nil
	}
	return append(out, update), nil
}

const overwriteKey = "__overwrite__"

// Overwrite 绕过 reducer 直接替换（对应 LangGraph 的 Overwrite）。
// 用一个特殊键表示，而不是 Go 类型，这样经过 JSON 序列化后依然认得出来。
func Overwrite(v any) any { return map[string]any{overwriteKey: v} }

func asOverwrite(v any) (any, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return nil, false
	}
	x, ok := m[overwriteKey]
	return x, ok
}

// Target 是下一步要运行的节点。Arg 不为 nil 时就是 Send：这个节点实例只看到 Arg，不看共享状态。
type Target struct {
	Node string `json:"node"`
	Arg  State  `json:"arg,omitempty"`
}

func To(node string) Target                { return Target{Node: node} }
func SendTo(node string, arg State) Target { return Target{Node: node, Arg: arg} }

// Command 让节点在一次返回里同时更新状态和指定下一跳。
type Command struct {
	Update State
	Goto   []Target
}

// NodeFunc 返回 nil、State（部分更新）或 Command。
type NodeFunc func(rt *Runtime, s State) (any, error)

// Router 是条件边：读更新后的状态，返回下一步的目标（节点名、END 或 Send）。
type Router func(s State) []Target

var (
	ErrUnknownNode    = errors.New("边引用了不存在的节点")
	ErrNoEntry        = errors.New("START 没有出边")
	ErrOrphan         = errors.New("有节点从 START 不可达")
	ErrDuplicateNode  = errors.New("节点重名或使用了保留名")
	ErrUnknownChannel = errors.New("更新了状态里没有声明的通道")
)

type node struct {
	fn    NodeFunc
	gotos []string // 这个节点可能用 Command 跳去的地方，只用于编译期检查
}

type Builder struct {
	channels    map[string]Reducer
	nodes       map[string]node
	edges       map[string][]string
	routers     map[string]Router
	routerDests map[string][]string
	err         error
}

func New() *Builder {
	return &Builder{channels: map[string]Reducer{}, nodes: map[string]node{}, edges: map[string][]string{},
		routers: map[string]Router{}, routerDests: map[string][]string{}}
}

// Channel 声明状态里的一个键。r 为 nil 时是 LastValue。
func (b *Builder) Channel(name string, r Reducer) *Builder {
	b.channels[name] = r
	return b
}

func (b *Builder) AddNode(name string, fn NodeFunc, gotos ...string) *Builder {
	if _, dup := b.nodes[name]; dup || name == START || name == END || name == "" {
		b.err = errors.Join(b.err, fmt.Errorf("%w：%q", ErrDuplicateNode, name))
	}
	b.nodes[name] = node{fn: fn, gotos: gotos}
	return b
}

func (b *Builder) AddEdge(from, to string) *Builder {
	b.edges[from] = append(b.edges[from], to)
	return b
}

// AddConditionalEdges 的 dests 声明路由可能去的节点，只用于编译期检查（LangGraph 用它画图）。
// 不声明时，编译器保守地认为它可能去任何节点。
func (b *Builder) AddConditionalEdges(from string, r Router, dests ...string) *Builder {
	b.routers[from] = r
	b.routerDests[from] = dests
	return b
}

// Graph 是编译后的图，只读，可以被多个 thread 并发使用。
type Graph struct {
	b  *Builder
	cp Checkpointer
}

// Compile 检查结构：边的两端存在、START 有出边、没有孤立节点。
func (b *Builder) Compile(cp Checkpointer) (*Graph, error) {
	if b.err != nil {
		return nil, b.err
	}
	exists := func(n string) bool { _, ok := b.nodes[n]; return ok }
	for from, tos := range b.edges {
		if from != START && !exists(from) {
			return nil, fmt.Errorf("%w：%s", ErrUnknownNode, from)
		}
		for _, to := range tos {
			if to != END && !exists(to) {
				return nil, fmt.Errorf("%w：%s → %s", ErrUnknownNode, from, to)
			}
		}
	}
	for from, dests := range b.routerDests {
		if from != START && !exists(from) {
			return nil, fmt.Errorf("%w：%s", ErrUnknownNode, from)
		}
		for _, d := range dests {
			if d != END && !exists(d) {
				return nil, fmt.Errorf("%w：%s ⇢ %s", ErrUnknownNode, from, d)
			}
		}
	}
	for name, n := range b.nodes {
		for _, d := range n.gotos {
			if d != END && !exists(d) {
				return nil, fmt.Errorf("%w：%s 的 Command 目标 %s", ErrUnknownNode, name, d)
			}
		}
	}
	if len(b.edges[START]) == 0 && b.routers[START] == nil {
		return nil, ErrNoEntry
	}

	// 从 START 出发做一次可达性遍历
	seen := map[string]bool{START: true}
	queue := []string{START}
	all := b.sortedNodes()
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		var next []string
		next = append(next, b.edges[cur]...)
		if _, ok := b.routers[cur]; ok {
			if d := b.routerDests[cur]; len(d) > 0 {
				next = append(next, d...)
			} else {
				next = append(next, all...)
			}
		}
		next = append(next, b.nodes[cur].gotos...)
		for _, n := range next {
			if n != END && !seen[n] {
				seen[n] = true
				queue = append(queue, n)
			}
		}
	}
	for _, n := range all {
		if !seen[n] {
			return nil, fmt.Errorf("%w：%s", ErrOrphan, n)
		}
	}
	return &Graph{b: b, cp: cp}, nil
}

func (b *Builder) sortedNodes() []string {
	out := make([]string, 0, len(b.nodes))
	for n := range b.nodes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
