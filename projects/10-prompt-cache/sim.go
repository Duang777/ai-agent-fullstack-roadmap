package promptcache

import (
	"fmt"
	"time"
)

// Strategy 是多轮 Agent 对话里缓存断点的放法。
type Strategy int

const (
	NoCache        Strategy = iota
	AutoLast                // 自动缓存：断点跟着最后一块走
	TimestampFirst          // 自动缓存，但 system 开头写了“当前时间”
	SystemOnly              // 只在 system 末尾放一个断点
)

func (s Strategy) String() string {
	return [...]string{"不缓存", "自动缓存", "system 里放时间戳", "只缓存 system"}[s]
}

// AgentSession 模拟一个 tools(2000) + system(6000) 的 Agent 跑 turns 轮，
// 每轮追加 user(200) + assistant 工具调用(150) + 工具结果(800) + assistant(300)，相邻两轮间隔 gap。
func AgentSession(turns int, s Strategy, gap time.Duration) (units float64, hitRate float64) {
	c := NewCache(5*time.Minute, 1024, 20)
	start := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	var hist []Block
	read, total := 0, 0
	for t := 0; t < turns; t++ {
		now := start.Add(time.Duration(t) * gap)
		sys := "你是一个代码助手……（6000 token 的规则与示例）"
		if s == TimestampFirst {
			sys = "当前时间 " + now.Format("2006-01-02 15:04:05") + "。" + sys
		}
		blocks := []Block{
			{Kind: "tool", Text: "search/read/edit 定义", Tokens: 2000},
			{Kind: "system", Text: sys, Tokens: 6000, Breakpoint: s == SystemOnly},
		}
		hist = append(hist, Block{Kind: "user", Text: fmt.Sprintf("第 %d 个问题", t), Tokens: 200})
		blocks = append(blocks, hist...)
		var u Usage
		if s == NoCache {
			u = Usage{Input: sumTokens(blocks)}
		} else {
			u, _ = c.Anthropic(blocks, s != SystemOnly, now)
		}
		units += Anthropic5m.Units(u)
		read += u.Read
		total += u.Total()
		hist = append(hist,
			Block{Kind: "assistant", Text: fmt.Sprintf("调用 search #%d", t), Tokens: 150},
			Block{Kind: "tool_result", Text: fmt.Sprintf("结果 #%d", t), Tokens: 800},
			Block{Kind: "assistant", Text: fmt.Sprintf("回答 #%d", t), Tokens: 300},
		)
	}
	return units, float64(read) / float64(total)
}

// TTLCost 比较 5 分钟和 1 小时 TTL：用户每次回复的间隔是 gaps（分钟）。前缀 tokens 不变。
func TTLCost(prefix int, gaps []int, ttl time.Duration, r Rates) float64 {
	c := NewCache(ttl, 1024, 20)
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	blocks := []Block{{Kind: "system", Text: "长文档", Tokens: prefix, Breakpoint: true}, {Kind: "user", Text: "q", Tokens: 0}}
	u, _ := c.Anthropic(blocks, false, now)
	units := r.Units(u)
	for _, g := range gaps {
		now = now.Add(time.Duration(g) * time.Minute)
		u, _ = c.Anthropic(blocks, false, now)
		units += r.Units(u)
	}
	return units
}
