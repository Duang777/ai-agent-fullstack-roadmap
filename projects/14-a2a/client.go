package a2a

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
)

// FetchCard 从 https://{域名}/.well-known/agent-card.json 取 Agent Card。
func FetchCard(ctx context.Context, base string) (*AgentCard, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(base, "/")+"/.well-known/agent-card.json", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("agent card: HTTP %d", resp.StatusCode)
	}
	var c AgentCard
	return &c, json.NewDecoder(resp.Body).Decode(&c)
}

type Client struct {
	URL     string // JSON-RPC 端点，来自 Agent Card 的 supportedInterfaces
	Version string // 默认 1.0
	HTTP    *http.Client
	id      atomic.Int64
}

func (c *Client) post(ctx context.Context, method string, params any) (*http.Response, error) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id.Add(1), "method": method, "params": params})
	req, _ := http.NewRequestWithContext(ctx, "POST", c.URL, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	v := c.Version
	if v == "" {
		v = Version
	}
	req.Header.Set("A2A-Version", v) // 每个请求都带
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	return h.Do(req)
}

func (c *Client) call(ctx context.Context, method string, params, out any) error {
	resp, err := c.post(ctx, method, params)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return err
	}
	if r.Error != nil {
		return r.Error
	}
	return json.Unmarshal(r.Result, out)
}

func UserText(text string) Message {
	return Message{MessageID: newID("msg-"), Role: RoleUser, Parts: []Part{{Text: text}}}
}

func (c *Client) Send(ctx context.Context, m Message, cfg *SendConfig) (*StreamResponse, error) {
	var out StreamResponse
	return &out, c.call(ctx, "SendMessage", SendMessageRequest{Message: m, Configuration: cfg}, &out)
}

func (c *Client) GetTask(ctx context.Context, id string, historyLength *int) (*Task, error) {
	var t Task
	return &t, c.call(ctx, "GetTask", map[string]any{"id": id, "historyLength": historyLength}, &t)
}

func (c *Client) Cancel(ctx context.Context, id string) (*Task, error) {
	var t Task
	return &t, c.call(ctx, "CancelTask", map[string]any{"id": id}, &t)
}

// Stream 发送流式消息，每收到一个事件调用一次 on。
func (c *Client) Stream(ctx context.Context, m Message, on func(StreamResponse)) error {
	resp, err := c.post(ctx, "SendStreamingMessage", SendMessageRequest{Message: m})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		var r struct{ Error *Error }
		json.NewDecoder(resp.Body).Decode(&r)
		if r.Error != nil {
			return r.Error
		}
		return fmt.Errorf("不是 SSE 响应")
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue // 空行、注释行（以冒号开头）
		}
		var r struct {
			Result StreamResponse `json:"result"`
			Error  *Error         `json:"error"`
		}
		if err := json.Unmarshal([]byte(line[6:]), &r); err != nil {
			return err
		}
		if r.Error != nil {
			return r.Error
		}
		on(r.Result)
	}
	return sc.Err()
}

// ---- 把远程 Agent 包装成本地 Agent 的一个工具（接 2.1 课的工具循环）----

type RemoteAgentTool struct {
	Card   *AgentCard
	Client *Client
}

// Definition 用 Agent Card 生成一份 Anthropic 风格的工具定义。
func (t *RemoteAgentTool) Definition() map[string]any {
	var skills []string
	for _, s := range t.Card.Skills {
		skills = append(skills, s.Name+"："+s.Description)
	}
	name := "ask_" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return '_'
	}, t.Card.Name)
	return map[string]any{
		"name": name,
		"description": "把任务委托给远程 Agent「" + t.Card.Name + "」。" + t.Card.Description +
			" 能力：" + strings.Join(skills, "；") + "。如果它需要更多信息，会返回 task_id，补充信息时带上同一个 task_id。",
		"input_schema": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"message"},
			"properties": map[string]any{
				"message": map[string]any{"type": "string", "description": "交给对方的完整任务描述，对方看不到你的对话历史"},
				"task_id": map[string]any{"type": "string", "description": "继续之前的任务时填写"},
			}},
	}
}

// Call 执行一次委托，返回给模型的文字和 is_error。
func (t *RemoteAgentTool) Call(ctx context.Context, message, taskID string) (string, bool) {
	m := UserText(message)
	m.TaskID = taskID
	r, err := t.Client.Send(ctx, m, nil)
	if err != nil {
		return "委托失败：" + err.Error(), true
	}
	if r.Message != nil {
		return partsText(r.Message.Parts), false
	}
	task := r.Task
	q := ""
	if task.Status.Message != nil {
		q = partsText(task.Status.Message.Parts)
	}
	switch task.Status.State {
	case StateCompleted:
		var out []string
		for _, a := range task.Artifacts {
			out = append(out, partsText(a.Parts))
		}
		if len(out) == 0 {
			out = append(out, q)
		}
		return strings.Join(out, "\n"), false
	case StateInputRequired:
		return fmt.Sprintf("对方需要更多信息：%s（task_id=%s，补充后用同一个 task_id 再调用）", q, task.ID), false
	case StateAuthRequired:
		return "对方需要用户授权才能继续：" + q, true
	default:
		return fmt.Sprintf("任务结束，状态 %s：%s", task.Status.State, q), true
	}
}

func partsText(ps []Part) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(p.Text)
	}
	return b.String()
}
