package reasoning

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// 这一部分模拟“推理块在多轮对话里的生命周期”，规则来自 Anthropic 的 thinking / preserved thinking 文档：
//
//  1. 思考块带一个 signature：完整思考的加密副本，回传时服务端靠它还原思考、验证没被篡改；
//  2. 签名绑定“产生它的模型”：每个模型只读得懂自己和一组固定模型的思考块，读不懂的静默丢弃；
//  3. 签名绑定“它之前的全部内容”：system、tools、之前的消息任何一处改了，这个块和之后所有思考块都失效；
//  4. 有的模型保留所有历史轮次的思考，有的只保留最近一轮；工具循环内的思考必须原样回传。
//
// OpenAI 的 encrypted_content 和 Gemini 的 thought signature 是同一类东西，细节不同（见课件）。

// Kind 是内容块的类型。
type Kind string

const (
	KindText       Kind = "text"
	KindThinking   Kind = "thinking"
	KindRedacted   Kind = "redacted_thinking" // 安全原因被整段加密，没有可读文本
	KindToolUse    Kind = "tool_use"
	KindToolResult Kind = "tool_result"
)

// Block 是一条消息里的一个内容块。为了简单，每个块自带 Role。
type Block struct {
	Role      string // "user" | "assistant"
	Kind      Kind
	Text      string // 可读部分：text 的正文、思考的“摘要”（display=omitted 时为空）
	Signature string // 思考块 / redacted 块的签名
}

// Server 模拟服务端：持有签名密钥，知道哪个模型能读哪些模型的思考。
type Server struct {
	key   []byte
	Reads map[string][]string // 模型 → 它能读的其他模型（自己总能读自己）
	// KeepAllTurns：哪些模型保留所有历史轮次的思考；不在表里的只保留最近一轮。
	KeepAllTurns map[string]bool
}

func NewServer(key string) *Server {
	return &Server{key: []byte(key), Reads: map[string][]string{}, KeepAllTurns: map[string]bool{}}
}

// prefixDigest 把一段内容规范化后取哈希。system 和 tools 也算前缀的一部分。
func prefixDigest(system string, tools []string, msgs []Block) string {
	h := sha256.New()
	fmt.Fprintf(h, "system:%q\n", system)
	for _, t := range tools {
		fmt.Fprintf(h, "tool:%q\n", t)
	}
	for _, b := range msgs {
		if b.Kind == KindThinking || b.Kind == KindRedacted {
			// 思考块只按签名计入前缀：签名已经覆盖了摘要，omitted 块里客户端写的文字则应被忽略。
			fmt.Fprintf(h, "%s|%s|%q\n", b.Role, b.Kind, b.Signature)
			continue
		}
		fmt.Fprintf(h, "%s|%s|%q\n", b.Role, b.Kind, b.Text)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sign 生成签名：“模型 . 加密的完整思考 . MAC(模型, 前缀, 完整思考, 展示文本)”。
// 真实的签名是加密的，这里只用 base64 表示“客户端看不懂、也不该解析”。
func (s *Server) sign(model, prefix, hidden, shown string) string {
	mac := hmac.New(sha256.New, s.key)
	fmt.Fprintf(mac, "%s\x00%s\x00%s\x00%s", model, prefix, hidden, shown)
	return model + "." + base64.StdEncoding.EncodeToString([]byte(hidden)) + "." + hex.EncodeToString(mac.Sum(nil))
}

// Generate 模拟一次回答：在 history 之后产出一个思考块和若干其他块。
// summarized=false 对应 display: "omitted"：思考块的 Text 为空，但签名里照样有完整思考。
func (s *Server) Generate(model, system string, tools []string, history []Block, hidden, summary string, summarized bool, rest ...Block) []Block {
	shown := ""
	if summarized {
		shown = summary
	}
	th := Block{Role: "assistant", Kind: KindThinking, Text: shown}
	th.Signature = s.sign(model, prefixDigest(system, tools, history), hidden, shown)
	return append([]Block{th}, rest...)
}

// ErrModified 对应文档里的 400：思考块被修改，或者它之前的前缀变了。
var ErrModified = errors.New("thinking blocks cannot be modified")

// Mismatch 是前缀或签名对不上时的处理方式（对应 block_binding.prefix_mismatch_behavior）。
type Mismatch int

const (
	MismatchError Mismatch = iota // 整个请求 400
	MismatchDrop                  // 丢掉这个块和之后所有思考块，继续
)

// Dropped 记录一个被丢掉的思考块（对应响应里的 input_transformations）。
type Dropped struct {
	Index  int
	Reason string // "unreadable_model" | "invalid_signature" | "after_invalid" | "prior_turn"
}

// Render 模拟服务端处理一次请求：决定哪些思考块真正进入模型的上下文。
// 返回渲染后的消息（思考块已还原为完整思考）和被丢弃的块。
func (s *Server) Render(model, system string, tools []string, msgs []Block, on Mismatch) ([]Block, []Dropped, error) {
	var out []Block
	var dropped []Dropped
	invalidFrom := -1
	lastTurn := lastTurnStart(msgs)
	for i, b := range msgs {
		if b.Kind != KindThinking && b.Kind != KindRedacted {
			out = append(out, b)
			continue
		}
		if invalidFrom >= 0 {
			dropped = append(dropped, Dropped{i, "after_invalid"})
			continue
		}
		producer, hidden, ok := s.verify(system, tools, msgs[:i], b)
		switch {
		case !ok:
			if on == MismatchError {
				return nil, nil, fmt.Errorf("messages[%d]: %w", i, ErrModified)
			}
			invalidFrom = i
			dropped = append(dropped, Dropped{i, "invalid_signature"})
		case !s.canRead(model, producer):
			dropped = append(dropped, Dropped{i, "unreadable_model"}) // 静默丢弃，不报错，不计费
		case i < lastTurn && !s.KeepAllTurns[model]:
			dropped = append(dropped, Dropped{i, "prior_turn"})
		default:
			out = append(out, Block{Role: b.Role, Kind: b.Kind, Text: hidden, Signature: b.Signature})
		}
	}
	return out, dropped, nil
}

func (s *Server) verify(system string, tools []string, before []Block, b Block) (producer, hidden string, ok bool) {
	parts := strings.SplitN(b.Signature, ".", 3)
	if len(parts) != 3 {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", false
	}
	producer, hidden = parts[0], string(raw)
	// display=omitted 的块，客户端在空 Text 里写的东西会被忽略（文档原话）；
	// summarized 的块，Text 是签名的一部分，改一个字就失效。
	want := s.sign(producer, prefixDigest(system, tools, before), hidden, b.Text)
	if hmac.Equal([]byte(want), []byte(b.Signature)) {
		return producer, hidden, true
	}
	omitted := s.sign(producer, prefixDigest(system, tools, before), hidden, "")
	if hmac.Equal([]byte(omitted), []byte(b.Signature)) {
		return producer, hidden, true
	}
	return "", "", false
}

func (s *Server) canRead(model, producer string) bool {
	return model == producer || slices.Contains(s.Reads[model], producer)
}

// lastTurnStart 返回最近一轮的起点：最后一条“带新输入的 user 消息”。
// 只装着 tool_result 的 user 消息不算新的一轮：整个工具循环是同一个 assistant 回合。
func lastTurnStart(msgs []Block) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" && msgs[i].Kind != KindToolResult {
			return i
		}
	}
	return 0
}

// NaiveStrip 是一个常见的客户端 bug：想“只保留思考块”，却只按 type == "thinking" 过滤，
// 结果把 redacted_thinking 弄丢了，破坏了回传协议。练习 2 会用到它。
func NaiveStrip(msgs []Block) []Block {
	var out []Block
	for _, b := range msgs {
		if b.Role == "assistant" && b.Kind != KindThinking && b.Kind != KindText && b.Kind != KindToolUse {
			continue
		}
		out = append(out, b)
	}
	return out
}
