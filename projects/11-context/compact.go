package context11

import "sort"

// 长任务 Agent 的上下文管理策略模拟。
// 每轮：用户 200 + 工具调用 100 + 工具结果 ToolTokens + 回答 300 token。
// 第 i 轮的工具结果里有一条事实 F(i)；从第 Lag 轮起，每 Every 轮需要用到 Lag 轮前的事实。

type Strategy int

const (
	KeepAll      Strategy = iota // 什么都不做
	ClearTools                   // 超过阈值后把旧工具结果换成占位符，只留最近 Keep 个
	ClearAndNote                 // 同上，但清除前把事实记进笔记（每条 NoteTokens，追加在末尾）
	Summarize                    // 超过阈值后把旧轮次压成一段摘要，只留最近 Keep 轮原文
)

func (s Strategy) String() string {
	return [...]string{"全部保留", "清旧工具结果", "清除前记笔记", "摘要压缩"}[s]
}

type Sim struct {
	Turns, Base, ToolTokens, Window, Trigger, Keep int
	Lag, Every                                     int
	NoteTokens, SummaryTokens, SummaryFacts        int
	ClearAtLeast                                   int // 一次至少能清掉这么多 token 才动手（对应 Anthropic 的 clear_at_least）
}

var DefaultSim = Sim{
	Turns: 60, Base: 8000, ToolTokens: 3000, Window: 100_000, Trigger: 60_000, Keep: 5,
	Lag: 12, Every: 4, NoteTokens: 40, SummaryTokens: 2000, SummaryFacts: 10,
}

type Report struct {
	FailedAt     int // 第几轮因为超窗口失败；0 表示跑完
	Peak         int // 峰值上下文
	TotalInput   int // 所有请求的输入 token 之和
	PrefixBreaks int // 改写历史的次数（每次都让缓存从改写点之后失效）
	Lost, Needed int // 需要的事实里，有多少已经不在上下文里
	CacheUnits   float64
}

type turn struct {
	tokens  int // 本轮当前占用
	cleared bool
	inSumm  bool
}

func (s Sim) Run(st Strategy) Report {
	var r Report
	turns := make([]turn, 0, s.Turns)
	notes := map[int]bool{}   // 笔记里记下的事实
	summary := map[int]bool{} // 摘要里保留的事实
	summTokens := 0
	prev := 0
	const per = 200 + 100 + 300
	ctx := func() int {
		n := s.Base + summTokens + len(notes)*s.NoteTokens
		for _, t := range turns {
			if !t.inSumm {
				n += t.tokens
			}
		}
		return n
	}
	for i := 0; i < s.Turns; i++ {
		turns = append(turns, turn{tokens: per + s.ToolTokens})
		broke := false
		if ctx() > s.Trigger {
			switch st {
			case ClearTools, ClearAndNote:
				clearable := 0
				for j := 0; j < len(turns)-s.Keep; j++ {
					if !turns[j].cleared {
						clearable += turns[j].tokens - (per + 10)
					}
				}
				if clearable < s.ClearAtLeast {
					break
				}
				for j := 0; j < len(turns)-s.Keep; j++ {
					if !turns[j].cleared {
						turns[j].cleared, turns[j].tokens, broke = true, per+10, true
						if st == ClearAndNote {
							notes[j] = true
						}
					}
				}
			case Summarize:
				var newly []int
				for j := 0; j < len(turns)-s.Keep; j++ {
					if !turns[j].inSumm {
						turns[j].inSumm, broke = true, true
						newly = append(newly, j)
					}
				}
				// 摘要容量有限：只保留最近的 SummaryFacts 条事实
				all := []int{}
				for j := range summary {
					all = append(all, j)
				}
				all = append(all, newly...)
				summary = map[int]bool{}
				for _, j := range lastN(all, s.SummaryFacts) {
					summary[j] = true
				}
				summTokens = s.SummaryTokens
			}
		}
		n := ctx()
		if n > s.Window {
			r.FailedAt = i + 1
			return r
		}
		if n > r.Peak {
			r.Peak = n
		}
		r.TotalInput += n
		if broke {
			r.PrefixBreaks++
			r.CacheUnits += float64(n) * 1.25
		} else {
			r.CacheUnits += float64(prev)*0.1 + float64(n-prev)*1.25
		}
		prev = n
		if i >= s.Lag && (i-s.Lag)%s.Every == 0 {
			need := i - s.Lag
			r.Needed++
			t := turns[need]
			available := (!t.cleared && !t.inSumm) || notes[need] || summary[need]
			if !available {
				r.Lost++
			}
		}
	}
	return r
}

func lastN(xs []int, n int) []int {
	sort.Ints(xs)
	if len(xs) > n {
		return xs[len(xs)-n:]
	}
	return xs
}
