package multiagent

import (
	"context"
	"encoding/json"
	"fmt"
)

type depthKey struct{}

func depth(ctx context.Context) int { d, _ := ctx.Value(depthKey{}).(int); return d }

// AsTool 把一个 agent 包装成工具（Supervisor 模式 / OpenAI 的 Agent.as_tool）。
// 子 agent 在全新的上下文里运行：只看到主管写的任务描述，看不到主管的历史。
// 它不能转交，跑完只把最终回答交回去；主管始终掌控对话。
func (rt *Runtime) AsTool(name string) Tool {
	return func(ctx context.Context, args string) (string, error) {
		var in struct {
			Task string `json:"task"`
		}
		if err := json.Unmarshal([]byte(args), &in); err != nil || in.Task == "" {
			return "", fmt.Errorf("参数要是 {\"task\": \"...\"}")
		}
		d := depth(ctx) + 1
		if rt.MaxDepth > 0 && d > rt.MaxDepth {
			return "", ErrTooDeep
		}
		ctx = context.WithValue(ctx, depthKey{}, d)

		sub := *rt.Agents[name]
		sub.Handoffs = nil
		h := []Message{{Role: User, Content: in.Task}}
		steps := rt.MaxSteps
		for i := 0; steps <= 0 || i < steps; i++ {
			msg, u, err := rt.Model.Next(ctx, &sub, rt.tools(&sub), h)
			if err != nil {
				return "", err
			}
			if err := rt.Meter.Add(sub.Name, u); err != nil {
				return "", err
			}
			msg.Role, msg.Agent = Assistant, sub.Name
			h = append(h, msg)
			if len(msg.Calls) == 0 {
				return msg.Content, nil
			}
			h = append(h, rt.runTools(ctx, &sub, msg.Calls)...)
		}
		return "", ErrMaxSteps
	}
}
