package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ToolFunc 执行一个工具。返回的 error 会作为 is_error 的结果交给模型，而不是让整个循环失败：
// 模型看到“参数错了”通常会自己改正重试。
type ToolFunc func(ctx context.Context, args json.RawMessage) (string, error)

var ErrMaxSteps = errors.New("llm: tool loop exceeded max steps")

// RunTools 是最小的 Agent 循环，对三家完全一样：
//
//	调模型 → 有工具调用就执行 → 把调用和结果追加进历史 → 再调模型 → 直到 Stop != tool_use
//
// 返回最后一次响应和完整历史。第 03 模块会在这个骨架上加并行执行、超时、审批、检查点。
func RunTools(ctx context.Context, p Provider, req Request, tools map[string]ToolFunc, maxSteps int) (*Response, []Message, error) {
	history := append([]Message(nil), req.Messages...)
	for step := 0; step < maxSteps; step++ {
		r := req
		r.Messages = history
		resp, err := p.Generate(ctx, &r)
		if err != nil {
			return nil, history, err
		}
		// 助手消息原样进历史：推理块、签名都在 Raw 里，下一轮会原样发回去
		history = append(history, resp.Message)
		switch resp.Stop {
		case StopToolUse:
		case StopPause: // 服务端工具循环暂停：原样再发一次即可继续
			continue
		default:
			return resp, history, nil
		}
		calls := resp.Message.ToolCalls()
		if len(calls) == 0 {
			return resp, history, fmt.Errorf("llm: stop=tool_use but no tool calls")
		}
		results := Message{Role: RoleUser}
		for _, c := range calls { // 每个调用都必须有结果，哪怕是错误
			res := Part{Type: PartToolResult, CallID: c.CallID, Name: c.Name}
			fn, ok := tools[c.Name]
			if !ok {
				res.Result, res.IsError = "unknown tool: "+c.Name, true
			} else if out, err := fn(ctx, c.Args); err != nil {
				res.Result, res.IsError = err.Error(), true
			} else {
				res.Result = out
			}
			results.Parts = append(results.Parts, res)
		}
		history = append(history, results)
	}
	return nil, history, ErrMaxSteps
}
