package structured

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Schema 是编译后的 JSON Schema 子集：结构化输出真正用得到的那部分。
type Schema struct {
	Types             []string // "object" "array" "string" "integer" "number" "boolean" "null"；空表示不限
	Props             []Prop   // 保持 schema 里的顺序
	Required          map[string]bool
	AdditionalAllowed bool // additionalProperties 不是 false
	Items             *Schema
	Enum              []any
	Const             any
	HasConst          bool
	AnyOf             []*Schema
	Ref               string // 只支持 "#"（根递归）

	Minimum, Maximum     *float64
	MinLength, MaxLength *int
	MinItems, MaxItems   *int

	root *Schema
}

type Prop struct {
	Name   string
	Schema *Schema
}

// Compile 把 ParseOrdered 得到的 schema 文档编译成 *Schema。
func Compile(doc any) (*Schema, error) {
	s, err := compile(doc, nil)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// MustCompileJSON 测试和示例用。
func MustCompileJSON(src string) *Schema {
	doc, err := ParseOrdered([]byte(src))
	if err != nil {
		panic(err)
	}
	s, err := Compile(doc)
	if err != nil {
		panic(err)
	}
	return s
}

func compile(doc any, root *Schema) (*Schema, error) {
	o, ok := doc.(Obj)
	if !ok {
		return nil, fmt.Errorf("schema must be an object, got %T", doc)
	}
	s := &Schema{Required: map[string]bool{}, AdditionalAllowed: true}
	if root == nil {
		root = s
	}
	s.root = root
	for _, kv := range o {
		var err error
		switch kv.Key {
		case "type":
			switch t := kv.Val.(type) {
			case string:
				s.Types = []string{t}
			case []any:
				for _, x := range t {
					s.Types = append(s.Types, fmt.Sprint(x))
				}
			}
		case "properties":
			props, _ := kv.Val.(Obj)
			for _, p := range props {
				ps, err := compile(p.Val, root)
				if err != nil {
					return nil, fmt.Errorf("properties.%s: %w", p.Key, err)
				}
				s.Props = append(s.Props, Prop{p.Key, ps})
			}
		case "required":
			arr, _ := kv.Val.([]any)
			for _, x := range arr {
				s.Required[fmt.Sprint(x)] = true
			}
		case "additionalProperties":
			if b, ok := kv.Val.(bool); ok && !b {
				s.AdditionalAllowed = false
			}
		case "items":
			s.Items, err = compile(kv.Val, root)
		case "enum":
			s.Enum, _ = kv.Val.([]any)
		case "const":
			s.Const, s.HasConst = kv.Val, true
		case "anyOf":
			arr, _ := kv.Val.([]any)
			for i, x := range arr {
				b, err := compile(x, root)
				if err != nil {
					return nil, fmt.Errorf("anyOf[%d]: %w", i, err)
				}
				s.AnyOf = append(s.AnyOf, b)
			}
		case "$ref":
			s.Ref, _ = kv.Val.(string)
			if s.Ref != "#" {
				return nil, fmt.Errorf("only $ref \"#\" is supported, got %q", s.Ref)
			}
		case "minimum":
			s.Minimum = num(kv.Val)
		case "maximum":
			s.Maximum = num(kv.Val)
		case "minLength":
			s.MinLength = intp(kv.Val)
		case "maxLength":
			s.MaxLength = intp(kv.Val)
		case "minItems":
			s.MinItems = intp(kv.Val)
		case "maxItems":
			s.MaxItems = intp(kv.Val)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", kv.Key, err)
		}
	}
	return s, nil
}

func num(v any) *float64 {
	if n, ok := v.(json.Number); ok {
		f, err := n.Float64()
		if err == nil {
			return &f
		}
	}
	return nil
}

func intp(v any) *int {
	if f := num(v); f != nil {
		i := int(*f)
		return &i
	}
	return nil
}

func (s *Schema) resolve() *Schema {
	if s.Ref == "#" {
		return s.root
	}
	return s
}

func (s *Schema) allows(t string) bool {
	if len(s.Types) == 0 {
		return true
	}
	for _, x := range s.Types {
		if x == t || (x == "number" && t == "integer") {
			return true
		}
	}
	return false
}

// ValidationError 带 JSON 路径，方便原样喂回给模型让它改。
type ValidationError struct {
	Path string // 比如 $.items[2].price
	Msg  string
}

func (e ValidationError) Error() string { return e.Path + ": " + e.Msg }

// Validate 校验一个 ParseOrdered 得到的值，返回全部错误（不是遇到第一个就停）。
func (s *Schema) Validate(v any) []ValidationError {
	var errs []ValidationError
	s.validate(v, "$", &errs)
	return errs
}

func typeOf(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		if _, err := strconv.ParseInt(t.String(), 10, 64); err == nil {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case Obj:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func jsonEq(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func (s *Schema) validate(v any, path string, errs *[]ValidationError) {
	s = s.resolve()
	add := func(format string, args ...any) {
		*errs = append(*errs, ValidationError{path, fmt.Sprintf(format, args...)})
	}
	if len(s.AnyOf) > 0 {
		for _, b := range s.AnyOf {
			if len(b.Validate(v)) == 0 {
				return
			}
		}
		add("does not match any of %d anyOf branches", len(s.AnyOf))
		return
	}
	t := typeOf(v)
	if !s.allows(t) {
		add("expected %s, got %s", strings.Join(s.Types, " or "), t)
		return
	}
	if s.HasConst && !jsonEq(v, s.Const) {
		add("must be const %v", s.Const)
	}
	if len(s.Enum) > 0 {
		ok := false
		for _, e := range s.Enum {
			if jsonEq(v, e) {
				ok = true
				break
			}
		}
		if !ok {
			add("must be one of %s", enumList(s.Enum))
		}
	}
	switch t {
	case "integer", "number":
		f, _ := v.(json.Number).Float64()
		if s.Minimum != nil && f < *s.Minimum {
			add("must be >= %v", *s.Minimum)
		}
		if s.Maximum != nil && f > *s.Maximum {
			add("must be <= %v", *s.Maximum)
		}
		if math.IsInf(f, 0) {
			add("number out of range")
		}
	case "string":
		n := utf8.RuneCountInString(v.(string))
		if s.MinLength != nil && n < *s.MinLength {
			add("length %d < minLength %d", n, *s.MinLength)
		}
		if s.MaxLength != nil && n > *s.MaxLength {
			add("length %d > maxLength %d", n, *s.MaxLength)
		}
	case "array":
		arr := v.([]any)
		if s.MinItems != nil && len(arr) < *s.MinItems {
			add("has %d items, minItems %d", len(arr), *s.MinItems)
		}
		if s.MaxItems != nil && len(arr) > *s.MaxItems {
			add("has %d items, maxItems %d", len(arr), *s.MaxItems)
		}
		if s.Items != nil {
			for i, x := range arr {
				s.Items.validate(x, fmt.Sprintf("%s[%d]", path, i), errs)
			}
		}
	case "object":
		o := v.(Obj)
		for _, p := range s.Props {
			if _, ok := o.Get(p.Name); !ok && s.Required[p.Name] {
				add("missing required property %q", p.Name)
			}
		}
		for _, kv := range o {
			ps := s.prop(kv.Key)
			if ps == nil {
				if !s.AdditionalAllowed {
					add("unexpected property %q", kv.Key)
				}
				continue
			}
			ps.validate(kv.Val, path+"."+kv.Key, errs)
		}
	}
}

func (s *Schema) prop(name string) *Schema {
	for _, p := range s.Props {
		if p.Name == name {
			return p.Schema
		}
	}
	return nil
}

func enumList(e []any) string {
	b, _ := json.Marshal(e)
	return string(b)
}
