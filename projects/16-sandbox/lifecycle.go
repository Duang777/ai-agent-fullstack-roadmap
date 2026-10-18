package sandbox

import (
	"errors"
	"fmt"
	"time"
)

// 沙箱的生命周期。三家托管服务的规则不同，但形状一样：
// 跑着要花钱，闲置会超时；超时后要么暂停（保留文件和内存），要么销毁（状态全丢）。
type State int

const (
	Running State = iota
	Paused
	Killed
)

func (s State) String() string { return [...]string{"running", "paused", "killed"}[s] }

var ErrGone = errors.New("沙箱已销毁：新建一个并重新上传文件")

type Box struct {
	ID       string
	State    State
	Files    map[string]string
	Deadline time.Time
}

type Manager struct {
	Now       func() time.Time
	Timeout   time.Duration // 每次使用后续期多久
	OnTimeout State         // Paused（E2B 可配置自动暂停）或 Killed
	boxes     map[string]*Box
	seq       int
}

func (m *Manager) Create() *Box {
	if m.boxes == nil {
		m.boxes = map[string]*Box{}
	}
	m.seq++
	b := &Box{ID: fmt.Sprintf("sbx_%d", m.seq), State: Running, Files: map[string]string{}, Deadline: m.Now().Add(m.Timeout)}
	m.boxes[b.ID] = b
	return b
}

// Tick 处理到期的沙箱。
func (m *Manager) Tick() {
	for _, b := range m.boxes {
		if b.State == Running && !m.Now().Before(b.Deadline) {
			b.State = m.OnTimeout
			if b.State == Killed {
				b.Files = nil
			}
		}
	}
}

// Use 在沙箱里做一件事：暂停的自动恢复，销毁的报 ErrGone；成功后续期。
func (m *Manager) Use(id string, f func(b *Box)) error {
	m.Tick()
	b, ok := m.boxes[id]
	if !ok || b.State == Killed {
		return ErrGone
	}
	b.State = Running
	f(b)
	b.Deadline = m.Now().Add(m.Timeout)
	return nil
}

func (m *Manager) Kill(id string) {
	if b, ok := m.boxes[id]; ok {
		b.State, b.Files = Killed, nil
	}
}
