# /// script
# requires-python = ">=3.12"
# dependencies = ["tiktoken>=0.8"]
# ///
"""对比 cl100k_base（GPT-4 时代）和 o200k_base（GPT-4o 之后）的切分。运行：uv run tokens.py"""
import tiktoken

TEXTS = [
    "Hello, how are you today?",
    "The quick brown fox jumps over the lazy dog.",
    "你好，今天过得怎么样？",
    "人工智能体需要可靠的工具调用。",
    " 12345678901234567890",
    "strawberry",
    # 练习 1：在这里加你自己的三段文本（Go 代码、JSON 工具参数、中英混排）
]


def show(enc: tiktoken.Encoding, text: str) -> str:
    pieces = []
    for t in enc.encode(text):
        b = enc.decode_single_token_bytes(t)
        try:
            pieces.append(b.decode("utf-8"))
        except UnicodeDecodeError:  # 半个汉字：单个 token 不是合法 UTF-8
            pieces.append(b.hex(" "))
    return " | ".join(pieces)


encs = [tiktoken.get_encoding(n) for n in ("cl100k_base", "o200k_base")]
for text in TEXTS:
    print(f"{text!r}  （{len(text.encode())} 字节）")
    for enc in encs:
        print(f"  {enc.name:12} {len(enc.encode(text)):3}  {show(enc, text)}")
