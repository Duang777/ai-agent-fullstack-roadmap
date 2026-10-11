// Package llmbasics 是 1.1 课的练习：手写字节级 BPE 分词器、采样器和因果注意力。
// 目的不是造一个能用的模型，而是把“文本 → token → logits → 下一个 token”这条链路
// 每一步都写成能测试的代码。
package llmbasics

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Pair 是相邻的两个 token id。
type Pair struct{ A, B int }

// Tokenizer 是字节级 BPE：
//   - id 0..255 固定表示单个字节，所以任何输入（中文、emoji、乱码）都能编码，永远不会有 <unk>；
//   - 训练时反复把出现最多的相邻 pair 合并成新 token，merges 按学到的先后顺序记录“优先级”。
type Tokenizer struct {
	merges map[Pair]int // pair -> 合并后的新 id；新 id 越小，学得越早，优先级越高
	vocab  [][]byte     // id -> 这个 token 对应的字节串
}

// pretokenRe 是简化版 GPT-2 预分词规则：
// 先把文本切成“可选的一个前导空格 + 一串字母/数字/其他符号”，BPE 只在每一段内部合并，
// 这样 "dog." 和 "dog!" 里的 "dog" 能学成同一个 token，合并也不会跨过空格。
var pretokenRe = regexp.MustCompile(`'s|'t|'re|'ve|'m|'ll|'d| ?\p{L}+| ?\p{N}+| ?[^\s\p{L}\p{N}]+|\s+`)

func pretokenize(text string) []string { return pretokenRe.FindAllString(text, -1) }

// Train 在 corpus 上学出 vocabSize-256 条合并规则。
func Train(corpus string, vocabSize int) (*Tokenizer, error) {
	if vocabSize < 256 {
		return nil, fmt.Errorf("vocabSize 至少 256（字节表），got %d", vocabSize)
	}
	t := newByteTokenizer()

	// 每个预分词片段 -> 出现次数。相同片段只需处理一次，乘上次数即可。
	counts := map[string]int{}
	for _, w := range pretokenize(corpus) {
		counts[w]++
	}
	// 为了训练结果确定（同一语料每次学出一样的 merges），按字典序处理片段。
	words := make([]string, 0, len(counts))
	for w := range counts {
		words = append(words, w)
	}
	sort.Strings(words)
	seqs := make([][]int, len(words))
	for i, w := range words {
		seqs[i] = bytesToIDs([]byte(w))
	}

	for next := 256; next < vocabSize; next++ {
		stats := map[Pair]int{}
		for i, s := range seqs {
			for j := 0; j+1 < len(s); j++ {
				stats[Pair{s[j], s[j+1]}] += counts[words[i]]
			}
		}
		best, ok := mostFrequent(stats)
		if !ok { // 所有片段都已经合并成单个 token，学不出新规则了
			break
		}
		t.merges[best] = next
		t.vocab = append(t.vocab, concat(t.vocab[best.A], t.vocab[best.B]))
		for i := range seqs {
			seqs[i] = merge(seqs[i], best, next)
		}
	}
	return t, nil
}

func newByteTokenizer() *Tokenizer {
	t := &Tokenizer{merges: map[Pair]int{}, vocab: make([][]byte, 256)}
	for i := range 256 {
		t.vocab[i] = []byte{byte(i)}
	}
	return t
}

// mostFrequent 选次数最多的 pair；次数相同时选 (A,B) 最小的，保证结果确定。
func mostFrequent(stats map[Pair]int) (Pair, bool) {
	var best Pair
	bestN := 0
	for p, n := range stats {
		if n > bestN || (n == bestN && less(p, best)) {
			best, bestN = p, n
		}
	}
	return best, bestN > 0
}

func less(a, b Pair) bool { return a.A < b.A || (a.A == b.A && a.B < b.B) }

// merge 把 seq 里所有不重叠的 p 替换成 id，从左往右扫。
func merge(seq []int, p Pair, id int) []int {
	out := make([]int, 0, len(seq))
	for i := 0; i < len(seq); {
		if i+1 < len(seq) && seq[i] == p.A && seq[i+1] == p.B {
			out = append(out, id)
			i += 2
			continue
		}
		out = append(out, seq[i])
		i++
	}
	return out
}

// Encode 把文本编码成 token id。
// 关键：编码时不是“哪个 pair 最多先合并”，而是“哪个 pair 学得最早先合并”，
// 这样才能复现训练时的合并顺序。
func (t *Tokenizer) Encode(text string) []int {
	var ids []int
	for _, w := range pretokenize(text) {
		seq := bytesToIDs([]byte(w))
		for len(seq) >= 2 {
			bestID, bestPair := -1, Pair{}
			for j := 0; j+1 < len(seq); j++ {
				p := Pair{seq[j], seq[j+1]}
				if id, ok := t.merges[p]; ok && (bestID == -1 || id < bestID) {
					bestID, bestPair = id, p
				}
			}
			if bestID == -1 {
				break // 没有任何可用的合并规则了
			}
			seq = merge(seq, bestPair, bestID)
		}
		ids = append(ids, seq...)
	}
	return ids
}

// Decode 把 id 拼回字节再转成字符串。单个 token 可能只是半个汉字，
// 只有拼完整段之后再转 string 才安全。
func (t *Tokenizer) Decode(ids []int) string {
	var b strings.Builder
	for _, id := range ids {
		b.Write(t.vocab[id])
	}
	return b.String()
}

// TokenBytes 返回单个 token 的原始字节（可能不是合法 UTF-8）。
func (t *Tokenizer) TokenBytes(id int) []byte { return t.vocab[id] }

// VocabSize 是词表大小：256 个字节 + 学到的合并数。
func (t *Tokenizer) VocabSize() int { return len(t.vocab) }

// StreamDecoder 用于流式输出：模型每吐一个 token 就要推给前端，
// 但 token 边界不一定是字符边界（cl100k_base 会把“智”拆成两个 token）。
// 它把不完整的 UTF-8 尾巴留在缓冲里，等下一个 token 补齐再输出。
type StreamDecoder struct {
	tok *Tokenizer
	buf []byte
}

func NewStreamDecoder(t *Tokenizer) *StreamDecoder { return &StreamDecoder{tok: t} }

// Push 喂一个 token，返回现在可以安全输出的文本（可能是空串）。
func (d *StreamDecoder) Push(id int) string {
	d.buf = append(d.buf, d.tok.vocab[id]...)
	n := completePrefix(d.buf)
	out := string(d.buf[:n])
	d.buf = append(d.buf[:0], d.buf[n:]...)
	return out
}

// Flush 在流结束时调用：把剩下的字节原样吐出（不合法的部分会变成 U+FFFD）。
func (d *StreamDecoder) Flush() string {
	out := string(d.buf)
	d.buf = d.buf[:0]
	return out
}

// completePrefix 返回 b 中可以安全解码的最长前缀长度：
// 只扣住结尾那个“开了头但还没收完”的多字节字符。
func completePrefix(b []byte) int {
	// UTF-8 一个字符最多 4 字节，所以只需要往回看最多 3 个字节找起始字节。
	for i := len(b) - 1; i >= 0 && i >= len(b)-3; i-- {
		c := b[i]
		if c&0xC0 == 0x80 { // 10xxxxxx：续字节，继续往前找
			continue
		}
		need := 0
		switch {
		case c&0x80 == 0x00:
			need = 1
		case c&0xE0 == 0xC0:
			need = 2
		case c&0xF0 == 0xE0:
			need = 3
		case c&0xF8 == 0xF0:
			need = 4
		default:
			return len(b) // 非法起始字节，没必要等，直接输出
		}
		if len(b)-i < need {
			return i // 这个字符还没收完
		}
		return len(b)
	}
	return len(b)
}

func bytesToIDs(b []byte) []int {
	ids := make([]int, len(b))
	for i, c := range b {
		ids[i] = int(c)
	}
	return ids
}

func concat(a, b []byte) []byte {
	out := make([]byte, 0, len(a)+len(b))
	return append(append(out, a...), b...)
}
