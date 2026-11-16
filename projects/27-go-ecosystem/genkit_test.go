package goeco

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
)

func TestGenkitFlowRunsToolLoop(t *testing.T) {
	ctx := context.Background()
	g := NewGenkit(ctx)
	m, script := DefineScriptedModel(g, "test/loop",
		GenkitCall("r1", "check_stock", map[string]any{"sku": "A1"}),
		ai.NewModelTextMessage("A1 还有 7 件"),
	)
	stock := DefineStockTool(g, map[string]int{"A1": 7}, false)
	flow := DefineAskFlow(g, m, stock)

	out, err := flow.Run(ctx, "  A1 还有货吗？ ")
	if err != nil {
		t.Fatal(err)
	}
	if out != "A1 还有 7 件" {
		t.Fatalf("flow output %q", out)
	}
	reqs := script.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model called %d times", len(reqs))
	}
	if len(reqs[0].Tools) != 1 || reqs[0].Tools[0].Name != "check_stock" {
		t.Fatalf("tool definitions %v", reqs[0].Tools)
	}
	if !strings.Contains(toJSON(reqs[0].Tools[0].InputSchema), "商品编码") {
		t.Fatalf("schema %s", toJSON(reqs[0].Tools[0].InputSchema))
	}
	// 第二轮：system、user（已 trim）、model(toolRequest)、tool(toolResponse)
	msgs := reqs[1].Messages
	if len(msgs) != 4 || msgs[1].Text() != "A1 还有货吗？" || msgs[3].Role != ai.RoleTool {
		t.Fatalf("second request messages: %d", len(msgs))
	}
	if got := msgs[3].Content[0].ToolResponse.Output; toJSON(got) != "7" {
		t.Fatalf("tool output %v", got)
	}
}

func TestGenkitToolFailGoesBackToModel(t *testing.T) {
	ctx := context.Background()
	g := NewGenkit(ctx)
	m, script := DefineScriptedModel(g, "test/fail",
		GenkitCall("r1", "check_stock", map[string]any{"sku": "B9"}),
		ai.NewModelTextMessage("没有 B9 这个商品"),
	)
	stock := DefineStockTool(g, map[string]int{}, false)
	resp, err := genkit.Generate(ctx, g, ai.WithModel(m), ai.WithPrompt("B9?"), ai.WithTools(stock))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text() != "没有 B9 这个商品" {
		t.Fatalf("text %q", resp.Text())
	}
	last := script.Requests()[1].Messages
	out := toJSON(last[len(last)-1].Content[0].ToolResponse.Output)
	if !strings.Contains(out, "no such sku") {
		t.Fatalf("model should see the error, got %s", out)
	}
}

func TestGenkitPlainErrorStopsLoop(t *testing.T) {
	ctx := context.Background()
	g := NewGenkit(ctx)
	m, script := DefineScriptedModel(g, "test/down",
		GenkitCall("r1", "check_stock", map[string]any{"sku": "A1"}),
		ai.NewModelTextMessage("不会走到这里"),
	)
	stock := DefineStockTool(g, nil, true)
	_, err := genkit.Generate(ctx, g, ai.WithModel(m), ai.WithPrompt("A1?"), ai.WithTools(stock))
	if !errors.Is(err, ErrInventoryDown) {
		t.Fatalf("want ErrInventoryDown, got %v", err)
	}
	if len(script.Requests()) != 1 {
		t.Fatalf("model should be called once, got %d", len(script.Requests()))
	}
}

func TestGenkitMaxTurns(t *testing.T) {
	ctx := context.Background()
	g := NewGenkit(ctx)
	var replies []*ai.Message
	for i := 0; i < 5; i++ {
		replies = append(replies, GenkitCall("r", "check_stock", map[string]any{"sku": "A1"}))
	}
	m, script := DefineScriptedModel(g, "test/turns", replies...)
	stock := DefineStockTool(g, map[string]int{"A1": 1}, false)
	_, err := genkit.Generate(ctx, g, ai.WithModel(m), ai.WithPrompt("x"), ai.WithTools(stock), ai.WithMaxTurns(2))
	if !errors.Is(err, ai.ErrMaxTurnsExceeded) {
		t.Fatalf("want ErrMaxTurnsExceeded, got %v", err)
	}
	if n := len(script.Requests()); n != 3 {
		t.Fatalf("model called %d times, want 3", n)
	}
}

func TestGenkitInterruptAndResume(t *testing.T) {
	ctx := context.Background()
	g := NewGenkit(ctx)
	m, _ := DefineScriptedModel(g, "test/hitl",
		GenkitCall("r1", "transfer", map[string]any{"to": "alice", "amount": 5000}),
		ai.NewModelTextMessage("转账完成"),
	)
	log := &Transfers{}
	transfer := DefineTransferTool(g, 1000, log)

	resp, err := genkit.Generate(ctx, g, ai.WithModel(m), ai.WithPrompt("给 alice 转 5000"), ai.WithTools(transfer))
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != ai.FinishReasonInterrupted {
		t.Fatalf("finish reason %s", resp.FinishReason)
	}
	parts := resp.Interrupts()
	if len(parts) != 1 || len(log.Done()) != 0 {
		t.Fatalf("interrupts %d done %d", len(parts), len(log.Done()))
	}
	call, ok := transfer.Interrupted(parts[0])
	if !ok || call.Input.Amount != 5000 {
		t.Fatalf("claim failed: %v %+v", ok, call)
	}
	hold, ok := ai.InterruptAs[TransferHold](parts[0])
	if !ok || hold.Reason != "over_limit" {
		t.Fatalf("interrupt data %+v", hold)
	}

	resp, err = genkit.Generate(ctx, g,
		ai.WithModel(m),
		ai.WithMessages(resp.History()...),
		ai.WithTools(transfer),
		ai.WithResume(call.Restart(TransferApproval{Approved: true})),
	)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text() != "转账完成" || len(log.Done()) != 1 {
		t.Fatalf("text %q done %v", resp.Text(), log.Done())
	}
}

func TestGenkitRespondSkipsTool(t *testing.T) {
	ctx := context.Background()
	g := NewGenkit(ctx)
	m, script := DefineScriptedModel(g, "test/respond",
		GenkitCall("r1", "transfer", map[string]any{"to": "bob", "amount": 9000}),
		ai.NewModelTextMessage("好的，不转了"),
	)
	log := &Transfers{}
	transfer := DefineTransferTool(g, 1000, log)
	resp, _ := genkit.Generate(ctx, g, ai.WithModel(m), ai.WithPrompt("转 9000"), ai.WithTools(transfer))
	call, _ := transfer.Interrupted(resp.Interrupts()[0])

	// Respond 直接替工具给出输出，工具函数不会再执行。
	resp, err := genkit.Generate(ctx, g, ai.WithModel(m),
		ai.WithMessages(resp.History()...), ai.WithTools(transfer),
		ai.WithResume(call.Respond("人工拒绝：超出当日限额")))
	if err != nil {
		t.Fatal(err)
	}
	if len(log.Done()) != 0 || resp.Text() != "好的，不转了" {
		t.Fatalf("done %v text %q", log.Done(), resp.Text())
	}
	msgs := script.Requests()[1].Messages
	if out := toJSON(msgs[len(msgs)-1].Content[0].ToolResponse.Output); !strings.Contains(out, "人工拒绝") {
		t.Fatalf("model should see the response, got %s", out)
	}
}
