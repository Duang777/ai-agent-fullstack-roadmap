package promptcache

// Rates 是相对“普通输入单价”的倍数。
type Rates struct {
	Read  float64 // 读缓存
	Write float64 // 写缓存
}

// 2026-10 文档摘录（以各家定价页为准）。
var (
	Anthropic5m = Rates{Read: 0.1, Write: 1.25}
	Anthropic1h = Rates{Read: 0.1, Write: 2.0}
	OpenAI56    = Rates{Read: 0.1, Write: 1.25}
	OpenAISol   = Rates{Read: 0.05, Write: 1.25} // GPT-6.1 Sol 读 0.05×
)

// Units 把一次用量换成“普通输入 token 当量”。乘以单价就是钱。
func (r Rates) Units(u Usage) float64 {
	return float64(u.Input) + float64(u.Read)*r.Read + float64(u.Write)*r.Write
}

// BreakEven 是 OpenAI 文档里的“最小长度陷阱”公式：
// 前缀 L 不够最小长度 M 时，把它补到 M 值不值？L > M(r + (w-r)/N) 就值。
func BreakEven(m int, r Rates, n int) float64 {
	return float64(m) * (r.Read + (r.Write-r.Read)/float64(n))
}

// GeminiExplicit 估算 Gemini 显式缓存（generateContent 的 cachedContents）的输入侧成本：
// 创建时按普通输入处理一次 + 存储费（token × 小时）+ 每次请求读缓存。
// 单价都是参数，示例值不代表官方价格。
func GeminiExplicit(cached, requests int, hours, readMult, storagePerTokenHour float64) float64 {
	return float64(cached) + float64(cached)*hours*storagePerTokenHour + float64(cached*requests)*readMult
}
