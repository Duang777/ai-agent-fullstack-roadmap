# 15-computer-use · 2.4 Computer Use / Browser Use 练习

对应课件：[`notes/02-tools/04-computer-use.md`](../../notes/02-tools/04-computer-use.md)

Computer Use 代理的“执行端”。模型只输出动作，点下去的是这里的代码。用一个纯 Go 的假浏览器（没有真实渲染）把最容易出错的部分做成可测试的。

| 文件 | 内容 |
|---|---|
| `browser.go` | 假浏览器：元素、坐标命中、点击、输入、按键、布局移动（插入 / 移除元素）、截图 |
| `scale.go` | 截图缩放系数、坐标换算、Gemini 归一化坐标、视觉 token 估算 |
| `actions.go` | Anthropic `computer_toolset_20260801`、OpenAI `computer_call`、Gemini `function_call` 三家格式转成统一的 `Action`，以及执行 |
| `guard.go` | 网址白名单（URL 解析、拒绝内网地址）、每个动作执行前的检查与人工确认 |
| `runner.go` | 三家的执行器：批量动作顺序执行、失败即停、规定原文、`toolset_name` 回显、`safety_acknowledgement` |
| `refs.go` | Browser Use 风格的元素引用：只读可见内容、引用稳定、过期报错、导航后作废 |
| `history.go` | 截图历史修剪策略：缓存失效次数与单请求图片数 |
| `loop.go` | 有步数上限、结束时核验真实状态的循环 |
| `cua_test.go` | 13 个测试 |

```bash
make test
```

练习 2（Gemini 的 type 清空与回车）和练习 3（重定向之后再检查）需要你自己写，参考答案在课件末尾。
