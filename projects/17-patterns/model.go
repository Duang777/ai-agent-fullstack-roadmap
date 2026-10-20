// Package patterns 是 3.1 课的练习：ReAct、Plan-and-Execute（含 ReWOO / LLMCompiler 式并行）、Reflexion。
// 模型用脚本化的假实现代替，测试只关心“循环本身写对了没有”：何时停、错误怎么回给模型、计划怎么执行和重排、反思怎么积累。
package patterns

import (
	"context"
	"errors"
	"fmt"
)

type Role string

const (
	User      Role = "user"
	Assistant Role = "assistant"
	ToolRole  Role = "tool"
)

type ToolCall struct {
	ID, Name, Args string
}

type Message struct {
	Role    Role
	Content string
	Calls   []ToolCall // assistant 发起的工具调用
	CallID  string     // tool 消息对应哪次调用
	IsError bool
}

// Model 给出下一条 assistant 消息：要么带工具调用，要么是最终回答。
type Model interface {
	Next(ctx context.Context, history []Message) (Message, error)
}

type Tool func(ctx context.Context, args string) (string, error)

// Script 按顺序返回预先写好的回复，并记下每次看到的历史，方便测试检查“模型看到了什么”。
type Script struct {
	Replies []Message
	Seen    [][]Message
}

func (s *Script) Next(_ context.Context, h []Message) (Message, error) {
	s.Seen = append(s.Seen, append([]Message(nil), h...))
	if len(s.Replies) == 0 {
		return Message{}, errors.New("脚本用完了")
	}
	m := s.Replies[0]
	s.Replies = s.Replies[1:]
	m.Role = Assistant
	return m, nil
}

func call(id, name, args string) ToolCall { return ToolCall{ID: id, Name: name, Args: args} }

func errf(format string, a ...any) error { return fmt.Errorf(format, a...) }
