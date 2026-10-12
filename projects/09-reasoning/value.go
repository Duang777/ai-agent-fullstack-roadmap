package reasoning

import "math"

// “什么时候值得多想”是一道经济题：多花的推理 token，能换来多少正确率？
// 这里用一个可以精确计算的模型来回答，不用随机数。

// Tier 是某个推理强度下的表现：在某类任务上的成功率，以及平均输出 token（含推理）。
type Tier struct {
	Effort  Effort
	Success float64
	Tokens  float64
}

// Expect 是一种策略在一类任务上的期望：成功率和期望 token。
type Expect struct {
	Success float64
	Tokens  float64
}

// Fixed：永远用同一个强度，跑一次。
func Fixed(t Tier) Expect { return Expect{t.Success, t.Tokens} }

// Escalate：从低到高依次尝试，每次失败后升一级，直到成功或用完。
// 前提是有一个可靠的验证器（单元测试、Schema 校验、能执行的检查）判断“成功了没有”。
// 假设各次尝试相互独立：
//
//	成功率 = 1 - Π(1 - p_i)
//	期望 token = Σ tokens_i × Π_{j<i}(1 - p_j)     （只有前面都失败，才会跑到第 i 级）
func Escalate(tiers []Tier) Expect {
	fail, tokens := 1.0, 0.0
	for _, t := range tiers {
		tokens += fail * t.Tokens
		fail *= 1 - t.Success
	}
	return Expect{1 - fail, tokens}
}

// Mix 按比例混合多类任务（比如 70% 简单、30% 困难）。
func Mix(weights []float64, parts []Expect) Expect {
	var e Expect
	for i, w := range weights {
		e.Success += w * parts[i].Success
		e.Tokens += w * parts[i].Tokens
	}
	return e
}

// MajorityAccuracy：同一个问题独立采样 n 次（n 为奇数）、多数投票的正确率。
// 这是另一种“测试时计算”（self-consistency）：不让单次想得更久，而是多想几次再投票。
// 前提：答案能比较是否相同（选择题、数值），而且单次正确率 p > 0.5。
func MajorityAccuracy(p float64, n int) float64 {
	acc := 0.0
	for k := n/2 + 1; k <= n; k++ {
		acc += binom(n, k) * math.Pow(p, float64(k)) * math.Pow(1-p, float64(n-k))
	}
	return acc
}

func binom(n, k int) float64 {
	r := 1.0
	for i := 1; i <= k; i++ {
		r = r * float64(n-k+i) / float64(i)
	}
	return r
}

// TokensPerCorrect 是“每答对一题平均花多少 token”，比较策略时比单看成功率或单看成本都公平。
func (e Expect) TokensPerCorrect() float64 {
	if e.Success == 0 {
		return math.Inf(1)
	}
	return e.Tokens / e.Success
}
