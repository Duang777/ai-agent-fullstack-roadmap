package coding

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Message 是发给模型的一条输入。Role 用 Responses API 的四种：system、developer、user、assistant，
// 外加 tool（工具结果，用 CallID 和调用配对）。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	Calls   []Call `json:"calls,omitempty"`
	CallID  string `json:"call_id,omitempty"`
}

type Call struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"` // JSON
}

const ProjectDocMax = 32 * 1024 // Codex 默认 32 KiB

// ProjectDocs 按 Codex 的规则收集项目说明：
// 从项目根目录一路走到 cwd，每一层取 AGENTS.override.md，没有就取 AGENTS.md。
// 越具体（越深）的越靠后，模型看到的最后一份就是最贴近当前目录的规则。
// 总量超过 max 就停止追加，并在末尾说明截断了。
func ProjectDocs(root, cwd string, max int) (string, error) {
	rel, err := filepath.Rel(root, cwd)
	if err != nil || !within(root, cwd) {
		return "", fmt.Errorf("cwd %s 不在项目 %s 里", cwd, root)
	}
	dirs := []string{root}
	if rel != "." {
		cur := root
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			cur = filepath.Join(cur, part)
			dirs = append(dirs, cur)
		}
	}
	var b strings.Builder
	for _, d := range dirs {
		var name string
		var data []byte
		for _, n := range []string{"AGENTS.override.md", "AGENTS.md"} {
			if x, err := os.ReadFile(filepath.Join(d, n)); err == nil {
				name, data = n, x
				break
			}
		}
		if data == nil {
			continue
		}
		r, _ := filepath.Rel(root, filepath.Join(d, name))
		block := fmt.Sprintf("# %s\n%s\n", filepath.ToSlash(r), strings.TrimSpace(string(data)))
		if b.Len()+len(block) > max {
			fmt.Fprintf(&b, "[项目说明超过 %d 字节，%s 及更深的说明未加载]\n", max, filepath.ToSlash(r))
			break
		}
		b.WriteString(block)
	}
	return b.String(), nil
}

// Settings 是会影响模型行为、而且对话中途可能变化的配置。
type Settings struct {
	Approval string // "on-request"、"never" ...
	Writable string // 可写目录
}

func permissionsText(s Settings) string {
	return fmt.Sprintf("<permissions>\n审批策略：%s\n可写目录：%s\n只有 bash 工具受这些规则约束。\n</permissions>", s.Approval, s.Writable)
}

func environmentText(cwd, shell string) string {
	return fmt.Sprintf("<environment_context>\n<cwd>%s</cwd>\n<shell>%s</shell>\n</environment_context>", cwd, shell)
}

// InitialInput 按 Codex 的顺序拼开头几条：权限说明（developer）→ 项目说明（user）→ 环境（user）。
// 这几条在整个会话里不再修改，保证每次请求的前缀都一样，prompt cache 才能命中。
func InitialInput(root, cwd string, s Settings) ([]Message, error) {
	docs, err := ProjectDocs(root, cwd, ProjectDocMax)
	if err != nil {
		return nil, err
	}
	in := []Message{{Role: "developer", Content: permissionsText(s)}}
	if docs != "" {
		in = append(in, Message{Role: "user", Content: "<project_instructions>\n" + docs + "</project_instructions>"})
	}
	return append(in, Message{Role: "user", Content: environmentText(cwd, "sh")}), nil
}
