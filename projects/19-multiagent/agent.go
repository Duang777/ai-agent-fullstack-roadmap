// Package multiagent 是 3.3 课的练习：Supervisor（agents as tools）、Handoff、Swarm。
package multiagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

type Role string

const (
	User      Role = "user"
	Assistant Role = "assistant"
	ToolRole  Role = "tool"
)

type ToolCall struct{ ID, Name, Args string }

type Message struct {
	Role    Role
	Content string
	Calls   []ToolCall
	CallID  string
	IsError bool
	Agent   string // 哪个 agent 说的；转交之后排查全靠它
}

type Usage struct{ Input, Output int }

type Tool func(ctx context.Context, args string) (string, error)

// Filter 在转交时改写下一个 agent 看到的历史。
type Filter func([]Message) []Message

type Agent struct {
	Name         string
	Instructions string
	Tools        map[string]Tool
	Handoffs     []string // 可以转交给谁（名字）
	Filter       Filter   // 转交到“我”时对历史做的过滤，nil 表示原样
}

// Model 看到当前 agent、它能用的工具名、历史，返回一条 assistant 消息。
type Model interface {
	Next(ctx context.Context, a *Agent, tools []string, h []Message) (Message, Usage, error)
}

var (
	ErrBudget          = errors.New("共享预算用完")
	ErrMaxSteps        = errors.New("步数用完")
	ErrTooManyHandoffs = errors.New("转交次数过多，可能在来回踢皮球")
	ErrTooDeep         = errors.New("子 agent 嵌套过深")
)

// Meter 是所有 agent 共享的预算，并按 agent 记账。
// 多智能体的 token 消耗是单 agent 的好几倍，不记账就不知道钱花在谁身上。
type Meter struct {
	mu       sync.Mutex
	Max      int // 0 不限
	total    int
	perAgent map[string]int
}

func (m *Meter) Add(agent string, u Usage) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.perAgent == nil {
		m.perAgent = map[string]int{}
	}
	n := u.Input + u.Output
	m.total += n
	m.perAgent[agent] += n
	if m.Max > 0 && m.total > m.Max {
		return fmt.Errorf("%w：已用 %d，上限 %d", ErrBudget, m.total, m.Max)
	}
	return nil
}

func (m *Meter) Total() int { m.mu.Lock(); defer m.mu.Unlock(); return m.total }
func (m *Meter) Of(agent string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.perAgent[agent]
}

const handoffPrefix = "transfer_to_"

// HandoffTool 和 OpenAI Agents SDK 的默认命名一致：transfer_to_<小写、空格换下划线>。
func HandoffTool(name string) string {
	return handoffPrefix + strings.ReplaceAll(strings.ToLower(name), " ", "_")
}

// RemoveTools 去掉结构化的工具调用和工具结果，只留对话文本，
// 对应 SDK 的 handoff_filters.remove_all_tools。
func RemoveTools(h []Message) []Message {
	var out []Message
	for _, m := range h {
		if m.Role == ToolRole {
			continue
		}
		if len(m.Calls) > 0 {
			if m.Content == "" {
				continue
			}
			m.Calls = nil
		}
		out = append(out, m)
	}
	return out
}
