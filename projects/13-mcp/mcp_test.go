package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var ctx = context.Background()

func stdioPair(t *testing.T, s *Server) *Client {
	cr, sw := io.Pipe() // server → client
	sr, cw := io.Pipe() // client → server
	go ServeStdio(ctx, s, sr, sw)
	t.Cleanup(func() { cw.Close() }) // 关掉 stdin：Server 读到 EOF 退出
	return &Client{T: NewStdioTransport(cr, cw), Info: Implementation{"test", "1"}, Caps: map[string]any{"elicitation": map[string]any{}}}
}

func rawCall(s *Server, msg string) Response {
	r := s.Handle(ctx, []byte(msg))
	if r == nil {
		return Response{}
	}
	return *r
}

const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

func TestDiscover(t *testing.T) {
	c := stdioPair(t, DemoServer())
	r, err := c.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	caps := r["capabilities"].(map[string]any)
	if r["resultType"] != "complete" || caps["tools"] == nil || caps["resources"] == nil || caps["prompts"] == nil {
		t.Fatal(r)
	}
	info := r["_meta"].(map[string]any)["io.modelcontextprotocol/serverInfo"].(map[string]any)
	if info["name"] != "demo" {
		t.Fatal(info)
	}
}

func TestStatelessMeta(t *testing.T) {
	s := DemoServer()
	// 没有握手，第一条请求就是 tools/list；但必须自带 _meta
	r := rawCall(s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	if r.Error == nil || r.Error.Code != CodeInvalidParams {
		t.Fatal("缺 _meta 应返回 -32602", r.Error)
	}
	r = rawCall(s, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	if r.Error == nil || r.Error.Code != CodeUnsupportedVer || !strings.Contains(fmt.Sprint(r.Error.Data), "2026-07-28") {
		t.Fatal("旧版本应返回 -32022 并列出支持的版本", r.Error)
	}
	r = rawCall(s, `{"jsonrpc":"2.0","id":null,"method":"tools/list","params":{`+meta+`}}`)
	if r.Error == nil || r.Error.Code != CodeInvalidRequest {
		t.Fatal("id 不能是 null", r.Error)
	}
	r = rawCall(s, `{"jsonrpc":"2.0","id":3,"method":"tools/lst","params":{`+meta+`}}`)
	if r.Error == nil || r.Error.Code != CodeMethodNotFound {
		t.Fatal(r.Error)
	}
	if s.Handle(ctx, []byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":9}}`)) != nil {
		t.Fatal("通知不能有响应")
	}
}

func TestToolsListDeterministicPaging(t *testing.T) {
	s := DemoServer()
	s.PageSize = 2
	c := stdioPair(t, s)
	ts, err := c.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, x := range ts {
		names = append(names, x["name"].(string))
	}
	if strings.Join(names, ",") != "add_item,create_basket,delete_repo,get_weather" {
		t.Fatal(names)
	}
	a := rawCall(s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+meta+`}}`)
	b := rawCall(s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+meta+`}}`)
	if string(a.Result) != string(b.Result) {
		t.Fatal("同样的工具集合，两次 tools/list 应逐字节相同（客户端缓存、Prompt Cache 才稳定）")
	}
	if !strings.Contains(string(a.Result), `"nextCursor":"2"`) || !strings.Contains(string(a.Result), `"ttlMs":300000`) {
		t.Fatal(string(a.Result))
	}
}

func TestProtocolErrorVsToolError(t *testing.T) {
	c := stdioPair(t, DemoServer())
	_, _, err := c.CallTool(ctx, "get_wether", map[string]any{"city": "北京"})
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeInvalidParams {
		t.Fatal("未知工具是协议错误", err)
	}
	r, _, err := c.CallTool(ctx, "get_weather", map[string]any{})
	if err != nil || r["isError"] != true {
		t.Fatal("缺参数是工具执行错误：isError 结果，模型可以自己补上", r, err)
	}
	r, _, _ = c.CallTool(ctx, "get_weather", map[string]any{"city": "北京"})
	if r["structuredContent"].(map[string]any)["temp_c"] != 18.0 || r["content"].([]any)[0].(map[string]any)["text"] != "北京：晴 18°C" {
		t.Fatal(r)
	}
}

func TestResourcesAndPrompts(t *testing.T) {
	c := stdioPair(t, DemoServer())
	txt, err := c.ReadResource(ctx, "notes://roadmap")
	if err != nil || !strings.Contains(txt, "2.2 MCP") {
		t.Fatal(txt, err)
	}
	_, err = c.ReadResource(ctx, "notes://nope")
	var e *Error
	if !errors.As(err, &e) || e.Code != -32602 || !strings.Contains(fmt.Sprint(e.Data), "notes://nope") {
		t.Fatal("资源不存在：-32602（不是旧的 -32002），也不能返回空 contents", err)
	}
	p, err := c.GetPrompt(ctx, "code_review", map[string]string{"code": "x := 1"})
	if err != nil || !strings.Contains(p, "关注 正确性") || !strings.HasSuffix(p, "x := 1") {
		t.Fatal(p, err)
	}
	if _, err := c.GetPrompt(ctx, "code_review", nil); !errors.As(err, &e) || e.Code != CodeInvalidParams {
		t.Fatal("缺必填参数", err)
	}
}

func TestMRTRConfirm(t *testing.T) {
	c := stdioPair(t, DemoServer())
	var asked string
	c.Elicit = func(msg string, schema map[string]any) (string, map[string]any) {
		asked = msg
		return "accept", map[string]any{"repo": "duang777/demo"}
	}
	r, rounds, err := c.CallTool(ctx, "delete_repo", map[string]any{"repo": "duang777/demo"})
	if err != nil || rounds != 2 || r["content"].([]any)[0].(map[string]any)["text"] != "已删除 duang777/demo" {
		t.Fatal(r, rounds, err)
	}
	t.Logf("第 1 轮：Server 返回 input_required，问用户：%s；第 2 轮：带着回答重发，完成", asked)

	c.Elicit = func(string, map[string]any) (string, map[string]any) { return "decline", nil }
	r, _, _ = c.CallTool(ctx, "delete_repo", map[string]any{"repo": "duang777/demo"})
	if r["content"].([]any)[0].(map[string]any)["text"] != "用户取消了删除。" {
		t.Fatal(r)
	}

	c.Caps = map[string]any{} // 不声明 elicitation
	_, _, err = c.CallTool(ctx, "delete_repo", map[string]any{"repo": "duang777/demo"})
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeMissingCap {
		t.Fatal("客户端没声明 elicitation，Server 不能发 elicitation 请求，应返回 -32021", err)
	}
}

func TestRequestStateSecurity(t *testing.T) {
	s := DemoServer()
	first := rawCall(s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_repo","arguments":{"repo":"a/b"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{"elicitation":{}}}}}`)
	var r struct {
		ResultType   string `json:"resultType"`
		RequestState string `json:"requestState"`
	}
	json.Unmarshal(first.Result, &r)
	if r.ResultType != "input_required" || r.RequestState == "" {
		t.Fatal(string(first.Result))
	}
	retry := func(id int, repo, st string) *Error {
		msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"delete_repo","arguments":{"repo":%q},"inputResponses":{"confirm":{"action":"accept","content":{"repo":%q}}},"requestState":%q,`+meta+`}}`, id, repo, repo, st)
		return rawCall(s, msg).Error
	}
	if e := retry(2, "a/b", r.RequestState); e != nil {
		t.Fatal("正常重试应该成功", e)
	}
	if e := retry(3, "victim/prod", r.RequestState); e == nil || !strings.Contains(e.Message, "不匹配") {
		t.Fatal("拿 a/b 的确认去删 victim/prod：必须拒绝", e)
	}
	tampered := r.RequestState[:len(r.RequestState)-2] + "AA"
	if e := retry(4, "a/b", tampered); e == nil || !strings.Contains(e.Message, "篡改") {
		t.Fatal("篡改签名必须拒绝", e)
	}
	if e := retry(5, "a/b", ""); e == nil {
		t.Fatal("没有 requestState 却带 inputResponses：必须拒绝")
	}
	s.Now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	if e := retry(6, "a/b", r.RequestState); e == nil || !strings.Contains(e.Message, "过期") {
		t.Fatal("过期必须拒绝", e)
	}
}

func TestStatefulHandle(t *testing.T) {
	c := stdioPair(t, DemoServer())
	r, _, _ := c.CallTool(ctx, "create_basket", map[string]any{})
	id := r["structuredContent"].(map[string]any)["basket_id"].(string)
	for i := 1; i <= 2; i++ {
		r, _, _ = c.CallTool(ctx, "add_item", map[string]any{"basket_id": id, "sku": fmt.Sprint("sku-", i)})
	}
	if r["content"].([]any)[0].(map[string]any)["text"] != "已加入，篮子里有 2 件" {
		t.Fatal(r)
	}
	r, _, _ = c.CallTool(ctx, "add_item", map[string]any{"basket_id": "bsk_unknown", "sku": "x"})
	if r["isError"] != true || !strings.Contains(r["content"].([]any)[0].(map[string]any)["text"].(string), "create_basket") {
		t.Fatal("未知句柄：工具执行错误，并告诉模型怎么恢复", r)
	}
}

func TestStdioOutOfOrder(t *testing.T) {
	c := stdioPair(t, DemoServer())
	type res struct {
		city string
		at   time.Time
	}
	ch := make(chan res, 2)
	for _, x := range []struct {
		city  string
		delay float64
	}{{"慢城", 80}, {"快城", 0}} {
		go func() {
			r, _, err := c.CallTool(ctx, "get_weather", map[string]any{"city": x.city, "delay_ms": x.delay})
			if err != nil {
				t.Error(err)
			}
			ch <- res{r["structuredContent"].(map[string]any)["city"].(string), time.Now()}
		}()
		time.Sleep(5 * time.Millisecond)
	}
	a, b := <-ch, <-ch
	if a.city != "快城" || b.city != "慢城" {
		t.Fatal("后发的快请求应先返回，且结果按 id 对得上", a, b)
	}
}

func TestStdioCancel(t *testing.T) {
	c := stdioPair(t, DemoServer())
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, _, err := c.CallTool(cctx, "get_weather", map[string]any{"city": "北京", "delay_ms": 200.0})
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatal(err)
	}
}

func TestHTTP(t *testing.T) {
	srv := httptest.NewServer(HTTPHandler(DemoServer(), "https://app.example"))
	defer srv.Close()
	c := &Client{T: &HTTPTransport{URL: srv.URL}, Caps: map[string]any{}}
	if _, err := c.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	txt, err := c.ReadResource(ctx, "notes://笔记/今天")
	if err != nil || txt != "今天学 MCP" {
		t.Fatal("非 ASCII 的 URI 走 base64 头也能对上", txt, err)
	}

	post := func(hdr map[string]string, body string) (int, string) {
		req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	ok := map[string]string{"MCP-Protocol-Version": Version, "Mcp-Method": "tools/call", "Mcp-Name": "get_weather"}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_weather","arguments":{"city":"北京"},` + meta + `}}`
	if code, _ := post(ok, body); code != 200 {
		t.Fatal(code)
	}
	bad := map[string]string{"MCP-Protocol-Version": Version, "Mcp-Method": "tools/call", "Mcp-Name": "get_weather"}
	evil := strings.Replace(body, `"name":"get_weather"`, `"name":"delete_repo"`, 1)
	if code, b := post(bad, evil); code != 400 || !strings.Contains(b, "-32020") {
		t.Fatal("头写 get_weather、body 写 delete_repo：网关按头放行、Server 按 body 执行，必须拒绝", code, b)
	}
	if code, _ := post(map[string]string{"Origin": "https://evil.example"}, body); code != 403 {
		t.Fatal("非法 Origin 应 403", code)
	}
	if code, _ := post(nil, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{}}`); code != 202 {
		t.Fatal("通知应 202", code)
	}
	nf := map[string]string{"MCP-Protocol-Version": Version, "Mcp-Method": "tools/lst"}
	if code, b := post(nf, `{"jsonrpc":"2.0","id":2,"method":"tools/lst","params":{`+meta+`}}`); code != 404 || !strings.Contains(b, "-32601") {
		t.Fatal("未知方法应 404 + -32601", code, b)
	}
	resp, _ := http.Get(srv.URL)
	if resp.StatusCode != 405 {
		t.Fatal("2026-07-28 没有 GET 流了：应 405", resp.StatusCode)
	}
}

func TestEncodeHeader(t *testing.T) {
	cases := map[string]string{
		"us-west1":           "us-west1",
		"Hello, 世界":          "=?base64?SGVsbG8sIOS4lueVjA==?=",
		" padded ":           "=?base64?IHBhZGRlZCA=?=",
		"line1\nline2":       "=?base64?bGluZTEKbGluZTI=?=",
		"=?base64?literal?=": "=?base64?PT9iYXNlNjQ/bGl0ZXJhbD89?=",
	}
	for in, want := range cases {
		if got := EncodeHeader(in); got != want || decodeHeader(got) != in {
			t.Errorf("%q → %q，期望 %q", in, got, want)
		}
	}
}

func TestHostAggregate(t *testing.T) {
	mk := func(name, answer string) *Client {
		s := NewServer(name)
		s.AddTool(Tool{Name: "search", Description: "在 " + name + " 里搜索",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}, "required": []string{"q"}},
			Handler: func(ctx context.Context, c CallCtx) (*ToolResult, *NeedInput) {
				return &ToolResult{Text: answer + "：" + c.Args["q"].(string)}, nil
			}})
		return stdioPair(t, s)
	}
	h := &Host{Servers: map[string]*Client{"github": mk("github", "GitHub 结果"), "docs": mk("docs", "文档结果")}}
	tools, err := h.Tools(ctx)
	if err != nil || len(tools) != 2 || tools[0]["name"] != "docs__search" || tools[1]["name"] != "github__search" {
		t.Fatal(tools, err)
	}
	if tools[0]["input_schema"] == nil {
		t.Fatal("要渲染成模型 API 的工具定义")
	}
	txt, isErr := h.Call(ctx, "github__search", map[string]any{"q": "mcp"})
	if isErr || txt != "GitHub 结果：mcp" {
		t.Fatal(txt)
	}
	if _, isErr := h.Call(ctx, "search", nil); !isErr {
		t.Fatal("没有前缀的名字找不到")
	}
}
