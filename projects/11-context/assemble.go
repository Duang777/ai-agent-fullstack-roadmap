package context11

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Item 是候选上下文的一段：系统规则、工具说明、记忆、检索结果、历史、本次问题……
type Item struct {
	Name     string
	Text     string
	Tokens   int
	Priority int  // 越大越重要
	Stable   bool // 跨请求不变：放前面，配合 1.5 的缓存
	Required bool // 必须带上，放不下就报错
}

type Assembly struct {
	Items   []Item
	Dropped []string
	Used    int
}

var ErrRequiredTooBig = errors.New("必需内容已经超出预算")

// Assemble 在 budget 内挑选内容：必需项全部保留，其余按优先级贪心放入，完全相同的文本只保留一份；
// 输出顺序：稳定内容在前、动态内容在后，组内保持原始顺序。
func Assemble(items []Item, budget int) (Assembly, error) {
	var a Assembly
	keep := make([]bool, len(items))
	seen := map[string]bool{}
	for i, it := range items {
		if it.Required {
			keep[i] = true
			a.Used += it.Tokens
			seen[it.Text] = true
		}
	}
	if a.Used > budget {
		return a, ErrRequiredTooBig
	}
	order := make([]int, 0, len(items))
	for i := range items {
		if !items[i].Required {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(x, y int) bool { return items[order[x]].Priority > items[order[y]].Priority })
	for _, i := range order {
		it := items[i]
		switch {
		case seen[it.Text]:
			a.Dropped = append(a.Dropped, it.Name+"（重复）")
		case a.Used+it.Tokens > budget:
			a.Dropped = append(a.Dropped, it.Name)
		default:
			keep[i] = true
			a.Used += it.Tokens
			seen[it.Text] = true
		}
	}
	for _, stable := range []bool{true, false} {
		for i, it := range items {
			if keep[i] && it.Stable == stable {
				a.Items = append(a.Items, it)
			}
		}
	}
	return a, nil
}

// TrimLines 把过长的工具输出裁成“头 head 行 + 省略说明 + 尾 tail 行”。
// 报错信息和结论通常在尾部，只截头是最常见的错误。
func TrimLines(text string, head, tail int) string {
	lines := strings.Split(text, "\n")
	if len(lines) <= head+tail {
		return text
	}
	out := append([]string{}, lines[:head]...)
	out = append(out, fmt.Sprintf("……省略 %d 行（完整输出已保存，可按需读取）……", len(lines)-head-tail))
	return strings.Join(append(out, lines[len(lines)-tail:]...), "\n")
}
