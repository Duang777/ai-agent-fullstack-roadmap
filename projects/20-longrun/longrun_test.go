package longrun

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// bank 是一个带幂等键的假外部系统：同一个键只扣一次款。
type bank struct {
	mu      sync.Mutex
	seen    map[string]bool
	calls   int // 收到的请求次数
	charges int // 真正生效的次数
	lastArg string
}

func (b *bank) transfer(_ context.Context, args, key string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.seen == nil {
		b.seen = map[string]bool{}
	}
	if b.seen[key] {
		return "重复请求，已忽略", nil
	}
	b.seen[key] = true
	b.charges++
	b.lastArg = args
	return "转账成功", nil
}

func overLimit(args map[string]any) bool {
	amt, _ := args["amount"].(float64)
	return amt > 100
}

// scripted 模型：没有工具结果时发起调用，看到工具结果后给出最终回答。
func scripted(calls ...ToolCall) Model {
	return ModelFunc(func(_ context.Context, h []Message) (Message, error) {
		last := h[len(h)-1]
		if last.Role == User {
			return Message{Calls: calls}, nil
		}
		var parts []string
		for _, m := range h {
			if m.Role == ToolRole {
				parts = append(parts, m.CallID+"="+m.Content)
			}
		}
		return Message{Content: "完成：" + strings.Join(parts, "; ")}, nil
	})
}

func newRunner(m Model, b *bank, s Store) *Runner {
	return &Runner{
		Model:   m,
		Store:   s,
		Ledger:  &Ledger{},
		Version: "v1",
		Tools: map[string]Tool{
			"transfer": {Fn: b.transfer, NeedsApproval: overLimit, Editable: true},
			"balance": {Fn: func(context.Context, string, string) (string, error) {
				return "余额 500", nil
			}},
		},
	}
}

var ctx = context.Background()

func big(id string) ToolCall {
	return ToolCall{ID: id, Name: "transfer", Args: `{"to":"alice","amount":300}`}
}
func small(id string) ToolCall {
	return ToolCall{ID: id, Name: "transfer", Args: `{"to":"bob","amount":20}`}
}

// 每个 tool_call 恰好有一个结果，顺序跟着 assistant 消息走。
func checkPairing(t *testing.T, h []Message) {
	t.Helper()
	open := map[string]bool{}
	for _, m := range h {
		for _, c := range m.Calls {
			open[c.ID] = true
		}
		if m.Role == ToolRole {
			if !open[m.CallID] {
				t.Fatalf("结果 %s 没有对应调用或重复", m.CallID)
			}
			delete(open, m.CallID)
		}
	}
	if len(open) > 0 {
		t.Fatalf("有调用没结果：%v", open)
	}
}

func TestCheckpointAfterEveryStep(t *testing.T) {
	b, s := &bank{}, &MemStore{}
	r := newRunner(scripted(ToolCall{ID: "c1", Name: "balance", Args: `{}`}, small("c2")), b, s)
	res, err := r.Start(ctx, "run1", "查余额再给 bob 转 20")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != Done || !strings.Contains(res.Final, "c2=转账成功") {
		t.Fatalf("结果不对：%+v", res)
	}
	// user、assistant(调用)、两个工具结果、最终回答、Done：至少 6 次保存
	if res.Seq < 6 {
		t.Fatalf("检查点太少：seq=%d", res.Seq)
	}
	cp, _ := s.Load(ctx, "run1")
	checkPairing(t, cp.Messages)
}

func TestInterruptBeforeRiskyCall(t *testing.T) {
	b, s := &bank{}, &MemStore{}
	r := newRunner(scripted(big("c1")), b, s)
	res, err := r.Start(ctx, "run1", "给 alice 转 300")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != Interrupted || len(res.Pending) != 1 || res.Pending[0].Call.ID != "c1" {
		t.Fatalf("应该停下等审批：%+v", res)
	}
	if b.calls != 0 {
		t.Fatal("审批前不能执行")
	}
	cp, _ := s.Load(ctx, "run1")
	if cp.Status != Interrupted {
		t.Fatal("暂停状态必须写进检查点")
	}
}

func TestResumeApproveRunsOnce(t *testing.T) {
	b, s := &bank{}, &MemStore{}
	r := newRunner(scripted(big("c1")), b, s)
	r.Start(ctx, "run1", "给 alice 转 300")
	// 换一个 Runner 实例恢复，相当于审批发生在另一个进程、几小时之后
	r2 := newRunner(scripted(big("c1")), b, s)
	r2.Ledger = r.Ledger
	res, err := r2.Resume(ctx, "run1", map[string]Decision{"c1": {Kind: Approve, By: "duang777"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != Done || b.charges != 1 {
		t.Fatalf("应该执行一次后完成：%+v charges=%d", res, b.charges)
	}
	cp, _ := s.Load(ctx, "run1")
	if cp.Decisions["c1"].By != "duang777" {
		t.Fatal("决定要留在检查点里供审计")
	}
	checkPairing(t, cp.Messages)
}

func TestResumeRejectTellsModel(t *testing.T) {
	b, s := &bank{}, &MemStore{}
	r := newRunner(scripted(big("c1")), b, s)
	r.Start(ctx, "run1", "给 alice 转 300")
	res, err := r.Resume(ctx, "run1", map[string]Decision{"c1": {Kind: Reject, Message: "金额太大"}})
	if err != nil {
		t.Fatal(err)
	}
	if b.calls != 0 {
		t.Fatal("拒绝后不能执行")
	}
	if !strings.Contains(res.Final, "用户拒绝了这个操作：金额太大") {
		t.Fatalf("拒绝理由要作为工具结果回给模型：%q", res.Final)
	}
}

func TestResumeEditArgs(t *testing.T) {
	b, s := &bank{}, &MemStore{}
	r := newRunner(scripted(big("c1")), b, s)
	r.Start(ctx, "run1", "给 alice 转 300")
	_, err := r.Resume(ctx, "run1", map[string]Decision{"c1": {Kind: Edit, Args: `{"to":"alice","amount":80}`}})
	if err != nil {
		t.Fatal(err)
	}
	if b.lastArg != `{"to":"alice","amount":80}` {
		t.Fatalf("应该用改过的参数执行：%s", b.lastArg)
	}

	// 不允许编辑的工具、非 JSON 对象的参数都要拒绝
	r.Tools["transfer"] = Tool{Fn: b.transfer, NeedsApproval: overLimit}
	r.Start(ctx, "run2", "给 alice 转 300")
	if _, err := r.Resume(ctx, "run2", map[string]Decision{"c1": {Kind: Edit, Args: `{"amount":1}`}}); !errors.Is(err, ErrBadDecision) {
		t.Fatalf("不可编辑的工具应拒绝：%v", err)
	}
}

func TestPartialDecisionsPauseAgain(t *testing.T) {
	b, s := &bank{}, &MemStore{}
	r := newRunner(scripted(big("c1"), small("c2"), big("c3")), b, s)
	res, _ := r.Start(ctx, "run1", "三笔转账")
	if len(res.Pending) != 2 || b.charges != 1 {
		t.Fatalf("小额的 c2 直接执行，c1、c3 等审批：%+v charges=%d", res, b.charges)
	}
	res, err := r.Resume(ctx, "run1", map[string]Decision{"c1": {Kind: Approve}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != Interrupted || len(res.Pending) != 1 || res.Pending[0].Call.ID != "c3" {
		t.Fatalf("c3 没决定，应该再次暂停：%+v", res)
	}
	res, _ = r.Resume(ctx, "run1", map[string]Decision{"c3": {Kind: Reject}})
	if res.Status != Done || b.charges != 2 {
		t.Fatalf("最终应完成且只扣两笔：%+v charges=%d", res, b.charges)
	}
	cp, _ := s.Load(ctx, "run1")
	checkPairing(t, cp.Messages)
}

func TestResumeValidation(t *testing.T) {
	b, s := &bank{}, &MemStore{}
	r := newRunner(scripted(big("c1")), b, s)
	r.Start(ctx, "run1", "给 alice 转 300")
	if _, err := r.Resume(ctx, "run1", map[string]Decision{"cX": {Kind: Approve}}); !errors.Is(err, ErrUnknownCall) {
		t.Fatalf("不在待审批列表里的 id 要拒绝：%v", err)
	}
	if _, err := r.Resume(ctx, "run1", map[string]Decision{"c1": {Kind: "yolo"}}); !errors.Is(err, ErrBadDecision) {
		t.Fatalf("未知决定类型要拒绝：%v", err)
	}
	r.Resume(ctx, "run1", map[string]Decision{"c1": {Kind: Approve}})
	if _, err := r.Resume(ctx, "run1", map[string]Decision{"c1": {Kind: Approve}}); !errors.Is(err, ErrNotInterrupted) {
		t.Fatalf("已完成的 run 不能再 Resume：%v", err)
	}
	if b.charges != 1 {
		t.Fatalf("重复提交不能重复扣款：%d", b.charges)
	}
}

func TestConcurrentResumeOnlyOneWins(t *testing.T) {
	b, s := &bank{}, &MemStore{}
	r := newRunner(scripted(big("c1")), b, s)
	r.Start(ctx, "run1", "给 alice 转 300")
	var ok, lost atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 每个请求一个独立 Runner（共享存储和账本），像 8 个 worker 同时收到“同意”
			ri := newRunner(scripted(big("c1")), b, s)
			ri.Ledger = r.Ledger
			_, err := ri.Resume(ctx, "run1", map[string]Decision{"c1": {Kind: Approve}})
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrConflict), errors.Is(err, ErrNotInterrupted):
				lost.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || b.calls != 1 {
		t.Fatalf("只能有一个 Resume 生效：ok=%d calls=%d", ok.Load(), b.calls)
	}
}

var errCrash = errors.New("进程崩溃")

func TestCrashAfterSideEffectDedupedByKey(t *testing.T) {
	b, s := &bank{}, &MemStore{}
	r := newRunner(scripted(small("c1")), b, s)
	crashed := false
	r.AfterTool = func(ToolCall) error {
		if !crashed {
			crashed = true
			return errCrash // 钱已经转出去了，但结果还没记账、没进检查点
		}
		return nil
	}
	if _, err := r.Start(ctx, "run1", "给 bob 转 20"); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	res, err := r.Continue(ctx, "run1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != Done {
		t.Fatalf("应该从检查点恢复并完成：%+v", res)
	}
	if b.calls != 2 || b.charges != 1 {
		t.Fatalf("重放会再发一次请求，但幂等键保证只扣一次：calls=%d charges=%d", b.calls, b.charges)
	}
}

// flaky 在第 n 次 Save 时失败一次，模拟“工具执行并记账了，但检查点没写进去”。
type flaky struct {
	Store
	n, i int
}

func (f *flaky) Save(c context.Context, cp Checkpoint, expect int) error {
	f.i++
	if f.i == f.n {
		return errCrash
	}
	return f.Store.Save(c, cp, expect)
}

func TestCrashAfterLedgerSkipsTool(t *testing.T) {
	b := &bank{}
	s := &flaky{Store: &MemStore{}, n: 3} // 1:user 2:assistant 3:工具结果
	r := newRunner(scripted(small("c1")), b, s)
	if _, err := r.Start(ctx, "run1", "给 bob 转 20"); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	res, err := r.Continue(ctx, "run1")
	if err != nil || res.Status != Done {
		t.Fatalf("%+v %v", res, err)
	}
	if b.calls != 1 {
		t.Fatalf("账本里已有结果，恢复时不该再调用工具：calls=%d", b.calls)
	}
}

func TestFileStoreSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	b := &bank{}
	led := &Ledger{}
	r := newRunner(scripted(big("c1")), b, &FileStore{Dir: dir})
	r.Ledger = led
	if res, _ := r.Start(ctx, "run1", "给 alice 转 300"); res.Status != Interrupted {
		t.Fatalf("%+v", res)
	}
	// 新进程：新的 FileStore、新的 Runner，只认磁盘上的文件
	r2 := newRunner(scripted(big("c1")), b, &FileStore{Dir: dir})
	r2.Ledger = led
	res, err := r2.Resume(ctx, "run1", map[string]Decision{"c1": {Kind: Approve}})
	if err != nil || res.Status != Done || b.charges != 1 {
		t.Fatalf("%+v %v charges=%d", res, err, b.charges)
	}
	if err := (&FileStore{Dir: dir}).Save(ctx, Checkpoint{RunID: "../evil"}, 0); err == nil {
		t.Fatal("runID 里的路径穿越要拒绝")
	}
}

func TestVersionMismatchRefused(t *testing.T) {
	b, s := &bank{}, &MemStore{}
	r := newRunner(scripted(big("c1")), b, s)
	r.Start(ctx, "run1", "给 alice 转 300")
	r2 := newRunner(scripted(big("c1")), b, s)
	r2.Version = "v2" // 工具或提示词改过了
	if _, err := r2.Resume(ctx, "run1", map[string]Decision{"c1": {Kind: Approve}}); !errors.Is(err, ErrVersion) {
		t.Fatalf("版本不一致要拒绝恢复：%v", err)
	}
	if b.calls != 0 {
		t.Fatal("版本不一致时什么都不能执行")
	}
}

func TestMalformedArgsFailClosed(t *testing.T) {
	b, s := &bank{}, &MemStore{}
	r := newRunner(scripted(ToolCall{ID: "c1", Name: "transfer", Args: `{"amount": 30`}), b, s)
	res, _ := r.Start(ctx, "run1", "转账")
	if res.Status != Interrupted || !strings.Contains(res.Pending[0].Reason, "无法解析") {
		t.Fatalf("参数解析失败要转人工，不能当成不需要审批：%+v", res)
	}
}

func TestApprovalExpiresAsReject(t *testing.T) {
	b, s := &bank{}, &MemStore{}
	now := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	r := newRunner(scripted(big("c1")), b, s)
	r.ApprovalTTL = time.Hour
	r.Now = func() time.Time { return now }
	res, _ := r.Start(ctx, "run1", "给 alice 转 300")
	if !res.Pending[0].Deadline.Equal(now.Add(time.Hour)) {
		t.Fatalf("截止时间不对：%v", res.Pending[0].Deadline)
	}
	now = now.Add(2 * time.Hour)
	res, err := r.Resume(ctx, "run1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.calls != 0 || !strings.Contains(res.Final, "审批超时") {
		t.Fatalf("过期应按拒绝处理：%+v calls=%d", res, b.calls)
	}
}

func TestCheckpointIsPlainJSON(t *testing.T) {
	b, s := &bank{}, &MemStore{}
	r := newRunner(scripted(big("c1")), b, s)
	r.Start(ctx, "run1", "给 alice 转 300")
	cp, _ := s.Load(ctx, "run1")
	raw, _ := json.Marshal(cp)
	for _, k := range []string{`"status":"interrupted"`, `"version":"v1"`, `"pending"`} {
		if !strings.Contains(string(raw), k) {
			t.Fatalf("检查点里缺 %s：%s", k, raw)
		}
	}
}
