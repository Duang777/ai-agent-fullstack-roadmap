package goeco

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// MemoryStore 是最简单的 CheckPointStore：Eino 只要求 Get/Set 两个方法，
// 值是框架序列化好的字节。生产环境换成 Redis 或数据库，接口不变。
type MemoryStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{m: map[string][]byte{}} }

func (s *MemoryStore) Get(_ context.Context, id string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[id]
	return v, ok, nil
}

func (s *MemoryStore) Set(_ context.Context, id string, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = append([]byte(nil), b...)
	return nil
}

// Len 返回保存的 checkpoint 个数。
func (s *MemoryStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// AgentOptions 收拢构造 ChatModelAgent 时本课用到的几个开关。
type AgentOptions struct {
	MaxIterations int
	Streaming     bool
	// UnknownTool 不为空时，模型调用不存在的工具会交给它处理，而不是让整次运行报错。
	UnknownTool func(ctx context.Context, name, input string) (string, error)
}

// NewEinoRunner 用 ADK 的 ChatModelAgent + Runner 搭一个 ReAct agent。
func NewEinoRunner(ctx context.Context, m model.ToolCallingChatModel, store adk.CheckPointStore, opts AgentOptions, tools ...tool.BaseTool) (*adk.Runner, error) {
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:          "support",
		Description:   "处理客服工单的 agent",
		Instruction:   "你是客服 agent，需要时调用工具。",
		Model:         m,
		MaxIterations: opts.MaxIterations,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
			Tools:               tools,
			UnknownToolsHandler: opts.UnknownTool,
		}},
	})
	if err != nil {
		return nil, err
	}
	return adk.NewRunner(ctx, adk.RunnerConfig{
		Agent:           agent,
		EnableStreaming: opts.Streaming,
		CheckPointStore: store,
	}), nil
}

// RunResult 是一次 Runner 运行（Query 或 Resume）收集到的结果。
type RunResult struct {
	Final       string            // 最后一条 assistant 文本
	ToolOutputs map[string]string // 工具名 → 工具输出
	Interrupts  []*adk.InterruptCtx
	Streamed    int // 以流的形式到达的事件数
}

// Collect 消费 Runner 返回的事件迭代器。流式事件要自己读完 MessageStream，
// 这里用 adk.GetMessage 统一处理：它会把流拼成一条完整消息。
func Collect(iter *adk.AsyncIterator[*adk.AgentEvent]) (*RunResult, error) {
	res := &RunResult{ToolOutputs: map[string]string{}}
	for {
		ev, ok := iter.Next()
		if !ok {
			return res, nil
		}
		if ev.Err != nil {
			return res, ev.Err
		}
		if ev.Action != nil && ev.Action.Interrupted != nil {
			for _, ic := range ev.Action.Interrupted.InterruptContexts {
				if ic.IsRootCause {
					res.Interrupts = append(res.Interrupts, ic)
				}
			}
		}
		if ev.Output == nil || ev.Output.MessageOutput == nil {
			continue
		}
		mv := ev.Output.MessageOutput
		if mv.IsStreaming {
			res.Streamed++
		}
		msg, _, err := adk.GetMessage(ev)
		if err != nil {
			return res, err
		}
		switch mv.Role {
		case schema.Tool:
			res.ToolOutputs[mv.ToolName] = msg.Content
		case schema.Assistant:
			if len(msg.ToolCalls) == 0 {
				res.Final = msg.Content
			}
		}
	}
}

// ---------- 需要人工审批的退款工具 ----------

// RefundArgs 是退款工具的参数。
type RefundArgs struct {
	OrderID string  `json:"order_id"`
	Amount  float64 `json:"amount"`
}

// RefundApproval 是 resume 时人传回来的决定。
type RefundApproval struct {
	Approved bool   `json:"approved"`
	Note     string `json:"note,omitempty"`
}

// RefundPending 是中断时交给人看的信息，也作为状态保存在 checkpoint 里。
type RefundPending struct {
	OrderID string  `json:"order_id"`
	Amount  float64 `json:"amount"`
}

func init() {
	// checkpoint 用 gob 序列化，中断状态里放自定义类型时必须先注册。
	schema.RegisterName[*RefundPending]("goeco_refund_pending")
}

// RefundTool 超过阈值的退款先中断等人批准。它示范 Eino 中断/恢复的三件套：
//   - tool.StatefulInterrupt：中断并保存状态
//   - tool.GetInterruptState：恢复时判断“我是不是之前中断过”
//   - tool.GetResumeContext：判断“这次恢复是不是冲着我来的”，并取回人给的数据
type RefundTool struct {
	Threshold float64
	mu        sync.Mutex
	executed  []RefundArgs
}

func (t *RefundTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "refund",
		Desc: "给订单退款",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"order_id": {Type: schema.String, Required: true, Desc: "订单号"},
			"amount":   {Type: schema.Number, Required: true, Desc: "退款金额"},
		}),
	}, nil
}

func (t *RefundTool) InvokableRun(ctx context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	var args RefundArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("refund: bad arguments: %w", err)
	}

	wasInterrupted, _, pending := tool.GetInterruptState[*RefundPending](ctx)
	if !wasInterrupted {
		if args.Amount <= t.Threshold {
			return t.execute(args), nil
		}
		p := &RefundPending{OrderID: args.OrderID, Amount: args.Amount}
		return "", tool.StatefulInterrupt(ctx, p, p)
	}

	isTarget, hasData, decision := tool.GetResumeContext[*RefundApproval](ctx)
	if !isTarget {
		// 同一批里别的中断被恢复了，但不是我：必须原样再中断一次，否则状态会丢。
		return "", tool.StatefulInterrupt(ctx, pending, pending)
	}
	if !hasData || decision == nil {
		return "", errors.New("refund: resumed without a decision")
	}
	if !decision.Approved {
		return "退款被拒绝：" + decision.Note, nil
	}
	return t.execute(RefundArgs{OrderID: pending.OrderID, Amount: pending.Amount}), nil
}

func (t *RefundTool) execute(a RefundArgs) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.executed = append(t.executed, a)
	return fmt.Sprintf("已退款 %.2f 元，订单 %s", a.Amount, a.OrderID)
}

// Executed 返回真正执行过的退款。
func (t *RefundTool) Executed() []RefundArgs {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]RefundArgs(nil), t.executed...)
}
