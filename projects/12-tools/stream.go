package tools

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Event 是两家流式事件里和工具调用有关的部分，已经归一化：
//   - Start：一个新的调用开始（OpenAI response.output_item.added / Anthropic content_block_start）
//   - Delta：参数的一小段 JSON 文本（OpenAI function_call_arguments.delta / Anthropic input_json_delta）
//   - Done：这个调用的参数结束
//
// Index 是 OpenAI 的 output_index / Anthropic 的 content block index：并行调用的片段会交错到达，只能靠它区分。
type Event struct {
	Kind  string // start / delta / done
	Index int
	ID    string
	Name  string
	Delta string
}

type Accumulator struct {
	calls map[int]*Call
	buf   map[int][]byte
	done  map[int]bool
}

func NewAccumulator() *Accumulator {
	return &Accumulator{calls: map[int]*Call{}, buf: map[int][]byte{}, done: map[int]bool{}}
}

func (a *Accumulator) Add(e Event) error {
	switch e.Kind {
	case "start":
		a.calls[e.Index] = &Call{ID: e.ID, Name: e.Name}
	case "delta":
		if _, ok := a.calls[e.Index]; !ok {
			return fmt.Errorf("index %d 的片段在 start 之前到达", e.Index)
		}
		a.buf[e.Index] = append(a.buf[e.Index], e.Delta...)
	case "done":
		c, ok := a.calls[e.Index]
		if !ok {
			return fmt.Errorf("index %d 没有 start", e.Index)
		}
		raw := a.buf[e.Index]
		if len(raw) == 0 {
			raw = []byte("{}") // Anthropic：无参数工具可能一个片段都没有
		}
		if !json.Valid(raw) {
			return fmt.Errorf("index %d 的参数拼完不是合法 JSON：%s", e.Index, raw)
		}
		c.Args = raw
		a.done[e.Index] = true
	}
	return nil
}

// Partial 返回某个调用到目前为止的参数文本（用于界面上实时显示，不能拿去执行）。
func (a *Accumulator) Partial(i int) string { return string(a.buf[i]) }

// Calls 按 index 顺序返回所有已完成的调用。
func (a *Accumulator) Calls() []Call {
	idx := make([]int, 0, len(a.done))
	for i := range a.done {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]Call, 0, len(idx))
	for _, i := range idx {
		out = append(out, *a.calls[i])
	}
	return out
}
