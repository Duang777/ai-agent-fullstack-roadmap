package claudesdk

import (
	"encoding/json"
	"sync"
)

// SessionStore 保存会话的对话历史（真实 SDK 写在 ~/.claude/projects/<cwd>/<id>.jsonl，
// 也可以通过 sessionStore 适配器镜像到自己的存储）。会话只保存对话，不保存文件系统。
type SessionStore struct {
	mu    sync.Mutex
	data  map[string][]byte
	order []string // 最近写入的在最后
}

func NewSessionStore() *SessionStore { return &SessionStore{data: map[string][]byte{}} }

func (s *SessionStore) Save(id string, msgs []Message) {
	b, _ := json.Marshal(msgs) // 存副本：之后改原切片不影响已保存的历史
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[id] = b
	for i, x := range s.order {
		if x == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.order = append(s.order, id)
}

func (s *SessionStore) Load(id string) ([]Message, bool) {
	s.mu.Lock()
	b, ok := s.data[id]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}
	var m []Message
	_ = json.Unmarshal(b, &m)
	return m, true
}

func (s *SessionStore) Latest() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.order) == 0 {
		return "", false
	}
	return s.order[len(s.order)-1], true
}
