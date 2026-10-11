package llmbasics

import "math"

// Matrix 是行优先的二维矩阵：Matrix[i] 是第 i 个 token 的向量。
type Matrix [][]float64

// MatMul 计算 a(n×k) · b(k×m)。
func MatMul(a, b Matrix) Matrix {
	n, k, m := len(a), len(b), len(b[0])
	out := make(Matrix, n)
	for i := range n {
		out[i] = make([]float64, m)
		for j := range m {
			s := 0.0
			for t := range k {
				s += a[i][t] * b[t][j]
			}
			out[i][j] = s
		}
	}
	return out
}

func dot(a, b []float64) float64 {
	s := 0.0
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// Attention 是单头缩放点积注意力：softmax(Q·Kᵀ / √d + mask) · V。
//   - Q、K、V 都是 n×d：每一行对应一个位置；
//   - causal=true 时位置 i 只能看到 0..i（解码器 / GPT 的做法），
//     这正是“训练时能并行、生成时只能一个一个吐”的根源。
//
// 返回输出（n×d）和注意力权重（n×n，便于测试和观察）。
func Attention(q, k, v Matrix, causal bool) (Matrix, Matrix) {
	n, d := len(q), len(q[0])
	scale := 1 / math.Sqrt(float64(d))
	weights := make(Matrix, n)
	out := make(Matrix, n)
	for i := range n {
		scores := make([]float64, len(k))
		for j := range k {
			if causal && j > i {
				scores[j] = math.Inf(-1) // 未来的位置：softmax 后权重严格为 0
				continue
			}
			scores[j] = dot(q[i], k[j]) * scale
		}
		w := Softmax(scores)
		weights[i] = w
		out[i] = make([]float64, len(v[0]))
		for j := range v {
			if w[j] == 0 {
				continue
			}
			for t := range v[j] {
				out[i][t] += w[j] * v[j][t]
			}
		}
	}
	return out, weights
}

// KVCache 用于增量解码：每生成一个 token 只算它自己的 q、k、v，
// 把 k、v 追加进缓存，再用这一个 q 去看缓存里的所有 k、v。
// 结果必须和“每次把整段重新算一遍”完全一致，但计算量从 O(n²) 降到 O(n)。
// 代价是显存：缓存随上下文长度线性增长，这就是长上下文贵的原因之一。
type KVCache struct {
	K, V Matrix
}

// Step 追加一个位置并返回该位置的注意力输出。
func (c *KVCache) Step(q, k, v []float64) []float64 {
	c.K = append(c.K, k)
	c.V = append(c.V, v)
	scale := 1 / math.Sqrt(float64(len(q)))
	scores := make([]float64, len(c.K))
	for j := range c.K {
		scores[j] = dot(q, c.K[j]) * scale // 缓存里只有过去和现在，天然满足因果
	}
	w := Softmax(scores)
	out := make([]float64, len(v))
	for j := range c.V {
		for t := range c.V[j] {
			out[t] += w[j] * c.V[j][t]
		}
	}
	return out
}

// KVCacheBytes 估算缓存占用：2(K 和 V) × 层数 × KV 头数 × 头维度 × 上下文长度 × 每个数的字节数。
// 用来做课件里的显存估算。
func KVCacheBytes(layers, kvHeads, headDim, seqLen, bytesPerElem int) int {
	return 2 * layers * kvHeads * headDim * seqLen * bytesPerElem
}
