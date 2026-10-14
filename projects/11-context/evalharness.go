package context11

// 提示词当代码管：每次改提示词都跑同一组用例，看修好了哪些、弄坏了哪些。

type Case struct{ ID, Input, Want string }

// Model 是“提示词 + 输入 → 输出”的函数；测试里用确定性的假模型代替真实 API。
type Model func(prompt, input string) string

type Result struct {
	Pass   int
	Total  int
	Passed map[string]bool
}

func (r Result) Rate() float64 { return float64(r.Pass) / float64(r.Total) }

func Eval(prompt string, cases []Case, m Model, grade func(got, want string) bool) Result {
	r := Result{Total: len(cases), Passed: map[string]bool{}}
	for _, c := range cases {
		if grade(m(prompt, c.Input), c.Want) {
			r.Pass++
			r.Passed[c.ID] = true
		}
	}
	return r
}

// Diff 返回从 base 到 next 修好的用例和弄坏的用例。通过率相同不代表没有变化。
func Diff(cases []Case, base, next Result) (fixed, broke []string) {
	for _, c := range cases {
		switch {
		case !base.Passed[c.ID] && next.Passed[c.ID]:
			fixed = append(fixed, c.ID)
		case base.Passed[c.ID] && !next.Passed[c.ID]:
			broke = append(broke, c.ID)
		}
	}
	return
}
