package context11

import "math"

// RecallModel 是一个“玩具”退化模型，只用来讲清两种现象的形状，参数不是任何模型的实测值：
//   - 长度退化（context rot）：上下文越长，整体召回越低；
//   - 位置效应（lost in the middle）：开头和结尾的信息比中间的更容易被用上。
type RecallModel struct {
	HalfAt float64 // 整体召回降到一半时的上下文 token 数
	Steep  float64 // 下降的陡峭程度
	Middle float64 // 正中间相对两端少掉的比例（0 表示没有位置效应）
}

var DefaultRecall = RecallModel{HalfAt: 400_000, Steep: 1.5, Middle: 0.3}

// Recall 返回放在第 pos 个 token 处的一条关键信息，在总长 n 的上下文里被正确用上的概率。
func (m RecallModel) Recall(pos, n int) float64 {
	base := 1 / (1 + math.Pow(float64(n)/m.HalfAt, m.Steep))
	x := (float64(pos) + 0.5) / float64(n)
	return base * (1 - m.Middle*4*x*(1-x))
}

// Stuff：把 docs 篇文档（每篇 docTokens）全塞进去，答案在第 idx 篇。
func (m RecallModel) Stuff(docs, docTokens, idx int) float64 {
	n := docs * docTokens
	return m.Recall(idx*docTokens+docTokens/2, n)
}

// Retrieve：先检索 topK 篇（检索器召回率 hit），把最相关的放在最后（离问题最近）。
func (m RecallModel) Retrieve(topK, docTokens int, hit float64) float64 {
	n := topK * docTokens
	return hit * m.Recall(n-docTokens/2, n)
}
