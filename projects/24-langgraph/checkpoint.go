package graph

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// Task 是一个 super-step 里要运行的一个节点实例。ID 由步号、节点名、序号组成，
// 恢复时靠它把 pending writes 和 resume 值对回到同一个任务上。
type Task struct {
	ID   string `json:"id"`
	Node string `json:"node"`
	Arg  State  `json:"arg,omitempty"`
}

// Checkpoint 是某个 super-step 边界上的状态快照。
type Checkpoint struct {
	ID     int    `json:"id"`
	Parent int    `json:"parent"`
	Step   int    `json:"step"`
	Source string `json:"source"` // input / loop / update
	Values State  `json:"values"`
	Next   []Task `json:"next"` // 下一步要跑的任务；为空表示这个 thread 已经跑完
}

// Interrupt 是节点抛出的一次暂停请求。Index 是它在节点里的序号：恢复值严格按序号匹配。
type Interrupt struct {
	ID    string `json:"id"`
	Task  string `json:"task"`
	Index int    `json:"index"`
	Value any    `json:"value"`
}

// Write 挂在某个 checkpoint 上，记录这一步里某个任务的结果（对应 LangGraph 的 checkpoint_writes 表）。
type Write struct {
	TaskID    string     `json:"task_id"`
	Kind      string     `json:"kind"` // result / interrupt / error / resume
	Update    State      `json:"update,omitempty"`
	Goto      []Target   `json:"goto,omitempty"`
	Interrupt *Interrupt `json:"interrupt,omitempty"`
	Resume    any        `json:"resume,omitempty"`
	Err       string     `json:"err,omitempty"`
}

// Tuple 是 checkpoint 加上挂在它上面的所有写入。
type Tuple struct {
	Checkpoint
	Writes []Write
}

// Checkpointer 对应 BaseCheckpointSaver 的 put / put_writes / get_tuple / list。
type Checkpointer interface {
	Put(thread string, cp Checkpoint) (int, error)
	PutWrites(thread string, checkpointID int, ws ...Write) error
	Get(thread string, id int) (Tuple, bool) // id 为 0 表示最新
	List(thread string) []Checkpoint         // 新的在前
}

// MemSaver 是内存实现。所有数据进出都做一次 JSON 往返：
// 既是深拷贝，也强制“状态和 interrupt 载荷必须能序列化”，和真实的数据库实现行为一致。
type MemSaver struct {
	mu      sync.Mutex
	nextID  int
	threads map[string]*threadData
}

type threadData struct {
	cps    []Checkpoint
	writes map[int][]Write
}

func NewMemSaver() *MemSaver { return &MemSaver{threads: map[string]*threadData{}} }

func (m *MemSaver) thread(t string) *threadData {
	td := m.threads[t]
	if td == nil {
		td = &threadData{writes: map[int][]Write{}}
		m.threads[t] = td
	}
	return td
}

func (m *MemSaver) Put(thread string, cp Checkpoint) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	cp.ID = m.nextID
	var c Checkpoint
	if err := jsonCopy(cp, &c); err != nil {
		m.nextID--
		return 0, err
	}
	td := m.thread(thread)
	td.cps = append(td.cps, c)
	return c.ID, nil
}

func (m *MemSaver) PutWrites(thread string, id int, ws ...Write) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var cp []Write
	if err := jsonCopy(ws, &cp); err != nil {
		return err
	}
	td := m.thread(thread)
	td.writes[id] = append(td.writes[id], cp...)
	return nil
}

func (m *MemSaver) Get(thread string, id int) (Tuple, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	td := m.threads[thread]
	if td == nil || len(td.cps) == 0 {
		return Tuple{}, false
	}
	idx := len(td.cps) - 1
	if id != 0 {
		idx = -1
		for i, c := range td.cps {
			if c.ID == id {
				idx = i
			}
		}
		if idx < 0 {
			return Tuple{}, false
		}
	}
	var t Tuple
	_ = jsonCopy(td.cps[idx], &t.Checkpoint)
	_ = jsonCopy(td.writes[t.ID], &t.Writes)
	return t, true
}

func (m *MemSaver) List(thread string) []Checkpoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	td := m.threads[thread]
	if td == nil {
		return nil
	}
	var out []Checkpoint
	_ = jsonCopy(td.cps, &out)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

func jsonCopy(in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("无法序列化：%w", err)
	}
	return json.Unmarshal(b, out)
}
