package cua

import (
	"fmt"
	"strings"
)

// Refs 给页面元素分配引用（ref_1、ref_2…），供 Browser Use 风格的工具使用。
// 规则：同一个页面版本内，同一个元素的引用不变；已经发出去的引用不重新编号；导航后全部作废。
type Refs struct {
	version int
	next    int
	byRef   map[string]string // ref → 元素 ID
	byID    map[string]string
}

func (r *Refs) reset(v int) {
	r.version, r.next = v, 0
	r.byRef, r.byID = map[string]string{}, map[string]string{}
}

// ReadPage 返回可访问性树风格的文本。只读用户看得见的元素：隐藏文字不能进入模型上下文。
func (r *Refs) ReadPage(b *Browser, interactiveOnly bool) string {
	if r.byRef == nil || r.version != b.Version {
		r.reset(b.Version)
	}
	var lines []string
	for _, e := range b.Page.Elements {
		if e.Hidden || (interactiveOnly && e.Role == "text") {
			continue
		}
		ref, ok := r.byID[e.ID]
		if !ok {
			r.next++
			ref = fmt.Sprintf("ref_%d", r.next)
			r.byID[e.ID], r.byRef[ref] = ref, e.ID
		}
		lines = append(lines, fmt.Sprintf("%s %q [%s]", e.Role, e.Label, ref))
	}
	return strings.Join(lines, "\n")
}

// Resolve 把引用换回元素。过期或不存在时返回一条能指导模型的错误。
func (r *Refs) Resolve(b *Browser, ref string) (*Element, error) {
	stale := fmt.Errorf("Error: %s is stale or not found on the current page. Re-read the page to get fresh references.", ref)
	if r.byRef == nil || r.version != b.Version {
		return nil, stale
	}
	id, ok := r.byRef[ref]
	if !ok {
		return nil, stale
	}
	e := b.ByID(id)
	if e == nil || e.Hidden {
		return nil, stale
	}
	return e, nil
}

// ClickRef 按引用点击：不依赖坐标，布局移动也不会点错。
func (r *Refs) ClickRef(b *Browser, g *Guard, ref string) error {
	e, err := r.Resolve(b, ref)
	if err != nil {
		return err
	}
	if err := g.Before(b, Action{Kind: "click", X: e.X + e.W/2, Y: e.Y + e.H/2, HasXY: true, Ref: ref}); err != nil {
		return err
	}
	b.Log = append(b.Log, "click "+ref)
	return b.activate(e)
}
