package goeco

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/ai/tool"
	"github.com/firebase/genkit/go/core"
	"github.com/firebase/genkit/go/genkit"
)

// GenkitScript 是 Genkit 版的假模型：每次调用返回脚本里的下一条消息，
// 并记录请求，测试用它检查 Genkit 交给模型的消息和工具定义。
type GenkitScript struct {
	mu       sync.Mutex
	replies  []*ai.Message
	next     int
	requests []*ai.ModelRequest
}

// Requests 返回模型收到的每个请求。
func (s *GenkitScript) Requests() []*ai.ModelRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*ai.ModelRequest(nil), s.requests...)
}

// NewGenkit 初始化一个不带任何插件的 Genkit 实例。真实项目在这里加
// genkit.WithPlugins(&googlegenai.GoogleAI{}) 之类的模型插件。
func NewGenkit(ctx context.Context) *genkit.Genkit {
	return genkit.Init(ctx)
}

// DefineScriptedModel 用 genkit.DefineModel 注册一个按脚本回复的模型。
// 模型在 Genkit 里就是一个注册过的 action，名字是 "provider/model"。
func DefineScriptedModel(g *genkit.Genkit, name string, replies ...*ai.Message) (ai.Model, *GenkitScript) {
	s := &GenkitScript{replies: replies}
	m := genkit.DefineModel(g, name, &ai.ModelOptions{
		Supports: &ai.ModelSupports{Multiturn: true, Tools: true, SystemRole: true},
	}, func(ctx context.Context, req *ai.ModelRequest, cb ai.ModelStreamCallback) (*ai.ModelResponse, error) {
		s.mu.Lock()
		s.requests = append(s.requests, req)
		if s.next >= len(s.replies) {
			s.mu.Unlock()
			return nil, errors.New("genkit script exhausted")
		}
		msg := s.replies[s.next]
		s.next++
		s.mu.Unlock()
		if cb != nil {
			if err := cb(ctx, &ai.ModelResponseChunk{Role: ai.RoleModel, Content: msg.Content}); err != nil {
				return nil, err
			}
		}
		return &ai.ModelResponse{Request: req, Message: msg, FinishReason: ai.FinishReasonStop}, nil
	})
	return m, s
}

// GenkitCall 构造一条请求调用工具的模型消息。
func GenkitCall(ref, name string, input map[string]any) *ai.Message {
	return ai.NewModelMessage(ai.NewToolRequestPart(&ai.ToolRequest{Ref: ref, Name: name, Input: input}))
}

// ---------- 工具 ----------

// StockQuery 是库存查询工具的输入。
type StockQuery struct {
	SKU string `json:"sku" jsonschema:"description=商品编码"`
}

// ErrNoSuchSKU 表示查无此商品：模型可以换个编码再试，所以用 tool.Fail 交还给模型。
var ErrNoSuchSKU = errors.New("no such sku")

// ErrInventoryDown 表示库存服务挂了：模型无能为力，直接中止整个循环。
var ErrInventoryDown = errors.New("inventory service down")

// DefineStockTool 注册库存查询工具。它示范 Genkit 对工具错误的两种处理：
// tool.Fail 包装的错误作为 {"error": ...} 交给模型，循环继续；
// 其他错误直接让 Generate 失败。
func DefineStockTool(g *genkit.Genkit, stock map[string]int, down bool) ai.Tool {
	return genkit.DefineTool(g, "check_stock", "查询商品库存",
		func(ctx *ai.ToolContext, in StockQuery) (int, error) {
			if down {
				return 0, ErrInventoryDown
			}
			n, ok := stock[in.SKU]
			if !ok {
				return 0, tool.Fail(ctx, fmt.Errorf("%w: %s", ErrNoSuchSKU, in.SKU))
			}
			return n, nil
		})
}

// TransferInput 是转账工具的输入。
type TransferInput struct {
	To     string  `json:"to"`
	Amount float64 `json:"amount"`
}

// TransferApproval 是转账恢复时人给的答复。
type TransferApproval struct {
	Approved bool `json:"approved"`
}

// TransferHold 是中断时交给调用方的数据。必须能序列化成 JSON 对象。
type TransferHold struct {
	Reason string  `json:"reason"`
	Amount float64 `json:"amount"`
}

// Transfers 记录真正执行过的转账。
type Transfers struct {
	mu   sync.Mutex
	done []TransferInput
}

func (t *Transfers) add(in TransferInput) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.done = append(t.done, in)
}

// Done 返回执行过的转账。
func (t *Transfers) Done() []TransferInput {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]TransferInput(nil), t.done...)
}

// DefineTransferTool 注册一个可恢复工具：金额超过 limit 时用 tool.Interrupt
// 暂停生成。第三个参数 approval 第一次调用时为 nil，恢复时是调用方给的答复。
func DefineTransferTool(g *genkit.Genkit, limit float64, log *Transfers) *ai.ResumableToolAction[TransferInput, string, TransferApproval] {
	return genkit.DefineResumableTool(g, "transfer", "转账",
		func(ctx context.Context, in TransferInput, approval *TransferApproval) (string, error) {
			if approval == nil && in.Amount > limit {
				return "", tool.Interrupt(ctx, TransferHold{Reason: "over_limit", Amount: in.Amount})
			}
			if approval != nil && !approval.Approved {
				return "转账已取消", nil
			}
			log.add(in)
			return fmt.Sprintf("已向 %s 转账 %.0f", in.To, in.Amount), nil
		})
}

// ---------- flow ----------

// DefineAskFlow 把一次带工具的生成包成 flow。flow 是 Genkit 的部署单元：
// 有类型的输入输出、自动 trace，能被 Dev UI 调用，也能挂成 HTTP handler。
func DefineAskFlow(g *genkit.Genkit, model ai.Model, tools ...ai.ToolRef) *core.Flow[string, string, struct{}] {
	return genkit.DefineFlow(g, "ask", func(ctx context.Context, question string) (string, error) {
		// genkit.Run 在 flow 的 trace 里记一个子步骤。
		q, err := genkit.Run(ctx, "normalize", func() (string, error) {
			return strings.TrimSpace(question), nil
		})
		if err != nil {
			return "", err
		}
		resp, err := genkit.Generate(ctx, g,
			ai.WithModel(model),
			ai.WithSystem("你是库存助手"),
			ai.WithPrompt(q),
			ai.WithTools(tools...),
		)
		if err != nil {
			return "", err
		}
		return resp.Text(), nil
	})
}
