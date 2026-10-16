package a2a

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Run 是一次执行：Agent 在里面汇报进度、产出 artifact，最后把任务带到终态或中断态。
type Run struct {
	s     *Server
	rec   *record
	Input Message // 这一轮客户端发来的消息
}

func (r *Run) TaskID() string    { return r.rec.task.ID }
func (r *Run) ContextID() string { return r.rec.task.ContextID }

// History 返回这个任务的全部消息（多轮时用来回看之前的回答）。
func (r *Run) History() []Message {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return append([]Message(nil), r.rec.task.History...)
}

func agentMsg(text string) *Message {
	if text == "" {
		return nil
	}
	return &Message{MessageID: newID("msg-"), Role: RoleAgent, Parts: []Part{{Text: text}}}
}

func (r *Run) Status(state TaskState, text string) { r.s.setStatus(r.rec, state, agentMsg(text)) }
func (r *Run) Working(text string)                 { r.Status(StateWorking, text) }
func (r *Run) NeedInput(question string)           { r.Status(StateInputRequired, question) }
func (r *Run) Complete(text string)                { r.Status(StateCompleted, text) }

// Artifact 产出（或追加）一个结果。append=true 表示接在同 ID 的 artifact 后面，last=true 表示这是最后一块。
func (r *Run) Artifact(a Artifact, appendTo, last bool) {
	s := r.s
	s.mu.Lock()
	t := &r.rec.task
	merged := false
	if appendTo {
		for i := range t.Artifacts {
			if t.Artifacts[i].ArtifactID == a.ArtifactID {
				t.Artifacts[i].Parts = append(t.Artifacts[i].Parts, a.Parts...)
				merged = true
			}
		}
	}
	if !merged {
		t.Artifacts = append(t.Artifacts, a)
	}
	ev := StreamResponse{ArtifactUpdate: &ArtifactUpdate{TaskID: t.ID, ContextID: t.ContextID, Artifact: a, Append: appendTo, LastChunk: last}}
	s.broadcast(r.rec, ev)
	s.mu.Unlock()
}

// Executor 是 Agent 的业务逻辑。返回 error 而没有设置终态时，任务记为 FAILED；什么都没设置就返回，记为 COMPLETED。
type Executor func(ctx context.Context, r *Run) error

type record struct {
	task    Task
	created int
	subs    []chan StreamResponse
	cancel  context.CancelFunc
	done    chan struct{}
}

type Server struct {
	Card AgentCard
	Exec Executor
	// Reply 可选：简单请求直接回一条 Message，不创建任务。返回 nil 表示需要创建任务。
	Reply func(m Message) *Message

	mu    sync.Mutex
	tasks map[string]*record
	seq   int
	byMsg map[string]string // messageId → taskId，用来识别重复发送
}

func NewServer(card AgentCard, exec Executor) *Server {
	return &Server{Card: card, Exec: exec, tasks: map[string]*record{}, byMsg: map[string]string{}}
}

func newID(prefix string) string {
	b := make([]byte, 8)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// broadcast 必须在持锁时调用。
func (s *Server) broadcast(rec *record, ev StreamResponse) {
	for _, ch := range rec.subs {
		select {
		case ch <- ev:
		default: // 订阅者太慢：丢弃（真实实现要么给更大的缓冲，要么断开它让它用 GetTask 重新同步）
		}
	}
}

func (s *Server) setStatus(rec *record, state TaskState, msg *Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := &rec.task
	if t.Status.State.Terminal() {
		return // 终态不可再变（例如已经被取消）
	}
	if msg != nil {
		msg.TaskID, msg.ContextID = t.ID, t.ContextID
		t.History = append(t.History, *msg)
	}
	t.Status = TaskStatus{State: state, Message: msg, Timestamp: now()}
	s.broadcast(rec, StreamResponse{StatusUpdate: &StatusUpdate{TaskID: t.ID, ContextID: t.ContextID, Status: t.Status}})
	if state.Terminal() || state.Interrupted() {
		for _, ch := range rec.subs {
			close(ch)
		}
		rec.subs = nil
		select {
		case <-rec.done:
		default:
			close(rec.done)
		}
	}
}

func snapshot(t Task, historyLength *int, artifacts bool) Task {
	c := t
	c.History = append([]Message(nil), t.History...)
	c.Artifacts = append([]Artifact(nil), t.Artifacts...)
	if historyLength != nil {
		if *historyLength == 0 {
			c.History = nil
		} else if len(c.History) > *historyLength {
			c.History = c.History[len(c.History)-*historyLength:]
		}
	}
	if !artifacts {
		c.Artifacts = nil
	}
	return c
}

func validateMessage(m Message) *Error {
	if m.MessageID == "" {
		return &Error{Code: CodeInvalidParams, Message: "message.messageId 必填"}
	}
	if m.Role != RoleUser {
		return &Error{Code: CodeInvalidParams, Message: "客户端消息的 role 必须是 ROLE_USER"}
	}
	if len(m.Parts) == 0 {
		return &Error{Code: CodeInvalidParams, Message: "message.parts 不能为空"}
	}
	for i, p := range m.Parts {
		if err := p.Validate(); err != nil {
			return &Error{Code: CodeInvalidParams, Message: fmt.Sprintf("parts[%d]：%v", i, err)}
		}
	}
	return nil
}

// start 处理 SendMessage / SendStreamingMessage 的公共部分。返回值三选一：直接回复的 Message、任务记录、错误。
func (s *Server) start(req SendMessageRequest, subscribe bool) (*Message, *record, chan StreamResponse, *Error) {
	m := req.Message
	if e := validateMessage(m); e != nil {
		return nil, nil, nil, e
	}
	s.mu.Lock()
	// 幂等：同一个 messageId 重发（比如网络超时后重试），不重复执行
	if id, ok := s.byMsg[m.MessageID]; ok {
		rec := s.tasks[id]
		var ch chan StreamResponse
		if subscribe {
			ch = s.subscribeLocked(rec)
		}
		s.mu.Unlock()
		return nil, rec, ch, nil
	}
	var rec *record
	if m.TaskID != "" {
		rec = s.tasks[m.TaskID]
		if rec == nil {
			s.mu.Unlock()
			return nil, nil, nil, &Error{Code: CodeTaskNotFound, Message: "task not found: " + m.TaskID}
		}
		if m.ContextID != "" && m.ContextID != rec.task.ContextID {
			s.mu.Unlock()
			return nil, nil, nil, &Error{Code: CodeInvalidParams, Message: "contextId 与 taskId 所属的 context 不一致"}
		}
		st := rec.task.Status.State
		if st.Terminal() {
			s.mu.Unlock()
			return nil, nil, nil, &Error{Code: CodeUnsupportedOp, Message: "任务已结束（" + string(st) + "），请新建任务并用 referenceTaskIds 引用它"}
		}
		if !st.Interrupted() {
			s.mu.Unlock()
			return nil, nil, nil, &Error{Code: CodeUnsupportedOp, Message: "任务仍在执行，本实现只在 input-required / auth-required 时接受新消息"}
		}
	} else {
		if s.Reply != nil {
			s.mu.Unlock()
			if r := s.Reply(m); r != nil {
				r.ContextID = m.ContextID
				if r.ContextID == "" {
					r.ContextID = newID("ctx-")
				}
				return r, nil, nil, nil
			}
			s.mu.Lock()
		}
		ctxID := m.ContextID // 接受客户端给的 contextId；没有就由服务端生成
		if ctxID == "" {
			ctxID = newID("ctx-")
		}
		s.seq++
		rec = &record{created: s.seq, task: Task{ID: newID("task-"), ContextID: ctxID,
			Status: TaskStatus{State: StateSubmitted, Timestamp: now()}}}
		s.tasks[rec.task.ID] = rec
	}
	m.TaskID, m.ContextID = rec.task.ID, rec.task.ContextID
	rec.task.History = append(rec.task.History, m)
	s.byMsg[m.MessageID] = rec.task.ID
	rec.done = make(chan struct{})
	var ch chan StreamResponse
	if subscribe {
		ch = s.subscribeLocked(rec)
	}
	// 执行用独立的 context：returnImmediately 时请求已经返回，任务还要继续跑
	runCtx, cancel := context.WithCancel(context.Background())
	rec.cancel = cancel
	s.mu.Unlock()

	go func() {
		defer cancel()
		run := &Run{s: s, rec: rec, Input: m}
		err := s.Exec(runCtx, run)
		s.mu.Lock()
		st := rec.task.Status.State
		s.mu.Unlock()
		if !st.Terminal() && !st.Interrupted() {
			if err != nil {
				s.setStatus(rec, StateFailed, agentMsg("执行失败："+err.Error()))
			} else {
				s.setStatus(rec, StateCompleted, nil)
			}
		}
	}()
	return nil, rec, ch, nil
}

func (s *Server) subscribeLocked(rec *record) chan StreamResponse {
	ch := make(chan StreamResponse, 256)
	st := rec.task.Status.State
	ch <- StreamResponse{Task: ptr(snapshot(rec.task, nil, true))} // 第一个事件：当前任务快照
	if st.Terminal() || (st.Interrupted() && isClosed(rec.done)) {
		close(ch)
		return ch
	}
	rec.subs = append(rec.subs, ch)
	return ch
}

func isClosed(c chan struct{}) bool {
	if c == nil {
		return false
	}
	select {
	case <-c:
		return true
	default:
		return false
	}
}

func ptr[T any](v T) *T { return &v }

func (s *Server) task(rec *record, hl *int, artifacts bool) *Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ptr(snapshot(rec.task, hl, artifacts))
}

// ---- HTTP + JSON-RPC 绑定 ----

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		json.NewEncoder(w).Encode(s.Card)
	})
	mux.HandleFunc("POST /a2a", s.serveRPC)
	return mux
}

// majorMinor 只比较 Major.Minor：补丁版本不影响兼容性。
func majorMinor(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return v
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, e *Error) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: e})
}

func (s *Server) serveRPC(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRPC(w, nil, nil, &Error{Code: -32700, Message: "parse error"})
		return
	}
	ver := r.Header.Get("A2A-Version")
	if ver == "" {
		ver = r.URL.Query().Get("A2A-Version")
	}
	if ver == "" {
		ver = "0.3" // 规范：没有版本头按 0.3 处理
	}
	if majorMinor(ver) != Version {
		writeRPC(w, req.ID, nil, &Error{Code: CodeVersionNotSupp, Message: "A2A version not supported: " + ver,
			Data: map[string]any{"supportedVersions": []string{Version}}})
		return
	}
	switch req.Method {
	case "SendMessage":
		var p SendMessageRequest
		if json.Unmarshal(req.Params, &p) != nil {
			writeRPC(w, req.ID, nil, &Error{Code: CodeInvalidParams, Message: "invalid params"})
			return
		}
		msg, rec, _, e := s.start(p, false)
		if e != nil {
			writeRPC(w, req.ID, nil, e)
			return
		}
		if msg != nil {
			writeRPC(w, req.ID, StreamResponse{Message: msg}, nil)
			return
		}
		cfg := p.Configuration
		if cfg == nil {
			cfg = &SendConfig{}
		}
		if !cfg.ReturnImmediately {
			// 默认阻塞：等到终态或中断态
			select {
			case <-rec.done:
			case <-r.Context().Done():
				return
			}
		}
		writeRPC(w, req.ID, StreamResponse{Task: s.task(rec, cfg.HistoryLength, true)}, nil)
	case "SendStreamingMessage":
		if !s.Card.Capabilities.Streaming {
			writeRPC(w, req.ID, nil, &Error{Code: CodeUnsupportedOp, Message: "streaming not supported"})
			return
		}
		var p SendMessageRequest
		if json.Unmarshal(req.Params, &p) != nil {
			writeRPC(w, req.ID, nil, &Error{Code: CodeInvalidParams, Message: "invalid params"})
			return
		}
		msg, _, ch, e := s.start(p, true)
		if e != nil {
			writeRPC(w, req.ID, nil, e)
			return
		}
		if msg != nil {
			s.streamSSE(w, r, req.ID, nil, msg)
			return
		}
		s.streamSSE(w, r, req.ID, ch, nil)
	case "GetTask":
		var p struct {
			ID            string `json:"id"`
			HistoryLength *int   `json:"historyLength"`
		}
		json.Unmarshal(req.Params, &p)
		s.mu.Lock()
		rec := s.tasks[p.ID]
		s.mu.Unlock()
		if rec == nil {
			writeRPC(w, req.ID, nil, &Error{Code: CodeTaskNotFound, Message: "task not found"})
			return
		}
		writeRPC(w, req.ID, s.task(rec, p.HistoryLength, true), nil)
	case "CancelTask":
		var p struct {
			ID string `json:"id"`
		}
		json.Unmarshal(req.Params, &p)
		s.mu.Lock()
		rec := s.tasks[p.ID]
		s.mu.Unlock()
		if rec == nil {
			writeRPC(w, req.ID, nil, &Error{Code: CodeTaskNotFound, Message: "task not found"})
			return
		}
		s.mu.Lock()
		st, cancel := rec.task.Status.State, rec.cancel
		s.mu.Unlock()
		if st.Terminal() {
			writeRPC(w, req.ID, nil, &Error{Code: CodeTaskNotCancelable, Message: "task is " + string(st)})
			return
		}
		// 先把状态设成 CANCELED，再通知 Executor 停下。顺序反过来的话，Executor 可能抢先返回、
		// 被记成 FAILED，CANCELED 反而写不进去。
		s.setStatus(rec, StateCanceled, agentMsg("已取消"))
		if cancel != nil {
			cancel()
		}
		writeRPC(w, req.ID, s.task(rec, nil, true), nil)
	case "ListTasks":
		s.listTasks(w, req)
	default:
		writeRPC(w, req.ID, nil, &Error{Code: CodeMethodNotFound, Message: "method not found: " + req.Method})
	}
}

func (s *Server) listTasks(w http.ResponseWriter, req rpcRequest) {
	var p struct {
		ContextID        string    `json:"contextId"`
		Status           TaskState `json:"status"`
		PageSize         *int      `json:"pageSize"`
		PageToken        string    `json:"pageToken"`
		IncludeArtifacts bool      `json:"includeArtifacts"`
		HistoryLength    *int      `json:"historyLength"`
	}
	json.Unmarshal(req.Params, &p)
	size := 50
	if p.PageSize != nil {
		size = *p.PageSize
	}
	if size < 1 || size > 100 {
		writeRPC(w, req.ID, nil, &Error{Code: CodeInvalidParams, Message: "pageSize 必须在 1 到 100 之间"})
		return
	}
	s.mu.Lock()
	all := make([]*record, s.seq+1)
	for _, rec := range s.tasks {
		all[rec.created] = rec
	}
	var match []Task
	for _, rec := range all {
		if rec == nil || (p.ContextID != "" && rec.task.ContextID != p.ContextID) || (p.Status != "" && rec.task.Status.State != p.Status) {
			continue
		}
		match = append(match, snapshot(rec.task, p.HistoryLength, p.IncludeArtifacts))
	}
	s.mu.Unlock()
	start := 0
	if p.PageToken != "" {
		n, err := strconv.Atoi(p.PageToken)
		if err != nil || n < 0 || n > len(match) {
			writeRPC(w, req.ID, nil, &Error{Code: CodeInvalidParams, Message: "invalid pageToken"})
			return
		}
		start = n
	}
	end := min(start+size, len(match))
	next := ""
	if end < len(match) {
		next = strconv.Itoa(end)
	}
	page := match[start:end]
	if page == nil {
		page = []Task{}
	}
	// nextPageToken 必须总是出现：最后一页是空字符串
	writeRPC(w, req.ID, map[string]any{"tasks": page, "nextPageToken": next, "pageSize": size, "totalSize": len(match)}, nil)
}

// streamSSE 把事件写成 SSE：每个事件一行 data，内容是一个 JSON-RPC 响应。ch 关闭（终态或中断态）时结束。
func (s *Server) streamSSE(w http.ResponseWriter, r *http.Request, id json.RawMessage, ch chan StreamResponse, direct *Message) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	send := func(ev StreamResponse) {
		b, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: id, Result: ev})
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	if direct != nil {
		send(StreamResponse{Message: direct})
		return
	}
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			send(ev)
		case <-r.Context().Done():
			return
		}
	}
}
