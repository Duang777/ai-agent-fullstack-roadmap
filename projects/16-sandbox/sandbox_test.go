package sandbox

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func run(t *testing.T, code string, l Limits) Result {
	t.Helper()
	r, err := Run(context.Background(), Spec{Code: code, Limits: l})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestExitCodeIsAResult(t *testing.T) {
	r := run(t, `echo hi; echo oops >&2; exit 3`, Limits{})
	if r.Stdout != "hi\n" || r.Stderr != "oops\n" || r.ExitCode != 3 {
		t.Fatalf("%+v", r)
	}
	content, isErr := ToolResult(r)
	if isErr || !strings.Contains(content, `"exit_code":3`) {
		t.Fatal("代码自己报错是正常结果：模型要看到错误去改代码", content)
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	r, err := Run(context.Background(), Spec{Dir: dir, Code: `sleep 30 & echo $! > child.pid; wait`,
		Limits: Limits{Timeout: 300 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	if !r.TimedOut || time.Since(start) > 3*time.Second {
		t.Fatalf("超时必须按时返回：%+v", r)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "child.pid"))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	time.Sleep(100 * time.Millisecond)
	if pid == 0 || syscall.Kill(pid, 0) == nil {
		t.Fatal("后台子进程还活着：只杀 sh 不够，要杀整个进程组", pid)
	}
	if _, isErr := ToolResult(r); !isErr {
		t.Fatal("超时是执行环境层面的失败")
	}
}

func TestEnvHasNoSecrets(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "tok-live-123")
	r := run(t, `echo "${DEMO_TOKEN:-none}"; env | wc -l`, Limits{})
	lines := strings.Fields(r.Stdout)
	if lines[0] != "none" {
		t.Fatal("父进程的密钥漏进了沙箱", r.Stdout)
	}
	if n, _ := strconv.Atoi(lines[1]); n > 8 {
		t.Fatal("环境变量应当是白名单，而不是继承后再删", n)
	}
}

func TestOutputCapKeepsHeadAndTail(t *testing.T) {
	r := run(t, `echo START; i=0; while [ $i -lt 5000 ]; do echo line$i; i=$((i+1)); done; echo END`, Limits{MaxOutput: 200})
	if !r.Truncated || len(r.Stdout) > 300 {
		t.Fatal(len(r.Stdout))
	}
	if !strings.HasPrefix(r.Stdout, "START") || !strings.HasSuffix(r.Stdout, "END\n") || !strings.Contains(r.Stdout, "省略") {
		t.Fatal("保留头尾，中间写明省略了多少", r.Stdout)
	}
	if c, _ := ToolResult(r); !strings.Contains(c, "截断") {
		t.Fatal(c)
	}
}

func TestCPULimit(t *testing.T) {
	r := run(t, `while :; do :; done`, Limits{CPUSeconds: 1, Timeout: 10 * time.Second})
	if r.TimedOut || r.Duration > 5*time.Second || r.Signal == "" {
		t.Fatalf("死循环应当被 CPU 时间限制先杀掉，而不是等墙钟超时：%+v", r)
	}
	if c, isErr := ToolResult(r); !isErr || !strings.Contains(c, "资源限制") {
		t.Fatal(c)
	}
}

func TestFileSizeLimit(t *testing.T) {
	// 不加 set -e：head 被 SIGXFSZ 杀掉，脚本照样往下跑，最后退出码是 0，失败被藏起来了
	r := run(t, `head -c 10000000 /dev/zero > big.bin; echo after`, Limits{FileBlocks: 100})
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "after") {
		t.Fatalf("%+v", r)
	}
	r = run(t, `set -e; head -c 10000000 /dev/zero > big.bin; echo after`, Limits{FileBlocks: 100})
	if r.ExitCode == 0 || strings.Contains(r.Stdout, "after") || !strings.Contains(r.Signal, "file size") {
		t.Fatalf("加 set -e 后第一处失败就停，退出码和信号都能看到：%+v", r)
	}
}

func TestArtifactsOnlyRegularFiles(t *testing.T) {
	r := run(t, `echo hi > "$OUTPUT_DIR/report.txt"; echo x > other.txt; ln -s /etc/passwd "$OUTPUT_DIR/passwd"; mkdir "$OUTPUT_DIR/sub"`, Limits{})
	if len(r.Files) != 1 || r.Files[0].Name != "report.txt" || r.Files[0].Size != 3 {
		t.Fatal("只收 $OUTPUT_DIR 顶层的普通文件；符号链接可能指向宿主机文件", r.Files)
	}
	c, _ := ToolResult(r)
	var p map[string]any
	json.Unmarshal([]byte(c), &p)
	if p["files"].([]any)[0] != "report.txt" {
		t.Fatal(c)
	}
}

func TestTempDirRemoved(t *testing.T) {
	r := run(t, `pwd`, Limits{})
	if _, err := os.Stat(strings.TrimSpace(r.Stdout)); !os.IsNotExist(err) {
		t.Fatal("一次性工作目录用完要删", r.Stdout)
	}
}

func TestNetPolicy(t *testing.T) {
	ip := netip.MustParseAddr
	open := NetPolicy{}
	if !open.Allowed("evil.example", ip("6.6.6.6")) {
		t.Fatal("什么都不配时默认放行：这就是为什么要显式拒绝")
	}
	p := NetPolicy{AllowOut: []string{"1.1.1.1", "8.8.8.0/24"}, DenyOut: []string{AllTraffic}}
	if !p.Allowed("", ip("8.8.8.9")) || !p.Allowed("", ip("1.1.1.1")) || p.Allowed("", ip("9.9.9.9")) {
		t.Fatal("AllowOut 优先于 DenyOut")
	}
	if (NetPolicy{AllowOut: []string{"pypi.org"}}).Validate() == nil {
		t.Fatal("只写域名白名单、不拒绝其他流量，白名单没有意义")
	}
	if (NetPolicy{DenyOut: []string{"evil.example"}}).Validate() == nil {
		t.Fatal("DenyOut 不支持域名")
	}
	d := NetPolicy{AllowOut: []string{"pypi.org"}, DenyOut: []string{AllTraffic}}
	if d.Validate() != nil || !d.Allowed("pypi.org", ip("151.101.0.223")) || d.Allowed("evil.example", ip("6.6.6.6")) {
		t.Fatal(d)
	}
	if !d.Allowed("", ip("8.8.8.8")) {
		t.Fatal("用了域名规则，DNS 服务器要自动放行")
	}
}

func TestDockerArgs(t *testing.T) {
	args := DockerArgs(ContainerSpec{Image: "python:3.13-slim", Runtime: "runsc", MemoryMB: 512, CPUs: 1, PidsLimit: 128, Code: "python3 main.py"})
	s := strings.Join(args, " ")
	for _, want := range []string{"--runtime=runsc", "--network=none", "--read-only", "--cap-drop=ALL",
		"--security-opt=no-new-privileges", "--user=65534:65534", "--memory=512m", "--memory-swap=512m", "--pids-limit=128", "--rm"} {
		if !strings.Contains(s, want) {
			t.Errorf("缺少 %s", want)
		}
	}
	if len(LintDockerArgs(args)) != 0 {
		t.Fatal(LintDockerArgs(args))
	}
	bad := LintDockerArgs([]string{"run", "--privileged", "-v", "/var/run/docker.sock:/var/run/docker.sock", "--network=host", "img"})
	if len(bad) != 3 {
		t.Fatal(bad)
	}
}

func TestLifecycle(t *testing.T) {
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	pause := &Manager{Now: clock, Timeout: 20 * time.Minute, OnTimeout: Paused}
	b := pause.Create()
	pause.Use(b.ID, func(b *Box) { b.Files["data.csv"] = "a,b" })
	now = now.Add(25 * time.Minute)
	pause.Tick()
	if b.State != Paused {
		t.Fatal(b.State)
	}
	var got string
	if err := pause.Use(b.ID, func(b *Box) { got = b.Files["data.csv"] }); err != nil || got != "a,b" || b.State != Running {
		t.Fatal("暂停的沙箱用的时候自动恢复，文件还在", err, got)
	}

	kill := &Manager{Now: clock, Timeout: 20 * time.Minute, OnTimeout: Killed}
	k := kill.Create()
	kill.Use(k.ID, func(b *Box) { b.Files["data.csv"] = "a,b" })
	now = now.Add(19 * time.Minute)
	kill.Use(k.ID, func(*Box) {}) // 每次使用都续期
	now = now.Add(19 * time.Minute)
	if err := kill.Use(k.ID, func(*Box) {}); err != nil {
		t.Fatal("续期之后还没到期", err)
	}
	now = now.Add(21 * time.Minute)
	if err := kill.Use(k.ID, func(*Box) {}); err != ErrGone {
		t.Fatal("闲置超时被销毁：调用方要新建并重新上传，而不是假装还在", err)
	}
}
