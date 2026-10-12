# 08-structured-output · 1.3 结构化输出练习

对应课件：[`notes/01-llm/03-structured-output.md`](../../notes/01-llm/03-structured-output.md)

把“让模型输出符合 schema 的 JSON”拆成能测试的几块。纯 Go 标准库。

| 文件 | 内容 |
|---|---|
| `ordered.go` | 保序 JSON 对象（字段顺序影响生成顺序） |
| `schema.go` | JSON Schema 子集：编译、校验，返回带路径的全部错误 |
| `grammar.go` | 前缀检查器：给定前缀，它还能不能写成合法文档（Invalid / Prefix / Complete） |
| `decode.go` | 约束解码：每一步把不合法 token 的 logit 设成 -∞；玩具模型与词表 |
| `strict.go` | OpenAI / Anthropic strict 规则检查，`ForOpenAI`、`ForAnthropic` 转换 |
| `extract.go` | 解析 → 校验 → 带错误重试的抽取循环；enum 大小写修正 |
| `structured_test.go` | 20 个测试 |

```bash
make test
```

练习 3（把 `maxLength` / `minLength` 放进语法）需要你自己写，参考答案在课件末尾。
