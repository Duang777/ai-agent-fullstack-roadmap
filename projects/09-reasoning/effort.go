// Package reasoning 是 1.4 课的练习：推理强度在三家之间的映射、推理 token 的用量与成本、
// 推理块在多轮工具调用里的生命周期（签名、前缀绑定、跨模型可读性），以及“什么时候值得多想”。
// 纯标准库，不调真实 API。
package reasoning

import (
	"fmt"
	"slices"
)

// Effort 是统一的推理强度。顺序有意义：越往后想得越多、越贵。
type Effort int

const (
	EffortNone Effort = iota
	EffortMinimal
	EffortLow
	EffortMedium
	EffortHigh
	EffortXHigh
	EffortMax
)

var effortNames = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

func (e Effort) String() string { return effortNames[e] }

func ParseEffort(s string) (Effort, error) {
	i := slices.Index(effortNames, s)
	if i < 0 {
		return 0, fmt.Errorf("unknown effort %q", s)
	}
	return Effort(i), nil
}

type Provider string

const (
	OpenAI    Provider = "openai"
	Anthropic Provider = "anthropic"
	Gemini    Provider = "gemini"
)

// Capability 描述一个模型能接受哪些推理强度。
// 下面的表摘自 2026 年 10 月三家文档，只用于练习；生产里应从模型页或 Models API 读取。
type Capability struct {
	Provider  Provider
	Supported []Effort // 能直接发送的强度
	Default   Effort   // 不传参数时的默认强度
	// CanDisable：能否彻底关掉推理；DisableMax：关掉推理时 effort 的上限（Anthropic 部分模型 xhigh/max 下不能关）。
	CanDisable bool
	DisableMax Effort
	// DisableAs：关闭推理用的写法（Anthropic Sonnet 5.5 是 between_tools，其他是 disabled）。
	DisableAs string
}

var Models = map[string]Capability{
	// OpenAI：reasoning.effort。GPT-6 Astra 不支持 none（传了 400）；GPT-6.1 Sol 不支持 none、minimal，默认 medium。
	"gpt-6-astra": {Provider: OpenAI, Supported: []Effort{EffortMinimal, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}, Default: EffortMedium},
	"gpt-6.1-sol": {Provider: OpenAI, Supported: []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}, Default: EffortMedium},
	"gpt-5.5":     {Provider: OpenAI, Supported: []Effort{EffortNone, EffortLow, EffortMedium, EffortHigh, EffortXHigh}, Default: EffortMedium},

	// Anthropic：thinking.type + output_config.effort。
	"claude-opus-5-5":   {Provider: Anthropic, Supported: []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}, Default: EffortMedium},
	"claude-sonnet-5-5": {Provider: Anthropic, Supported: []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}, Default: EffortHigh, CanDisable: true, DisableMax: EffortHigh, DisableAs: "between_tools"},
	"claude-haiku-5-5":  {Provider: Anthropic, Supported: []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}, Default: EffortMedium, CanDisable: true, DisableMax: EffortHigh, DisableAs: "disabled"},
	"claude-sonnet-4-6": {Provider: Anthropic, Supported: []Effort{EffortLow, EffortMedium, EffortHigh, EffortMax}, Default: EffortHigh, CanDisable: true, DisableMax: EffortMax, DisableAs: "disabled"},

	// Gemini：generation_config.thinking_level，只有 minimal/low/medium/high。
	"gemini-3.8-flash":       {Provider: Gemini, Supported: []Effort{EffortLow, EffortMedium, EffortHigh}, Default: EffortMedium},
	"gemini-3.6-flash":       {Provider: Gemini, Supported: []Effort{EffortMinimal, EffortLow, EffortMedium, EffortHigh}, Default: EffortMedium},
	"gemini-3.1-pro-preview": {Provider: Gemini, Supported: []Effort{EffortLow, EffortMedium, EffortHigh}, Default: EffortHigh},
}

// Resolved 是映射结果：要合并进请求体的字段，加上发生了哪些降级。
type Resolved struct {
	Body     map[string]any
	Effort   Effort   // 实际生效的强度
	Warnings []string // 被调整过时说明原因；Strict 模式下改为返回错误
}

// Resolve 把统一强度翻译成某个模型的请求参数。
// strict=true 时，不支持的强度直接报错（等价于服务端会返回的 400）；
// strict=false 时，取最近的受支持强度（距离相同取更便宜的那个），并记一条警告。
func Resolve(model string, want Effort, strict bool) (Resolved, error) {
	c, ok := Models[model]
	if !ok {
		return Resolved{}, fmt.Errorf("unknown model %q", model)
	}
	var r Resolved

	// “关掉推理”在 Anthropic 是单独的开关，先处理。
	if c.Provider == Anthropic && want == EffortNone {
		if c.CanDisable {
			r.Effort = EffortNone
			r.Body = map[string]any{"thinking": map[string]any{"type": c.DisableAs}}
			return r, nil
		}
		if strict {
			return r, fmt.Errorf("%s: thinking cannot be disabled", model)
		}
		r.Warnings = append(r.Warnings, model+": thinking cannot be disabled, using lowest effort")
	}

	eff := want
	if !slices.Contains(c.Supported, want) {
		if strict {
			return r, fmt.Errorf("%s does not support effort %q", model, want)
		}
		eff = nearest(c.Supported, want)
		r.Warnings = append(r.Warnings, fmt.Sprintf("%s: effort %s not supported, using %s", model, want, eff))
	}
	r.Effort = eff

	switch c.Provider {
	case OpenAI:
		r.Body = map[string]any{"reasoning": map[string]any{"effort": eff.String()}}
	case Anthropic:
		r.Body = map[string]any{
			"thinking":      map[string]any{"type": "adaptive"},
			"output_config": map[string]any{"effort": eff.String()},
		}
	case Gemini:
		r.Body = map[string]any{"generation_config": map[string]any{"thinking_level": eff.String()}}
	}
	return r, nil
}

// nearest 返回离 want 最近的受支持强度；距离相同取更低（更便宜）的。
func nearest(supported []Effort, want Effort) Effort {
	best, bestD := supported[0], 1<<30
	for _, s := range supported {
		d := int(s) - int(want)
		if d < 0 {
			d = -d
		}
		if d < bestD || (d == bestD && s < best) {
			best, bestD = s, d
		}
	}
	return best
}
