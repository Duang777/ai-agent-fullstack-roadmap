package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// DemoServer 是测试和课件里用的示例 Server。
func DemoServer() *Server {
	s := NewServer("demo")
	obj := func(props map[string]any, req ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": req, "additionalProperties": false}
	}
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }

	s.AddTool(Tool{Name: "get_weather", Title: "天气", Description: "查询城市当前天气。返回温度和天气描述，不提供预报。",
		InputSchema: obj(map[string]any{"city": str("城市名，例如 北京")}, "city"),
		Handler: func(ctx context.Context, c CallCtx) (*ToolResult, *NeedInput) {
			if d, ok := c.Args["delay_ms"].(float64); ok {
				time.Sleep(time.Duration(d) * time.Millisecond)
			}
			city := c.Args["city"].(string)
			return &ToolResult{Text: city + "：晴 18°C", Structured: map[string]any{"city": city, "temp_c": 18}}, nil
		}})

	// 危险操作：用 MRTR 请用户确认。Server 自己不保存任何“等待确认”的状态。
	s.AddTool(Tool{Name: "delete_repo", Title: "删除仓库", Description: "永久删除一个仓库。不可恢复，执行前会请用户确认。",
		InputSchema: obj(map[string]any{"repo": str("仓库全名，例如 duang777/demo")}, "repo"),
		Handler: func(ctx context.Context, c CallCtx) (*ToolResult, *NeedInput) {
			repo := c.Args["repo"].(string)
			raw, ok := c.InputResponses["confirm"]
			if !ok {
				return nil, &NeedInput{Cap: "elicitation", Requests: map[string]any{"confirm": map[string]any{
					"method": "elicitation/create",
					"params": map[string]any{"mode": "form", "message": "确认永久删除 " + repo + "？请输入仓库名确认。",
						"requestedSchema": obj(map[string]any{"repo": str("再输入一次仓库名")}, "repo")},
				}}}
			}
			var ans struct {
				Action  string
				Content map[string]any
			}
			json.Unmarshal(raw, &ans)
			if ans.Action != "accept" {
				return &ToolResult{Text: "用户取消了删除。"}, nil
			}
			if ans.Content["repo"] != repo {
				return &ToolResult{Text: "确认的仓库名不一致，未删除。", IsError: true}, nil
			}
			return &ToolResult{Text: "已删除 " + repo}, nil
		}})

	// 跨调用的状态：显式句柄，而不是“会话”
	var mu sync.Mutex
	baskets := map[string][]string{}
	s.AddTool(Tool{Name: "create_basket", Description: "创建一个购物篮，返回 basket_id。购物篮 24 小时不活动后过期。",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false},
		Handler: func(ctx context.Context, c CallCtx) (*ToolResult, *NeedInput) {
			b := make([]byte, 8)
			rand.Read(b)
			id := "bsk_" + hex.EncodeToString(b)
			mu.Lock()
			baskets[id] = nil
			mu.Unlock()
			return &ToolResult{Text: "已创建购物篮 " + id, Structured: map[string]any{"basket_id": id}}, nil
		}})
	s.AddTool(Tool{Name: "add_item", Description: "往购物篮里加一件商品。basket_id 来自 create_basket。",
		InputSchema: obj(map[string]any{"basket_id": str("create_basket 返回的 ID"), "sku": str("商品编号")}, "basket_id", "sku"),
		Handler: func(ctx context.Context, c CallCtx) (*ToolResult, *NeedInput) {
			id, sku := c.Args["basket_id"].(string), c.Args["sku"].(string)
			mu.Lock()
			defer mu.Unlock()
			items, ok := baskets[id]
			if !ok {
				return &ToolResult{Text: fmt.Sprintf("购物篮 %s 不存在或已过期，请先调用 create_basket 创建新的。", id), IsError: true}, nil
			}
			baskets[id] = append(items, sku)
			return &ToolResult{Text: fmt.Sprintf("已加入，篮子里有 %d 件", len(baskets[id]))}, nil
		}})

	s.AddResource(Resource{URI: "notes://roadmap", Name: "roadmap", MimeType: "text/markdown", Text: "# 路线图\n2.2 MCP"})
	s.AddResource(Resource{URI: "notes://笔记/今天", Name: "today", MimeType: "text/plain", Text: "今天学 MCP"})
	s.AddPrompt(Prompt{Name: "code_review", Description: "请模型审查一段代码",
		Args: []PromptArg{{"code", "要审查的代码", true}, {"focus", "关注点", false}},
		Render: func(a map[string]string) string {
			return "请审查这段代码（关注 " + or(a["focus"], "正确性") + "）：\n" + a["code"]
		},
	})
	return s
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
