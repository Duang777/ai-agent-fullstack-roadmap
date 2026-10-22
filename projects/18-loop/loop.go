package loop

import (
	"context"
	"errors"
	"time"
)

type Role string

const (
	User      Role = "user"
	Assistant Role = "assistant"
	ToolRole  Role = "tool"
)

type ToolCall struct{ ID, Name, Args string }

type Message struct {
	Role    Role
	Content string
	Calls   []ToolCall
	CallID  string
	IsError bool
}

type Response struct {
	Message Message
	Stop    Stop
	Usage   Usage
}

type CallOpts struct {
	NoTools bool // 收尾调用：禁止再调工具，只能给回答
}

type Model interface {
	Next(ctx context.Context, h []Message, o CallOpts) (Response, error)
}

type Tool func(ctx context.Context, args string) (string, error)

type Config struct {
	Model            Model
	Tools            map[string]Tool
	Price            Price
	Budget           Budget
	Retry            Retry
	ToolTimeout      time.Duration // 单个工具调用的超时，0 不限
	MaxContinuations int           // 被截断后最多续写几次
	Finalize         bool          // 预算用完时，再做一次禁用工具的调用让模型收尾
	Now              func() time.Time
}

var (
	ErrRefused     = errors.New("模型拒绝回答")
	ErrContextFull = errors.New("上下文窗口已满，需要压缩（1.6 课）")
	ErrTruncated   = errors.New("输出被截断且无法续写")
	ErrUnknownStop = errors.New("未知的停止原因")
)

// Result 不管成功失败都返回：停在哪、为什么、花了多少，都是排查和计费要用的。
type Result struct {
	Answer  string
	Reason  string // done / finalized / budget:<维度> / refused / ...
	Spent   Spent
	History []Message
}

func Run(ctx context.Context, cfg Config, task string) (Result, error) {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	sp := Spent{Start: now()}
	h := []Message{{Role: User, Content: task}}
	continuations := 0
	res := func(reason, answer string) Result {
		return Result{Answer: answer, Reason: reason, Spent: sp, History: h}
	}

	call := func(o CallOpts) (Response, error) {
		var r Response
		err := cfg.Retry.Do(ctx, func(ctx context.Context) error {
			var e error
			r, e = cfg.Model.Next(ctx, h, o)
			return e
		})
		if err == nil {
			// 只有成功的调用计入；失败的请求一般不计费
			sp.Steps++
			sp.Usage.Add(r.Usage)
			sp.Cost += cfg.Price.Cost(r.Usage)
		}
		return r, err
	}

	for {
		if err := ctx.Err(); err != nil {
			return res("canceled", ""), err
		}
		if err := cfg.Budget.Check(sp, now()); err != nil {
			var be *BudgetError
			errors.As(err, &be)
			if cfg.Finalize {
				// 收尾：告诉模型预算到了，禁用工具，让它基于已有信息给出回答
				h = append(h, Message{Role: User, Content: "预算已用完，不能再调用工具。请根据目前已有的信息给出最终回答，并说明哪些部分没能完成。"})
				r, e := call(CallOpts{NoTools: true})
				if e == nil && r.Stop == StopDone {
					h = append(h, r.Message)
					return res("finalized:"+be.Dim, r.Message.Content), err
				}
			}
			return res("budget:"+be.Dim, ""), err
		}

		r, err := call(CallOpts{})
		if err != nil {
			return res("error", ""), err
		}

		switch r.Stop {
		case StopDone:
			h = append(h, r.Message)
			return res("done", r.Message.Content), nil

		case StopToolUse:
			h = append(h, r.Message)
			for _, c := range r.Message.Calls {
				sp.ToolCalls++
				h = append(h, runTool(ctx, cfg, c))
			}

		case StopPaused:
			// 原样发回，不加 user 消息，服务端会接着跑
			h = append(h, r.Message)

		case StopTruncated:
			// 带着半截工具调用的截断没法续，参数 JSON 不完整
			if len(r.Message.Calls) > 0 || continuations >= cfg.MaxContinuations {
				return res("truncated", r.Message.Content), ErrTruncated
			}
			continuations++
			h = append(h, r.Message, Message{Role: User, Content: "你的回答被截断了，请从断开的地方接着写，不要重复已经写过的内容。"})

		case StopRefused:
			return res("refused", ""), ErrRefused
		case StopContextFull:
			return res("context_full", r.Message.Content), ErrContextFull
		default:
			return res("unknown", ""), ErrUnknownStop
		}
	}
}

func runTool(ctx context.Context, cfg Config, c ToolCall) Message {
	t, ok := cfg.Tools[c.Name]
	if !ok {
		return Message{Role: ToolRole, CallID: c.ID, IsError: true, Content: "没有这个工具：" + c.Name}
	}
	tctx := ctx
	if cfg.ToolTimeout > 0 {
		var cancel context.CancelFunc
		tctx, cancel = context.WithTimeout(ctx, cfg.ToolTimeout)
		defer cancel()
	}
	out, err := t(tctx, c.Args)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return Message{Role: ToolRole, CallID: c.ID, IsError: true, Content: "工具超时（" + cfg.ToolTimeout.String() + "），可以缩小范围再试"}
		}
		return Message{Role: ToolRole, CallID: c.ID, IsError: true, Content: err.Error()}
	}
	return Message{Role: ToolRole, CallID: c.ID, Content: out}
}
