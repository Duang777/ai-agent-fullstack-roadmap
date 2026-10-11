# 06-llm-basics · 1.1 Transformer、Tokenizer 与采样参数练习

对应课件：[`notes/01-llm/01-transformer-tokenizer-sampling.md`](../../notes/01-llm/01-transformer-tokenizer-sampling.md)

把“文本 → token → logits → 下一个 token”这条链路的每一步写成能测试的 Go 代码。纯标准库，无第三方依赖。

| 文件 | 内容 |
|---|---|
| `bpe.go` | 字节级 BPE：预分词、训练（确定性并列处理）、按合并顺序编码、解码；流式解码器扣住不完整的 UTF-8 尾巴 |
| `sampling.go` | 数值稳定的 softmax；logit bias、重复 / 存在 / 频率惩罚、温度、top-k、top-p、min-p；贪心与按种子抽样 |
| `attention.go` | 缩放点积注意力、因果掩码、KV Cache 增量计算、显存估算 |
| `lm.go` | bigram“语言模型”和自回归生成循环（`finish_reason`） |
| `llm_test.go` | 21 个测试 |
| `tokens.py` | 用 tiktoken 对比 cl100k_base 与 o200k_base |
| `testdata/corpus.txt` | 2 KB 中英文训练语料 |

```bash
make test     # go vet + go test -race -v
make tokens   # 需要 uv
```

练习 3（跨 token 的停止序列 + 流式推送）需要你自己在 `lm.go` 里加 `GenerateText`，参考答案在课件末尾。
