package context11

import (
	"fmt"
	"strings"
	"testing"
)

func TestWindowCheck(t *testing.T) {
	w := Window{Size: 1_000_000}
	cases := []struct {
		in, max int
		want    Outcome
	}{
		{900_000, 64_000, Fits},
		{950_000, 128_000, MayTruncate},
		{1_000_001, 1, Rejected},
	}
	for _, c := range cases {
		if got := w.Check(c.in, c.max); got != c.want {
			t.Errorf("Check(%d,%d)=%v want %v", c.in, c.max, got, c.want)
		}
	}
}

func TestEstimateTokens(t *testing.T) {
	if got := EstimateTokens("hello world!"); got != 3 {
		t.Fatal(got)
	}
	if got := EstimateTokens("上下文工程"); got != 5 {
		t.Fatal(got)
	}
}

func TestRecallShape(t *testing.T) {
	m := DefaultRecall
	n := 200_000
	head, mid, tail := m.Recall(0, n), m.Recall(n/2, n), m.Recall(n-1, n)
	t.Logf("20 万 token：开头 %.3f，中间 %.3f，结尾 %.3f", head, mid, tail)
	if !(mid < head && mid < tail) {
		t.Fatal("中间应该最差")
	}
	short, long := m.Recall(0, 20_000), m.Recall(0, 800_000)
	t.Logf("同样放在开头：2 万 token %.3f，80 万 token %.3f", short, long)
	if long >= short {
		t.Fatal("越长越差")
	}
}

func TestStuffVsRetrieve(t *testing.T) {
	m := DefaultRecall
	stuffMid := m.Stuff(200, 4000, 100)
	stuffEnd := m.Stuff(200, 4000, 199)
	rag := m.Retrieve(8, 4000, 0.9)
	t.Logf("塞 200 篇（80 万 token）：答案在中间 %.3f，在最后 %.3f；检索 8 篇（召回 0.9）%.3f", stuffMid, stuffEnd, rag)
	if !(rag > stuffEnd && stuffEnd > stuffMid) {
		t.Fatal("期望 检索 > 塞满放最后 > 塞满放中间")
	}
	// 资料只有 5 万 token 时，塞满放最后反而比烂检索器好：检索不是免费的午餐
	small := m.Stuff(50, 1000, 49)
	bad := m.Retrieve(8, 1000, 0.6)
	t.Logf("资料 5 万 token 全塞、答案在最后 %.3f；召回 0.6 的检索 %.3f", small, bad)
	if bad >= small {
		t.Fatal("资料不多时，烂检索器不如直接塞")
	}
}

func items() []Item {
	return []Item{
		{Name: "系统规则", Text: "rules", Tokens: 3000, Stable: true, Required: true},
		{Name: "工具说明", Text: "tools", Tokens: 2000, Stable: true, Required: true},
		{Name: "当前时间", Text: "now", Tokens: 20, Required: true},
		{Name: "用户记忆", Text: "memory", Tokens: 800, Priority: 5},
		{Name: "检索 A", Text: "docA", Tokens: 2500, Priority: 8},
		{Name: "检索 A 副本", Text: "docA", Tokens: 2500, Priority: 7},
		{Name: "检索 B", Text: "docB", Tokens: 2500, Priority: 6},
		{Name: "检索 C", Text: "docC", Tokens: 2500, Priority: 2},
		{Name: "问题", Text: "q", Tokens: 100, Required: true},
	}
}

func TestAssemble(t *testing.T) {
	a, err := Assemble(items(), 11_000)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, it := range a.Items {
		names = append(names, it.Name)
	}
	got := strings.Join(names, ",")
	if got != "系统规则,工具说明,当前时间,用户记忆,检索 A,检索 B,问题" {
		t.Fatal(got)
	}
	if strings.Join(a.Dropped, ",") != "检索 A 副本（重复）,检索 C" || a.Used != 10920 {
		t.Fatal(a.Dropped, a.Used)
	}
}

func TestAssembleStableFirst(t *testing.T) {
	a, _ := Assemble(items(), 11_000)
	seenDynamic := false
	for _, it := range a.Items {
		if !it.Stable {
			seenDynamic = true
		} else if seenDynamic {
			t.Fatal("稳定内容必须都在动态内容之前（缓存前缀）")
		}
	}
}

func TestAssembleRequiredTooBig(t *testing.T) {
	if _, err := Assemble(items(), 5000); err != ErrRequiredTooBig {
		t.Fatal(err)
	}
}

func TestTrimLines(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&b, "line %d", i)
		if i < 100 {
			b.WriteString("\n")
		}
	}
	out := TrimLines(b.String(), 3, 2)
	want := "line 1\nline 2\nline 3\n……省略 95 行（完整输出已保存，可按需读取）……\nline 99\nline 100"
	if out != want {
		t.Fatal(out)
	}
	if TrimLines("a\nb", 3, 2) != "a\nb" {
		t.Fatal("短输出不裁")
	}
}

func TestCompactionStrategies(t *testing.T) {
	s := DefaultSim
	res := map[Strategy]Report{}
	for _, st := range []Strategy{KeepAll, ClearTools, ClearAndNote, Summarize} {
		r := s.Run(st)
		res[st] = r
		t.Logf("%-8s 失败轮=%d 峰值=%d 总输入=%d 改写=%d 丢失=%d/%d 缓存当量=%.0f",
			st, r.FailedAt, r.Peak, r.TotalInput, r.PrefixBreaks, r.Lost, r.Needed, r.CacheUnits)
	}
	if res[KeepAll].FailedAt == 0 {
		t.Fatal("全部保留应在 60 轮内撑爆 10 万窗口")
	}
	if res[ClearTools].FailedAt != 0 || res[ClearTools].Lost == 0 {
		t.Fatal("只清工具结果能跑完，但会丢事实")
	}
	if res[ClearAndNote].Lost != 0 {
		t.Fatal("清除前记笔记不应丢事实")
	}
	if res[Summarize].FailedAt != 0 || res[Summarize].Peak > s.Trigger+s.ToolTokens+600 {
		t.Fatal("摘要应把峰值压在阈值附近")
	}
}

func TestClearAtLeast(t *testing.T) {
	// 阈值一过就清：几乎每轮都只清掉一个工具结果，每次都让缓存从那里失效
	s := DefaultSim
	eager := s.Run(ClearTools)
	s.ClearAtLeast = 20_000
	batched := s.Run(ClearTools)
	t.Logf("每次都清：改写 %d 次、缓存当量 %.0f；至少清 2 万才清：改写 %d 次、缓存当量 %.0f",
		eager.PrefixBreaks, eager.CacheUnits, batched.PrefixBreaks, batched.CacheUnits)
	if batched.PrefixBreaks >= eager.PrefixBreaks || batched.CacheUnits >= eager.CacheUnits || batched.FailedAt != 0 {
		t.Fatal("攒够了再清应该改写更少、更便宜，并且仍能跑完")
	}
}

// 假模型：按提示词里的关键词决定行为，用来演示回归评测。
func fakeModel(prompt, input string) string {
	ans := map[string]string{"1+1": "2", "北京到上海多远": "约 1200 公里", "水的沸点": "100 摄氏度"}[input]
	if strings.Contains(prompt, "只回答数字") && ans != "" {
		ans = strings.Fields(strings.NewReplacer("约", "", "公里", " ", "摄氏度", " ").Replace(ans))[0]
	}
	if strings.Contains(prompt, "用中文") && ans == "" {
		ans = "我不知道"
	}
	return ans
}

func TestEvalDiff(t *testing.T) {
	cases := []Case{
		{"add", "1+1", "2"},
		{"dist", "北京到上海多远", "约 1200 公里"},
		{"boil", "水的沸点", "100 摄氏度"},
		{"unknown", "明天彩票号码", "我不知道"},
	}
	eq := func(got, want string) bool { return got == want }
	v1 := Eval("你是助手。", cases, fakeModel, eq)
	v2 := Eval("你是助手。用中文。只回答数字。", cases, fakeModel, eq)
	fixed, broke := Diff(cases, v1, v2)
	t.Logf("v1 %d/%d，v2 %d/%d；修好 %v，弄坏 %v", v1.Pass, v1.Total, v2.Pass, v2.Total, fixed, broke)
	if v1.Pass != 3 || v2.Pass != 2 || len(fixed) != 1 || len(broke) != 2 {
		t.Fatal("期望 v2 修好 1 个、弄坏 2 个")
	}
}

func TestSummaryCapacity(t *testing.T) {
	s := DefaultSim
	s.SummaryFacts = 2
	r := s.Run(Summarize)
	t.Logf("摘要只记得住 2 条事实：丢失 %d/%d", r.Lost, r.Needed)
	if r.Lost == 0 {
		t.Fatal("摘要容量太小时应丢事实")
	}
}
