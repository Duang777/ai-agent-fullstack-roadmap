// Package coding 是一个最小的 coding agent harness：
// 文件工具（窗口化读取、精确替换编辑、搜索）、带权限的 shell、
// AGENTS.md 指令拼装、保持前缀稳定的 agent 循环、编辑撤销和“完成前必须验证”。
package coding

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	ErrOutside   = errors.New("路径在工作区之外")
	ErrNotRead   = errors.New("编辑前必须先 Read 这个文件")
	ErrStale     = errors.New("文件在你上次 Read 之后被修改过，请重新 Read")
	ErrNoMatch   = errors.New("old_string 在文件里找不到")
	ErrAmbiguous = errors.New("old_string 出现了不止一次")
	ErrSyntax    = errors.New("编辑后代码有语法错误，编辑已撤回")
	ErrTooMany   = errors.New("搜索结果太多")
)

const (
	ViewLines  = 100 // SWE-agent 消融：100 行窗口最好（30 行 14.3%，整个文件 12.7%，100 行 18.0%）
	MaxResults = 50  // SWE-agent：超过 50 条不给结果，让模型换更具体的搜索
)

// Workspace 是 agent 能碰到的全部文件系统。所有路径都相对 Root。
type Workspace struct {
	Root string
	read map[string][32]byte // 上次 Read 时整个文件的哈希
	undo []snapshot
	Lint bool // 对 .go 文件做语法检查，不通过就拒绝编辑
}

type snapshot struct {
	path    string
	existed bool
	data    []byte
}

func NewWorkspace(root string) (*Workspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// 把 Root 本身的符号链接也解析掉，后面比较前缀才可靠
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	return &Workspace{Root: abs, read: map[string][32]byte{}, Lint: true}, nil
}

// resolve 把模型给的路径变成工作区里的绝对路径。
// 三种逃逸都要挡：..、绝对路径、指向外面的符号链接。
func (w *Workspace) resolve(p string) (string, error) {
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("%w：%s（只接受相对路径）", ErrOutside, p)
	}
	full := filepath.Join(w.Root, p) // Join 会 Clean，../ 在这里被折叠
	if !within(w.Root, full) {
		return "", fmt.Errorf("%w：%s", ErrOutside, p)
	}
	// 文件可能还不存在（Write 新文件），就检查最近一个存在的上级目录
	probe := full
	for {
		real, err := filepath.EvalSymlinks(probe)
		if err == nil {
			if !within(w.Root, real) {
				return "", fmt.Errorf("%w：%s 经符号链接指向 %s", ErrOutside, p, real)
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		probe = filepath.Dir(probe)
	}
	return full, nil
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Read 返回带行号的窗口，头部写明路径、总行数、窗口外还有多少行。
// offset 从 1 开始；limit<=0 用 ViewLines。
func (w *Workspace) Read(p string, offset, limit int) (string, error) {
	full, err := w.resolve(p)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	w.read[full] = sha256.Sum256(data)
	lines := splitLines(data)
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = ViewLines
	}
	if len(lines) == 0 {
		return fmt.Sprintf("[%s：空文件]", p), nil
	}
	if offset > len(lines) {
		return fmt.Sprintf("[%s 只有 %d 行]", p, len(lines)), nil
	}
	end := min(offset+limit-1, len(lines))
	var b strings.Builder
	fmt.Fprintf(&b, "[文件 %s，共 %d 行，显示 %d-%d]\n", p, len(lines), offset, end)
	if offset > 1 {
		fmt.Fprintf(&b, "(上面还有 %d 行)\n", offset-1)
	}
	for i := offset; i <= end; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i, lines[i-1])
	}
	if end < len(lines) {
		fmt.Fprintf(&b, "(下面还有 %d 行)\n", len(lines)-end)
	}
	return b.String(), nil
}

func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	s := strings.TrimSuffix(string(data), "\n")
	return strings.Split(s, "\n")
}

// Edit 做精确字符串替换，和 Claude Code 的 Edit 工具同一套规则：
// 先读后改、精确匹配、唯一（或显式 replaceAll）、读后被改过就要求重读。
// 另外加 SWE-agent 的做法：.go 文件改完先过语法检查，不通过就撤回。
func (w *Workspace) Edit(p, old, new string, replaceAll bool) (string, error) {
	full, err := w.resolve(p)
	if err != nil {
		return "", err
	}
	seen, ok := w.read[full]
	if !ok {
		return "", fmt.Errorf("%w：%s", ErrNotRead, p)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	if sha256.Sum256(data) != seen {
		return "", fmt.Errorf("%w：%s", ErrStale, p)
	}
	if old == "" || old == new {
		return "", errors.New("old_string 不能为空，也不能和 new_string 相同")
	}
	n := strings.Count(string(data), old)
	switch {
	case n == 0:
		return "", fmt.Errorf("%w：%s", ErrNoMatch, p)
	case n > 1 && !replaceAll:
		return "", fmt.Errorf("%w：%s 中出现 %d 次。多带几行上下文让它唯一，或者设置 replace_all", ErrAmbiguous, p, n)
	}
	k := 1
	if replaceAll {
		k = -1
	}
	out := []byte(strings.Replace(string(data), old, new, k))
	if err := w.lint(p, out); err != nil {
		return "", err // 文件没动过
	}
	if err := w.write(full, out); err != nil {
		return "", err
	}
	return fmt.Sprintf("已修改 %s：替换 %d 处", p, map[bool]int{true: n, false: 1}[replaceAll]), nil
}

// Write 创建新文件或整体覆盖。覆盖已有文件同样要求先读过。
func (w *Workspace) Write(p, content string) (string, error) {
	full, err := w.resolve(p)
	if err != nil {
		return "", err
	}
	if old, err := os.ReadFile(full); err == nil {
		seen, ok := w.read[full]
		if !ok {
			return "", fmt.Errorf("%w：%s 已存在", ErrNotRead, p)
		}
		if sha256.Sum256(old) != seen {
			return "", fmt.Errorf("%w：%s", ErrStale, p)
		}
	}
	if err := w.lint(p, []byte(content)); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}
	if err := w.write(full, []byte(content)); err != nil {
		return "", err
	}
	return fmt.Sprintf("已写入 %s（%d 行）", p, len(splitLines([]byte(content)))), nil
}

// write 先拍快照再落盘，然后把“已读”状态更新成新内容：
// agent 自己写进去的东西不需要再读一遍。
func (w *Workspace) write(full string, data []byte) error {
	old, err := os.ReadFile(full)
	w.undo = append(w.undo, snapshot{path: full, existed: err == nil, data: old})
	if err := os.WriteFile(full, data, 0o644); err != nil {
		w.undo = w.undo[:len(w.undo)-1]
		return err
	}
	w.read[full] = sha256.Sum256(data)
	return nil
}

func (w *Workspace) lint(p string, src []byte) error {
	if !w.Lint || filepath.Ext(p) != ".go" {
		return nil
	}
	_, err := parser.ParseFile(token.NewFileSet(), p, src, parser.AllErrors)
	if err != nil {
		return fmt.Errorf("%w：%v", ErrSyntax, firstLine(err.Error()))
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Undo 撤回最近 n 次文件修改（Claude Code 按两次 Esc 的 rewind）。
// 只覆盖文件；shell 命令造成的副作用（数据库、网络、git push）撤不回。
func (w *Workspace) Undo(n int) (int, error) {
	done := 0
	for ; done < n && len(w.undo) > 0; done++ {
		s := w.undo[len(w.undo)-1]
		w.undo = w.undo[:len(w.undo)-1]
		var err error
		if s.existed {
			err = os.WriteFile(s.path, s.data, 0o644)
		} else {
			err = os.Remove(s.path)
		}
		if err != nil {
			return done, err
		}
		delete(w.read, s.path) // 撤回后内容变了，要求重新读
	}
	return done, nil
}

// Grep 在工作区里按正则搜索。结果按文件汇总；
// 超过 MaxResults 条就不给结果，直接让模型缩小范围（SWE-agent 的设计）。
func (w *Workspace) Grep(pattern, glob string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("正则无效：%v", err)
	}
	type hit struct {
		path string
		line int
		text string
	}
	var hits []hit
	err = filepath.WalkDir(w.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if glob != "" {
			if ok, _ := filepath.Match(glob, d.Name()); !ok {
				return nil
			}
		}
		data, err := os.ReadFile(path)
		if err != nil || bytes.IndexByte(data, 0) >= 0 { // 跳过二进制
			return nil
		}
		rel, _ := filepath.Rel(w.Root, path)
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for i := 1; sc.Scan(); i++ {
			if re.MatchString(sc.Text()) {
				hits = append(hits, hit{filepath.ToSlash(rel), i, strings.TrimSpace(sc.Text())})
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return fmt.Sprintf("没有匹配 %q 的内容", pattern), nil
	}
	if len(hits) > MaxResults {
		return "", fmt.Errorf("%w：%q 共 %d 处匹配，超过 %d。请换更具体的模式或加文件过滤", ErrTooMany, pattern, len(hits), MaxResults)
	}
	byFile := map[string][]hit{}
	var files []string
	for _, h := range hits {
		if _, ok := byFile[h.path]; !ok {
			files = append(files, h.path)
		}
		byFile[h.path] = append(byFile[h.path], h)
	}
	sort.Strings(files)
	var b strings.Builder
	fmt.Fprintf(&b, "%d 处匹配，%d 个文件\n", len(hits), len(files))
	for _, f := range files {
		for _, h := range byFile[f] {
			fmt.Fprintf(&b, "%s:%d: %s\n", f, h.line, h.text)
		}
	}
	return b.String(), nil
}
