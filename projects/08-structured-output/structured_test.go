package structured

import (
	"context"
	"errors"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

const personSchema = `{
  "type": "object",
  "properties": {
    "name": {"type": "string"},
    "age": {"type": "integer", "minimum": 0},
    "role": {"type": "string", "enum": ["admin", "member"]},
    "email": {"type": ["string", "null"]},
    "tags": {"type": "array", "items": {"type": "string"}, "maxItems": 3}
  },
  "required": ["name", "age", "role", "email", "tags"],
  "additionalProperties": false
}`

func parse(t *testing.T, s string) any {
	t.Helper()
	v, err := ParseOrdered([]byte(s))
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

// ---------- 保序 JSON 与校验 ----------

func TestParseOrderedKeepsOrder(t *testing.T) {
	v := parse(t, `{"z":1,"a":{"y":true,"b":null},"m":[1.5,"x"]}`)
	if got := Compact(v); got != `{"z":1,"a":{"y":true,"b":null},"m":[1.5,"x"]}` {
		t.Fatalf("order lost: %s", got)
	}
}

func TestValidateCollectsAllErrors(t *testing.T) {
	sc := MustCompileJSON(personSchema)
	v := parse(t, `{"name":"Ann","age":"30","role":"Admin","tags":["a","b","c","d"],"extra":1}`)
	var got []string
	for _, e := range sc.Validate(v) {
		got = append(got, e.Error())
	}
	want := []string{
		`$: missing required property "email"`,
		`$.age: expected integer, got string`,
		`$.role: must be one of ["admin","member"]`,
		`$.tags: has 4 items, maxItems 3`,
		`$: unexpected property "extra"`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if errs := sc.Validate(parse(t, `{"name":"Ann","age":1.5,"role":"admin","email":null,"tags":[]}`)); len(errs) != 1 || errs[0].Msg != "expected integer, got number" {
		t.Fatalf("1.5 is not an integer: %v", errs)
	}
}

func TestValidateAnyOfAndRecursion(t *testing.T) {
	tree := MustCompileJSON(`{"type":"object","properties":{
	  "name":{"type":"string"},
	  "children":{"type":"array","items":{"$ref":"#"}}},
	  "required":["name","children"],"additionalProperties":false}`)
	ok := parse(t, `{"name":"a","children":[{"name":"b","children":[]},{"name":"c","children":[{"name":"d","children":[]}]}]}`)
	if errs := tree.Validate(ok); len(errs) != 0 {
		t.Fatal(errs)
	}
	bad := parse(t, `{"name":"a","children":[{"name":"b","children":[{"name":1,"children":[]}]}]}`)
	errs := tree.Validate(bad)
	if len(errs) != 1 || errs[0].Path != "$.children[0].children[0].name" {
		t.Fatalf("got %v", errs)
	}
	u := MustCompileJSON(`{"anyOf":[{"type":"string"},{"type":"integer","minimum":10}]}`)
	if len(u.Validate(parse(t, `"x"`))) != 0 || len(u.Validate(parse(t, `12`))) != 0 || len(u.Validate(parse(t, `3`))) != 1 {
		t.Fatal("anyOf")
	}
}

// ---------- 前缀检查（约束解码的“语法”） ----------

func TestCheckPrefixes(t *testing.T) {
	sc := MustCompileJSON(personSchema)
	cases := []struct {
		in   string
		want Status
	}{
		{``, Prefix},
		{`{`, Prefix},
		{`{"na`, Prefix},
		{`{"name":"An`, Prefix},
		{`{"name":"Ann","age":3`, Prefix},
		{`{"name":"Ann","age":-`, Prefix},
		{`{"name":"Ann","age":30,"role":"me`, Prefix},
		{`{"name":"Ann","age":30,"role":"admin","email":nu`, Prefix},
		{`{"name":"Ann","age":30,"role":"admin","email":null,"tags":["x",`, Prefix},
		{`{"name":"Ann","age":30,"role":"admin","email":null,"tags":[]}`, Complete},
		{`{"name":"Ann","age":30,"role":"admin","email":"a@b.c","tags":["x","y","z"]}`, Complete},
		{` {`, Invalid},                     // 不允许空白
		{`{"age"`, Invalid},                 // 键的顺序固定：先 name
		{`{"name":1`, Invalid},              // 类型错
		{`{"name":"Ann","age":3.`, Invalid}, // integer 不能有小数点
		{`{"name":"Ann","age":01`, Invalid}, // 不允许前导零
		{`{"name":"Ann","age":30,"role":"Admin"`, Invalid},
		{`{"name":"Ann","age":30,"role":"admin","email":null,"tags":["a","b","c",`, Invalid}, // maxItems
		{`{"name":"Ann","age":30,"role":"admin","email":null,"tags":[]}x`, Invalid},
		{`{"name":"Ann","age":30,"role":"admin","email":null,"tags":[],"extra"`, Invalid},
	}
	for _, c := range cases {
		if got := sc.Check(c.in); got != c.want {
			t.Errorf("Check(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestCheckNumberCanEndOrContinue(t *testing.T) {
	n := MustCompileJSON(`{"type":"number"}`)
	for in, want := range map[string]Status{"12": Complete, "1.": Prefix, "1.5": Complete, "-0": Complete, "-": Prefix, "1.5.": Invalid} {
		if got := n.Check(in); got != want {
			t.Errorf("%q: %s want %s", in, got, want)
		}
	}
}

func TestCheckOptionalFieldsAndStrings(t *testing.T) {
	sc := MustCompileJSON(`{"type":"object","properties":{
	  "a":{"type":"string"},"b":{"type":"integer"},"c":{"type":"boolean"}},
	  "required":["a","c"],"additionalProperties":false}`)
	for in, want := range map[string]Status{
		`{"a":"x","c":true}`:        Complete, // 跳过可选的 b
		`{"a":"x","b":1,"c":false}`: Complete,
		`{"a":"x","c":true,"b":1}`:  Invalid, // 顺序固定
		`{"a":"x"}`:                 Invalid, // c 必填
		`{"a":"say \"hi\"\n","c":t`: Prefix,
		`{"a":"\u00e9","c":true}`:   Complete,
		`{"a":"\q`:                  Invalid, // 非法转义
		"{\"a\":\"line\nbreak":      Invalid, // 裸换行是控制字符
		`{"a":"\u00g`:               Invalid,
		`{"a":"中文也可以","c":true}`:    Complete,
	} {
		if got := sc.Check(in); got != want {
			t.Errorf("Check(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestCheckRecursiveAndArrays(t *testing.T) {
	tree := MustCompileJSON(`{"type":"object","properties":{"v":{"type":"integer"},"kids":{"type":"array","items":{"$ref":"#"},"minItems":0}},"required":["v","kids"],"additionalProperties":false}`)
	if got := tree.Check(`{"v":1,"kids":[{"v":2,"kids":[{"v":3,"kids":[]}]}]}`); got != Complete {
		t.Fatal(got)
	}
	pair := MustCompileJSON(`{"type":"array","items":{"type":"boolean"},"minItems":2,"maxItems":2}`)
	for in, want := range map[string]Status{`[true]`: Invalid, `[true,false]`: Complete, `[true,false,`: Invalid, `[]`: Invalid} {
		if got := pair.Check(in); got != want {
			t.Errorf("%s: %s want %s", in, got, want)
		}
	}
}

// ---------- 约束解码 ----------

// 模型“想”输出的东西：有空格、age 是字符串、多了一个字段、role 大小写不对。
const sloppy = `{"name": "Ann", "age": "30", "role": "Admin", "nickname": "A"}`

func TestUnconstrainedModelBreaksSchema(t *testing.T) {
	sc := MustCompileJSON(personSchema)
	out, finish, err := Generate(ScriptModel{sloppy}, ByteVocab(), nil, 200, nil)
	if err != nil || finish != "stop" || out != sloppy {
		t.Fatalf("out=%q finish=%s err=%v", out, finish, err)
	}
	errs := sc.Validate(parse(t, out))
	if len(errs) == 0 {
		t.Fatal("expected schema violations")
	}
	t.Logf("不加约束：JSON 合法，但有 %d 处不符合 schema", len(errs))
}

func TestConstrainedOutputAlwaysMatchesStructure(t *testing.T) {
	sc := MustCompileJSON(personSchema)
	vocab := ByteVocab(`",`, `":`, `":"`, `"}`, `name`, `age`, `role`, `admin`, `null`, `Ann`) // 跨结构的多字符 token
	out, finish, err := Generate(ScriptModel{sloppy}, vocab, sc, 300, nil)
	if err != nil || finish != "stop" {
		t.Fatalf("out=%q finish=%s err=%v", out, finish, err)
	}
	if sc.Check(out) != Complete {
		t.Fatalf("not complete: %s", out)
	}
	if errs := sc.Validate(parse(t, out)); len(errs) != 0 {
		t.Fatalf("%s: %v", out, errs)
	}
	t.Logf("加约束：%s", out)
}

func TestConstrainedForcesAChoice(t *testing.T) {
	sc := MustCompileJSON(`{"type":"object","properties":{"answer":{"type":"string","enum":["yes","no"]}},"required":["answer"],"additionalProperties":false}`)
	out, _, err := Generate(ScriptModel{`{"answer":"unsure"}`}, ByteVocab(), sc, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"answer":"yes"}` && out != `{"answer":"no"}` {
		t.Fatalf("got %s", out)
	}
	t.Logf("模型想说 unsure，被迫输出 %s：格式保证了，意思可能被扭曲", out)
}

func TestConstrainedTruncatedByMaxTokens(t *testing.T) {
	sc := MustCompileJSON(personSchema)
	out, finish, err := Generate(ScriptModel{sloppy}, ByteVocab(), sc, 15, nil)
	if err != nil || finish != "length" {
		t.Fatalf("finish=%s err=%v", finish, err)
	}
	if sc.Check(out) != Prefix {
		t.Fatalf("%s should be a valid but incomplete prefix", out)
	}
	if _, err := ParseOrdered([]byte(out)); err == nil {
		t.Fatal("truncated output should not parse")
	}
}

func TestConstrainedDeadEnd(t *testing.T) {
	sc := MustCompileJSON(`{"type":"string","enum":["é"]}`) // 词表里只有 ASCII
	_, _, err := Generate(ScriptModel{`"x"`}, ByteVocab(), sc, 10, nil)
	if !errors.Is(err, ErrDeadEnd) {
		t.Fatalf("got %v", err)
	}
}

// biasModel：所有 token 等概率，但偏爱收尾符号，让随机生成能在有限步内结束。
type biasModel struct{}

func (biasModel) Logits(_ string, vocab []string) []float64 {
	out := make([]float64, len(vocab))
	for i, tok := range vocab {
		switch tok {
		case EOS, `"`, "}", "]", ",":
			out[i] = 3
		}
	}
	return out
}

// 性质测试：随机抽样 300 次，只要正常结束，结果一定能解析并通过校验。
func TestConstrainedRandomSamplingProperty(t *testing.T) {
	sc := MustCompileJSON(`{"type":"object","properties":{
	  "id":{"type":"integer"},
	  "score":{"type":"number"},
	  "kind":{"type":"string","enum":["a","b"]},
	  "note":{"type":["string","null"]},
	  "tags":{"type":"array","items":{"type":"string"},"maxItems":2},
	  "meta":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}},
	  "required":["id","score","kind","note","tags"],"additionalProperties":false}`)
	rng := rand.New(rand.NewPCG(1, 2))
	vocab := ByteVocab(`":`, `",`, `null`, `true`)
	stopped := 0
	for i := 0; i < 100; i++ {
		out, finish, err := Generate(biasModel{}, vocab, sc, 400, rng)
		if err != nil {
			t.Fatal(err)
		}
		if finish != "stop" {
			continue
		}
		stopped++
		v, err := ParseOrdered([]byte(out))
		if err != nil {
			t.Fatalf("run %d: %s does not parse: %v", i, out, err)
		}
		if errs := sc.Validate(v); len(errs) != 0 {
			t.Fatalf("run %d: %s: %v", i, out, errs)
		}
	}
	if stopped < 50 {
		t.Fatalf("only %d runs finished", stopped)
	}
	t.Logf("%d/100 次正常结束，全部通过校验", stopped)
}

// 语法只管结构。minimum 这种语义约束，约束解码不管，要靠校验。
func TestGrammarDoesNotEnforceSemantics(t *testing.T) {
	sc := MustCompileJSON(`{"type":"object","properties":{"age":{"type":"integer","minimum":0}},"required":["age"],"additionalProperties":false}`)
	out, _, err := Generate(ScriptModel{`{"age":-5}`}, ByteVocab(), sc, 50, nil)
	if err != nil || out != `{"age":-5}` {
		t.Fatalf("out=%s err=%v", out, err)
	}
	if errs := sc.Validate(parse(t, out)); len(errs) != 1 {
		t.Fatalf("validator must catch minimum: %v", errs)
	}
}

// ---------- 三家 strict 规则 ----------

const looseSchema = `{
  "type": "object",
  "properties": {
    "title": {"type": "string", "maxLength": 50, "description": "标题"},
    "score": {"type": "integer", "minimum": 1, "maximum": 5},
    "summary": {"type": "string"},
    "items": {"type": "array", "minItems": 2, "items": {"type": "object", "properties": {"sku": {"type": "string"}}}}
  },
  "required": ["title", "score"]
}`

func msgs(is []Issue) []string {
	var out []string
	for _, i := range is {
		out = append(out, i.String())
	}
	return out
}

func TestCheckOpenAI(t *testing.T) {
	got := msgs(CheckOpenAI(parse(t, looseSchema)))
	want := []string{
		"$: additionalProperties must be false",
		`$: property "summary" must be listed in required`,
		`$: property "items" must be listed in required`,
		"$.properties.items.items: additionalProperties must be false",
		`$.properties.items.items: property "sku" must be listed in required`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got\n%s", strings.Join(got, "\n"))
	}
	if is := CheckOpenAI(parse(t, `{"anyOf":[{"type":"object"},{"type":"string"}]}`)); len(is) < 2 {
		t.Fatalf("root anyOf: %v", is)
	}
}

func TestForOpenAI(t *testing.T) {
	orig := parse(t, looseSchema)
	before := Compact(orig)
	out := ForOpenAI(orig)
	if Compact(orig) != before {
		t.Fatal("ForOpenAI must not modify its input")
	}
	if is := CheckOpenAI(out); len(is) != 0 {
		t.Fatalf("still has issues: %v", is)
	}
	o := out.(Obj)
	props, _ := o.Get("properties")
	sum, _ := props.(Obj).Get("summary")
	if Compact(sum) != `{"type":["string","null"]}` {
		t.Fatalf("optional field must become nullable: %s", Compact(sum))
	}
	var order []string
	for _, kv := range props.(Obj) {
		order = append(order, kv.Key)
	}
	if strings.Join(order, ",") != "title,score,summary,items" {
		t.Fatalf("property order changed: %v", order)
	}
	// 转换后的 schema 依然能编译，而且接受 null
	sc, err := Compile(out)
	if err != nil {
		t.Fatal(err)
	}
	if errs := sc.Validate(parse(t, `{"title":"t","score":3,"summary":null,"items":[{"sku":"a"},{"sku":"b"}]}`)); len(errs) != 0 {
		t.Fatal(errs)
	}
}

func TestAnthropicCheckAndTransform(t *testing.T) {
	orig := parse(t, looseSchema)
	issues := msgs(CheckAnthropic(orig))
	for _, want := range []string{"$.properties.title: maxLength is not supported", "$.properties.score: minimum is not supported", "$.properties.items: minItems only supports 0 or 1"} {
		found := false
		for _, i := range issues {
			found = found || i == want
		}
		if !found {
			t.Errorf("missing issue %q in %v", want, issues)
		}
	}
	out := ForAnthropic(orig)
	if is := CheckAnthropic(out); len(is) != 0 {
		t.Fatalf("still has issues: %v", is)
	}
	props, _ := out.(Obj).Get("properties")
	title, _ := props.(Obj).Get("title")
	if Compact(title) != `{"type":"string","description":"标题 {maxLength: 50}"}` {
		t.Fatalf("title: %s", Compact(title))
	}
	// 发出去的 schema 放宽了，所以结果必须用原 schema 再校验
	strictSc := MustCompileJSON(looseSchema)
	sentSc, _ := Compile(out)
	v := parse(t, `{"title":"t","score":9}`)
	if len(sentSc.Validate(v)) != 0 || len(strictSc.Validate(v)) != 1 {
		t.Fatal("score 9 passes the sent schema but must fail the original")
	}
}

// ---------- 抽取循环 ----------

type scripted struct {
	replies  []struct{ text, stop string }
	feedback [][]string
}

func (s *scripted) Generate(_ context.Context, fb []string) (string, string, error) {
	s.feedback = append(s.feedback, append([]string(nil), fb...))
	r := s.replies[len(s.feedback)-1]
	return r.text, r.stop, nil
}

func reply(text, stop string) struct{ text, stop string } {
	return struct{ text, stop string }{text, stop}
}

func TestExtractRetriesWithFeedback(t *testing.T) {
	sc := MustCompileJSON(personSchema)
	g := &scripted{replies: []struct{ text, stop string }{
		reply("好的，结果如下：{\"name\":", "end"),
		reply("```json\n{\"name\":\"Ann\",\"age\":-1,\"role\":\"admin\",\"email\":null,\"tags\":[]}\n```", "end"),
		reply(`{"name":"Ann","age":30,"role":"admin","email":null,"tags":["vip"]}`, "end"),
	}}
	v, attempts, err := Extract(context.Background(), g, sc, 3)
	if err != nil || attempts != 3 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
	if !strings.HasPrefix(g.feedback[1][0], "输出不是合法 JSON") {
		t.Fatalf("feedback 1: %v", g.feedback[1])
	}
	if !reflect.DeepEqual(g.feedback[2], []string{"$.age: must be >= 0"}) {
		t.Fatalf("feedback 2: %v", g.feedback[2])
	}
	if Compact(v) != `{"name":"Ann","age":30,"role":"admin","email":null,"tags":["vip"]}` {
		t.Fatal(Compact(v))
	}
}

func TestExtractRefusalTruncationAndGiveUp(t *testing.T) {
	sc := MustCompileJSON(personSchema)
	_, _, err := Extract(context.Background(), &scripted{replies: []struct{ text, stop string }{reply("I can't help", "refusal")}}, sc, 3)
	if !errors.Is(err, ErrRefusal) {
		t.Fatal(err)
	}
	_, _, err = Extract(context.Background(), &scripted{replies: []struct{ text, stop string }{reply(`{"name":"An`, "length")}}, sc, 3)
	if !errors.Is(err, ErrTruncated) {
		t.Fatal(err)
	}
	bad := reply(`{"name":"Ann"}`, "end")
	_, n, err := Extract(context.Background(), &scripted{replies: []struct{ text, stop string }{bad, bad}}, sc, 2)
	var ee *ExtractError
	if !errors.As(err, &ee) || n != 2 || len(ee.Last) != 4 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestNormalizeEnums(t *testing.T) {
	sc := MustCompileJSON(`{"type":"object","properties":{"topic":{"type":"string","enum":["Conversation Topic 1","Conversation topic 3"]},"list":{"type":"array","items":{"enum":["Red","blue"]}}},"required":["topic","list"],"additionalProperties":false}`)
	v := parse(t, `{"topic":"Conversation Topic 3","list":["red","BLUE","Red"]}`)
	if len(sc.Validate(v)) == 0 {
		t.Fatal("should be invalid before normalizing")
	}
	v, n := NormalizeEnums(sc, v)
	if n != 3 || len(sc.Validate(v)) != 0 {
		t.Fatalf("n=%d %s", n, Compact(v))
	}
}
