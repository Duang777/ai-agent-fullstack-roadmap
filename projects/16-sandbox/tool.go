package sandbox

import (
	"encoding/json"
	"fmt"
)

// ToolResult 把执行结果变成交给模型的工具结果。
// 关键区分：代码自己报错（退出码非 0）是正常结果，模型要看到错误去改代码；
// 超时、被资源限制杀死是执行环境层面的失败，标成 is_error，并告诉模型怎么调整。
func ToolResult(r Result) (content string, isError bool) {
	type payload struct {
		ExitCode  int      `json:"exit_code"`
		Stdout    string   `json:"stdout"`
		Stderr    string   `json:"stderr"`
		Truncated bool     `json:"truncated,omitempty"`
		Files     []string `json:"files,omitempty"`
		Note      string   `json:"note,omitempty"`
	}
	p := payload{ExitCode: r.ExitCode, Stdout: r.Stdout, Stderr: r.Stderr, Truncated: r.Truncated}
	for _, f := range r.Files {
		p.Files = append(p.Files, f.Name)
	}
	switch {
	case r.TimedOut:
		p.Note, isError = fmt.Sprintf("超时（%s）被终止：缩小数据量，或把长任务拆成几步", r.Duration.Round(100e6)), true
	case r.Signal != "":
		p.Note, isError = "被资源限制终止："+r.Signal, true
	case r.Truncated:
		p.Note = "输出过长已截断：只打印需要的部分，或写入 $OUTPUT_DIR"
	}
	b, _ := json.Marshal(p)
	return string(b), isError
}
