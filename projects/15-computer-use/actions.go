package cua

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Action 是三家格式统一后的一个动作。X、Y 已经换算成真实屏幕像素。
type Action struct {
	Kind   string // click / double_click / type / key / scroll / screenshot / zoom / navigate / wait / unsupported
	X, Y   int
	HasXY  bool
	Ref    string // Browser Use 的元素引用
	Text   string
	URL    string
	Repeat int
	CallID string
	Name   string // 原始动作名（回传结果时要用）
	Intent string // Gemini 3.x 每个动作附带的意图
	// Gemini 的 safety_decision = require_confirmation
	NeedsConfirm bool
	Explanation  string
}

// ---- Anthropic：computer_toolset_20260801，每个成员工具是一个 tool_use 块 ----

type AnthropicBlock struct {
	Type        string                     `json:"type"`
	ID          string                     `json:"id"`
	Name        string                     `json:"name"`
	ToolsetName string                     `json:"toolset_name"`
	Input       map[string]json.RawMessage `json:"input"`
}

func xy(raw json.RawMessage) (int, int, bool) {
	var p []int
	if json.Unmarshal(raw, &p) != nil || len(p) != 2 {
		return 0, 0, false
	}
	return p[0], p[1], true
}

func str(raw json.RawMessage) string {
	var s string
	json.Unmarshal(raw, &s)
	return s
}

// FromAnthropic 把一个成员调用转成 Action。scale 是你缩放截图时用的系数。
func FromAnthropic(b AnthropicBlock, scale float64) Action {
	a := Action{CallID: b.ID, Name: b.Name}
	in := b.Input
	if x, y, ok := xy(in["coordinate"]); ok {
		a.X, a.Y = ToScreen(x, y, scale)
		a.HasXY = true
	}
	switch b.Name {
	case "left_click":
		a.Kind = "click"
	case "double_click":
		a.Kind = "double_click"
	case "type":
		a.Kind, a.Text = "type", str(in["text"])
	case "key":
		a.Kind, a.Text, a.Repeat = "key", str(in["text"]), 1
		if r, ok := in["repeat"]; ok {
			json.Unmarshal(r, &a.Repeat)
		}
	case "screenshot":
		a.Kind = "screenshot"
	case "zoom":
		a.Kind = "zoom"
	case "scroll":
		a.Kind = "scroll"
	case "wait":
		a.Kind = "wait"
	default:
		a.Kind = "unsupported"
	}
	return a
}

// ---- OpenAI：一个 computer_call 带一个有序的 actions 数组，共用一个 call_id ----

type OpenAICall struct {
	Type    string `json:"type"`
	CallID  string `json:"call_id"`
	Actions []struct {
		Type   string   `json:"type"`
		Button string   `json:"button"`
		X      int      `json:"x"`
		Y      int      `json:"y"`
		Text   string   `json:"text"`
		Keys   []string `json:"keys"`
	} `json:"actions"`
}

func FromOpenAI(c OpenAICall, scale float64) []Action {
	var out []Action
	for _, x := range c.Actions {
		a := Action{CallID: c.CallID, Name: x.Type}
		switch x.Type {
		case "click":
			a.Kind = "click"
			if x.Button != "" && x.Button != "left" {
				a.Kind = "unsupported"
			}
			a.X, a.Y = ToScreen(x.X, x.Y, scale)
			a.HasXY = true
		case "double_click":
			a.Kind = "double_click"
			a.X, a.Y = ToScreen(x.X, x.Y, scale)
			a.HasXY = true
		case "type":
			a.Kind, a.Text = "type", x.Text
		case "keypress":
			a.Kind, a.Text, a.Repeat = "key", strings.Join(x.Keys, "+"), 1
		case "screenshot", "wait", "scroll":
			a.Kind = x.Type
		default:
			a.Kind = "unsupported"
		}
		out = append(out, a)
	}
	return out
}

// ---- Gemini：Interactions 的 function_call 步骤，坐标是 0–999 归一化 ----

type GeminiStep struct {
	Type      string         `json:"type"`
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func num(v any) int {
	f, _ := v.(float64)
	return int(f)
}

func FromGemini(s GeminiStep, w, h int) Action {
	a := Action{CallID: s.ID, Name: s.Name}
	args := s.Arguments
	a.Intent, _ = args["intent"].(string)
	if _, ok := args["x"]; ok {
		a.X, a.Y = Denormalize(num(args["x"]), w), Denormalize(num(args["y"]), h)
		a.HasXY = true
	}
	if sd, ok := args["safety_decision"].(map[string]any); ok && sd["decision"] == "require_confirmation" {
		a.NeedsConfirm = true
		a.Explanation, _ = sd["explanation"].(string)
	}
	switch s.Name {
	case "click":
		a.Kind = "click"
	case "type":
		a.Kind, a.Text = "type", fmt.Sprint(args["text"])
	case "navigate":
		a.Kind, a.URL = "navigate", fmt.Sprint(args["url"])
	case "take_screenshot":
		a.Kind = "screenshot"
	case "wait":
		a.Kind = "wait"
	default:
		a.Kind = "unsupported"
	}
	return a
}

// ---- 执行 ----

type Result struct {
	Text  string
	Image *Shot
}

// Do 在浏览器里执行一个动作。动作前的安全检查由调用方（Runner）负责。
func (b *Browser) Do(a Action) (Result, error) {
	switch a.Kind {
	case "screenshot", "zoom":
		s := b.Screenshot()
		return Result{Image: &s}, nil
	case "click", "double_click":
		if !a.HasXY {
			return Result{}, fmt.Errorf("%s 缺少坐标", a.Kind)
		}
		if a.Kind == "double_click" {
			if err := b.Click(a.X, a.Y); err != nil {
				return Result{}, err
			}
		}
		return Result{Text: "OK"}, b.Click(a.X, a.Y)
	case "type":
		if a.HasXY { // Gemini 的 type 可以带坐标：先点再输入
			if err := b.Click(a.X, a.Y); err != nil {
				return Result{}, err
			}
		}
		return Result{Text: "OK"}, b.Type(a.Text)
	case "key":
		return Result{Text: "OK"}, b.Key(a.Text, a.Repeat)
	case "navigate":
		return Result{Text: "OK"}, b.Navigate(a.URL)
	case "wait", "scroll":
		b.Log = append(b.Log, a.Kind)
		return Result{Text: "OK"}, nil
	}
	return Result{}, fmt.Errorf("不支持的动作：%s", a.Name)
}
