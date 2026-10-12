package structured

import "encoding/json"

// 约束解码的核心问题：给定已经生成的前缀，它还能不能被续写成一个符合 schema 的 JSON？
//
// 这里用“续体传递”（continuation-passing）写一个递归下降解析器：每个解析函数接收
// “我解析完之后接着做什么”的回调 k。遇到分支（anyOf、可选字段、数字可以继续也可以结束）时，
// 每条路都试一遍，取最好的结果。前缀在中途用完，说明“到这里为止还没错”，返回 Prefix。
//
// 为了让语法简单、确定，生成的 JSON 是紧凑格式：没有空白，对象的键按 schema 里的顺序出现
// （和 OpenAI strict 模式的行为一致：输出按 schema 的键顺序生成）。

// Status 是前缀检查的结果。
type Status int

const (
	Invalid  Status = iota // 无论再接什么都不可能合法
	Prefix                 // 合法前缀，还没结束
	Complete               // 已经是一个完整、合法的值（有时还能继续，比如数字）
)

func (s Status) String() string { return [...]string{"invalid", "prefix", "complete"}[s] }

type checker struct {
	s      []rune
	budget int // 防止病态 schema 指数爆炸
}

// Check 判断 text 相对 schema 的状态。
func (sc *Schema) Check(text string) Status {
	c := &checker{s: []rune(text), budget: 200000}
	return c.value(0, sc, func(i int) Status {
		if i == len(c.s) {
			return Complete
		}
		return Invalid // 值后面不允许再有任何字符
	})
}

type cont func(int) Status

func best(a, b Status) Status {
	if a > b {
		return a
	}
	return b
}

func (c *checker) eof(i int) bool { return i >= len(c.s) }

// lit 匹配一段固定文本。前缀在中途用完也算合法。
func (c *checker) lit(i int, s string, k cont) Status {
	for _, r := range s {
		if c.eof(i) {
			return Prefix
		}
		if c.s[i] != r {
			return Invalid
		}
		i++
	}
	return k(i)
}

func (c *checker) value(i int, sc *Schema, k cont) Status {
	c.budget--
	if c.budget < 0 {
		return Invalid
	}
	sc = sc.resolve()
	if c.eof(i) {
		return Prefix
	}
	if len(sc.AnyOf) > 0 {
		r := Invalid
		for _, b := range sc.AnyOf {
			r = best(r, c.value(i, b, k))
			if r == Complete {
				break
			}
		}
		return r
	}
	if sc.HasConst {
		return c.literalValue(i, sc.Const, k)
	}
	if len(sc.Enum) > 0 {
		r := Invalid
		for _, e := range sc.Enum {
			r = best(r, c.literalValue(i, e, k))
		}
		return r
	}
	r := Invalid
	for _, t := range c.types(sc) {
		switch t {
		case "object":
			r = best(r, c.object(i, sc, k))
		case "array":
			r = best(r, c.array(i, sc, k))
		case "string":
			r = best(r, c.str(i, sc, k))
		case "integer":
			r = best(r, c.number(i, false, k))
		case "number":
			r = best(r, c.number(i, true, k))
		case "boolean":
			r = best(r, best(c.lit(i, "true", k), c.lit(i, "false", k)))
		case "null":
			r = best(r, c.lit(i, "null", k))
		}
	}
	return r
}

func (c *checker) types(sc *Schema) []string {
	if len(sc.Types) > 0 {
		return sc.Types
	}
	if len(sc.Props) > 0 {
		return []string{"object"}
	}
	return []string{"object", "array", "string", "number", "boolean", "null"}
}

// literalValue：enum / const 的每个候选值都编码成紧凑 JSON，再当固定文本匹配。
func (c *checker) literalValue(i int, v any, k cont) Status {
	b, err := json.Marshal(v)
	if err != nil {
		return Invalid
	}
	return c.lit(i, string(b), k)
}

// object：{ 然后按 schema 顺序逐个字段；可选字段可以跳过；不允许额外字段。
func (c *checker) object(i int, sc *Schema, k cont) Status {
	return c.lit(i, "{", func(j int) Status {
		return c.fields(j, sc, 0, true, k)
	})
}

func (c *checker) fields(i int, sc *Schema, idx int, first bool, k cont) Status {
	if idx == len(sc.Props) {
		return c.lit(i, "}", k)
	}
	p := sc.Props[idx]
	key, _ := json.Marshal(p.Name)
	sep := ","
	if first {
		sep = ""
	}
	take := c.lit(i, sep+string(key)+":", func(j int) Status {
		return c.value(j, p.Schema, func(m int) Status {
			return c.fields(m, sc, idx+1, false, k)
		})
	})
	if sc.Required[p.Name] || take == Complete {
		return take
	}
	return best(take, c.fields(i, sc, idx+1, first, k)) // 可选字段：也可以跳过
}

func (c *checker) array(i int, sc *Schema, k cont) Status {
	minN, maxN := 0, -1
	if sc.MinItems != nil {
		minN = *sc.MinItems
	}
	if sc.MaxItems != nil {
		maxN = *sc.MaxItems
	}
	items := sc.Items
	if items == nil {
		items = &Schema{root: sc.root}
	}
	var elems func(j, n int) Status
	elems = func(j, n int) Status {
		r := Invalid
		if n >= minN {
			r = c.lit(j, "]", k)
		}
		if maxN < 0 || n < maxN {
			sep := ","
			if n == 0 {
				sep = ""
			}
			r = best(r, c.lit(j, sep, func(m int) Status {
				return c.value(m, items, func(e int) Status { return elems(e, n+1) })
			}))
		}
		return r
	}
	return c.lit(i, "[", func(j int) Status { return elems(j, 0) })
}

// str：JSON 字符串。控制字符不允许出现，转义只认标准的几种。
func (c *checker) str(i int, sc *Schema, k cont) Status {
	if c.s[i] != '"' {
		return Invalid
	}
	i++
	n := 0 // 已经生成的字符数（练习 3 用它实现 maxLength）
	for {
		if c.eof(i) {
			return Prefix
		}
		r := c.s[i]
		switch {
		case r == '"':
			return k(i + 1)
		case r < 0x20:
			return Invalid
		case r == '\\':
			i++
			if c.eof(i) {
				return Prefix
			}
			switch c.s[i] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				i++
			case 'u':
				i++
				for h := 0; h < 4; h++ {
					if c.eof(i) {
						return Prefix
					}
					if !isHex(c.s[i]) {
						return Invalid
					}
					i++
				}
			default:
				return Invalid
			}
		default:
			i++
		}
		n++
	}
}

func isHex(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// number：-?(0|[1-9][0-9]*)(\.[0-9]+)?  —— 不支持指数，够用。
// 数字没有结束符：“12” 已经完整，但还可以继续变成 “123”。这里贪心地吃完所有数字再交给 k，
// 因为数字后面不可能紧跟另一个数字。前缀恰好停在数字中间时，k 在末尾会返回
// Prefix（后面还能接 , } ]）或 Complete（顶层），两种情况都不是 Invalid，正好表达“可以结束也可以继续”。
func (c *checker) number(i int, frac bool, k cont) Status {
	if c.s[i] == '-' {
		i++
		if c.eof(i) {
			return Prefix
		}
	}
	if !isDigit(c.s[i]) {
		return Invalid
	}
	if c.s[i] == '0' {
		i++ // 0 后面不能再跟数字：01 不合法
	} else {
		for !c.eof(i) && isDigit(c.s[i]) {
			i++
		}
	}
	if !frac || c.eof(i) || c.s[i] != '.' {
		return k(i)
	}
	i++ // '.'
	if c.eof(i) {
		return Prefix
	}
	if !isDigit(c.s[i]) {
		return Invalid
	}
	for !c.eof(i) && isDigit(c.s[i]) {
		i++
	}
	return k(i)
}
