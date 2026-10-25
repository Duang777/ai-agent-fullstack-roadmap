// Package longrun 是 3.4 课的练习：检查点、可恢复执行、Human-in-the-loop。
package longrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

type Role string

const (
	User      Role = "user"
	Assistant Role = "assistant"
	ToolRole  Role = "tool"
)

type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"`
}

type Message struct {
	Role    Role       `json:"role"`
	Content string     `json:"content,omitempty"`
	Calls   []ToolCall `json:"calls,omitempty"`
	CallID  string     `json:"call_id,omitempty"`
	IsError bool       `json:"is_error,omitempty"`
}

type Status string

const (
	Running     Status = "running"
	Interrupted Status = "interrupted" // 等人
	Done        Status = "done"
)

// Pending 是一个等人审批的工具调用。
type Pending struct {
	Call     ToolCall  `json:"call"`
	Reason   string    `json:"reason"`
	Deadline time.Time `json:"deadline,omitempty"` // 零值表示不过期
}

type DecisionKind string

const (
	Approve DecisionKind = "approve"
	Reject  DecisionKind = "reject"
	Edit    DecisionKind = "edit" // 改参数后执行
)

type Decision struct {
	Kind    DecisionKind `json:"kind"`
	Args    string       `json:"args,omitempty"`    // Edit 用
	Message string       `json:"message,omitempty"` // Reject 时给模型看的理由
	By      string       `json:"by,omitempty"`      // 谁做的决定，审计用
}

// Checkpoint 是一次 run 的全部可恢复状态。只存数据，不存函数和连接。
type Checkpoint struct {
	RunID     string              `json:"run_id"`
	Seq       int                 `json:"seq"`     // 每保存一次 +1，用来做乐观锁
	Version   string              `json:"version"` // agent 定义的版本：提示词、工具、模型
	Status    Status              `json:"status"`
	Step      int                 `json:"step"` // 已经调用模型的次数
	Messages  []Message           `json:"messages"`
	Pending   []Pending           `json:"pending,omitempty"`
	Decisions map[string]Decision `json:"decisions,omitempty"` // callID -> 人的决定
}

var (
	ErrNotFound       = errors.New("检查点不存在")
	ErrConflict       = errors.New("检查点已被别人更新（seq 不匹配）")
	ErrVersion        = errors.New("检查点版本和当前 agent 定义不一致")
	ErrNotInterrupted = errors.New("run 不在等待审批")
	ErrUnknownCall    = errors.New("决定对应的调用不在待审批列表里")
	ErrBadDecision    = errors.New("决定不合法")
	ErrMaxSteps       = errors.New("步数用完")
)

// Store 保存检查点。Save 是比较并交换：只有存储里的 seq 等于 expect 才写入，
// 写入后的 seq = expect+1。新建 run 时 expect 传 0。
type Store interface {
	Save(ctx context.Context, cp Checkpoint, expect int) error
	Load(ctx context.Context, runID string) (Checkpoint, error)
}

// clone 用 JSON 往返做深拷贝：既切断别名，也顺便证明检查点确实能序列化。
func clone(cp Checkpoint) (Checkpoint, error) {
	b, err := json.Marshal(cp)
	if err != nil {
		return Checkpoint{}, err
	}
	var out Checkpoint
	err = json.Unmarshal(b, &out)
	return out, err
}

type MemStore struct {
	mu sync.Mutex
	m  map[string]Checkpoint
}

func (s *MemStore) Save(_ context.Context, cp Checkpoint, expect int) error {
	c, err := clone(cp)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]Checkpoint{}
	}
	if s.m[cp.RunID].Seq != expect {
		return fmt.Errorf("%w：存储 %d，期望 %d", ErrConflict, s.m[cp.RunID].Seq, expect)
	}
	c.Seq = expect + 1
	s.m[cp.RunID] = c
	return nil
}

func (s *MemStore) Load(_ context.Context, runID string) (Checkpoint, error) {
	s.mu.Lock()
	cp, ok := s.m[runID]
	s.mu.Unlock()
	if !ok {
		return Checkpoint{}, ErrNotFound
	}
	return clone(cp)
}

// FileStore 每个 run 一个 JSON 文件，写临时文件再 rename，进程在写一半时崩溃也不会留下半个文件。
// 互斥锁只保护同一进程；多进程要用数据库的 UPDATE ... WHERE seq = $1。
type FileStore struct {
	Dir string
	mu  sync.Mutex
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (s *FileStore) path(runID string) (string, error) {
	if !safeID.MatchString(runID) { // runID 可能来自外部，防止 ../ 之类的路径穿越
		return "", fmt.Errorf("非法 runID %q", runID)
	}
	return filepath.Join(s.Dir, runID+".json"), nil
}

func (s *FileStore) Save(ctx context.Context, cp Checkpoint, expect int) error {
	p, err := s.path(cp.RunID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := 0
	if old, err := s.load(p); err == nil {
		cur = old.Seq
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	if cur != expect {
		return fmt.Errorf("%w：存储 %d，期望 %d", ErrConflict, cur, expect)
	}
	cp.Seq = expect + 1
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir, cp.RunID+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // rename 成功后这里删不到东西，失败时清理临时文件
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil { // 先落盘再 rename
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (s *FileStore) load(p string) (Checkpoint, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return Checkpoint{}, ErrNotFound
	}
	if err != nil {
		return Checkpoint{}, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return Checkpoint{}, fmt.Errorf("检查点损坏：%w", err)
	}
	return cp, nil
}

func (s *FileStore) Load(_ context.Context, runID string) (Checkpoint, error) {
	p, err := s.path(runID)
	if err != nil {
		return Checkpoint{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(p)
}

// Ledger 记录已经执行过的工具调用结果，键是 runID/callID。
// 执行工具前先查，有结果就直接用，不再执行第二次。
type Ledger struct {
	mu sync.Mutex
	m  map[string]Message
}

func (l *Ledger) Get(key string) (Message, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, ok := l.m[key]
	return m, ok
}

func (l *Ledger) Put(key string, m Message) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]Message{}
	}
	l.m[key] = m
}
