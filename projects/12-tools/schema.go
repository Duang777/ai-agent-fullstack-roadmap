// Package tools 是 2.1 课的练习：工具定义与校验、两家的调用/结果格式、流式参数拼装、并行执行与错误回传。
// 纯 Go 标准库，不调真实 API。
package tools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Prop 是 JSON Schema 的一个最小子集：够描述大多数工具参数。
type Prop struct {
	Type        string   `json:"type"` // string / number / integer / boolean
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Nullable    bool     `json:"-"` // strict 模式下表示“可选”：类型写成 [type, "null"]
}

type Tool struct {
	Name        string
	Description string
	Props       map[string]Prop
	Required    []string
	ReadOnly    bool // 只读、无副作用：可以并行、可以去重
}

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// Lint 检查一个工具定义里最常见的问题。
func (t Tool) Lint(strict bool) []string {
	var w []string
	if !nameRe.MatchString(t.Name) {
		w = append(w, "名字只能用字母、数字、下划线、连字符，1–128 个字符")
	}
	if len([]rune(t.Description)) < 30 {
		w = append(w, "描述太短：要写清做什么、什么时候用、什么时候不用、返回什么")
	}
	for _, k := range sortedKeys(t.Props) {
		p := t.Props[k]
		if p.Description == "" && len(p.Enum) == 0 {
			w = append(w, fmt.Sprintf("参数 %s 没有说明格式", k))
		}
	}
	if strict {
		req := map[string]bool{}
		for _, r := range t.Required {
			req[r] = true
		}
		for _, k := range sortedKeys(t.Props) {
			if !req[k] {
				w = append(w, fmt.Sprintf("strict 模式要求所有参数都在 required 里；可选参数 %s 请标成可为 null", k))
			}
		}
	}
	return w
}

func (t Tool) schema(strict bool) map[string]any {
	props := map[string]any{}
	for k, p := range t.Props {
		m := map[string]any{"type": p.Type}
		if p.Nullable {
			m["type"] = []string{p.Type, "null"}
		}
		if p.Description != "" {
			m["description"] = p.Description
		}
		if len(p.Enum) > 0 {
			m["enum"] = p.Enum
		}
		props[k] = m
	}
	s := map[string]any{"type": "object", "properties": props, "required": t.Required}
	if strict {
		s["additionalProperties"] = false
	}
	return s
}

// OpenAI 返回 Responses API 的 function 工具定义。
func (t Tool) OpenAI() map[string]any {
	return map[string]any{"type": "function", "name": t.Name, "description": t.Description,
		"parameters": t.schema(true), "strict": true}
}

// Anthropic 返回 Messages API 的工具定义（strict 时加 strict: true）。
func (t Tool) Anthropic(strict bool) map[string]any {
	m := map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.schema(strict)}
	if strict {
		m["strict"] = true
	}
	return m
}

// Validate 检查模型给的参数。没开 strict 时模型是“尽力而为”，这一步必须自己做。
func (t Tool) Validate(raw json.RawMessage) (map[string]any, error) {
	var args map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&args); err != nil {
		return nil, fmt.Errorf("参数不是合法的 JSON 对象：%v", err)
	}
	for _, r := range t.Required {
		if _, ok := args[r]; !ok {
			return nil, fmt.Errorf("缺少必填参数 %q", r)
		}
	}
	for _, k := range sortedKeys(args) {
		p, ok := t.Props[k]
		if !ok {
			return nil, fmt.Errorf("未知参数 %q", k)
		}
		v := args[k]
		if v == nil {
			if p.Nullable {
				continue
			}
			return nil, fmt.Errorf("参数 %q 不能为 null", k)
		}
		if err := checkType(k, p, v); err != nil {
			return nil, err
		}
	}
	return args, nil
}

func checkType(k string, p Prop, v any) error {
	bad := fmt.Errorf("参数 %q 应为 %s", k, p.Type)
	switch p.Type {
	case "string":
		s, ok := v.(string)
		if !ok {
			return bad
		}
		if len(p.Enum) > 0 {
			for _, e := range p.Enum {
				if e == s {
					return nil
				}
			}
			return fmt.Errorf("参数 %q 只能是 %s 之一，收到 %q", k, strings.Join(p.Enum, " / "), s)
		}
	case "number":
		if _, ok := v.(json.Number); !ok {
			return bad
		}
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return bad
		}
		if _, err := n.Int64(); err != nil {
			return bad
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return bad
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
