package research

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Evidence 是一条带出处的论断。报告里的每句话最终都要能指回一条 Evidence。
type Evidence struct {
	Aspect  string  `json:"aspect"`
	Claim   string  `json:"claim"`
	URL     string  `json:"url"`
	Quality Quality `json:"quality"`
}

// Finding 是子代理交回主研究员的东西：摘要 + 工件引用，而不是全部原文。
// 研究的本质是压缩；子代理在自己的上下文里读完几十个页面，只把最重要的 token 交回去。
type Finding struct {
	TaskID      string   `json:"task_id"`
	Aspect      string   `json:"aspect"`
	Summary     string   `json:"summary"`
	Ref         string   `json:"ref"` // 工件存储里的键，主研究员需要细节时自己去取
	Evidence    int      `json:"evidence"`
	Calls       int      `json:"calls"`
	Queries     []string `json:"queries"`
	Quarantined []string `json:"quarantined,omitempty"`
	Errors      []string `json:"errors,omitempty"`
}

// Store 是持久化存储：计划、检查点、工件都放这里。
type Store interface {
	Put(key string, v []byte) error
	Get(key string) ([]byte, bool)
}

type MemStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func NewMemStore() *MemStore { return &MemStore{m: map[string][]byte{}} }

func (s *MemStore) Put(k string, v []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = append([]byte(nil), v...)
	return nil
}

func (s *MemStore) Get(k string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	return v, ok
}

// 网页内容是数据，不是指令。看起来像在给 agent 下命令的句子一律隔离：
// 不当证据，不执行，只记录下来交给主研究员。
var injection = regexp.MustCompile(`(?i)(ignore (all |any )?(previous|prior|above) instructions|system prompt|you are now|send .+ to \S+@|call the \w+ tool)`)

// Researcher 是一个子代理。它只有 search 和 fetch，没有任何能把数据送出去的工具：
// 读公开网页的上下文里不放私有数据和写操作，注入就算漏过去也无处可用。
type Researcher struct {
	Web   *Web
	Store Store
}

func (r *Researcher) Run(ctx context.Context, t Task) (Finding, error) {
	f := Finding{TaskID: t.ID, Aspect: t.Aspect}
	if len(t.Keywords) == 0 {
		return f, fmt.Errorf("任务 %s 没有关键词", t.ID)
	}
	// 先宽后窄：先用一个短词看看有什么，再用完整的词组收窄。
	// agent 天然倾向写又长又具体的查询，结果往往一条都搜不到。
	queries := []string{t.Keywords[0]}
	if len(t.Keywords) > 1 {
		queries = append(queries, strings.Join(t.Keywords, " "))
	}
	need := min(2, len(t.Keywords))
	kw := map[string]bool{}
	for _, k := range t.Keywords {
		kw[strings.ToLower(k)] = true
	}

	seen := map[string]bool{}
	claims := map[string]bool{}
	var ev []Evidence
	enough := func() bool { return t.MinEvidence > 0 && len(ev) >= t.MinEvidence }

	for _, q := range queries {
		if f.Calls >= t.MaxCalls || enough() {
			break
		}
		res, err := r.Web.Search(ctx, q)
		f.Calls++
		f.Queries = append(f.Queries, q)
		if err != nil {
			f.Errors = append(f.Errors, "search "+q+": "+err.Error())
			continue
		}
		// 搜索引擎按 SEO 排，研究员按来源质量重排（稳定排序，同质量保持原顺序）
		sort.SliceStable(res, func(i, j int) bool { return SourceQuality(res[i].URL) > SourceQuality(res[j].URL) })
		for _, hit := range res {
			if f.Calls >= t.MaxCalls || enough() {
				break
			}
			c := Canonical(hit.URL)
			if seen[c] {
				continue
			}
			seen[c] = true
			page, err := r.Web.Fetch(ctx, hit.URL)
			f.Calls++
			if err != nil {
				// 工具失败告诉模型、继续干活，而不是整个任务崩掉
				f.Errors = append(f.Errors, "fetch "+hit.URL+": "+err.Error())
				continue
			}
			for _, s := range sentences(page.Body) {
				if injection.MatchString(s) {
					f.Quarantined = append(f.Quarantined, c+"："+s)
					continue
				}
				hits := 0
				for _, tok := range uniq(Tokens(s)) {
					if kw[tok] {
						hits++
					}
				}
				key := strings.ToLower(s)
				if hits >= need && !claims[key] {
					claims[key] = true
					ev = append(ev, Evidence{Aspect: t.Aspect, Claim: s, URL: c, Quality: SourceQuality(c)})
				}
			}
		}
	}

	data, err := json.Marshal(ev)
	if err != nil {
		return f, err
	}
	f.Ref = "artifacts/" + t.ID + ".json"
	if err := r.Store.Put(f.Ref, data); err != nil {
		return f, err
	}
	f.Evidence = len(ev)
	f.Summary = summarize(ev, 200)
	return f, nil
}

// summarize 只交回最好的几条，总长度有上限。
func summarize(ev []Evidence, maxRunes int) string {
	if len(ev) == 0 {
		return "没有找到相关证据"
	}
	sorted := append([]Evidence(nil), ev...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Quality > sorted[j].Quality })
	var parts []string
	used := 0
	for i, e := range sorted {
		n := len([]rune(e.Claim))
		if i == 3 || used+n > maxRunes {
			parts = append(parts, fmt.Sprintf("（另有 %d 条见工件）", len(sorted)-i))
			break
		}
		parts = append(parts, e.Claim)
		used += n
	}
	return strings.Join(parts, "；")
}

func sentences(body string) []string {
	var out []string
	for _, s := range strings.Split(body, ". ") {
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "."))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func uniq(xs []string) []string {
	m := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !m[x] {
			m[x] = true
			out = append(out, x)
		}
	}
	return out
}
