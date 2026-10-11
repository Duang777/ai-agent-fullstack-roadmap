package llmbasics

import (
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func corpus(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/corpus.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func trained(t *testing.T, vocab int) *Tokenizer {
	t.Helper()
	tok, err := Train(corpus(t), vocab)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// ---------- 分词器 ----------

// 字节级 BPE 的第一条性质：任何字节串都能编码，并且无损还原。
func TestBPERoundTrip(t *testing.T) {
	tok := trained(t, 512)
	cases := []string{
		"",
		"The model calls a tool.",
		"智能体调用工具，然后读取结果。",
		"emoji 也行 🤖🚀",
		"没见过的词：Kubernetes、量子纠缠",
		"  多个空格\t和\n换行  ",
		string([]byte{0xff, 0xfe, 0x00}), // 非法 UTF-8 也要原样还原
	}
	for _, s := range cases {
		ids := tok.Encode(s)
		if got := tok.Decode(ids); got != s {
			t.Errorf("round trip %q: got %q (ids=%v)", s, got, ids)
		}
	}
}

// 第二条性质：训练确定。同样的语料、同样的词表大小，学出同样的合并规则。
func TestBPETrainDeterministic(t *testing.T) {
	a, b := trained(t, 400), trained(t, 400)
	if a.VocabSize() != b.VocabSize() {
		t.Fatalf("vocab size differ: %d vs %d", a.VocabSize(), b.VocabSize())
	}
	for id := range a.VocabSize() {
		if string(a.TokenBytes(id)) != string(b.TokenBytes(id)) {
			t.Fatalf("token %d differ: %q vs %q", id, a.TokenBytes(id), b.TokenBytes(id))
		}
	}
}

// 经典例子：在 "aaabdaaabac" 上第一条学到的合并是 "aa"（出现 4 次）。
// 这里没有空格，预分词只切出一段，正好对应教科书里的手算过程。
func TestBPEFirstMerge(t *testing.T) {
	tok, err := Train("aaabdaaabac", 257)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(tok.TokenBytes(256)); got != "aa" {
		t.Fatalf("first merge = %q, want \"aa\"", got)
	}
}

// 编码必须按“学到的先后”合并，而不是“从左往右遇到哪个先合并哪个”。
// 手工构造：先学会 "bc"（256），后学会 "ab"（257）。
// "abc" 按规则应切成 a + bc；如果从左扫描先合并 ab，就会得到 ab + c，和训练时的切法不一致。
func TestBPEEncodeFollowsMergeRank(t *testing.T) {
	tok := newByteTokenizer()
	tok.merges[Pair{'b', 'c'}] = 256
	tok.vocab = append(tok.vocab, []byte("bc"))
	tok.merges[Pair{'a', 'b'}] = 257
	tok.vocab = append(tok.vocab, []byte("ab"))
	got := tok.Encode("abc")
	if !slices.Equal(got, []int{'a', 256}) {
		t.Fatalf("Encode(abc) = %q, want [a bc]", pieces(tok, got))
	}
}

// 词表越大，同一段文本切出的 token 越少；常见词会变成一个 token。
func TestBPECompression(t *testing.T) {
	text := "The model reads the context and calls a tool."
	prev := math.MaxInt
	for _, v := range []int{256, 300, 400, 600} {
		n := len(trained(t, v).Encode(text))
		if n > prev {
			t.Errorf("vocab %d: %d tokens, more than smaller vocab (%d)", v, n, prev)
		}
		prev = n
	}
	tok := trained(t, 600)
	ids := tok.Encode(" model")
	if len(ids) != 1 {
		t.Errorf(`" model" should be one token with vocab 600, got %d: %q`, len(ids), pieces(tok, ids))
	}
	if got := len(trained(t, 256).Encode(text)); got != len(text) {
		t.Errorf("vocab 256 must be pure bytes: %d tokens for %d bytes", got, len(text))
	}
}

// 预分词保证合并不会跨空格：每个 token 最多以一个空格开头，空格不会出现在 token 中间或结尾。
func TestBPENoMergeAcrossSpaces(t *testing.T) {
	tok := trained(t, 600)
	for id := 256; id < tok.VocabSize(); id++ {
		s := string(tok.TokenBytes(id))
		if strings.TrimSpace(s) == "" {
			continue // 纯空白 token（比如连续空格）是允许的
		}
		if strings.Contains(strings.TrimPrefix(s, " "), " ") {
			t.Errorf("token %d %q crosses a space", id, s)
		}
	}
}

// 中文在“小词表 + 以英文为主的语料”下更费 token：一个汉字 3 个字节，常常切成多个 token。
func TestChineseCostsMoreTokens(t *testing.T) {
	tok := trained(t, 400)
	en := "The tool returns the result."
	zh := "工具返回结果。"
	perCharEN := float64(len(tok.Encode(en))) / float64(utf8.RuneCountInString(en))
	perCharZH := float64(len(tok.Encode(zh))) / float64(utf8.RuneCountInString(zh))
	if perCharZH <= perCharEN {
		t.Errorf("tokens per char: zh=%.2f en=%.2f, expected zh > en", perCharZH, perCharEN)
	}
	t.Logf("tokens/字符：英文 %.2f，中文 %.2f", perCharEN, perCharZH)
}

// 流式解码：token 边界可能在汉字中间，直接 string(tokenBytes) 会输出乱码。
func TestStreamDecoderNeverSplitsRunes(t *testing.T) {
	tok := trained(t, 256) // 纯字节词表：每个汉字一定被拆成 3 个 token，最极端
	text := "首字延迟🤖是用户能感觉到的"
	d := NewStreamDecoder(tok)
	var got strings.Builder
	for _, id := range tok.Encode(text) {
		chunk := d.Push(id)
		if !utf8.ValidString(chunk) {
			t.Fatalf("emitted invalid UTF-8 chunk %q", chunk)
		}
		got.WriteString(chunk)
	}
	got.WriteString(d.Flush())
	if got.String() != text {
		t.Fatalf("got %q, want %q", got.String(), text)
	}

	// 反例：不做缓冲的写法一定会吐出非法 UTF-8。
	naiveBroken := false
	for _, id := range tok.Encode(text) {
		if !utf8.Valid(tok.TokenBytes(id)) {
			naiveBroken = true
		}
	}
	if !naiveBroken {
		t.Fatal("expected naive per-token decode to produce invalid UTF-8")
	}
}

// ---------- 采样 ----------

func TestSoftmaxStable(t *testing.T) {
	p := Softmax([]float64{1000, 1001, 1002})
	sum := 0.0
	for _, q := range p {
		if math.IsNaN(q) || math.IsInf(q, 0) {
			t.Fatalf("softmax overflowed: %v", p)
		}
		sum += q
	}
	if math.Abs(sum-1) > 1e-12 {
		t.Fatalf("sum = %v", sum)
	}
	// softmax 只看差值：整体加常数不改变结果。
	q := Softmax([]float64{0, 1, 2})
	for i := range p {
		if math.Abs(p[i]-q[i]) > 1e-12 {
			t.Fatalf("shift invariance broken: %v vs %v", p, q)
		}
	}
	if z := Softmax([]float64{math.Inf(-1), 0}); z[0] != 0 || z[1] != 1 {
		t.Fatalf("-Inf should give exactly 0: %v", z)
	}
}

// 温度越低分布越尖（熵越小），越高越平。
func TestTemperatureEntropy(t *testing.T) {
	logits := []float64{2.0, 1.0, 0.5, 0.1, -1}
	prev := -1.0
	for _, temp := range []float64{0.2, 0.5, 1, 1.5, 3} {
		h := Entropy(Softmax(Process(logits, nil, SamplingParams{Temperature: temp})))
		if h <= prev {
			t.Errorf("T=%.1f entropy %.3f not greater than %.3f", temp, h, prev)
		}
		prev = h
	}
}

func TestGreedyIsDeterministic(t *testing.T) {
	logits := []float64{0.1, 3.2, 3.1, -2}
	for seed := range uint64(20) {
		rng := rand.New(rand.NewPCG(seed, 0))
		if got := Sample(logits, nil, SamplingParams{Temperature: 0}, rng); got != 1 {
			t.Fatalf("greedy picked %d", got)
		}
	}
}

func TestTopK(t *testing.T) {
	logits := []float64{1, 5, 3, 4, 2}
	out := Process(logits, nil, SamplingParams{Temperature: 1, TopK: 2})
	kept := survivors(out)
	if !slices.Equal(kept, []int{1, 3}) {
		t.Fatalf("top-k=2 kept %v, want [1 3]", kept)
	}
}

// top-p 保留“累计概率刚好达到 p 的最小集合”：包括跨过阈值的那一个。
func TestTopP(t *testing.T) {
	// 概率 0.5、0.3、0.15、0.05
	logits := []float64{math.Log(0.5), math.Log(0.3), math.Log(0.15), math.Log(0.05)}
	cases := []struct {
		p    float64
		want []int
	}{
		{0.5, []int{0}},       // 0.5 本身就达到了
		{0.6, []int{0, 1}},    // 0.5 不够，加上 0.3
		{0.9, []int{0, 1, 2}}, // 0.8 不够，加上 0.15
		{0.99, []int{0, 1, 2, 3}},
	}
	for _, c := range cases {
		got := survivors(Process(logits, nil, SamplingParams{Temperature: 1, TopP: c.p}))
		if !slices.Equal(got, c.want) {
			t.Errorf("top-p=%.2f kept %v, want %v", c.p, got, c.want)
		}
	}
}

// min-p 的阈值随最大概率缩放：模型很确定时砍得狠，不确定时留得多。
func TestMinPAdaptsToConfidence(t *testing.T) {
	confident := []float64{math.Log(0.9), math.Log(0.05), math.Log(0.03), math.Log(0.02)}
	unsure := []float64{math.Log(0.3), math.Log(0.28), math.Log(0.22), math.Log(0.2)}
	p := SamplingParams{Temperature: 1, MinP: 0.1}
	if got := survivors(Process(confident, nil, p)); !slices.Equal(got, []int{0}) {
		t.Errorf("confident: kept %v, want [0]", got)
	}
	if got := survivors(Process(unsure, nil, p)); len(got) != 4 {
		t.Errorf("unsure: kept %v, want all 4", got)
	}
}

func TestPenaltiesAndBias(t *testing.T) {
	logits := []float64{2, 2, -1, 0}
	hist := []int{0, 0, 0, 2}

	freq := Process(logits, hist, SamplingParams{Temperature: 1, FrequencyPenalty: 0.5})
	if freq[0] != 2-1.5 || freq[2] != -1-0.5 || freq[1] != 2 {
		t.Errorf("frequency penalty: %v", freq)
	}
	pres := Process(logits, hist, SamplingParams{Temperature: 1, PresencePenalty: 0.5})
	if pres[0] != 1.5 || pres[2] != -1.5 {
		t.Errorf("presence penalty: %v", pres)
	}
	// 乘法惩罚：正数除、负数乘，两者都让 token 更不可能。
	rep := Process(logits, hist, SamplingParams{Temperature: 1, RepetitionPenalty: 2})
	if rep[0] != 1 || rep[2] != -2 || rep[1] != 2 {
		t.Errorf("repetition penalty: %v", rep)
	}

	// logit bias -100 等于禁用；贪心模式下也要生效。
	rng := rand.New(rand.NewPCG(1, 2))
	if got := Sample([]float64{5, 1}, nil, SamplingParams{LogitBias: map[int]float64{0: -100}}, rng); got != 1 {
		t.Errorf("bias ignored under greedy: picked %d", got)
	}
}

// 同一个种子 → 同一串样本；经验频率接近真实分布。
func TestSampleSeedAndDistribution(t *testing.T) {
	logits := []float64{math.Log(0.6), math.Log(0.3), math.Log(0.1)}
	p := SamplingParams{Temperature: 1}
	run := func(seed uint64) []int {
		rng := rand.New(rand.NewPCG(seed, 0))
		out := make([]int, 20000)
		for i := range out {
			out[i] = Sample(logits, nil, p, rng)
		}
		return out
	}
	a, b := run(42), run(42)
	if !slices.Equal(a, b) {
		t.Fatal("same seed gave different samples")
	}
	freq := make([]float64, 3)
	for _, id := range a {
		freq[id]++
	}
	for i, want := range []float64{0.6, 0.3, 0.1} {
		got := freq[i] / float64(len(a))
		if math.Abs(got-want) > 0.015 {
			t.Errorf("token %d: freq %.3f, want ≈%.1f", i, got, want)
		}
	}
	// 被 top-k 砍掉的 token 一次都不能被抽中。
	rng := rand.New(rand.NewPCG(7, 0))
	for range 5000 {
		if Sample(logits, nil, SamplingParams{Temperature: 2, TopK: 1}, rng) != 0 {
			t.Fatal("sampled a token removed by top-k")
		}
	}
}

// ---------- 注意力 ----------

func randMatrix(rng *rand.Rand, n, d int) Matrix {
	m := make(Matrix, n)
	for i := range m {
		m[i] = make([]float64, d)
		for j := range m[i] {
			m[i][j] = rng.NormFloat64()
		}
	}
	return m
}

// 因果掩码：改动未来位置的 K、V，不能影响过去位置的输出。
func TestCausalMask(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 3))
	q, k, v := randMatrix(rng, 6, 8), randMatrix(rng, 6, 8), randMatrix(rng, 6, 8)
	out1, w := Attention(q, k, v, true)
	for i := range w {
		sum := 0.0
		for j, x := range w[i] {
			if j > i && x != 0 {
				t.Fatalf("position %d attends to future %d: %v", i, j, x)
			}
			sum += x
		}
		if math.Abs(sum-1) > 1e-9 {
			t.Fatalf("row %d weights sum %v", i, sum)
		}
	}
	k[5], v[5] = randMatrix(rng, 1, 8)[0], randMatrix(rng, 1, 8)[0]
	out2, _ := Attention(q, k, v, true)
	for i := range 5 {
		if !approxEqual(out1[i], out2[i]) {
			t.Fatalf("position %d changed after editing the future", i)
		}
	}
	if approxEqual(out1[5], out2[5]) {
		t.Fatal("last position should change")
	}
	// 不加掩码时，过去位置会受影响（这就是编码器 / BERT 的双向注意力）。
	full1, _ := Attention(q, k, v, false)
	k[5] = randMatrix(rng, 1, 8)[0]
	full2, _ := Attention(q, k, v, false)
	if approxEqual(full1[0], full2[0]) {
		t.Fatal("bidirectional attention should see the future")
	}
}

// KV Cache 增量计算和整段重算必须完全一致。
func TestKVCacheMatchesFull(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 9))
	q, k, v := randMatrix(rng, 10, 16), randMatrix(rng, 10, 16), randMatrix(rng, 10, 16)
	full, _ := Attention(q, k, v, true)
	var c KVCache
	for i := range 10 {
		if got := c.Step(q[i], k[i], v[i]); !approxEqual(got, full[i]) {
			t.Fatalf("step %d differs from full attention", i)
		}
	}
	// 显存估算：8B 级别模型（32 层、8 个 KV 头、头维度 128）在 fp16 下，每 token 128 KiB。
	if got := KVCacheBytes(32, 8, 128, 1, 2); got != 128*1024 {
		t.Fatalf("kv bytes per token = %d", got)
	}
}

// 不除以 √d 时，维度越大点积越大，softmax 越接近 one-hot（梯度消失）。
func TestScalingKeepsSoftmaxSoft(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 5))
	d := 512
	q, k := randMatrix(rng, 1, d), randMatrix(rng, 32, d)
	scores := make([]float64, len(k))
	scaled := make([]float64, len(k))
	for j := range k {
		scores[j] = dot(q[0], k[j])
		scaled[j] = scores[j] / math.Sqrt(float64(d))
	}
	hRaw, hScaled := Entropy(Softmax(scores)), Entropy(Softmax(scaled))
	if hRaw >= hScaled {
		t.Fatalf("entropy raw %.3f should be < scaled %.3f", hRaw, hScaled)
	}
	t.Logf("d=%d 时注意力熵：不缩放 %.3f bit，缩放后 %.3f bit（上限 %.3f）", d, hRaw, hScaled, math.Log2(32))
}

// ---------- 串起来 ----------

func TestGenerateLoop(t *testing.T) {
	tok := trained(t, 500)
	m := TrainBigram(tok.Encode(corpus(t)), tok.VocabSize())
	prompt := tok.Encode("The model")
	stop := map[int]bool{}
	for _, id := range tok.Encode(".") {
		stop[id] = true
	}

	greedy := func() GenerateResult {
		return Generate(m, prompt, 30, stop, SamplingParams{Temperature: 0}, rand.New(rand.NewPCG(0, 0)))
	}
	a, b := greedy(), greedy()
	if !slices.Equal(a.IDs, b.IDs) {
		t.Fatal("greedy generation not deterministic")
	}
	t.Logf("贪心：%q（%s）", tok.Decode(a.IDs), a.FinishReason)

	r := Generate(m, prompt, 5, map[int]bool{}, SamplingParams{Temperature: 1}, rand.New(rand.NewPCG(1, 1)))
	if r.FinishReason != "length" || len(r.IDs) != 5 {
		t.Fatalf("want 5 tokens with finish_reason=length, got %d %s", len(r.IDs), r.FinishReason)
	}

	// 不同种子、高温度：至少有两种不同输出。
	seen := map[string]bool{}
	for seed := range uint64(10) {
		r := Generate(m, prompt, 12, stop, SamplingParams{Temperature: 1.2, TopP: 0.95}, rand.New(rand.NewPCG(seed, 0)))
		seen[tok.Decode(r.IDs)] = true
	}
	if len(seen) < 2 {
		t.Fatal("sampling with temperature should vary across seeds")
	}
}

// ---------- 工具函数 ----------

func survivors(logits []float64) []int {
	var ids []int
	for i, l := range logits {
		if !math.IsInf(l, -1) {
			ids = append(ids, i)
		}
	}
	return ids
}

func approxEqual(a, b []float64) bool {
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-9 {
			return false
		}
	}
	return true
}

func pieces(tok *Tokenizer, ids []int) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(tok.TokenBytes(id))
	}
	return out
}
