package promptcache

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)

func base() []Block {
	return []Block{
		{Kind: "tool", Text: "tools", Tokens: 2000},
		{Kind: "system", Text: "rules", Tokens: 6000, Breakpoint: true},
	}
}

func withUser(q string) []Block { return append(base(), Block{Kind: "user", Text: q, Tokens: 50}) }

func TestWriteThenRead(t *testing.T) {
	c := NewCache(5*time.Minute, 1024, 20)
	u1, _ := c.Anthropic(withUser("a"), false, t0)
	if u1 != (Usage{Read: 0, Write: 8000, Input: 50}) {
		t.Fatalf("首次请求 %+v", u1)
	}
	u2, _ := c.Anthropic(withUser("b"), false, t0.Add(time.Minute))
	if u2 != (Usage{Read: 8000, Write: 0, Input: 50}) {
		t.Fatalf("第二次请求 %+v", u2)
	}
}

func TestChangeBeforeBreakpointMisses(t *testing.T) {
	c := NewCache(5*time.Minute, 1024, 20)
	c.Anthropic(withUser("a"), false, t0)
	b := withUser("b")
	b[0].Text = "tools v2" // 改了工具定义：之后所有层级全部失效
	u, _ := c.Anthropic(b, false, t0.Add(time.Minute))
	if u.Read != 0 || u.Write != 8000 {
		t.Fatalf("改工具后应全部重写 %+v", u)
	}
}

func TestBreakpointOnVolatileBlockNeverHits(t *testing.T) {
	c := NewCache(5*time.Minute, 1024, 20)
	for i := 0; i < 3; i++ {
		b := base()
		b[1].Breakpoint = false
		b = append(b, Block{Kind: "user", Text: fmt.Sprintf("时间 %d + 问题", i), Tokens: 50, Breakpoint: true})
		u, _ := c.Anthropic(b, false, t0.Add(time.Duration(i)*time.Minute))
		if u.Read != 0 || u.Write != 8050 {
			t.Fatalf("第 %d 次：断点放在变化的块上，回看找不到任何以前写过的条目 %+v", i, u)
		}
	}
}

func TestBelowMinimumSilentlySkipped(t *testing.T) {
	c := NewCache(5*time.Minute, 1024, 20)
	b := []Block{{Kind: "system", Text: "short", Tokens: 900, Breakpoint: true}, {Kind: "user", Text: "q", Tokens: 10}}
	for i := 0; i < 2; i++ {
		u, err := c.Anthropic(b, false, t0)
		if err != nil || u.Read != 0 || u.Write != 0 || u.Input != 910 {
			t.Fatalf("低于最小长度应静默不缓存 %+v %v", u, err)
		}
	}
}

func TestTTLExpiryAndRefresh(t *testing.T) {
	c := NewCache(5*time.Minute, 1024, 20)
	c.Anthropic(withUser("a"), false, t0)
	u, _ := c.Anthropic(withUser("b"), false, t0.Add(4*time.Minute))
	if u.Read != 8000 {
		t.Fatal("4 分钟内应命中")
	}
	u, _ = c.Anthropic(withUser("c"), false, t0.Add(8*time.Minute)) // 距上次读 4 分钟：读会续期
	if u.Read != 8000 {
		t.Fatal("命中会续期，8 分钟时仍应命中")
	}
	u, _ = c.Anthropic(withUser("d"), false, t0.Add(14*time.Minute))
	if u.Read != 0 || u.Write != 8000 {
		t.Fatalf("空闲 6 分钟应过期重写 %+v", u)
	}
}

func TestTooManyBreakpoints(t *testing.T) {
	c := NewCache(5*time.Minute, 1024, 20)
	var b []Block
	for i := 0; i < 4; i++ {
		b = append(b, Block{Kind: "system", Text: fmt.Sprint(i), Tokens: 500, Breakpoint: true})
	}
	b = append(b, Block{Kind: "user", Text: "q", Tokens: 10})
	if _, err := c.Anthropic(b, true, t0); err != ErrTooManyBreakpoints {
		t.Fatal("4 个显式断点 + 自动缓存应 400")
	}
}

// 一轮追加 25 块时，单断点的回看窗口（20）够不着上一轮写的条目；提前多放一个断点就能接上。
func TestLookbackWindow(t *testing.T) {
	grow := func(n int) []Block {
		b := []Block{{Kind: "system", Text: "rules", Tokens: 2000}}
		for i := 0; i < n; i++ {
			b = append(b, Block{Kind: "user", Text: fmt.Sprintf("chunk %d", i), Tokens: 40})
		}
		return b
	}
	c := NewCache(5*time.Minute, 1024, 20)
	r1 := grow(10)
	r1[len(r1)-1].Breakpoint = true
	c.Anthropic(r1, false, t0)
	r2 := grow(35)
	r2[len(r2)-1].Breakpoint = true
	if u, _ := c.Anthropic(r2, false, t0.Add(time.Minute)); u.Read != 0 {
		t.Fatalf("超出回看窗口应未命中 %+v", u)
	}
	c2 := NewCache(5*time.Minute, 1024, 20)
	c2.Anthropic(r1, false, t0)
	r3 := grow(35)
	r3[10].Breakpoint = true // 上一轮末尾的位置
	r3[len(r3)-1].Breakpoint = true
	if u, _ := c2.Anthropic(r3, false, t0.Add(time.Minute)); u.Read != 2400 {
		t.Fatalf("第二个断点应接上上一轮 %+v", u)
	}
}

func TestOpenAIImplicitVsExplicit(t *testing.T) {
	req := func(q string, explicit bool) []Block {
		return []Block{
			{Kind: "system", Text: "developer rules", Tokens: 3000, Breakpoint: explicit},
			{Kind: "user", Text: q, Tokens: 100},
		}
	}
	// implicit：断点落在用户消息末尾，用户问题一变，较短的共享前缀也接不上。
	c := NewCache(30*time.Minute, 1024, 20)
	c.OpenAI(req("a", false), true, t0)
	u, _ := c.OpenAI(req("b", false), true, t0.Add(time.Minute))
	if u.Cached != 0 || u.InputTokens != 3100 {
		t.Fatalf("隐式模式不应命中 %+v", u)
	}
	// explicit：断点放在静态内容后，只写 3000，后面的用户内容不收写入费。
	c = NewCache(30*time.Minute, 1024, 20)
	u1, _ := c.OpenAI(req("a", true), false, t0)
	u2, _ := c.OpenAI(req("b", true), false, t0.Add(time.Minute))
	if u1.CacheWrite != 3000 || u2.Cached != 3000 || u2.CacheWrite != 0 {
		t.Fatalf("显式模式 %+v %+v", u1, u2)
	}
}

func TestOpenAIMultiTurnImplicit(t *testing.T) {
	c := NewCache(30*time.Minute, 1024, 20)
	turn1 := []Block{{Kind: "system", Text: "dev", Tokens: 8000}, {Kind: "user", Text: "q1", Tokens: 2000}}
	u1, _ := c.OpenAI(turn1, true, t0)
	turn2 := append(append([]Block{}, turn1...), Block{Kind: "user", Text: "q2", Tokens: 3000})
	u2, _ := c.OpenAI(turn2, true, t0.Add(time.Minute))
	// 对照文档示例（去掉 2000 工具 token）：第一轮写 10000；第二轮读 10000、写 3000
	if u1 != (OpenAIUsage{10000, 0, 10000}) || u2 != (OpenAIUsage{13000, 10000, 3000}) {
		t.Fatalf("%+v %+v", u1, u2)
	}
}

func TestSwitchToExplicitMissesImplicitWrite(t *testing.T) {
	c := NewCache(30*time.Minute, 1024, 20)
	r1 := []Block{{Kind: "system", Text: "dev", Tokens: 3000}, {Kind: "user", Text: "q1", Tokens: 100}}
	c.OpenAI(r1, true, t0)
	r2 := append(append([]Block{}, r1...), Block{Kind: "user", Text: "follow", Tokens: 100, Breakpoint: true})
	if u, _ := c.OpenAI(r2, false, t0.Add(time.Minute)); u.Cached != 0 {
		t.Fatalf("切到 explicit-only 后只查自己的显式断点 %+v", u)
	}
}

func TestUnitsAndBreakEven(t *testing.T) {
	// 文档：写一次读一次 = 1.35×；写一次读九次 = 2.15×
	two := OpenAI56.Units(Usage{Write: 1000}) + OpenAI56.Units(Usage{Read: 1000})
	ten := OpenAI56.Units(Usage{Write: 1000}) + 9*OpenAI56.Units(Usage{Read: 1000})
	if math.Abs(two-1350) > 1e-9 || math.Abs(ten-2150) > 1e-9 {
		t.Fatal(two, ten)
	}
	if be := BreakEven(1024, OpenAI56, 10); math.Abs(be-220.16) > 1e-9 {
		t.Fatal(be)
	}
	if be := BreakEven(1024, OpenAI56, 1_000_000); be < 102.4 || be > 102.5 {
		t.Fatal("N 趋于无穷时逼近 102.4", be)
	}
}

func TestDiagnose(t *testing.T) {
	p := withUser("a")
	n := append(withUser("a"), Block{Kind: "assistant", Text: "x", Tokens: 1})
	if d := Diagnose(p, n); d.Index != -1 {
		t.Fatal(d)
	}
	m := withUser("a")
	m[1].Text = "rules 已更新"
	if d := Diagnose(p, m); d.Index != 1 || !strings.Contains(d.Reason, "改写") {
		t.Fatal(d)
	}
	r := []Block{p[1], p[0], p[2]}
	if d := Diagnose(p, r); d.Index != 0 || !strings.Contains(d.Reason, "顺序") {
		t.Fatal(d)
	}
	if d := Diagnose(p, p[:2]); !strings.Contains(d.Reason, "截断") {
		t.Fatal(d)
	}
}

func TestLint(t *testing.T) {
	b := []Block{{Kind: "system", Text: "现在是 2026-10-13 20:41，你是助手", Tokens: 3000, Breakpoint: true}}
	if w := Lint(b, 1024); len(w) != 1 || !strings.Contains(w[0], "时间戳") {
		t.Fatal(w)
	}
	short := []Block{{Kind: "system", Text: "hi", Tokens: 300, Breakpoint: true}}
	if w := Lint(short, 1024); len(w) != 1 || !strings.Contains(w[0], "最小") {
		t.Fatal(w)
	}
	// 时间戳放在断点之后就没问题
	ok := []Block{{Kind: "system", Text: "rules", Tokens: 3000, Breakpoint: true}, {Kind: "user", Text: "2026-10-13 20:41 的问题", Tokens: 20}}
	if w := Lint(ok, 1024); len(w) != 0 {
		t.Fatal(w)
	}
}

func TestJSONKeyOrder(t *testing.T) {
	m := map[string]any{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5, "f": 6, "g": 7, "h": 8}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		seen[NaiveJSON(m)] = true
		if CanonicalJSON(m) != `{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6,"g":7,"h":8}` {
			t.Fatal("encoding/json 应按键排序")
		}
	}
	if len(seen) < 2 {
		t.Fatal("手写遍历 map 应出现不同顺序")
	}
}

func TestAgentStrategies(t *testing.T) {
	none, _ := AgentSession(30, NoCache, time.Minute)
	auto, hit := AgentSession(30, AutoLast, time.Minute)
	ts, tsHit := AgentSession(30, TimestampFirst, time.Minute)
	sys, _ := AgentSession(30, SystemOnly, time.Minute)
	t.Logf("不缓存 %.0f；自动 %.0f（%.1f%%，命中率 %.1f%%）；只缓存 system %.0f（%.1f%%）；时间戳 %.0f（%.1f%%，命中率 %.1f%%）",
		none, auto, 100*auto/none, 100*hit, sys, 100*sys/none, ts, 100*ts/none, 100*tsHit)
	if !(auto < sys && sys < none && none < ts) || tsHit != 0 {
		t.Fatal("期望 自动 < 只缓存 system < 不缓存 < 时间戳")
	}
	slow, _ := AgentSession(30, AutoLast, 6*time.Minute)
	t.Logf("每轮间隔 6 分钟（5m TTL 全部过期）：%.0f（%.1f%%）", slow, 100*slow/none)
	if slow <= none {
		t.Fatal("全部过期时每轮都在付 1.25× 写入费，应比不缓存还贵")
	}
}

func TestTTLChoice(t *testing.T) {
	gaps := []int{3, 12, 7, 25, 4, 18, 9, 40, 2, 15}
	five := TTLCost(20000, gaps, 5*time.Minute, Anthropic5m)
	hour := TTLCost(20000, gaps, time.Hour, Anthropic1h)
	t.Logf("用户回复间隔 %v 分钟：5m TTL %.0f，1h TTL %.0f，不缓存 %d", gaps, five, hour, 20000*11)
	if hour >= five {
		t.Fatal("间隔多数超过 5 分钟时 1h 更便宜")
	}
	fast := []int{1, 2, 1, 3, 2, 1, 2, 4, 1, 2}
	f5 := TTLCost(20000, fast, 5*time.Minute, Anthropic5m)
	f1 := TTLCost(20000, fast, time.Hour, Anthropic1h)
	if f5 >= f1 {
		t.Fatal("间隔都在 5 分钟内时 5m 更便宜（读会免费续期）")
	}
}

func TestGeminiExplicit(t *testing.T) {
	// 示例参数：读 0.1×，存储每 token 每小时 0.25× 普通输入价（不是官方价）
	cost := func(n int) float64 { return GeminiExplicit(100000, n, 1, 0.1, 0.25) }
	plain := func(n int) float64 { return float64(100000 * n) }
	if cost(1) <= plain(1) {
		t.Fatal("只用一次时显式缓存更贵")
	}
	if cost(10) >= plain(10) {
		t.Fatal("一小时用 10 次时显式缓存更便宜")
	}
}
