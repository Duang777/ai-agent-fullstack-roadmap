// Package structured 是 1.3 课的练习：JSON Schema 校验、三家 strict 规则检查与转换、
// 约束解码（constrained decoding）、以及“解析 → 校验 → 带错误重试”的抽取循环。纯标准库。
package structured

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Go 的 map 没有顺序，json.Marshal 会按 key 排序。
// 但结构化输出里字段顺序有意义（OpenAI 按 schema 里的顺序生成），所以需要一个保序的 JSON 对象。

// KV 是对象里的一个键值对。
type KV struct {
	Key string
	Val any
}

// Obj 是保序的 JSON 对象。值的类型：Obj、[]any、string、json.Number、bool、nil。
type Obj []KV

func (o Obj) Get(k string) (any, bool) {
	for _, kv := range o {
		if kv.Key == k {
			return kv.Val, true
		}
	}
	return nil, false
}

// Set 覆盖已有的键（位置不变），没有就追加到末尾。
func (o *Obj) Set(k string, v any) {
	for i := range *o {
		if (*o)[i].Key == k {
			(*o)[i].Val = v
			return
		}
	}
	*o = append(*o, KV{k, v})
}

func (o *Obj) Del(k string) {
	out := (*o)[:0]
	for _, kv := range *o {
		if kv.Key != k {
			out = append(out, kv)
		}
	}
	*o = out
}

func (o Obj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, kv := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(kv.Key)
		b.Write(k)
		b.WriteByte(':')
		v, err := json.Marshal(kv.Val)
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// ParseOrdered 解析 JSON，对象保持原始键顺序，数字保留为 json.Number（区分 1 和 1.0）。
func ParseOrdered(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := readValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return v, nil
}

func readValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := Obj{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				v, err := readValue(dec)
				if err != nil {
					return nil, err
				}
				o = append(o, KV{kt.(string), v})
			}
			_, err := dec.Token() // }
			return o, err
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := readValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			_, err := dec.Token() // ]
			return arr, err
		}
	}
	return tok, nil // string、json.Number、bool、nil
}

// Clone 深拷贝，转换 schema 时不改动调用方的原件。
func Clone(v any) any {
	switch t := v.(type) {
	case Obj:
		o := make(Obj, len(t))
		for i, kv := range t {
			o[i] = KV{kv.Key, Clone(kv.Val)}
		}
		return o
	case []any:
		a := make([]any, len(t))
		for i, x := range t {
			a[i] = Clone(x)
		}
		return a
	}
	return v
}
