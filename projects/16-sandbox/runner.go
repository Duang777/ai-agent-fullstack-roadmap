// Package sandbox 是 2.5 课的练习：让 Agent 运行它自己写的代码，但不让它伤到你。
// 这里用本机进程演示执行端必须做对的几件事：超时要杀掉整个进程组、环境变量不能带密钥、
// 输出要截断、资源要限制、产物只收指定目录里的普通文件。真正的隔离（容器、gVisor、微虚拟机）
// 见 docker.go 和课件；本机进程本身不是安全边界。
package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Limits struct {
	Timeout    time.Duration // 墙钟时间
	CPUSeconds int           // CPU 时间（ulimit -t），防死循环
	FileBlocks int           // 单个文件大小上限（ulimit -f，单位是块）
	MaxOutput  int           // stdout、stderr 各自最多保留的字节数
}

type Spec struct {
	Code   string
	Dir    string // 工作目录；为空则建临时目录，跑完删除
	Limits Limits
}

type Result struct {
	Stdout, Stderr string
	ExitCode       int
	Signal         string // 被信号杀死时的信号名
	TimedOut       bool
	Truncated      bool
	Files          []File // $OUTPUT_DIR 里的产物
	Duration       time.Duration
}

type File struct {
	Name string
	Size int64
}

// capped 只保留开头和结尾各一半：报错信息常在最后，命令回显常在开头（1.6 课：工具输出保留头尾）。
type capped struct {
	max        int
	head, tail []byte
	total      int
}

func (c *capped) Write(p []byte) (int, error) {
	n0 := len(p)
	c.total += n0
	half := c.max / 2
	if room := half - len(c.head); room > 0 {
		n := min(room, len(p))
		c.head = append(c.head, p[:n]...)
		p = p[n:]
	}
	c.tail = append(c.tail, p...)
	if len(c.tail) > c.max-half {
		c.tail = c.tail[len(c.tail)-(c.max-half):]
	}
	return n0, nil
}

func (c *capped) String() (string, bool) {
	kept := len(c.head) + len(c.tail)
	if kept >= c.total {
		return string(c.head) + string(c.tail), false
	}
	return fmt.Sprintf("%s\n…[省略 %d 字节]…\n%s", c.head, c.total-kept, c.tail), true
}

// cleanEnv：子进程只拿到这几个变量。父进程里的 API Key、云凭证一个都不能漏进去。
func cleanEnv(dir, out string) []string {
	return []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + dir, "LANG=C.UTF-8", "OUTPUT_DIR=" + out}
}

func Run(ctx context.Context, s Spec) (Result, error) {
	l := s.Limits
	if l.Timeout == 0 {
		l.Timeout = 10 * time.Second
	}
	if l.MaxOutput == 0 {
		l.MaxOutput = 16 << 10
	}
	dir := s.Dir
	if dir == "" {
		d, err := os.MkdirTemp("", "sbx-")
		if err != nil {
			return Result{}, err
		}
		defer os.RemoveAll(d)
		dir = d
	}
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		return Result{}, err
	}
	var prefix strings.Builder
	if l.CPUSeconds > 0 {
		fmt.Fprintf(&prefix, "ulimit -t %d; ", l.CPUSeconds)
	}
	if l.FileBlocks > 0 {
		fmt.Fprintf(&prefix, "ulimit -f %d; ", l.FileBlocks)
	}
	cmd := exec.Command("/bin/sh", "-c", prefix.String()+s.Code)
	cmd.Dir, cmd.Env = dir, cleanEnv(dir, out)
	cmd.Stdin = bytes.NewReader(nil)
	// 自己当进程组组长：超时时连同它派生的所有子进程一起杀
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	so, se := &capped{max: l.MaxOutput}, &capped{max: l.MaxOutput}
	cmd.Stdout, cmd.Stderr = so, se

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var r Result
	ctx, cancel := context.WithTimeout(ctx, l.Timeout)
	defer cancel()
	var werr error
	select {
	case werr = <-done:
	case <-ctx.Done():
		r.TimedOut = true
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // 负号 = 整个进程组
		werr = <-done
	}
	r.Duration = time.Since(start)
	if ee, ok := werr.(*exec.ExitError); ok {
		ws := ee.Sys().(syscall.WaitStatus)
		r.ExitCode = ee.ExitCode()
		if ws.Signaled() {
			r.Signal = ws.Signal().String()
		}
	} else if werr != nil {
		return r, werr
	}
	// sh 把子进程的信号死亡转成 128+n 的退出码
	if r.Signal == "" && r.ExitCode > 128 {
		r.Signal = syscall.Signal(r.ExitCode - 128).String()
	}
	var t1, t2 bool
	r.Stdout, t1 = so.String()
	r.Stderr, t2 = se.String()
	r.Truncated = t1 || t2
	files, err := collect(out)
	r.Files = files
	return r, err
}

// collect 只收 $OUTPUT_DIR 顶层的普通文件。符号链接一律不跟：
// 代码可以 ln -s /etc/passwd out/x，让你把宿主机上的文件当产物“下载”出去。
func collect(out string) ([]File, error) {
	ents, err := os.ReadDir(out)
	if err != nil {
		return nil, err
	}
	var fs []File
	for _, e := range ents {
		info, err := os.Lstat(filepath.Join(out, e.Name()))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		fs = append(fs, File{Name: e.Name(), Size: info.Size()})
	}
	return fs, nil
}
