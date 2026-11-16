package goeco

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connect(t *testing.T, opts *mcp.ClientOptions) (*DocsServer, *mcp.ClientSession) {
	t.Helper()
	d := NewDocsServer()
	cs, err := ConnectInMemory(context.Background(), d.Server, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return d, cs
}

func TestMCPNegotiatesStatelessVersion(t *testing.T) {
	_, cs := connect(t, nil)
	if v := cs.InitializeResult().ProtocolVersion; v != "2026-07-28" {
		t.Fatalf("protocol version %s", v)
	}
}

func TestMCPSchemaInferredFromStruct(t *testing.T) {
	_, cs := connect(t, nil)
	var search *mcp.Tool
	for tl, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		if tl.Name == "search_docs" {
			search = tl
		}
	}
	if search == nil {
		t.Fatal("search_docs not listed")
	}
	in := toJSON(search.InputSchema)
	if !strings.Contains(in, `"required":["query"]`) || !strings.Contains(in, "要搜索的关键词") {
		t.Fatalf("input schema %s", in)
	}
	if out := toJSON(search.OutputSchema); !strings.Contains(out, `"titles"`) {
		t.Fatalf("output schema %s", out)
	}
}

func TestMCPValidatesBeforeHandler(t *testing.T) {
	d, cs := connect(t, nil)
	ctx := context.Background()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "search_docs", Arguments: map[string]any{"limit": 1}})
	// 缺少必填的 query：handler 不会被调用，结果是工具错误，不是协议错误。
	if err != nil {
		t.Fatalf("want tool error, got protocol error %v", err)
	}
	if !res.IsError || d.HandlerCalls() != 0 {
		t.Fatalf("IsError=%v handler calls=%d", res.IsError, d.HandlerCalls())
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "search_docs", Arguments: map[string]any{"query": "eino"}})
	if err != nil || res.IsError {
		t.Fatalf("valid call failed: %v %v", err, res)
	}
	if toJSON(res.StructuredContent) != `{"titles":["Eino Graph 入门","Eino ADK 中断与恢复"]}` {
		t.Fatalf("structured %s", toJSON(res.StructuredContent))
	}

	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "nope"}); err == nil {
		t.Fatal("unknown tool should be a protocol error")
	}
}

func TestMCPMultiRoundTripIsTransparent(t *testing.T) {
	var asked string
	_, cs := connect(t, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			asked = req.Params.Message
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"ok": true}}, nil
		},
	})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "delete_doc", Arguments: map[string]any{"title": "旧周报"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if asked != "确认删除《旧周报》？" || resultText(res) != "已删除《旧周报》" {
		t.Fatalf("asked %q result %q", asked, resultText(res))
	}
}

func TestMCPToolInsideEinoAgent(t *testing.T) {
	ctx := context.Background()
	_, cs := connect(t, nil)
	tools, err := MCPToEino(ctx, cs)
	if err != nil {
		t.Fatal(err)
	}
	m := NewScriptedModel(
		CallTool("c1", "broken", `{}`),
		CallTool("c2", "search_docs", `{"query":"genkit"}`),
		Say("找到 1 篇"),
	)
	runner, err := NewEinoRunner(ctx, m, nil, AgentOptions{}, tools...)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Collect(runner.Query(ctx, "找 genkit 文档"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Final != "找到 1 篇" {
		t.Fatalf("final %q", res.Final)
	}
	if !strings.HasPrefix(res.ToolOutputs["broken"], "工具出错：index is rebuilding") {
		t.Fatalf("broken output %q", res.ToolOutputs["broken"])
	}
	if res.ToolOutputs["search_docs"] != `{"titles":["Genkit flow 部署"]}` {
		t.Fatalf("search output %q", res.ToolOutputs["search_docs"])
	}
	// schema 经过 JSON 转换后，Eino 交给模型的参数定义里仍然有 required 和描述。
	info, _ := tools[indexOf(t, ctx, tools, "search_docs")].Info(ctx)
	js, _ := info.ParamsOneOf.ToJSONSchema()
	if b, _ := json.Marshal(js); !strings.Contains(string(b), `"required":["query"]`) {
		t.Fatalf("eino schema %s", b)
	}
}

func TestMCPToolInsideGenkit(t *testing.T) {
	ctx := context.Background()
	_, cs := connect(t, nil)
	g := NewGenkit(ctx)
	tools, err := MCPToGenkit(ctx, g, cs)
	if err != nil {
		t.Fatal(err)
	}
	m, script := DefineScriptedModel(g, "test/mcp",
		GenkitCall("r1", "mcp_broken", map[string]any{}),
		GenkitCall("r2", "mcp_search_docs", map[string]any{"query": "mcp"}),
		ai.NewModelTextMessage("done"),
	)
	resp, err := genkit.Generate(ctx, g, ai.WithModel(m), ai.WithPrompt("找 mcp"), ai.WithTools(tools...))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text() != "done" {
		t.Fatalf("text %q", resp.Text())
	}
	reqs := script.Requests()
	failed := toJSON(lastToolOutput(reqs[1]))
	if !strings.Contains(failed, "index is rebuilding") {
		t.Fatalf("model should see tool failure, got %s", failed)
	}
	if got := toJSON(lastToolOutput(reqs[2])); got != `{"titles":["MCP Go SDK 传输层"]}` {
		t.Fatalf("search output %s", got)
	}
	if !strings.Contains(toJSON(reqs[0].Tools), "要搜索的关键词") {
		t.Fatal("genkit tool definition lost the MCP schema")
	}
}

func lastToolOutput(req *ai.ModelRequest) any {
	msgs := req.Messages
	return msgs[len(msgs)-1].Content[0].ToolResponse.Output
}

func indexOf(t *testing.T, ctx context.Context, tools []tool.BaseTool, name string) int {
	t.Helper()
	for i, tl := range tools {
		if info, _ := tl.Info(ctx); info.Name == name {
			return i
		}
	}
	t.Fatalf("tool %s not found", name)
	return -1
}
