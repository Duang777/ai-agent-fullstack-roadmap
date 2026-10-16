package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var ctx = context.Background()

func setup(t *testing.T, streaming bool) (*Server, *Client, string) {
	s := NewServer(AgentCard{}, DemoExec)
	s.Reply = DemoReply
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	s.Card = DemoCard(srv.URL+"/a2a", streaming)
	return s, &Client{URL: srv.URL + "/a2a"}, srv.URL
}

func code(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

func TestAgentCard(t *testing.T) {
	_, _, base := setup(t, true)
	resp, _ := http.Get(base + "/.well-known/agent-card.json")
	if !strings.Contains(resp.Header.Get("Cache-Control"), "max-age") {
		t.Fatal("Agent Card 应该带缓存头")
	}
	resp.Body.Close()
	card, err := FetchCard(ctx, base)
	if err != nil || card.SupportedInterfaces[0].ProtocolBinding != "JSONRPC" || card.SupportedInterfaces[0].ProtocolVersion != "1.0" || len(card.Skills) != 2 {
		t.Fatal(card, err)
	}
}

func TestBlockingComplete(t *testing.T) {
	_, c, _ := setup(t, true)
	r, err := c.Send(ctx, UserText("你好"), nil)
	if err != nil || r.Task == nil {
		t.Fatal(r, err)
	}
	tk := r.Task
	if tk.Status.State != StateCompleted || partsText(tk.Status.Message.Parts) != "收到：你好" {
		t.Fatal("默认阻塞：返回时应已到终态", tk.Status)
	}
	if !strings.HasPrefix(tk.ContextID, "ctx-") || len(tk.History) != 2 || tk.History[0].TaskID != tk.ID {
		t.Fatal("服务端生成 contextId，history 有用户和 Agent 两条消息", tk)
	}
}

func TestDirectMessage(t *testing.T) {
	_, c, _ := setup(t, true)
	r, _ := c.Send(ctx, UserText("ping"), nil)
	if r.Task != nil || r.Message == nil || partsText(r.Message.Parts) != "pong" || r.Message.ContextID == "" {
		t.Fatal("简单请求可以直接回 Message，不创建任务；但要带 contextId", r)
	}
}

func TestMultiTurnInputRequired(t *testing.T) {
	_, c, _ := setup(t, true)
	r, _ := c.Send(ctx, UserText("帮我订机票"), nil)
	tk := r.Task
	if tk.Status.State != StateInputRequired || partsText(tk.Status.Message.Parts) != "从哪里出发，到哪里？" {
		t.Fatal("阻塞模式在中断态也会返回", tk.Status)
	}
	bad := UserText("北京到上海")
	bad.TaskID, bad.ContextID = tk.ID, "ctx-other"
	if _, err := c.Send(ctx, bad, nil); code(err) != CodeInvalidParams {
		t.Fatal("contextId 与 taskId 不一致必须拒绝", err)
	}
	ghost := UserText("北京到上海")
	ghost.TaskID = "task-nope"
	if _, err := c.Send(ctx, ghost, nil); code(err) != CodeTaskNotFound {
		t.Fatal("taskId 必须是已存在的任务", err)
	}
	next := UserText("北京到上海")
	next.TaskID = tk.ID // 只给 taskId：服务端推断 contextId
	r, err := c.Send(ctx, next, nil)
	if err != nil || r.Task.Status.State != StateCompleted || r.Task.ContextID != tk.ContextID {
		t.Fatal(r, err)
	}
	if partsText(r.Task.Artifacts[0].Parts) != "已预订：北京到上海" || len(r.Task.History) != 4 {
		t.Fatal(r.Task)
	}
	again := UserText("改成去广州")
	again.TaskID = tk.ID
	if _, err := c.Send(ctx, again, nil); code(err) != CodeUnsupportedOp {
		t.Fatal("终态任务不能再续，应新建任务并用 referenceTaskIds 引用", err)
	}
}

func TestContextAndListTasks(t *testing.T) {
	_, c, _ := setup(t, true)
	for _, x := range []struct{ ctx, text string }{{"ctx-trip", "第一件事"}, {"ctx-trip", "第二件事"}, {"ctx-work", "别的"}} {
		m := UserText(x.text)
		m.ContextID = x.ctx // 本实现接受客户端提供的 contextId
		if r, err := c.Send(ctx, m, nil); err != nil || r.Task.ContextID != x.ctx {
			t.Fatal(r, err)
		}
	}
	resp, _ := c.post(ctx, "ListTasks", map[string]any{"contextId": "ctx-trip", "pageSize": 1})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var r struct {
		Result struct {
			Tasks         []json.RawMessage
			NextPageToken *string
			TotalSize     int
		}
	}
	json.Unmarshal(body, &r)
	if r.Result.TotalSize != 2 || len(r.Result.Tasks) != 1 || r.Result.NextPageToken == nil || *r.Result.NextPageToken != "1" {
		t.Fatal(string(body))
	}
	if strings.Contains(string(r.Result.Tasks[0]), `"artifacts"`) {
		t.Fatal("includeArtifacts 默认 false：artifacts 字段必须完全省略")
	}
	resp, _ = c.post(ctx, "ListTasks", map[string]any{"contextId": "ctx-trip", "pageSize": 1, "pageToken": "1"})
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"nextPageToken":""`) {
		t.Fatal("最后一页 nextPageToken 必须存在且为空字符串", string(body))
	}
	var out map[string]any
	if err := c.call(ctx, "ListTasks", map[string]any{"pageSize": 0}, &out); code(err) != CodeInvalidParams {
		t.Fatal(err)
	}
}

func TestStreaming(t *testing.T) {
	_, c, _ := setup(t, true)
	var kinds []string
	var taskID string
	err := c.Stream(ctx, UserText("帮我写行程"), func(ev StreamResponse) {
		switch {
		case ev.Task != nil:
			kinds = append(kinds, "task")
			taskID = ev.Task.ID
		case ev.StatusUpdate != nil:
			kinds = append(kinds, "status:"+strings.TrimPrefix(string(ev.StatusUpdate.Status.State), "TASK_STATE_"))
		case ev.ArtifactUpdate != nil:
			a := ev.ArtifactUpdate
			k := "artifact"
			if a.Append {
				k += "+append"
			}
			if a.LastChunk {
				k += "+last"
			}
			kinds = append(kinds, k)
		}
	})
	want := "task,status:WORKING,artifact,artifact+append,artifact+append+last,status:COMPLETED"
	if err != nil || strings.Join(kinds, ",") != want {
		t.Fatalf("事件顺序\n got  %s\n want %s (%v)", strings.Join(kinds, ","), want, err)
	}
	tk, _ := c.GetTask(ctx, taskID, nil)
	if len(tk.Artifacts) != 1 || partsText(tk.Artifacts[0].Parts) != "第一天：故宫。第二天：长城。第三天：798。" {
		t.Fatal("append 的块要合并进同一个 artifact", tk.Artifacts)
	}
}

func TestStreamingNotSupported(t *testing.T) {
	_, c, _ := setup(t, false)
	err := c.Stream(ctx, UserText("帮我写行程"), func(StreamResponse) {})
	if code(err) != CodeUnsupportedOp {
		t.Fatal("Agent Card 没声明 streaming，就必须返回 UnsupportedOperationError", err)
	}
}

func TestReturnImmediatelyAndCancel(t *testing.T) {
	_, c, _ := setup(t, true)
	start := time.Now()
	r, _ := c.Send(ctx, UserText("慢慢做"), &SendConfig{ReturnImmediately: true})
	if time.Since(start) > 500*time.Millisecond || r.Task.Status.State.Terminal() {
		t.Fatal("returnImmediately：创建任务后立即返回", r.Task.Status)
	}
	var tk *Task
	for i := 0; i < 50; i++ { // 轮询
		tk, _ = c.GetTask(ctx, r.Task.ID, nil)
		if tk.Status.State == StateWorking {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if tk.Status.State != StateWorking {
		t.Fatal(tk.Status)
	}
	tk, err := c.Cancel(ctx, r.Task.ID)
	if err != nil || tk.Status.State != StateCanceled {
		t.Fatal(tk, err)
	}
	if _, err := c.Cancel(ctx, r.Task.ID); code(err) != CodeTaskNotCancelable {
		t.Fatal("终态任务不能再取消", err)
	}
	time.Sleep(30 * time.Millisecond)
	tk, _ = c.GetTask(ctx, r.Task.ID, nil)
	if tk.Status.State != StateCanceled {
		t.Fatal("Executor 被取消后返回 ctx.Err()，不能把 CANCELED 覆盖成 FAILED", tk.Status)
	}
}

func TestFailureAndValidation(t *testing.T) {
	_, c, _ := setup(t, true)
	r, _ := c.Send(ctx, UserText("让它失败"), nil)
	if r.Task.Status.State != StateFailed || !strings.Contains(partsText(r.Task.Status.Message.Parts), "下游服务不可用") {
		t.Fatal(r.Task.Status)
	}
	m := UserText("x")
	m.Parts = []Part{{Text: "a", URL: "https://example.com/a.pdf"}}
	if _, err := c.Send(ctx, m, nil); code(err) != CodeInvalidParams {
		t.Fatal("一个 part 只能有一种内容", err)
	}
	m = UserText("x")
	m.Role = RoleAgent
	if _, err := c.Send(ctx, m, nil); code(err) != CodeInvalidParams {
		t.Fatal(err)
	}
}

func TestIdempotentRetry(t *testing.T) {
	var runs atomic.Int32
	s := NewServer(AgentCard{}, func(ctx context.Context, r *Run) error { runs.Add(1); r.Complete("ok"); return nil })
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	c := &Client{URL: srv.URL + "/a2a"}
	m := UserText("扣款 100 元")
	a, _ := c.Send(ctx, m, nil)
	b, _ := c.Send(ctx, m, nil) // 网络超时后用同一个 messageId 重试
	if a.Task.ID != b.Task.ID || runs.Load() != 1 {
		t.Fatal("同一个 messageId 不能执行两次", a.Task.ID, b.Task.ID, runs.Load())
	}
}

func TestVersion(t *testing.T) {
	_, c, _ := setup(t, true)
	raw := func(ver string) string {
		req, _ := http.NewRequest("POST", c.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"x"}}`))
		if ver != "" {
			req.Header.Set("A2A-Version", ver)
		}
		resp, _ := http.DefaultClient.Do(req)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}
	if b := raw(""); !strings.Contains(b, "-32009") || !strings.Contains(b, "0.3") {
		t.Fatal("没有版本头按 0.3 处理，本 Agent 只支持 1.0", b)
	}
	if b := raw("2.0"); !strings.Contains(b, "-32009") || !strings.Contains(b, `"supportedVersions":["1.0"]`) {
		t.Fatal(b)
	}
	if b := raw("1.0"); !strings.Contains(b, "-32001") {
		t.Fatal("版本对了，才轮到 TaskNotFound", b)
	}
}

func TestHistoryLength(t *testing.T) {
	_, c, _ := setup(t, true)
	r, _ := c.Send(ctx, UserText("你好"), nil)
	zero, one := 0, 1
	a, _ := c.GetTask(ctx, r.Task.ID, &zero)
	b, _ := c.GetTask(ctx, r.Task.ID, &one)
	if len(a.History) != 0 || len(b.History) != 1 || b.History[0].Role != RoleAgent {
		t.Fatal(a.History, b.History)
	}
}

func TestRemoteAgentTool(t *testing.T) {
	_, c, base := setup(t, true)
	card, _ := FetchCard(ctx, base)
	tool := &RemoteAgentTool{Card: card, Client: c}
	def := tool.Definition()
	if def["name"] != "ask_trip_agent" || !strings.Contains(def["description"].(string), "订机票") {
		t.Fatal(def)
	}
	out, isErr := tool.Call(ctx, "帮我订机票", "")
	m := regexp.MustCompile(`task_id=(task-[0-9a-f]+)`).FindStringSubmatch(out)
	if isErr || m == nil || !strings.Contains(out, "从哪里出发") {
		t.Fatal("input-required 不是错误：把问题和 task_id 交给模型，让它去问用户", out)
	}
	out, isErr = tool.Call(ctx, "北京到上海", m[1])
	if isErr || out != "已预订：北京到上海" {
		t.Fatal(out)
	}
	out, isErr = tool.Call(ctx, "让它失败", "")
	if !isErr || !strings.Contains(out, "TASK_STATE_FAILED") {
		t.Fatal(out)
	}
}
