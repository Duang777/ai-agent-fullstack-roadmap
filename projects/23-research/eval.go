package research

import (
	"fmt"
	"strings"
)

// Expect 是一道评测题：必须覆盖的方面、必须出现的事实、调用预算。
type Expect struct {
	Required []string
	Facts    []string
	Budget   int
}

// Score 对应 Anthropic 的 LLM 评委细则：事实准确、引用准确、完整、来源质量、工具效率，
// 每项 0.0–1.0，外加通过与否。他们发现一次调用、一个提示词同时给出全部分数，
// 比拆成多个评委更稳定、更接近人工判断。这里用确定性规则代替模型评委。
type Score struct {
	Factual       float64
	Citation      float64
	Completeness  float64
	SourceQuality float64
	Efficiency    float64
	Pass          bool
	Notes         []string
}

func Grade(c Cited, calls int, e Expect) Score {
	var s Score
	var text strings.Builder
	supported, covered := 0, map[string]bool{}
	primary := 0
	for _, l := range c.Lines {
		if l.Supported {
			supported++
			covered[l.Aspect] = true
			text.WriteString(strings.ToLower(l.Text) + "\n")
		}
	}
	if len(c.Lines) > 0 {
		s.Citation = float64(supported) / float64(len(c.Lines))
	}
	if len(e.Required) > 0 {
		n := 0
		for _, a := range e.Required {
			if covered[a] {
				n++
			} else {
				s.Notes = append(s.Notes, "缺少方面："+a)
			}
		}
		s.Completeness = float64(n) / float64(len(e.Required))
	}
	if len(e.Facts) > 0 {
		n := 0
		for _, f := range e.Facts {
			if strings.Contains(text.String(), strings.ToLower(f)) {
				n++
			} else {
				s.Notes = append(s.Notes, "缺少事实："+f)
			}
		}
		s.Factual = float64(n) / float64(len(e.Facts))
	}
	for _, src := range c.Sources {
		if src.Quality == Primary {
			primary++
		}
	}
	if len(c.Sources) > 0 {
		s.SourceQuality = float64(primary) / float64(len(c.Sources))
	}
	switch {
	case calls <= 0:
		s.Efficiency = 1
	case e.Budget > 0:
		s.Efficiency = min(1, float64(e.Budget)/float64(calls))
	}
	s.Pass = s.Factual >= 0.7 && s.Citation >= 0.7 && s.Completeness >= 0.7 && s.SourceQuality >= 0.5 && s.Efficiency >= 0.7
	if s.Citation < 0.7 {
		s.Notes = append(s.Notes, fmt.Sprintf("有出处的句子只占 %.0f%%", s.Citation*100))
	}
	return s
}
