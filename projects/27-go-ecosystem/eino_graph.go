package goeco

import (
	"context"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
)

// Ticket 是工单分流图的输入。jsonschema 标签会进入工具的参数 schema。
type Ticket struct {
	ID   string `json:"id" jsonschema:"description=工单编号"`
	Text string `json:"text" jsonschema:"description=用户的原话"`
}

// Triage 是分流结果。
type Triage struct {
	TicketID string   `json:"ticket_id"`
	Queue    string   `json:"queue"`
	Priority string   `json:"priority"`
	Path     []string `json:"path"`
}

// triageState 是图内的局部状态，用来记录走过的节点。
type triageState struct {
	path []string
}

// classified 在 classify 节点和分支之间传递。
type classified struct {
	Ticket Ticket
	Kind   string
}

// NewTriageGraph 构建一个确定性的工单分流图：
//
//	START → classify →(branch)→ refund | bug | human → END
//
// 这是 Eino 文档推荐的分工：流程固定的部分用 compose 写成 Graph，
// 再包装成工具交给 agent，由 LLM 决定什么时候调用。
func NewTriageGraph(ctx context.Context) (compose.Runnable[Ticket, Triage], error) {
	g := compose.NewGraph[Ticket, Triage](compose.WithGenLocalState(func(context.Context) *triageState {
		return &triageState{}
	}))

	visit := func(name string) compose.GraphAddNodeOpt {
		return compose.WithStatePreHandler(func(_ context.Context, in classified, s *triageState) (classified, error) {
			s.path = append(s.path, name)
			return in, nil
		})
	}

	_ = g.AddLambdaNode("classify", compose.InvokableLambda(func(_ context.Context, t Ticket) (classified, error) {
		text := strings.ToLower(t.Text)
		kind := "other"
		switch {
		case strings.Contains(text, "退款") || strings.Contains(text, "refund"):
			kind = "refund"
		case strings.Contains(text, "报错") || strings.Contains(text, "崩溃") || strings.Contains(text, "bug"):
			kind = "bug"
		}
		return classified{Ticket: t, Kind: kind}, nil
	}))

	route := func(queue, priority string) *compose.Lambda {
		return compose.InvokableLambda(func(ctx context.Context, c classified) (Triage, error) {
			var path []string
			_ = compose.ProcessState(ctx, func(_ context.Context, s *triageState) error {
				path = append([]string{"classify"}, s.path...)
				return nil
			})
			return Triage{TicketID: c.Ticket.ID, Queue: queue, Priority: priority, Path: path}, nil
		})
	}
	_ = g.AddLambdaNode("refund", route("billing", "P2"), visit("refund"))
	_ = g.AddLambdaNode("bug", route("engineering", "P1"), visit("bug"))
	_ = g.AddLambdaNode("human", route("frontline", "P3"), visit("human"))

	_ = g.AddEdge(compose.START, "classify")
	_ = g.AddBranch("classify", compose.NewGraphBranch(func(_ context.Context, c classified) (string, error) {
		switch c.Kind {
		case "refund":
			return "refund", nil
		case "bug":
			return "bug", nil
		}
		return "human", nil
	}, map[string]bool{"refund": true, "bug": true, "human": true}))
	for _, n := range []string{"refund", "bug", "human"} {
		_ = g.AddEdge(n, compose.END)
	}
	// 构图时第一个出错的 AddXxx 会把错误记在图上，之后的 AddXxx 和 Compile 都返回它，
	// 所以上面可以不逐个检查，统一看 Compile 的返回值。
	return g.Compile(ctx, compose.WithGraphName("triage"))
}

// TriageTool 把编译好的图包装成 agent 可调用的工具。参数 schema 由 Ticket
// 结构体推断，工具输出是 Triage 的 JSON。
func TriageTool(r compose.Runnable[Ticket, Triage]) (tool.InvokableTool, error) {
	return utils.InferTool("triage_ticket", "把工单分到对应队列并给出优先级",
		func(ctx context.Context, t Ticket) (Triage, error) {
			return r.Invoke(ctx, t)
		})
}
