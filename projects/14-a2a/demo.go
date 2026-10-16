package a2a

import (
	"context"
	"errors"
	"strings"
	"time"
)

func DemoCard(url string, streaming bool) AgentCard {
	return AgentCard{
		Name: "Trip Agent", Description: "订机票、写行程。", Version: "0.1.0",
		SupportedInterfaces: []AgentInterface{{URL: url, ProtocolBinding: "JSONRPC", ProtocolVersion: Version}},
		Capabilities:        AgentCapabilities{Streaming: streaming},
		DefaultInputModes:   []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"},
		Skills: []AgentSkill{
			{ID: "book-flight", Name: "订机票", Description: "需要出发地和目的地", Tags: []string{"travel"}},
			{ID: "write-itinerary", Name: "写行程", Description: "分段流式输出行程", Tags: []string{"travel", "writing"}},
		},
	}
}

// DemoExec 根据消息内容演示几种典型行为。
func DemoExec(ctx context.Context, r *Run) error {
	text := partsText(r.Input.Parts)
	first := partsText(r.History()[0].Parts)
	switch {
	case strings.Contains(first, "订机票"):
		if !strings.Contains(text, "到") {
			r.NeedInput("从哪里出发，到哪里？")
			return nil
		}
		r.Working("正在查航班")
		r.Artifact(Artifact{ArtifactID: "ticket", Name: "机票", Parts: []Part{{Text: "已预订：" + text}}}, false, true)
		r.Complete("")
	case strings.Contains(text, "写行程"):
		r.Working("开始写")
		for i, chunk := range []string{"第一天：故宫。", "第二天：长城。", "第三天：798。"} {
			time.Sleep(10 * time.Millisecond)
			r.Artifact(Artifact{ArtifactID: "itinerary", Name: "行程", Parts: []Part{{Text: chunk}}}, i > 0, i == 2)
		}
	case strings.Contains(text, "慢"):
		r.Working("处理中")
		select {
		case <-time.After(2 * time.Second):
			r.Complete("终于好了")
		case <-ctx.Done(): // 被取消：尽快停下
			return ctx.Err()
		}
	case strings.Contains(text, "失败"):
		return errors.New("下游服务不可用")
	default:
		r.Complete("收到：" + text)
	}
	return nil
}

func DemoReply(m Message) *Message {
	if partsText(m.Parts) == "ping" {
		return &Message{MessageID: newID("msg-"), Role: RoleAgent, Parts: []Part{{Text: "pong"}}}
	}
	return nil
}
