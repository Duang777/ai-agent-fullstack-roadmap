package streamproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"
)

type ToolCall struct {
	ID   string
	Name string
	Args json.RawMessage // 先不解析，交给具体工具
}

type ToolResult struct {
	ID     string
	Output string
	Err    error
}

// toolTimeout 是单个工具的超时；测试里会调小。
var toolTimeout = 20 * time.Second

// execTool 执行单个工具；做成变量方便测试替换。
var execTool = func(ctx context.Context, c ToolCall) (string, error) {
	return "", fmt.Errorf("unknown tool %q", c.Name)
}

// runTools 并行执行工具调用：最多 4 个并发，单工具超时，单个失败不影响其他。
func runTools(ctx context.Context, calls []ToolCall) ([]ToolResult, error) {
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	results := make([]ToolResult, len(calls)) // 每个 goroutine 写自己的下标，无需加锁

	for i, c := range calls {
		g.Go(func() error { // Go 1.22+ 循环变量每轮独立
			tctx, cancel := context.WithTimeout(ctx, toolTimeout)
			defer cancel()
			out, err := execTool(tctx, c)
			results[i] = ToolResult{ID: c.ID, Output: out, Err: err}
			return nil // 工具失败作为观察回传给模型
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}
