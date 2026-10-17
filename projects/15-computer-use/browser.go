// Package cua 是 2.4 课的练习：Computer Use / Browser Use 代理的“执行端”。
// 模型只会说“点 (512, 384)”“在 ref_3 里输入”，真正动手的是你的代码。这里用一个假的浏览器
// （纯 Go，没有真实渲染）把执行端最容易出错的部分做成可测试的：坐标换算、三家动作格式、
// 批量动作的顺序与中止、逐个动作的安全检查、元素引用、截图历史管理、循环与结果核验。
package cua

import (
	"errors"
	"fmt"
	"strings"
)

// Element 是页面上的一个元素。坐标是视口像素（左上角为原点）。
type Element struct {
	ID     string
	Role   string // button / link / textbox / text
	Label  string
	X, Y   int
	W, H   int
	Hidden bool   // display:none、白字白底、屏幕外：用户看不到，模型也不该读到
	Href   string // 链接
	// Consequential 表示点了会产生真实后果（付款、删除、发送、同意条款）。
	Consequential bool
	OnClick       func(b *Browser)
}

func (e *Element) contains(x, y int) bool {
	return !e.Hidden && x >= e.X && x < e.X+e.W && y >= e.Y && y < e.Y+e.H
}

type Page struct {
	URL      string
	Title    string
	Elements []*Element
}

// Site 是假浏览器能打开的网站：URL → 生成页面的函数。
type Site map[string]func() *Page

type Browser struct {
	W, H    int // 视口大小（真实像素）
	Site    Site
	Page    *Page
	Version int // 每次导航加 1：元素引用随之失效
	Focus   *Element
	Inputs  map[string]string // 元素 ID → 已输入的文字
	Log     []string          // 实际执行过的动作，用来核验和审计
}

func NewBrowser(w, h int, site Site, start string) (*Browser, error) {
	b := &Browser{W: w, H: h, Site: site, Inputs: map[string]string{}}
	return b, b.Navigate(start)
}

func (b *Browser) Navigate(url string) error {
	mk, ok := b.Site[url]
	if !ok {
		return fmt.Errorf("无法打开 %s", url)
	}
	b.Page, b.Focus = mk(), nil
	b.Page.URL = url
	b.Version++
	b.Log = append(b.Log, "navigate "+url)
	return nil
}

// ElementAt 返回坐标处最上层的可见元素（后加入的在上层）。
func (b *Browser) ElementAt(x, y int) *Element {
	els := b.Page.Elements
	for i := len(els) - 1; i >= 0; i-- {
		if els[i].contains(x, y) {
			return els[i]
		}
	}
	return nil
}

func (b *Browser) ByID(id string) *Element {
	for _, e := range b.Page.Elements {
		if e.ID == id {
			return e
		}
	}
	return nil
}

// Insert 在页面最前面插入元素，并把它下方的元素整体下移：模拟横幅、Cookie 弹窗、懒加载造成的布局移动。
func (b *Browser) Insert(e *Element) {
	for _, old := range b.Page.Elements {
		if old.Y >= e.Y {
			old.Y += e.H
		}
	}
	b.Page.Elements = append(b.Page.Elements, e)
}

// Remove 隐藏一个元素，并把它下方的元素整体上移：模拟关闭提示条、折叠面板。
func (b *Browser) Remove(id string) {
	e := b.ByID(id)
	if e == nil || e.Hidden {
		return
	}
	e.Hidden = true
	for _, old := range b.Page.Elements {
		if old != e && old.Y >= e.Y+e.H {
			old.Y -= e.H
		}
	}
}

var ErrOutOfBounds = errors.New("坐标超出视口")

func (b *Browser) Click(x, y int) error {
	if x < 0 || y < 0 || x >= b.W || y >= b.H {
		return fmt.Errorf("%w：(%d, %d) 不在 %dx%d 内", ErrOutOfBounds, x, y, b.W, b.H)
	}
	e := b.ElementAt(x, y)
	b.Log = append(b.Log, fmt.Sprintf("click %d,%d", x, y))
	if e == nil {
		return nil // 点在空白处：真实浏览器也不会报错
	}
	return b.activate(e)
}

func (b *Browser) activate(e *Element) error {
	b.Log = append(b.Log, "activate "+e.ID)
	if e.Role == "textbox" {
		b.Focus = e
	}
	if e.OnClick != nil {
		e.OnClick(b)
	}
	if e.Href != "" {
		return b.Navigate(e.Href)
	}
	return nil
}

func (b *Browser) Type(text string) error {
	if b.Focus == nil {
		return errors.New("没有获得焦点的输入框")
	}
	b.Inputs[b.Focus.ID] += text
	b.Log = append(b.Log, "type "+text)
	return nil
}

func (b *Browser) Key(key string, repeat int) error {
	if repeat < 1 {
		repeat = 1
	}
	for i := 0; i < repeat; i++ {
		b.Log = append(b.Log, "key "+key)
		if strings.EqualFold(key, "Return") || strings.EqualFold(key, "Enter") {
			if b.Focus != nil {
				if e := b.ByID(b.Focus.ID + "-submit"); e != nil {
					if err := b.activate(e); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// Shot 代表一张截图。真实实现里是 PNG 字节，这里只保留核验需要的信息。
type Shot struct {
	W, H    int
	URL     string
	Version int
}

func (b *Browser) Screenshot() Shot { return Shot{W: b.W, H: b.H, URL: b.Page.URL, Version: b.Version} }
