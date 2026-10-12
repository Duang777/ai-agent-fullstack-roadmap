package structured

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// 三家都说“支持 JSON Schema”，但支持的都是子集，而且子集不一样。
// 这里把 2026 年 10 月官方文档里的规则写成代码：先查，再按需转换。

// Issue 是一条不兼容项。
type Issue struct {
	Path string
	Msg  string
}

func (i Issue) String() string { return i.Path + ": " + i.Msg }

type walker func(path string, o Obj, depth int)

func walk(doc any, path string, depth int, f walker) {
	o, ok := doc.(Obj)
	if !ok {
		return
	}
	f(path, o, depth)
	if props, ok := o.Get("properties"); ok {
		for _, p := range props.(Obj) {
			walk(p.Val, path+".properties."+p.Key, depth+1, f)
		}
	}
	for _, key := range []string{"items", "additionalProperties"} {
		if v, ok := o.Get(key); ok {
			walk(v, path+"."+key, depth+1, f)
		}
	}
	for _, key := range []string{"anyOf", "allOf", "oneOf"} {
		if v, ok := o.Get(key); ok {
			for i, x := range v.([]any) {
				walk(x, fmt.Sprintf("%s.%s[%d]", path, key, i), depth, f)
			}
		}
	}
	for _, key := range []string{"$defs", "definitions"} {
		if v, ok := o.Get(key); ok {
			for _, d := range v.(Obj) {
				walk(d.Val, path+"."+key+"."+d.Key, depth, f)
			}
		}
	}
}

func isObjectSchema(o Obj) bool {
	if t, ok := o.Get("type"); ok {
		if t == "object" {
			return true
		}
		if arr, ok := t.([]any); ok {
			for _, x := range arr {
				if x == "object" {
					return true
				}
			}
		}
	}
	_, hasProps := o.Get("properties")
	return hasProps
}

func requiredSet(o Obj) map[string]bool {
	m := map[string]bool{}
	if r, ok := o.Get("required"); ok {
		for _, x := range r.([]any) {
			m[fmt.Sprint(x)] = true
		}
	}
	return m
}

func additionalFalse(o Obj) bool {
	v, ok := o.Get("additionalProperties")
	b, isBool := v.(bool)
	return ok && isBool && !b
}

// CheckOpenAI：Responses API 的 text.format（strict: true）规则。
func CheckOpenAI(doc any) []Issue {
	var out []Issue
	root, _ := doc.(Obj)
	if !isObjectSchema(root) {
		out = append(out, Issue{"$", "root must be an object"})
	}
	if _, ok := root.Get("anyOf"); ok {
		out = append(out, Issue{"$", "root must not be anyOf"})
	}
	unsupported := []string{"allOf", "not", "if", "then", "else", "dependentRequired", "dependentSchemas", "patternProperties"}
	props := 0
	walk(doc, "$", 1, func(path string, o Obj, depth int) {
		for _, k := range unsupported {
			if _, ok := o.Get(k); ok {
				out = append(out, Issue{path, k + " is not supported"})
			}
		}
		if depth > 10 {
			out = append(out, Issue{path, "nesting deeper than 10 levels"})
		}
		if !isObjectSchema(o) {
			return
		}
		if !additionalFalse(o) {
			out = append(out, Issue{path, "additionalProperties must be false"})
		}
		req := requiredSet(o)
		if p, ok := o.Get("properties"); ok {
			for _, kv := range p.(Obj) {
				props++
				if !req[kv.Key] {
					out = append(out, Issue{path, fmt.Sprintf("property %q must be listed in required", kv.Key)})
				}
			}
		}
	})
	if props > 5000 {
		out = append(out, Issue{"$", "more than 5000 object properties"})
	}
	return out
}

// CheckAnthropic：output_config.format 和 strict 工具共用的规则。
func CheckAnthropic(doc any) []Issue {
	var out []Issue
	unsupported := []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "minLength", "maxLength", "maxItems"}
	optional, unions := 0, 0
	walk(doc, "$", 1, func(path string, o Obj, depth int) {
		for _, k := range unsupported {
			if _, ok := o.Get(k); ok {
				out = append(out, Issue{path, k + " is not supported"})
			}
		}
		if v, ok := o.Get("minItems"); ok {
			if n := intp(v); n != nil && *n > 1 {
				out = append(out, Issue{path, "minItems only supports 0 or 1"})
			}
		}
		if ref, ok := o.Get("$ref"); ok && ref == "#" {
			out = append(out, Issue{path, "recursive schemas are not supported"})
		}
		if _, ok := o.Get("anyOf"); ok {
			unions++
		}
		if t, ok := o.Get("type"); ok {
			if _, isArr := t.([]any); isArr {
				unions++
			}
		}
		if !isObjectSchema(o) {
			return
		}
		if !additionalFalse(o) {
			out = append(out, Issue{path, "additionalProperties must be false"})
		}
		req := requiredSet(o)
		if p, ok := o.Get("properties"); ok {
			for _, kv := range p.(Obj) {
				if !req[kv.Key] {
					optional++
				}
			}
		}
	})
	if optional > 24 {
		out = append(out, Issue{"$", fmt.Sprintf("%d optional parameters (limit 24)", optional)})
	}
	if unions > 16 {
		out = append(out, Issue{"$", fmt.Sprintf("%d union-typed parameters (limit 16)", unions)})
	}
	return out
}

// mapSchema 自底向上重建 schema：先递归处理子 schema，再对当前节点调用 f。
// 返回新值，不修改原件。
func mapSchema(doc any, f func(Obj) Obj) any {
	o, ok := doc.(Obj)
	if !ok {
		return doc
	}
	out := make(Obj, 0, len(o))
	for _, kv := range o {
		v := kv.Val
		switch kv.Key {
		case "properties", "$defs", "definitions":
			if m, ok := v.(Obj); ok {
				nm := make(Obj, len(m))
				for i, p := range m {
					nm[i] = KV{p.Key, mapSchema(p.Val, f)}
				}
				v = nm
			}
		case "items", "additionalProperties":
			v = mapSchema(v, f)
		case "anyOf", "allOf", "oneOf":
			if arr, ok := v.([]any); ok {
				na := make([]any, len(arr))
				for i, x := range arr {
					na[i] = mapSchema(x, f)
				}
				v = na
			}
		default:
			v = Clone(v)
		}
		out = append(out, KV{kv.Key, v})
	}
	return f(out)
}

// ForOpenAI 把一个“普通”schema 转成 OpenAI strict 能接受的形状：
//   - 每个对象加 additionalProperties: false；
//   - 可选字段改成必填，但类型加上 "null"：模型必须输出这个键，用 null 表示“没有”。
//
// 注意这改变了语义：原来“缺省”现在变成“显式 null”，读结果的代码要能处理 null。
func ForOpenAI(doc any) any {
	return mapSchema(doc, func(o Obj) Obj {
		if !isObjectSchema(o) {
			return o
		}
		req := requiredSet(o)
		all := []any{}
		if p, ok := o.Get("properties"); ok {
			props := p.(Obj)
			for i, kv := range props {
				all = append(all, kv.Key)
				if !req[kv.Key] {
					props[i].Val = nullable(kv.Val)
				}
			}
		}
		o.Set("required", all)
		o.Set("additionalProperties", false)
		return o
	})
}

// ForAnthropic 学 SDK 的做法：删掉不支持的约束，把它们写进 description 提示模型，
// 加上 additionalProperties: false。被删掉的约束要在拿到结果后用原 schema 再校验一遍。
func ForAnthropic(doc any) any {
	moved := []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "minLength", "maxLength", "maxItems"}
	return mapSchema(doc, func(o Obj) Obj {
		var notes []string
		for _, k := range moved {
			if v, ok := o.Get(k); ok {
				notes = append(notes, fmt.Sprintf("%s: %v", k, v))
				o.Del(k)
			}
		}
		if v, ok := o.Get("minItems"); ok {
			if n := intp(v); n != nil && *n > 1 {
				notes = append(notes, fmt.Sprintf("minItems: %d", *n))
				o.Del("minItems")
			}
		}
		if len(notes) > 0 {
			sort.Strings(notes)
			desc := ""
			if d, ok := o.Get("description"); ok {
				desc = fmt.Sprint(d) + " "
			}
			o.Set("description", desc+"{"+strings.Join(notes, ", ")+"}")
		}
		if isObjectSchema(o) {
			o.Set("additionalProperties", false)
		}
		return o
	})
}

func nullable(v any) any {
	o, ok := v.(Obj)
	if !ok {
		return v
	}
	if t, ok := o.Get("type"); ok {
		switch tt := t.(type) {
		case string:
			if tt != "null" {
				o.Set("type", []any{tt, "null"})
			}
		case []any:
			for _, x := range tt {
				if x == "null" {
					return o
				}
			}
			o.Set("type", append(tt, "null"))
		}
		return o
	}
	return Obj{{"anyOf", []any{o, Obj{{"type", "null"}}}}}
}

// Compact 输出紧凑 JSON，测试比对用。
func Compact(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
