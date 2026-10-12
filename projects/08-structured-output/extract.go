package structured

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Generator 是“调一次模型”的抽象。feedback 是上一轮的校验错误，第一次为空。
// 返回模型的文本输出和 stop（"end"、"length"、"refusal"，和 1.2 课的统一口径一致）。
type Generator interface {
	Generate(ctx context.Context, feedback []string) (text, stop string, err error)
}

var (
	ErrRefusal   = errors.New("structured: model refused")
	ErrTruncated = errors.New("structured: output truncated by max tokens")
)

// ExtractError：重试用完还是不合法。
type ExtractError struct {
	Attempts int
	Last     []string
}

func (e *ExtractError) Error() string {
	return fmt.Sprintf("structured: still invalid after %d attempts: %s", e.Attempts, strings.Join(e.Last, "; "))
}

// Extract 是结构化输出在应用层的标准循环：
//
//	生成 → 去掉代码围栏 → 解析 → 按“原始” schema 校验 → 有错就把错误原文喂回去重试
//
// 即使用了 strict 模式也要校验：转换时删掉的约束（minimum、maxLength……）、
// 拒答、截断、enum 大小写，都会让结果不符合你真正的 schema。
func Extract(ctx context.Context, g Generator, sc *Schema, maxAttempts int) (any, int, error) {
	var feedback []string
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		text, stop, err := g.Generate(ctx, feedback)
		if err != nil {
			return nil, attempt, err
		}
		switch stop {
		case "refusal":
			return nil, attempt, ErrRefusal // 拒答不重试：换个说法再问是另一回事
		case "length":
			return nil, attempt, ErrTruncated // 同样的上限重试还是会截断，要调大上限
		}
		v, err := ParseOrdered([]byte(stripFence(text)))
		if err != nil {
			feedback = []string{"输出不是合法 JSON：" + err.Error()}
			continue
		}
		errs := sc.Validate(v)
		if len(errs) == 0 {
			return v, attempt, nil
		}
		feedback = feedback[:0]
		for _, e := range errs {
			feedback = append(feedback, e.Error())
		}
	}
	return nil, maxAttempts, &ExtractError{Attempts: maxAttempts, Last: feedback}
}

// stripFence 去掉模型常见的 ```json ... ``` 包裹和首尾空白。
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}

// NormalizeEnums 处理 Anthropic 文档里写明的坑：enum / const 的大小写不保证。
// 值和某个候选只差大小写时，改成候选的原样。返回改了几处。
func NormalizeEnums(sc *Schema, v any) (any, int) {
	n := 0
	var fix func(s *Schema, v any) any
	fix = func(s *Schema, v any) any {
		s = s.resolve()
		switch t := v.(type) {
		case string:
			cands := s.Enum
			if s.HasConst {
				cands = append(cands, s.Const)
			}
			for _, e := range cands {
				if es, ok := e.(string); ok && es != t && strings.EqualFold(es, t) {
					n++
					return es
				}
			}
		case Obj:
			for i, kv := range t {
				if ps := s.prop(kv.Key); ps != nil {
					t[i].Val = fix(ps, kv.Val)
				}
			}
		case []any:
			if s.Items != nil {
				for i := range t {
					t[i] = fix(s.Items, t[i])
				}
			}
		}
		return v
	}
	return fix(sc, v), n
}
