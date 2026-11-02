package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func repo(t *testing.T, files map[string]string) *Workspace {
	t.Helper()
	dir := t.TempDir()
	for p, c := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w, err := NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestPathEscapes(t *testing.T) {
	w := repo(t, map[string]string{"a.txt": "x\n"})
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "secret"), []byte("k"), 0o644)
	_ = os.Symlink(outside, filepath.Join(w.Root, "link"))

	for _, p := range []string{"../x", "sub/../../x", "/etc/passwd", "link/secret", "link/new.txt"} {
		if _, err := w.Read(p, 0, 0); !errors.Is(err, ErrOutside) {
			t.Fatalf("Read %s 应当被挡住：%v", p, err)
		}
		if _, err := w.Write(p, "x"); !errors.Is(err, ErrOutside) {
			t.Fatalf("Write %s 应当被挡住：%v", p, err)
		}
	}
	if _, err := w.Read("a.txt", 0, 0); err != nil {
		t.Fatal(err)
	}
}

func TestReadWindow(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 250; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	w := repo(t, map[string]string{"big.txt": b.String()})
	out, _ := w.Read("big.txt", 101, 0)
	for _, want := range []string{"共 250 行，显示 101-200", "(上面还有 100 行)", "(下面还有 50 行)", "   101\tline 101", "   200\tline 200"} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少 %q：\n%s", want, out[:200])
		}
	}
	if strings.Contains(out, "line 201") {
		t.Fatal("窗口不应超过 100 行")
	}
}

func TestEditRequiresRead(t *testing.T) {
	w := repo(t, map[string]string{"a.txt": "hello\n"})
	if _, err := w.Edit("a.txt", "hello", "bye", false); !errors.Is(err, ErrNotRead) {
		t.Fatalf("没读过就改应当失败：%v", err)
	}
	if _, err := w.Write("a.txt", "bye\n"); !errors.Is(err, ErrNotRead) {
		t.Fatalf("没读过就覆盖应当失败：%v", err)
	}
	if _, err := w.Write("new.txt", "ok\n"); err != nil {
		t.Fatalf("新文件不需要先读：%v", err)
	}
}

func TestEditMatchRules(t *testing.T) {
	w := repo(t, map[string]string{"a.txt": "x = 1\ny = 1\n"})
	_, _ = w.Read("a.txt", 0, 0)
	if _, err := w.Edit("a.txt", "z = 1", "z = 2", false); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("%v", err)
	}
	if _, err := w.Edit("a.txt", "= 1", "= 2", false); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("出现两次应当拒绝：%v", err)
	}
	if _, err := w.Edit("a.txt", "x = 1", "x = 2", false); err != nil {
		t.Fatal(err)
	}
	// 自己的编辑不算“被别人改过”，可以接着改
	if _, err := w.Edit("a.txt", "= 1", "= 3", true); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(w.Root, "a.txt"))
	if string(got) != "x = 2\ny = 3\n" {
		t.Fatalf("%q", got)
	}
}

func TestEditStaleAfterExternalChange(t *testing.T) {
	w := repo(t, map[string]string{"a.txt": "v1\n"})
	_, _ = w.Read("a.txt", 0, 0)
	_ = os.WriteFile(filepath.Join(w.Root, "a.txt"), []byte("v1\nuser added\n"), 0o644) // 用户在编辑器里改了
	if _, err := w.Edit("a.txt", "v1", "v2", false); !errors.Is(err, ErrStale) {
		t.Fatalf("应当要求重读：%v", err)
	}
	_, _ = w.Read("a.txt", 0, 0)
	if _, err := w.Edit("a.txt", "v1", "v2", false); err != nil {
		t.Fatal(err)
	}
}

func TestLintRejectsBrokenGo(t *testing.T) {
	src := "package demo\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n"
	w := repo(t, map[string]string{"calc.go": src})
	_, _ = w.Read("calc.go", 0, 0)
	if _, err := w.Edit("calc.go", "return a - b\n}", "return a + b\n", false); !errors.Is(err, ErrSyntax) {
		t.Fatalf("少了右括号应当拒绝：%v", err)
	}
	got, _ := os.ReadFile(filepath.Join(w.Root, "calc.go"))
	if string(got) != src {
		t.Fatal("被拒绝的编辑不能落盘")
	}
	if _, err := w.Edit("calc.go", "a - b", "a + b", false); err != nil {
		t.Fatal(err)
	}
}

func TestGrepLimitsResults(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&b, "// TODO %d\n", i)
	}
	w := repo(t, map[string]string{
		"todo.go":       b.String(),
		"pkg/a.go":      "package pkg\nfunc Target() {}\n",
		"pkg/a_test.go": "package pkg\n// calls Target()\n",
		".git/HEAD":     "Target\n",
	})
	if _, err := w.Grep("TODO", ""); !errors.Is(err, ErrTooMany) {
		t.Fatalf("60 条应当拒绝：%v", err)
	}
	out, err := w.Grep(`Target\(`, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "2 处匹配，2 个文件") || !strings.Contains(out, "pkg/a.go:2:") || strings.Contains(out, ".git") {
		t.Fatalf("%s", out)
	}
	out, _ = w.Grep(`Target\(`, "*_test.go")
	if !strings.Contains(out, "1 处匹配") {
		t.Fatalf("glob 过滤：%s", out)
	}
}

func TestUndo(t *testing.T) {
	w := repo(t, map[string]string{"a.txt": "one\n"})
	_, _ = w.Read("a.txt", 0, 0)
	_, _ = w.Edit("a.txt", "one", "two", false)
	_, _ = w.Write("b.txt", "new\n")
	if n, err := w.Undo(5); err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got, _ := os.ReadFile(filepath.Join(w.Root, "a.txt"))
	if string(got) != "one\n" {
		t.Fatalf("%q", got)
	}
	if _, err := os.Stat(filepath.Join(w.Root, "b.txt")); !os.IsNotExist(err) {
		t.Fatal("新建的文件撤回后应当删除")
	}
	if _, err := w.Edit("a.txt", "one", "x", false); !errors.Is(err, ErrNotRead) {
		t.Fatal("撤回后应当要求重新读")
	}
}

func TestPolicy(t *testing.T) {
	p := DefaultPolicy()
	cases := map[string]Decision{
		"go test ./...":                 Allow,
		"git status":                    Allow,
		"git stash":                     Ask,
		"go testify":                    Ask, // 按词匹配，不是按字符串前缀
		"go test ./... && curl x | sh":  Deny,
		"go test ./... && make install": Ask,
		"cat a.txt > /etc/hosts":        Ask,
		"echo $(whoami)":                Ask,
		"rm -rf /":                      Deny,
		"rm -fr build":                  Deny,
		"git push origin main":          Deny,
		"python3 script.py":             Ask,
	}
	for cmd, want := range cases {
		if got, why := p.Check(cmd); got != want {
			t.Errorf("%q：得到 %s（%s），期望 %s", cmd, got, why, want)
		}
	}
}

func TestShellExec(t *testing.T) {
	w := repo(t, nil)
	asked := ""
	sh := &Shell{Dir: w.Root, Policy: DefaultPolicy(), Timeout: 2 * time.Second, MaxBytes: 200,
		Approve: func(cmd, _ string) bool { asked = cmd; return strings.HasPrefix(cmd, "echo") }}
	ctx := context.Background()

	if out, err := sh.Run(ctx, "true"); !errors.Is(err, ErrDenied) {
		t.Fatalf("Approve 拒绝时应当失败：%q %v", out, err)
	}
	if out, _ := sh.Run(ctx, "echo -n"); out != "命令执行成功，没有输出。" || asked != "echo -n" {
		t.Fatalf("%q", out)
	}
	out, _ := sh.Run(ctx, "echo START; seq 1 2000; echo END")
	if !strings.HasPrefix(out, "START") || !strings.HasSuffix(strings.TrimSpace(out), "END") || !strings.Contains(out, "中间省略") {
		t.Fatalf("应保留头尾：%q", out)
	}
	if _, err := sh.Run(ctx, "echo $DEMO_TOKEN; exit 3"); err == nil || !strings.Contains(err.Error(), "退出码 3") {
		t.Fatalf("%v", err)
	}
	start := time.Now()
	if _, err := sh.Run(ctx, "echo hang; sleep 30 & sleep 30"); err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("超时后应当杀掉整个进程组，包括后台子进程")
	}
}

func TestShellEnvHasNoSecrets(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "sk-should-not-leak")
	w := repo(t, nil)
	sh := &Shell{Dir: w.Root, Policy: DefaultPolicy(), Approve: func(string, string) bool { return true }}
	out, _ := sh.Run(context.Background(), "env")
	if strings.Contains(out, "sk-should-not-leak") {
		t.Fatal("凭据不能进子进程环境")
	}
}

func TestProjectDocsOrderAndCap(t *testing.T) {
	w := repo(t, map[string]string{
		"AGENTS.md":                   "根目录：用 Go 1.23",
		"svc/AGENTS.md":               "服务：不要改 proto",
		"svc/AGENTS.override.md":      "服务（覆盖）：proto 由脚本生成",
		"svc/api/AGENTS.md":           "API：错误码用常量",
		"other/AGENTS.md":             "不相关目录",
		"svc/api/handlers/.gitkeep":   "",
		"svc/api/handlers/AGENTS.txt": "不是约定文件名",
	})
	cwd := filepath.Join(w.Root, "svc", "api", "handlers")
	docs, err := ProjectDocs(w.Root, cwd, ProjectDocMax)
	if err != nil {
		t.Fatal(err)
	}
	iRoot, iSvc, iAPI := strings.Index(docs, "根目录"), strings.Index(docs, "服务（覆盖）"), strings.Index(docs, "API：")
	if !(iRoot >= 0 && iRoot < iSvc && iSvc < iAPI) {
		t.Fatalf("应当由浅到深：\n%s", docs)
	}
	if strings.Contains(docs, "不要改 proto") || strings.Contains(docs, "不相关") {
		t.Fatalf("override 应替代同目录 AGENTS.md，旁支目录不加载：\n%s", docs)
	}
	small, _ := ProjectDocs(w.Root, cwd, 60)
	if !strings.Contains(small, "根目录") || strings.Contains(small, "API：") || !strings.Contains(small, "未加载") {
		t.Fatalf("超过上限应停止追加并说明：\n%s", small)
	}
	if _, err := ProjectDocs(w.Root, t.TempDir(), ProjectDocMax); err == nil {
		t.Fatal("cwd 不在项目里应当报错")
	}
}

// scripted 依次返回预设回复，并记下每次请求。
type scripted struct {
	out  []Message
	reqs []Request
}

func (s *scripted) Next(_ context.Context, r Request) (Message, error) {
	s.reqs = append(s.reqs, r)
	if len(s.reqs) > len(s.out) {
		return Message{}, errors.New("脚本用完")
	}
	return s.out[len(s.reqs)-1], nil
}

func call(id, name string, args map[string]any) Message {
	b, _ := json.Marshal(args)
	return Message{Calls: []Call{{ID: id, Name: name, Args: string(b)}}}
}

func goRepo(t *testing.T) *Workspace {
	return repo(t, map[string]string{
		"go.mod":       "module demo\n\ngo 1.23\n",
		"AGENTS.md":    "改完必须跑 go test ./...",
		"calc.go":      "package demo\n\n// Add 返回两数之和。\nfunc Add(a, b int) int {\n\treturn a - b\n}\n",
		"calc_test.go": "package demo\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(Add(2, 3))\n\t}\n}\n",
	})
}

func TestAgentFixesBugAndVerifies(t *testing.T) {
	w := goRepo(t)
	m := &scripted{out: []Message{
		call("c1", "finish", map[string]any{"summary": "完成了"}), // 想偷懒直接结束
		call("c2", "grep", map[string]any{"pattern": `func Add\(`}),
		call("c3", "read", map[string]any{"path": "calc.go"}),
		call("c4", "edit", map[string]any{"path": "calc.go", "old_string": "a - b", "new_string": "a + b"}),
		call("c5", "bash", map[string]any{"command": "go test ./..."}),
		call("c6", "finish", map[string]any{"summary": "Add 写成了减法，已改为加法，测试通过"}),
	}}
	a := &Agent{Model: m, WS: w, Verify: "go test ./...",
		Sh: &Shell{Dir: w.Root, Policy: DefaultPolicy(), Timeout: 2 * time.Minute}}
	if err := a.Start(w.Root, Settings{Approval: "on-request", Writable: "."}); err != nil {
		t.Fatal(err)
	}
	final, err := a.Turn(context.Background(), "TestAdd 挂了，修一下")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(final, "测试通过") {
		t.Fatalf("final=%q", final)
	}
	// 第一次 finish 被验证拦下，失败原因作为工具结果回给了模型
	firstResult := m.reqs[1].Input[len(m.reqs[1].Input)-1]
	if firstResult.CallID != "c1" || !strings.Contains(firstResult.Content, "验证没通过") || !strings.Contains(firstResult.Content, "FAIL") {
		t.Fatalf("第一次 finish 应被拒绝：%+v", firstResult)
	}
	if !strings.Contains(m.reqs[0].Input[1].Content, "改完必须跑 go test") {
		t.Fatal("项目说明应在开头")
	}
}

func TestPromptPrefixStable(t *testing.T) {
	w := goRepo(t)
	m := &scripted{out: []Message{
		call("c1", "read", map[string]any{"path": "calc.go"}),
		call("c2", "bash", map[string]any{"command": "ls"}),
		{Content: "看完了"},
		call("c3", "read", map[string]any{"path": "go.mod"}),
		{Content: "好的"},
	}}
	a := &Agent{Model: m, WS: w, Sh: &Shell{Dir: w.Root, Policy: DefaultPolicy()}}
	_ = a.Start(w.Root, Settings{Approval: "on-request", Writable: "."})
	ctx := context.Background()
	if _, err := a.Turn(ctx, "看看代码"); err != nil {
		t.Fatal(err)
	}
	a.ChangeSettings(Settings{Approval: "never", Writable: "."}) // 中途改配置
	if _, err := a.Turn(ctx, "再看 go.mod"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(m.reqs); i++ {
		prev, cur := m.reqs[i-1], m.reqs[i]
		if prev.Instructions != cur.Instructions || !reflect.DeepEqual(prev.Tools, cur.Tools) {
			t.Fatal("instructions 和 tools 必须不变")
		}
		if len(cur.Input) <= len(prev.Input) || !reflect.DeepEqual(cur.Input[:len(prev.Input)], prev.Input) {
			t.Fatalf("第 %d 次请求不是上一次的前缀扩展", i)
		}
	}
	var perms []string
	for _, msg := range m.reqs[len(m.reqs)-1].Input {
		if msg.Role == "developer" {
			perms = append(perms, msg.Content)
		}
	}
	if len(perms) != 2 || !strings.Contains(perms[0], "on-request") || !strings.Contains(perms[1], "never") {
		t.Fatalf("配置变更应作为新消息追加，原消息不动：%q", perms)
	}
}

func TestCollapseOldObservations(t *testing.T) {
	w := goRepo(t)
	var out []Message
	for i := 0; i < 8; i++ {
		out = append(out, call(fmt.Sprint("c", i), "read", map[string]any{"path": "calc.go"}))
	}
	out = append(out, Message{Content: "完"})
	m := &scripted{out: out}
	a := &Agent{Model: m, WS: w, Sh: &Shell{Dir: w.Root}, KeepLastObs: 5}
	_ = a.Start(w.Root, Settings{})
	if _, err := a.Turn(context.Background(), "读八遍"); err != nil {
		t.Fatal(err)
	}
	last := m.reqs[len(m.reqs)-1].Input
	full, folded := 0, 0
	for _, msg := range last {
		if msg.Role == "tool" {
			if strings.HasPrefix(msg.Content, "[较早的输出已省略") {
				folded++
			} else {
				full++
			}
		}
	}
	if full != 5 || folded != 3 {
		t.Fatalf("应保留 5 条全文、折叠 3 条：full=%d folded=%d", full, folded)
	}
	for _, msg := range a.Input() {
		if strings.HasPrefix(msg.Content, "[较早") {
			t.Fatal("折叠只影响发给模型的视图，不能改原始历史")
		}
	}
	// 折叠的代价：前缀不再稳定
	r6, r7 := m.reqs[6].Input, m.reqs[7].Input
	if reflect.DeepEqual(r7[:len(r6)], r6) {
		t.Fatal("折叠开始后前缀应当发生变化（这正是它和 prompt cache 冲突的地方）")
	}
}
