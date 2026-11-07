// Package research 是一个最小的 deep research 系统：
// 按问题复杂度分配预算 → 主研究员拆任务 → 子研究员并行“先宽后窄”地搜索和阅读 →
// 结果写进工件存储、只把摘要和引用交回主研究员 → 按缺口追加任务 → 每轮存检查点 →
// 写报告 → 引用核对 → 按评分细则打分。网络是内存里的假语料，所有行为都可重复。
package research

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// Quality 是来源质量。研究员只能从 URL 推断它（和人一样：先看是谁说的）。
type Quality int

const (
	Farm      Quality = iota // SEO 内容农场
	Secondary                // 新闻、博客、二手转述
	Primary                  // 官方、论文、原始数据
)

func (q Quality) String() string { return [...]string{"farm", "secondary", "primary"}[q] }

// SourceQuality 是写进提示词里的那条启发式规则的代码版：
// 优先一手来源，警惕 SEO 农场。Anthropic 发现早期 agent 总是选排名靠前的内容农场，
// 而不是排名靠后但更权威的论文 PDF。
func SourceQuality(raw string) Quality {
	u, err := url.Parse(raw)
	if err != nil {
		return Farm
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	switch {
	case strings.Contains(host, "top10") || strings.Contains(host, "best-") || strings.Contains(host, "seo"):
		return Farm
	case strings.HasSuffix(host, ".gov") || strings.HasSuffix(host, ".edu") || host == "arxiv.org":
		return Primary
	default:
		return Secondary
	}
}

// Canonical 把同一个页面的不同写法归一：http/https、www、末尾斜杠、跟踪参数、锚点。
// 不归一的话，同一篇文章会被当成两个来源，既浪费调用又虚增“来源数量”。
func Canonical(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return raw
	}
	u.Scheme = "https"
	u.Host = strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	u.Fragment = ""
	q := u.Query()
	for k := range q {
		if strings.HasPrefix(k, "utm_") || k == "ref" || k == "fbclid" || k == "gclid" {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u.String()
}

type Page struct {
	URL   string
	Title string
	Body  string
	SEO   int // 搜索引擎排名加成：农场很高，论文很低
}

type Result struct {
	URL   string
	Title string
}

// Web 是假搜索引擎 + 假网页，并发安全，记录调用次数和最大并发。
type Web struct {
	mu    sync.Mutex
	pages []Page
	log   []string

	Delay    time.Duration // 模拟网络延迟，用来测并行
	Searches atomic.Int64
	Fetches  atomic.Int64
	inflight atomic.Int64
	maxIn    atomic.Int64
	FailURL  string // 抓这个 URL 时返回错误，模拟工具故障
}

var ErrFetch = errors.New("抓取失败")

func (w *Web) Add(p Page) { w.mu.Lock(); w.pages = append(w.pages, p); w.mu.Unlock() }

func (w *Web) enter() func() {
	n := w.inflight.Add(1)
	for {
		m := w.maxIn.Load()
		if n <= m || w.maxIn.CompareAndSwap(m, n) {
			break
		}
	}
	if w.Delay > 0 {
		time.Sleep(w.Delay)
	}
	return func() { w.inflight.Add(-1) }
}

func (w *Web) MaxInflight() int64 { return w.maxIn.Load() }
func (w *Web) Calls() int64       { return w.Searches.Load() + w.Fetches.Load() }

func (w *Web) Queries() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.log...)
}

// Search 按“命中词数 × 10 + SEO 加成”排序，最多 10 条。
// 这正是真实搜索的毛病：排名高不等于可信。
func (w *Web) Search(ctx context.Context, q string) ([]Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	defer w.enter()()
	w.Searches.Add(1)
	terms := Tokens(q)
	w.mu.Lock()
	w.log = append(w.log, q)
	pages := append([]Page(nil), w.pages...)
	w.mu.Unlock()

	type scored struct {
		p     Page
		score int
	}
	var hits []scored
	for _, p := range pages {
		doc := map[string]bool{}
		for _, t := range Tokens(p.Title + " " + p.Body) {
			doc[t] = true
		}
		n := 0
		for _, t := range terms {
			if doc[t] {
				n++
			}
		}
		if n > 0 {
			hits = append(hits, scored{p, n*10 + p.SEO})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].p.URL < hits[j].p.URL
	})
	var out []Result
	for i := 0; i < len(hits) && i < 10; i++ {
		out = append(out, Result{URL: hits[i].p.URL, Title: hits[i].p.Title})
	}
	return out, nil
}

func (w *Web) Fetch(ctx context.Context, raw string) (Page, error) {
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	defer w.enter()()
	w.Fetches.Add(1)
	if w.FailURL != "" && raw == w.FailURL {
		return Page{}, ErrFetch
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.pages {
		if p.URL == raw {
			return p, nil
		}
	}
	return Page{}, ErrFetch
}

var stop = map[string]bool{"the": true, "a": true, "an": true, "of": true, "and": true, "in": true,
	"to": true, "is": true, "for": true, "on": true, "with": true, "per": true, "our": true}

// Tokens 小写、按非字母数字切分、去停用词。
func Tokens(s string) []string {
	f := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	out := f[:0]
	for _, t := range f {
		if !stop[t] {
			out = append(out, t)
		}
	}
	return out
}
