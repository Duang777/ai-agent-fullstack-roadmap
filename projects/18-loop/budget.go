package loop

import (
	"fmt"
	"time"
)

type Usage struct {
	Input, CachedInput, Output int // Input 不含 CachedInput
}

func (u *Usage) Add(o Usage) {
	u.Input += o.Input
	u.CachedInput += o.CachedInput
	u.Output += o.Output
}

func (u Usage) Total() int { return u.Input + u.CachedInput + u.Output }

// Price 每百万 token 的价格，单位微美元（1 美元 = 1e6），用整数避免浮点误差累积。
type Price struct {
	Input, CachedInput, Output int64
}

// Cost 向上取整：宁可多算一点，预算才不会被悄悄突破。
func (p Price) Cost(u Usage) int64 {
	n := int64(u.Input)*p.Input + int64(u.CachedInput)*p.CachedInput + int64(u.Output)*p.Output
	return (n + 999_999) / 1_000_000
}

// Budget 的每一项为 0 表示不限制。
type Budget struct {
	MaxSteps     int           // 模型调用次数
	MaxToolCalls int           // 工具调用次数
	MaxTokens    int           // 累计 token
	MaxCost      int64         // 累计费用，微美元
	MaxWall      time.Duration // 墙钟时间
}

type Spent struct {
	Steps, ToolCalls int
	Usage            Usage
	Cost             int64
	Start            time.Time
}

type BudgetError struct {
	Dim         string // steps / tool_calls / tokens / cost / wall
	Used, Limit int64
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("预算用完：%s 已用 %d，上限 %d", e.Dim, e.Used, e.Limit)
}

// Check 在每次调用模型之前做：已经超了就不要再发请求。
func (b Budget) Check(s Spent, now time.Time) error {
	switch {
	case b.MaxSteps > 0 && s.Steps >= b.MaxSteps:
		return &BudgetError{"steps", int64(s.Steps), int64(b.MaxSteps)}
	case b.MaxToolCalls > 0 && s.ToolCalls >= b.MaxToolCalls:
		return &BudgetError{"tool_calls", int64(s.ToolCalls), int64(b.MaxToolCalls)}
	case b.MaxTokens > 0 && s.Usage.Total() >= b.MaxTokens:
		return &BudgetError{"tokens", int64(s.Usage.Total()), int64(b.MaxTokens)}
	case b.MaxCost > 0 && s.Cost >= b.MaxCost:
		return &BudgetError{"cost", s.Cost, b.MaxCost}
	case b.MaxWall > 0 && now.Sub(s.Start) >= b.MaxWall:
		return &BudgetError{"wall", int64(now.Sub(s.Start) / time.Millisecond), int64(b.MaxWall / time.Millisecond)}
	}
	return nil
}
