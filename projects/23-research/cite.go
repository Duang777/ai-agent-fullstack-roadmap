package research

import (
	"fmt"
	"sort"
	"strings"
)

// Line 是报告里的一句话，属于某个方面。
type Line struct {
	Aspect string
	Text   string
}

// Synthesize 是一个确定性的“写作者”：每个必需方面挑质量最高的两条证据。
// 真实系统里这一步是模型写的，所以下一步的引用核对才有意义——模型会写出没有出处的句子。
func Synthesize(required []string, ev []Evidence) []Line {
	var out []Line
	for _, a := range required {
		var mine []Evidence
		for _, e := range ev {
			if e.Aspect == a {
				mine = append(mine, e)
			}
		}
		sort.SliceStable(mine, func(i, j int) bool { return mine[i].Quality > mine[j].Quality })
		for i := 0; i < len(mine) && i < 2; i++ {
			out = append(out, Line{Aspect: a, Text: mine[i].Claim})
		}
	}
	return out
}

type Source struct {
	URL     string
	Quality Quality
}

type CitedLine struct {
	Line
	Source    *Source // nil 表示没有证据支持
	Supported bool
}

type Cited struct {
	Lines   []CitedLine
	Sources []Source // 按首次引用顺序编号
}

const supportThreshold = 0.6

// Cite 是 CitationAgent：报告写完后单独走一遍，给每句话找出处。
// 句子里至少 60% 的词出现在某条证据里才算有支持；多条都支持时选质量最高的。
// 找不到出处的句子不删，标出来让人（或主研究员）决定。
func Cite(lines []Line, ev []Evidence) Cited {
	var c Cited
	index := map[string]int{}
	for _, l := range lines {
		cl := CitedLine{Line: l}
		var best *Evidence
		bestOverlap := 0.0
		for i := range ev {
			o := overlap(l.Text, ev[i].Claim)
			if o < supportThreshold {
				continue
			}
			if best == nil || ev[i].Quality > best.Quality || (ev[i].Quality == best.Quality && o > bestOverlap) {
				best, bestOverlap = &ev[i], o
			}
		}
		if best != nil {
			u := Canonical(best.URL)
			if _, ok := index[u]; !ok {
				index[u] = len(c.Sources)
				c.Sources = append(c.Sources, Source{URL: u, Quality: best.Quality})
			}
			s := c.Sources[index[u]]
			cl.Source, cl.Supported = &s, true
		}
		c.Lines = append(c.Lines, cl)
	}
	return c
}

func overlap(sentence, claim string) float64 {
	st := uniq(Tokens(sentence))
	if len(st) == 0 {
		return 0
	}
	ct := map[string]bool{}
	for _, t := range Tokens(claim) {
		ct[t] = true
	}
	n := 0
	for _, t := range st {
		if ct[t] {
			n++
		}
	}
	return float64(n) / float64(len(st))
}

// Render 输出带 [n] 角标的报告和来源列表，没有出处的句子标 [?]。
func (c Cited) Render() string {
	idx := map[string]int{}
	for i, s := range c.Sources {
		idx[s.URL] = i + 1
	}
	var b strings.Builder
	for _, l := range c.Lines {
		if l.Supported {
			fmt.Fprintf(&b, "%s [%d]\n", l.Text, idx[l.Source.URL])
		} else {
			fmt.Fprintf(&b, "%s [?]\n", l.Text)
		}
	}
	b.WriteString("\n来源\n")
	for i, s := range c.Sources {
		fmt.Fprintf(&b, "[%d] %s（%s）\n", i+1, s.URL, s.Quality)
	}
	return b.String()
}

func (c Cited) Unsupported() []string {
	var out []string
	for _, l := range c.Lines {
		if !l.Supported {
			out = append(out, l.Text)
		}
	}
	return out
}
