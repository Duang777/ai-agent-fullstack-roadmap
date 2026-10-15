package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

type Transport interface {
	RoundTrip(ctx context.Context, method string, params map[string]any) (Response, error)
}

// Client 给每个请求自动带上 _meta。Elicit 是“问用户”的回调：Server 通过 MRTR 要求补充信息时调用。
type Client struct {
	T       Transport
	Info    Implementation
	Version string
	Caps    map[string]any
	Elicit  func(message string, schema map[string]any) (action string, content map[string]any)
	MaxMRTR int
}

func (c *Client) call(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	p := maps.Clone(params)
	if p == nil {
		p = map[string]any{}
	}
	ver := c.Version
	if ver == "" {
		ver = Version
	}
	caps := c.Caps
	if caps == nil {
		caps = map[string]any{}
	}
	p["_meta"] = map[string]any{metaVersion: ver, metaClient: c.Info, metaCaps: caps}
	resp, err := c.T.RoundTrip(ctx, method, p)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	var out map[string]any
	json.Unmarshal(resp.Result, &out)
	if _, ok := out["resultType"]; !ok {
		out["resultType"] = "complete" // 老版本 Server 没有这个字段，按 complete 处理
	}
	return out, nil
}

func (c *Client) Discover(ctx context.Context) (map[string]any, error) {
	return c.call(ctx, "server/discover", nil)
}

// ListTools 自动翻页。
func (c *Client) ListTools(ctx context.Context) ([]map[string]any, error) {
	var all []map[string]any
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		r, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		for _, t := range r["tools"].([]any) {
			all = append(all, t.(map[string]any))
		}
		next, _ := r["nextCursor"].(string)
		if next == "" {
			return all, nil
		}
		cursor = next
	}
}

// CallTool 处理多轮请求：收到 input_required 就去问用户，然后用新的 id 重发原请求，
// 带上 inputResponses 和原样的 requestState。
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (map[string]any, int, error) {
	params := map[string]any{"name": name, "arguments": args}
	limit := c.MaxMRTR
	if limit == 0 {
		limit = 3
	}
	for round := 1; ; round++ {
		r, err := c.call(ctx, "tools/call", params)
		if err != nil {
			return nil, round, err
		}
		if r["resultType"] != "input_required" {
			return r, round, nil
		}
		if round >= limit {
			return nil, round, fmt.Errorf("多轮请求超过 %d 轮", limit)
		}
		reqs, _ := r["inputRequests"].(map[string]any)
		answers := map[string]any{}
		for key, v := range reqs {
			ir := v.(map[string]any)
			if ir["method"] != "elicitation/create" || c.Elicit == nil {
				return nil, round, fmt.Errorf("不支持的 inputRequest：%v", ir["method"])
			}
			ps := ir["params"].(map[string]any)
			schema, _ := ps["requestedSchema"].(map[string]any)
			action, content := c.Elicit(fmt.Sprint(ps["message"]), schema)
			answers[key] = map[string]any{"action": action, "content": content}
		}
		params = map[string]any{"name": name, "arguments": args, "inputResponses": answers}
		if st, ok := r["requestState"].(string); ok {
			params["requestState"] = st // 原样带回，不解析、不修改
		}
	}
}

func (c *Client) ReadResource(ctx context.Context, uri string) (string, error) {
	r, err := c.call(ctx, "resources/read", map[string]any{"uri": uri})
	if err != nil {
		return "", err
	}
	cs := r["contents"].([]any)
	return cs[0].(map[string]any)["text"].(string), nil
}

func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]string) (string, error) {
	r, err := c.call(ctx, "prompts/get", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", err
	}
	m := r["messages"].([]any)[0].(map[string]any)
	return m["content"].(map[string]any)["text"].(string), nil
}

// Host 聚合多个 Server 的工具，加前缀防重名，再交给模型（2.1 课的工具定义格式）。
type Host struct {
	Servers map[string]*Client // key 是你给 Server 起的唯一别名，不要用 serverInfo.name（不保证唯一）
	route   map[string][2]string
}

const sep = "__"

func (h *Host) Tools(ctx context.Context) ([]map[string]any, error) {
	h.route = map[string][2]string{}
	var out []map[string]any
	for _, alias := range slices.Sorted(maps.Keys(h.Servers)) {
		ts, err := h.Servers[alias].ListTools(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", alias, err)
		}
		for _, t := range ts {
			name := alias + sep + t["name"].(string)
			h.route[name] = [2]string{alias, t["name"].(string)}
			// 渲染成 Anthropic 的工具定义：inputSchema → input_schema
			out = append(out, map[string]any{"name": name, "description": t["description"], "input_schema": t["inputSchema"]})
		}
	}
	return out, nil
}

// Call 把模型发起的调用路由到对应 Server，返回给模型的文字和 is_error。
func (h *Host) Call(ctx context.Context, name string, args map[string]any) (string, bool) {
	rt, ok := h.route[name]
	if !ok {
		return fmt.Sprintf("未知工具 %q", name), true
	}
	r, _, err := h.Servers[rt[0]].CallTool(ctx, rt[1], args)
	if err != nil {
		return "调用失败：" + err.Error(), true
	}
	var texts []string
	for _, c := range r["content"].([]any) {
		texts = append(texts, c.(map[string]any)["text"].(string))
	}
	isErr, _ := r["isError"].(bool)
	return strings.Join(texts, "\n"), isErr
}
