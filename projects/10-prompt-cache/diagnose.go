package promptcache

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Divergence 描述两次请求的前缀从哪里开始不同（类似官方的 cache diagnostics）。
type Divergence struct {
	Index  int // -1 表示 next 完整包含 prev（只在末尾追加）
	Reason string
}

func Diagnose(prev, next []Block) Divergence {
	for i := 0; i < len(prev) && i < len(next); i++ {
		a, b := prev[i], next[i]
		if a.Kind == b.Kind && a.Text == b.Text {
			continue
		}
		for j := i + 1; j < len(prev); j++ {
			if prev[j].Kind == b.Kind && prev[j].Text == b.Text {
				return Divergence{i, fmt.Sprintf("第 %d 块被删除或顺序变了", i)}
			}
		}
		if a.Kind != b.Kind {
			return Divergence{i, fmt.Sprintf("第 %d 块类型从 %s 变成 %s", i, a.Kind, b.Kind)}
		}
		return Divergence{i, fmt.Sprintf("第 %d 块（%s）内容被改写", i, a.Kind)}
	}
	if len(next) < len(prev) {
		return Divergence{len(next), "历史被截断"}
	}
	return Divergence{-1, "只追加，前缀完整"}
}

var volatile = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}|request[_-]?id|\b[0-9a-f]{8}-[0-9a-f]{4}-`)

// Lint 找出会让缓存永远不命中的写法。
func Lint(blocks []Block, minTokens int) []string {
	var warns []string
	last, n := -1, 0
	for i, b := range blocks {
		if b.Breakpoint {
			last, n = i, n+1
		}
	}
	if n > 4 {
		warns = append(warns, "断点超过 4 个，请求会 400")
	}
	if last < 0 {
		return append(warns, "没有显式断点：只能依赖自动/隐式缓存")
	}
	for i := 0; i <= last; i++ {
		if volatile.MatchString(blocks[i].Text) {
			warns = append(warns, fmt.Sprintf("第 %d 块在断点之前含时间戳或请求 ID：每次请求前缀都不同", i))
		}
	}
	if sumTokens(blocks[:last+1]) < minTokens {
		warns = append(warns, fmt.Sprintf("断点前只有 %d token，低于最小可缓存长度 %d，静默不缓存", sumTokens(blocks[:last+1]), minTokens))
	}
	return warns
}

// NaiveJSON 手写拼接 map：Go 的 map 遍历顺序是随机的，工具参数每次字节都可能不同。
func NaiveJSON(m map[string]any) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		b, _ := json.Marshal(v)
		parts = append(parts, fmt.Sprintf("%q:%s", k, b))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// CanonicalJSON 用 encoding/json：它会按键排序 map，输出稳定。
func CanonicalJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
