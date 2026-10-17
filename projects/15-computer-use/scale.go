package cua

import "math"

// AnthropicScale：截图要先缩到模型的图片上限以内（API 不会替你缩，超了直接拒绝）。
// 例：上限长边 1568、总像素约 115 万（较早的模型）。返回缩放系数，≤1。
func AnthropicScale(w, h, maxLongEdge int, maxPixels float64) float64 {
	long := math.Max(float64(w), float64(h))
	s := math.Min(1, float64(maxLongEdge)/long)
	return math.Min(s, math.Sqrt(maxPixels/float64(w*h)))
}

// ToScreen 把模型给的截图坐标换回真实屏幕坐标：模型看到的是缩小后的图，坐标也在缩小后的空间里。
func ToScreen(x, y int, scale float64) (int, int) {
	return int(math.Round(float64(x) / scale)), int(math.Round(float64(y) / scale))
}

// Denormalize：Gemini 的坐标是 0–999 的归一化值，和截图尺寸无关，要按真实宽高换算。
func Denormalize(v, size int) int { return v * size / 1000 }

// VisualTokens：高分辨率档模型的图片 token 估算，⌈w/28⌉ × ⌈h/28⌉。
func VisualTokens(w, h int) int { return ((w + 27) / 28) * ((h + 27) / 28) }
