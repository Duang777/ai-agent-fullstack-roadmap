package claudesdk

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"
	"sync"
)

// Mode 是权限模式。
type Mode string

const (
	ModeDefault     Mode = "default"
	ModeAcceptEdits Mode = "acceptEdits"
	ModePlan        Mode = "plan"
	ModeDontAsk     Mode = "dontAsk"
	ModeBypass      Mode = "bypassPermissions"
)

// ---------- 规则："Bash"、"Bash(npm *)"、"Edit(//secrets/**)"、"mcp__github__get_*" ----------

type rule struct {
	tool, spec string
	hasSpec    bool
}

func parseRule(s string) rule {
	if i := strings.IndexByte(s, '('); i > 0 && strings.HasSuffix(s, ")") {
		return rule{tool: s[:i], spec: s[i+1 : len(s)-1], hasSpec: true}
	}
	return rule{tool: s}
}

// glob：* 匹配任意字符序列（包括 /），** 同理。
func glob(pat, s string) bool {
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(pat), `\*`, `.*`) + "$"
	ok, _ := regexp.MatchString(re, s)
	return ok
}

// toolNameMatch：deny 规则接受任意工具名通配（"*"、"mcp__*"）；
// allow 规则只接受 mcp__<server>__ 字面前缀之后的通配，"*" 和 "mcp__*" 被忽略。
func toolNameMatch(pat, name string, allow bool) bool {
	if !strings.Contains(pat, "*") {
		return pat == name
	}
	if allow {
		if !strings.HasPrefix(pat, "mcp__") {
			return false
		}
		rest := pat[len("mcp__"):]
		i := strings.Index(rest, "__")
		if i <= 0 || strings.Contains(rest[:i], "*") {
			return false
		}
	}
	return glob(pat, name)
}

func resolveRulePath(p, cwd string) string {
	if strings.HasPrefix(p, "//") { // //path：绝对路径
		return p[1:]
	}
	return path.Join(cwd, p) // /path 或 path：工作目录内
}

func inside(cwd, p string) bool {
	p = path.Clean(p)
	return p == cwd || strings.HasPrefix(p, cwd+"/")
}

func (r rule) matches(t *Tool, in map[string]any, cwd string, allow bool) bool {
	if !r.hasSpec {
		return toolNameMatch(r.tool, t.Name, allow)
	}
	v, _ := in[t.SpecKey].(string)
	if t.EditsFiles || t.SpecKey == "file_path" {
		// 写文件的路径规则一律写成 Edit(path)：它管 Write、Edit 等所有写文件工具。
		// Write(path) 这种写法不匹配任何调用。
		want := t.Name
		if t.EditsFiles {
			want = "Edit"
		}
		if r.tool != want {
			return false
		}
		return glob(resolveRulePath(r.spec, cwd), path.Clean(v))
	}
	return toolNameMatch(r.tool, t.Name, allow) && glob(r.spec, v)
}

func anyRule(rules []string, t *Tool, in map[string]any, cwd string, allow bool) bool {
	for _, s := range rules {
		if parseRule(s).matches(t, in, cwd, allow) {
			return true
		}
	}
	return false
}

// removedByDeny：只有不带括号的 deny 规则会把工具定义从请求里拿掉。
func removedByDeny(name string, deny []string) bool {
	for _, s := range deny {
		r := parseRule(s)
		if !r.hasSpec && toolNameMatch(r.tool, name, false) {
			return true
		}
	}
	return false
}

// ---------- Hooks ----------

type HookInput struct {
	Event      string
	SessionID  string
	AgentType  string // 在子 agent 里触发时是子 agent 的类型
	ToolName   string
	ToolUseID  string
	ToolInput  map[string]any
	ToolOutput string
}

type HookOutput struct {
	PermissionDecision string // PreToolUse：allow / deny / ask / defer
	Reason             string
	UpdatedInput       map[string]any
	AdditionalContext  string  // PostToolUse：追加到工具结果后面
	UpdatedToolOutput  *string // PostToolUse：替换工具结果
}

// HookFunc 返回 nil 输出算“失败”（等价于 SDK 里没有 return 的回调）。没有意见就返回 &HookOutput{}。
type HookFunc func(ctx context.Context, in HookInput) (*HookOutput, error)

type HookMatcher struct {
	Matcher   string // 空：全部；"Write|Edit"：精确列表；其他：正则
	Hooks     []HookFunc
	OnFailure string // "" / "continue" / "block"
}

var exactList = regexp.MustCompile(`^[A-Za-z0-9_|]+$`)

func matcherMatch(m, target string) bool {
	if m == "" || m == "*" {
		return true
	}
	if exactList.MatchString(m) {
		for _, p := range strings.Split(m, "|") {
			if p == target {
				return true
			}
		}
		return false
	}
	re, err := regexp.Compile(m)
	return err == nil && re.MatchString(target)
}

type hookResult struct {
	out    *HookOutput
	failed bool
	block  bool
}

// runHooks 并发执行所有匹配的回调，结果按注册顺序返回。
func runHooks(ctx context.Context, hooks map[string][]HookMatcher, event, target string, in HookInput) []hookResult {
	type job struct {
		f     HookFunc
		block bool
	}
	var jobs []job
	for _, m := range hooks[event] {
		if !matcherMatch(m.Matcher, target) {
			continue
		}
		for _, f := range m.Hooks {
			jobs = append(jobs, job{f, m.OnFailure == "block"})
		}
	}
	in.Event = event
	res := make([]hookResult, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			out, err := j.f(ctx, in)
			res[i] = hookResult{out: out, failed: err != nil || out == nil, block: j.block}
		}(i, j)
	}
	wg.Wait()
	return res
}

var decisionRank = map[string]int{"": 0, "allow": 1, "ask": 2, "defer": 3, "deny": 4}

// aggregatePre：deny > defer > ask > allow；onFailure=block 的失败按 deny 处理，其余失败忽略。
func aggregatePre(rs []hookResult) (dec, reason string, upd map[string]any) {
	for _, r := range rs {
		d, why := "", ""
		if r.failed {
			if !r.block {
				continue
			}
			d, why = "deny", `hook failed; blocking because onFailure is "block"`
		} else {
			d, why = r.out.PermissionDecision, r.out.Reason
		}
		if decisionRank[d] > decisionRank[dec] {
			dec, reason = d, why
			if !r.failed && r.out.UpdatedInput != nil {
				upd = r.out.UpdatedInput
			}
		}
		if upd == nil && !r.failed && r.out.UpdatedInput != nil {
			upd = r.out.UpdatedInput
		}
	}
	return
}

// ---------- canUseTool ----------

type PermissionRequest struct {
	ToolName    string
	ToolUseID   string
	Input       map[string]any
	AgentType   string
	Reason      string   // 为什么走到回调：ask_rule / plan / default
	Suggestions []string // 可以原样回传到 UpdatedPermissions 的规则
}

type PermissionResult struct {
	Behavior           string // allow / deny
	UpdatedInput       map[string]any
	UpdatedPermissions []string // “批准并记住”：追加的 allow 规则
	Message            string
}

type CanUseTool func(ctx context.Context, req PermissionRequest) PermissionResult

// decision 是一次权限判定的结论。Via 记录在哪一步定下来的，便于测试和审计。
type decision struct {
	Allow    bool
	Deferred bool
	Input    map[string]any
	Message  string
	Via      string
}

func deny(msg, via string) decision { return decision{Message: msg, Via: via} }

func suggestionFor(t *Tool, in map[string]any) []string {
	if t.SpecKey != "" {
		if v, ok := in[t.SpecKey].(string); ok && !t.EditsFiles {
			return []string{fmt.Sprintf("%s(%s)", t.Name, v)}
		}
	}
	return []string{t.Name}
}

// decide 按官方文档的六步判定一次工具调用：
// 1 hooks → 2 deny 规则 → 3 ask 规则 → 4 权限模式 → 5 allow 规则 → 6 canUseTool。
func (r *agentRun) decide(ctx context.Context, t *Tool, c Block, allowDefer bool) decision {
	o := r.sh.opts
	in := c.Input
	hin := HookInput{SessionID: r.sh.sessionID, AgentType: r.agentType, ToolName: t.Name, ToolUseID: c.ID, ToolInput: in}

	// 1. hooks
	dec, reason, upd := aggregatePre(runHooks(ctx, o.Hooks, "PreToolUse", t.Name, hin))
	hookAllow, forceAsk := false, false
	switch dec {
	case "deny":
		if reason == "" {
			reason = "Blocked by PreToolUse hook"
		}
		return deny(reason, "hook")
	case "defer":
		if allowDefer {
			return decision{Deferred: true, Via: "hook"}
		}
		dec = "" // 一轮里有多个调用时 defer 被忽略，走正常流程
	case "ask":
		forceAsk = true
	case "allow":
		hookAllow = true
	}
	if upd != nil {
		in = upd
	}

	// 2. deny 规则：任何模式下都生效，hook 的 allow 也绕不过去
	if anyRule(o.DisallowedTools, t, in, o.Cwd, false) {
		return deny(fmt.Sprintf("Permission to use %s has been denied.", t.Name), "deny_rule")
	}
	// 3. ask 规则：即使 bypassPermissions 也要问
	if forceAsk || anyRule(o.AskRules, t, in, o.Cwd, false) {
		return r.callback(ctx, t, c.ID, in, "ask_rule")
	}
	// 4. 权限模式
	switch r.mode {
	case ModeBypass:
		return decision{Allow: true, Input: in, Via: "mode"}
	case ModeAcceptEdits:
		if p, _ := in["file_path"].(string); t.EditsFiles && inside(o.Cwd, p) {
			return decision{Allow: true, Input: in, Via: "mode"}
		}
	case ModePlan:
		if t.EditsFiles { // 规划期间的编辑：allow 规则和 hook 的 allow 都不算数
			return r.callback(ctx, t, c.ID, in, "plan")
		}
	}
	if hookAllow {
		return decision{Allow: true, Input: in, Via: "hook"}
	}
	// 5. allow 规则，以及工具自己就能批准的调用
	r.sh.mu.Lock()
	allow := append(append([]string{}, o.AllowedTools...), r.sh.remembered...)
	r.sh.mu.Unlock()
	if anyRule(allow, t, in, o.Cwd, true) || (t.SelfApprove != nil && t.SelfApprove(o.Cwd, in)) {
		return decision{Allow: true, Input: in, Via: "allow_rule"}
	}
	// 6. canUseTool
	return r.callback(ctx, t, c.ID, in, "default")
}

func (r *agentRun) callback(ctx context.Context, t *Tool, id string, in map[string]any, why string) decision {
	o := r.sh.opts
	if r.mode == ModeDontAsk {
		return deny(fmt.Sprintf("Permission to use %s was denied (dontAsk).", t.Name), "dontAsk")
	}
	if o.CanUseTool == nil {
		return deny(fmt.Sprintf("Permission to use %s was denied: no approver.", t.Name), "no_callback")
	}
	pr := o.CanUseTool(ctx, PermissionRequest{
		ToolName: t.Name, ToolUseID: id, Input: in, AgentType: r.agentType,
		Reason: why, Suggestions: suggestionFor(t, in),
	})
	if pr.Behavior != "allow" {
		msg := pr.Message
		if msg == "" {
			msg = fmt.Sprintf("Permission to use %s was denied.", t.Name)
		}
		return deny(msg, "callback")
	}
	if pr.UpdatedInput != nil {
		in = pr.UpdatedInput
	}
	if len(pr.UpdatedPermissions) > 0 {
		r.sh.mu.Lock()
		r.sh.remembered = append(r.sh.remembered, pr.UpdatedPermissions...)
		r.sh.mu.Unlock()
	}
	return decision{Allow: true, Input: in, Via: "callback"}
}
