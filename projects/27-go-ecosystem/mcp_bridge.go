package goeco

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	einojs "github.com/eino-contrib/jsonschema"
	"github.com/firebase/genkit/go/ai"
	gktool "github.com/firebase/genkit/go/ai/tool"
	"github.com/firebase/genkit/go/genkit"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------- MCP 服务端：用官方 Go SDK 写 ----------

// DocQuery 是 search_docs 的输入。官方 SDK 用 google/jsonschema-go 从结构体推断
// schema，字段描述写在 jsonschema 标签里（整段就是描述，不是 key=value）。
type DocQuery struct {
	Query string `json:"query" jsonschema:"要搜索的关键词"`
	Limit int    `json:"limit,omitempty" jsonschema:"最多返回几条"`
}

// DocHits 是 search_docs 的输出，会同时出现在 structuredContent 和文本内容里。
type DocHits struct {
	Titles []string `json:"titles"`
}

// DocsServer 包着一个 mcp.Server，并记录 handler 真正被调用的次数。
type DocsServer struct {
	Server *mcp.Server
	calls  atomic.Int64
}

// HandlerCalls 返回 search_docs 的 handler 被调用的次数。
func (d *DocsServer) HandlerCalls() int64 { return d.calls.Load() }

var docs = []string{"Eino Graph 入门", "Eino ADK 中断与恢复", "Genkit flow 部署", "MCP Go SDK 传输层"}

// NewDocsServer 创建一个有三个工具的 MCP 服务端：
//   - search_docs：带类型的工具，输入输出 schema 自动推断
//   - broken：handler 返回 error，SDK 会把它变成 IsError 结果而不是协议错误
//   - delete_doc：先用 MRTR（2026-07-28 的多轮请求）向客户端要确认，再执行
func NewDocsServer() *DocsServer {
	d := &DocsServer{Server: mcp.NewServer(&mcp.Implementation{Name: "docs", Version: "v0.1.0"}, nil)}

	mcp.AddTool(d.Server, &mcp.Tool{Name: "search_docs", Description: "搜索内部文档"},
		func(_ context.Context, _ *mcp.CallToolRequest, in DocQuery) (*mcp.CallToolResult, DocHits, error) {
			d.calls.Add(1)
			limit := in.Limit
			if limit <= 0 {
				limit = 10
			}
			var hits []string
			for _, t := range docs {
				if strings.Contains(strings.ToLower(t), strings.ToLower(in.Query)) && len(hits) < limit {
					hits = append(hits, t)
				}
			}
			return nil, DocHits{Titles: hits}, nil
		})

	mcp.AddTool(d.Server, &mcp.Tool{Name: "broken", Description: "总是失败"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return nil, nil, errors.New("index is rebuilding, retry later")
		})

	mcp.AddTool(d.Server, &mcp.Tool{Name: "delete_doc", Description: "删除文档，需要用户确认"},
		func(_ context.Context, req *mcp.CallToolRequest, in struct {
			Title string `json:"title"`
		}) (*mcp.CallToolResult, any, error) {
			if len(req.Params.InputResponses) == 0 {
				return &mcp.CallToolResult{
					InputRequests: mcp.InputRequestMap{"confirm": &mcp.ElicitParams{
						Message: "确认删除《" + in.Title + "》？",
						RequestedSchema: &jsonschema.Schema{
							Type:       "object",
							Properties: map[string]*jsonschema.Schema{"ok": {Type: "boolean"}},
						},
					}},
					RequestState: "title=" + in.Title,
				}, nil, nil
			}
			res, _ := req.Params.InputResponses["confirm"].(*mcp.ElicitResult)
			if res == nil || res.Action != "accept" || res.Content["ok"] != true {
				return textResult("已取消删除"), nil, nil
			}
			return textResult("已删除《" + in.Title + "》"), nil, nil
		})
	return d
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// ConnectInMemory 用内存传输把客户端和服务端接起来，测试和同进程部署都可以这样用。
// 换成 &mcp.CommandTransport{...} 或 &mcp.StreamableClientTransport{...} 就是真实部署。
func ConnectInMemory(ctx context.Context, srv *mcp.Server, opts *mcp.ClientOptions) (*mcp.ClientSession, error) {
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		return nil, err
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "bridge", Version: "v0.1.0"}, opts)
	return c.Connect(ctx, ct, nil)
}

// ---------- MCP → Eino ----------

// MCPToEino 把 MCP 服务端的全部工具适配成 Eino 工具。
func MCPToEino(ctx context.Context, cs *mcp.ClientSession) ([]tool.BaseTool, error) {
	var out []tool.BaseTool
	for t, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		info, err := einoToolInfo(t)
		if err != nil {
			return nil, err
		}
		out = append(out, &einoMCPTool{cs: cs, info: info})
	}
	return out, nil
}

// einoToolInfo 转换 schema。两个框架用的是不同的 JSON Schema 库
// （Eino 用 eino-contrib/jsonschema，MCP SDK 用 google/jsonschema-go），
// 最稳的桥是 JSON 本身：序列化再反序列化。
func einoToolInfo(t *mcp.Tool) (*schema.ToolInfo, error) {
	raw, err := json.Marshal(t.InputSchema)
	if err != nil {
		return nil, err
	}
	var js einojs.Schema
	if err := json.Unmarshal(raw, &js); err != nil {
		return nil, fmt.Errorf("tool %s: input schema: %w", t.Name, err)
	}
	return &schema.ToolInfo{Name: t.Name, Desc: t.Description, ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&js)}, nil
}

type einoMCPTool struct {
	cs   *mcp.ClientSession
	info *schema.ToolInfo
}

func (t *einoMCPTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }

// InvokableRun 调用远端工具。注意两类失败要分开：
//   - 协议错误（连接断了、工具不存在）→ 返回 error，Eino 会让这次运行失败
//   - 工具错误（IsError=true）→ 当作普通输出交还模型，让它决定重试还是换办法
func (t *einoMCPTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	res, err := t.cs.CallTool(ctx, &mcp.CallToolParams{Name: t.info.Name, Arguments: json.RawMessage(args)})
	if err != nil {
		return "", err
	}
	text := resultText(res)
	if res.IsError {
		return "工具出错：" + text, nil
	}
	return text, nil
}

// resultText 优先用 structuredContent（机器可读），没有时拼接文本内容。
func resultText(res *mcp.CallToolResult) string {
	if res.StructuredContent != nil && !res.IsError {
		return toJSON(res.StructuredContent)
	}
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// ---------- MCP → Genkit ----------

// MCPToGenkit 把 MCP 工具注册成 Genkit 工具。输入 schema 原样交给
// ai.WithInputSchema，输入类型因此是 any。
func MCPToGenkit(ctx context.Context, g *genkit.Genkit, cs *mcp.ClientSession) ([]ai.ToolRef, error) {
	var out []ai.ToolRef
	for t, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		var in map[string]any
		raw, _ := json.Marshal(t.InputSchema)
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		name := t.Name
		out = append(out, genkit.DefineTool(g, "mcp_"+name, t.Description,
			func(tc *ai.ToolContext, input any) (any, error) {
				res, err := cs.CallTool(tc, &mcp.CallToolParams{Name: name, Arguments: input})
				if err != nil {
					return nil, err
				}
				if res.IsError {
					// Genkit 里交还给模型的错误要显式用 tool.Fail 标出来。
					return nil, gktool.Fail(tc, errors.New(resultText(res)))
				}
				if res.StructuredContent != nil {
					return res.StructuredContent, nil
				}
				return resultText(res), nil
			}, ai.WithInputSchema(in)))
	}
	return out, nil
}
