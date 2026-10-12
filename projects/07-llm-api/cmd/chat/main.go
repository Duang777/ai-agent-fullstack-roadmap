// chat 用真实 API Key 试一下统一客户端。
//
//	export OPENAI_API_KEY=... ANTHROPIC_API_KEY=... GEMINI_API_KEY=...
//	go run ./cmd/chat -p anthropic -m claude-sonnet-5-5 "用一句话解释 KV Cache"
//	go run ./cmd/chat -p gemini -m gemini-3.8-flash -tools "现在几点？再算一下 17*23"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	llm "github.com/Duang777/ai-agent-fullstack-roadmap/projects/07-llm-api"
)

func main() {
	provider := flag.String("p", "openai", "openai | anthropic | gemini")
	model := flag.String("m", "", "模型名（必填，以各家文档为准）")
	useTools := flag.Bool("tools", false, "开启 get_time / multiply 两个演示工具，走 RunTools 循环")
	flag.Parse()
	prompt := strings.Join(flag.Args(), " ")
	if *model == "" || prompt == "" {
		flag.Usage()
		os.Exit(2)
	}

	var p llm.Provider
	switch *provider {
	case "openai":
		p = &llm.OpenAI{APIKey: os.Getenv("OPENAI_API_KEY")}
	case "anthropic":
		p = &llm.Anthropic{APIKey: os.Getenv("ANTHROPIC_API_KEY")}
	case "gemini":
		p = &llm.Gemini{APIKey: os.Getenv("GEMINI_API_KEY")}
	default:
		fmt.Fprintln(os.Stderr, "unknown provider", *provider)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt) // Ctrl-C 取消请求
	defer stop()
	req := llm.Request{Model: *model, Messages: []llm.Message{llm.UserText(prompt)}, MaxTokens: 2048}

	if !*useTools {
		resp, err := llm.Collect(p.Stream(ctx, &req), func(ev llm.Event) {
			if ev.Type == llm.EventText {
				fmt.Print(ev.Text)
			}
		})
		fmt.Println()
		report(resp, err)
		return
	}

	req.Tools = []llm.Tool{
		{Name: "get_time", Description: "返回当前时间（Asia/Shanghai）", Schema: json.RawMessage(`{"type":"object","properties":{}}`)},
		{Name: "multiply", Description: "两个整数相乘", Schema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}`)},
	}
	tools := map[string]llm.ToolFunc{
		"get_time": func(context.Context, json.RawMessage) (string, error) {
			loc, _ := time.LoadLocation("Asia/Shanghai")
			return time.Now().In(loc).Format(time.RFC3339), nil
		},
		"multiply": func(_ context.Context, args json.RawMessage) (string, error) {
			var a struct{ A, B int }
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			return fmt.Sprint(a.A * a.B), nil
		},
	}
	resp, hist, err := llm.RunTools(ctx, p, req, tools, 8)
	for _, m := range hist[1:] {
		for _, part := range m.Parts {
			switch part.Type {
			case llm.PartToolCall:
				fmt.Printf("→ %s(%s)\n", part.Name, part.Args)
			case llm.PartToolResult:
				fmt.Printf("← %s\n", part.Result)
			}
		}
	}
	if resp != nil {
		fmt.Println(resp.Message.Text())
	}
	report(resp, err)
}

func report(resp *llm.Response, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v (retryable=%v)\n", err, llm.IsRetryable(err))
		os.Exit(1)
	}
	u := resp.Usage
	fmt.Fprintf(os.Stderr, "[stop=%s raw=%s] input=%d (cached %d) output=%d (reasoning %d)\n",
		resp.Stop, resp.RawStop, u.InputTokens, u.CachedTokens, u.OutputTokens, u.ReasoningTokens)
}
