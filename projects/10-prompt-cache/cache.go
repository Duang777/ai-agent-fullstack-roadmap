// Package promptcache 用纯 Go 模拟 Prompt Caching 的前缀匹配、断点、回看窗口与 TTL。
// 规则按 2026-10 的 Anthropic / OpenAI 官方文档摘录，只为理解机制，不是厂商实现。
package promptcache

import (
	"crypto/sha256"
	"errors"
	"time"
)

// Block 是渲染后提示词里的一个内容块（tools → system → messages 依次排列）。
type Block struct {
	Kind       string // "tool" / "system" / "user" / "assistant" / "tool_result"
	Text       string
	Tokens     int
	Breakpoint bool // 显式缓存断点（Anthropic 的 cache_control / OpenAI 的 prompt_cache_breakpoint）
}

type entry struct {
	tokens  int
	expires time.Time
}

// Cache 是一台机器上的 KV 缓存索引：键是“到某个块为止的累计前缀哈希”。
type Cache struct {
	TTL       time.Duration
	MinTokens int // 低于它的前缀不写缓存，也不报错
	Lookback  int // Anthropic：每个断点最多往回查几个位置（含断点自身）
	entries   map[[32]byte]entry
}

func NewCache(ttl time.Duration, minTokens, lookback int) *Cache {
	return &Cache{TTL: ttl, MinTokens: minTokens, Lookback: lookback, entries: map[[32]byte]entry{}}
}

// Usage 用 Anthropic 的三段口径：总输入 = Read + Write + Input。
type Usage struct{ Read, Write, Input int }

func (u Usage) Total() int { return u.Read + u.Write + u.Input }

var ErrTooManyBreakpoints = errors.New("最多 4 个缓存断点")

// prefixes 返回每个位置的累计哈希与累计 token：改动第 i 块，i 及之后所有哈希都变。
func prefixes(blocks []Block) ([][32]byte, []int) {
	hs := make([][32]byte, len(blocks))
	cum := make([]int, len(blocks))
	var prev [32]byte
	total := 0
	for i, b := range blocks {
		h := sha256.New()
		h.Write(prev[:])
		h.Write([]byte(b.Kind))
		h.Write([]byte{0})
		h.Write([]byte(b.Text))
		copy(prev[:], h.Sum(nil))
		hs[i] = prev
		total += b.Tokens
		cum[i] = total
	}
	return hs, cum
}

func sumTokens(blocks []Block) int {
	n := 0
	for _, b := range blocks {
		n += b.Tokens
	}
	return n
}

// serve 是两家共用的核心：先在 lookups 位置找最长命中，再在 writes 位置写条目。
func (c *Cache) serve(blocks []Block, lookups, writes []int, now time.Time) Usage {
	hs, cum := prefixes(blocks)
	hit := 0
	for _, p := range lookups {
		if e, ok := c.entries[hs[p]]; ok && now.Before(e.expires) {
			if cum[p] > hit {
				hit = cum[p]
			}
			c.entries[hs[p]] = entry{e.tokens, now.Add(c.TTL)} // 命中即续期，不另收费
		}
	}
	written := hit
	for _, p := range writes {
		if cum[p] < c.MinTokens {
			continue
		}
		if e, ok := c.entries[hs[p]]; ok && now.Before(e.expires) {
			continue
		}
		c.entries[hs[p]] = entry{cum[p], now.Add(c.TTL)}
		if cum[p] > written {
			written = cum[p]
		}
	}
	total := sumTokens(blocks)
	return Usage{Read: hit, Write: written - hit, Input: total - written}
}

// Anthropic 模拟 Messages API：显式断点 + 可选自动缓存（auto 把断点放在最后一块）。
// 每个断点只在自己的位置写一个条目；读时从断点往回最多 Lookback 个位置找“以前写过的条目”。
func (c *Cache) Anthropic(blocks []Block, auto bool, now time.Time) (Usage, error) {
	var bps []int
	for i, b := range blocks {
		if b.Breakpoint {
			bps = append(bps, i)
		}
	}
	if auto && len(blocks) > 0 && (len(bps) == 0 || bps[len(bps)-1] != len(blocks)-1) {
		bps = append(bps, len(blocks)-1)
	}
	if len(bps) > 4 {
		return Usage{}, ErrTooManyBreakpoints
	}
	var lookups []int
	for _, bp := range bps {
		for j := bp; j >= 0 && j > bp-c.Lookback; j-- {
			lookups = append(lookups, j)
		}
	}
	return c.serve(blocks, lookups, bps, now), nil
}

// OpenAIUsage 是 Responses API 的口径：InputTokens 是总数，Cached 和 CacheWrite 是其中的两部分。
type OpenAIUsage struct{ InputTokens, Cached, CacheWrite int }

// OpenAI 模拟 GPT-5.6+：implicit 模式在最后一个“可作断点的消息结尾”放隐式断点，
// 并把更早的最多 20 个消息结尾也当查找边界；explicit 模式只认显式断点。
func (c *Cache) OpenAI(blocks []Block, implicit bool, now time.Time) (OpenAIUsage, error) {
	var explicit, endings []int
	for i, b := range blocks {
		if b.Breakpoint {
			explicit = append(explicit, i)
		}
		if b.Kind == "user" || b.Kind == "tool_result" {
			endings = append(endings, i)
		}
	}
	writes := append([]int(nil), explicit...)
	lookups := append([]int(nil), explicit...)
	if implicit && len(endings) > 0 {
		last := endings[len(endings)-1]
		writes = append(writes, last)
		from := len(endings) - 21
		if from < 0 {
			from = 0
		}
		lookups = append(lookups, endings[from:]...)
	}
	if len(writes) > 4 {
		return OpenAIUsage{}, ErrTooManyBreakpoints
	}
	u := c.serve(blocks, lookups, writes, now)
	return OpenAIUsage{InputTokens: u.Total(), Cached: u.Read, CacheWrite: u.Write}, nil
}
