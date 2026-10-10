"""把 notes/ 下的课件渲染成 GitHub Pages 上的在线文章（docs/notes/*.html）。

用法：
    pip install markdown pygments
    python scripts/build_articles.py
"""
from __future__ import annotations

import html
import posixpath
import re
from pathlib import Path

import markdown
from pygments.formatters import HtmlFormatter  # noqa: F401  (确保 pygments 可用)

ROOT = Path(__file__).resolve().parent.parent
OUT = ROOT / "docs" / "notes"
REPO = "https://github.com/Duang777/ai-agent-fullstack-roadmap"

# 课件顺序。新增一课，在这里加一行。
LESSONS = [
    ("notes/00-foundations/01-go.md", "00-01-go", "0.1", "Go 基础与并发", "5–6 小时"),
    ("notes/00-foundations/02-typescript.md", "00-02-typescript", "0.2", "TypeScript", "4–5 小时"),
    ("notes/00-foundations/03-python.md", "00-03-python", "0.3", "Python（够用即可）", "3–4 小时"),
    ("notes/00-foundations/04-streaming.md", "00-04-streaming", "0.4", "HTTP / SSE / WebSocket", "4–5 小时"),
    ("notes/00-foundations/05-engineering.md", "00-05-engineering", "0.5", "Git、Docker、Linux", "4–5 小时"),
]

CSS = r"""
:root{
  --bg:#0a0a0a;--surface:#111;--text:#ededed;--text-2:#a1a1a1;--text-3:rgb(255 255 255/.4);
  --line:rgb(255 255 255/.1);--line-2:rgb(255 255 255/.06);
  --sans:Inter,-apple-system,BlinkMacSystemFont,"SF Pro Text","PingFang SC","Hiragino Sans GB","Noto Sans SC","Microsoft YaHei","Helvetica Neue",sans-serif;
  --mono:ui-monospace,"SF Mono",SFMono-Regular,Menlo,Consolas,"Liberation Mono",monospace;
}
*{box-sizing:border-box}
html{scroll-behavior:smooth;scroll-padding-top:80px}
body{margin:0;background:var(--bg);color:var(--text);font-family:var(--sans);font-size:16px;line-height:1.8;-webkit-font-smoothing:antialiased}
a{color:inherit}
.nav{position:sticky;top:0;z-index:10;height:56px;display:flex;align-items:center;justify-content:space-between;padding:0 24px;border-bottom:1px solid var(--line);background:rgb(10 10 10/.8);backdrop-filter:blur(12px)}
.nav__brand{display:flex;align-items:center;gap:10px;text-decoration:none;font-size:14px;font-weight:500}
.nav__dot{width:10px;height:10px;border-radius:3px;background:var(--text)}
.nav__links{display:flex;gap:20px;font-size:14px;color:var(--text-2)}
.nav__links a{text-decoration:none}.nav__links a:hover{color:var(--text)}
.wrap{max-width:1200px;margin:0 auto;padding:0 24px;display:grid;grid-template-columns:220px minmax(0,1fr);gap:56px}
.toc{position:sticky;top:80px;align-self:start;max-height:calc(100vh - 100px);overflow:auto;padding:40px 0 40px;font-size:13px;line-height:1.6}
.toc__label{font-family:var(--mono);font-size:11px;letter-spacing:.08em;color:var(--text-3);text-transform:uppercase;margin-bottom:12px}
.toc ul{list-style:none;margin:0;padding:0}
.toc li{margin:0}
.toc a{display:block;padding:4px 0 4px 12px;border-left:1px solid var(--line);color:var(--text-2);text-decoration:none}
.toc a:hover,.toc a.on{color:var(--text);border-left-color:var(--text)}
.toc ul ul a{padding-left:24px;font-size:12.5px;color:var(--text-3)}
.toc ul ul a:hover,.toc ul ul a.on{color:var(--text)}
article{min-width:0;max-width:760px;padding:48px 0 96px}
.meta{font-family:var(--mono);font-size:12px;letter-spacing:.06em;color:var(--text-3);margin-bottom:16px}
article h1.title{font-size:40px;line-height:1.2;letter-spacing:-.02em;font-weight:600;margin:0 0 24px}
article h1{font-size:28px;line-height:1.3;letter-spacing:-.01em;font-weight:600;margin:72px 0 20px;padding-top:32px;border-top:1px solid var(--line)}
article h2{font-size:22px;line-height:1.4;font-weight:600;margin:48px 0 16px}
article h3{font-size:18px;font-weight:600;margin:36px 0 12px}
article h4{font-size:16px;font-weight:600;margin:28px 0 8px}
article p{margin:0 0 18px;color:#d4d4d4}
article strong{color:var(--text);font-weight:600}
article ul,article ol{padding-left:22px;margin:0 0 18px;color:#d4d4d4}
article li{margin:4px 0}
article a{text-decoration:underline;text-decoration-color:var(--text-3);text-underline-offset:3px}
article a:hover{text-decoration-color:var(--text)}
article hr{border:0;border-top:1px solid var(--line);margin:48px 0}
article blockquote{margin:0 0 20px;padding:12px 18px;border-left:2px solid var(--text-2);background:var(--surface);border-radius:0 8px 8px 0;color:var(--text-2)}
article blockquote p{margin:6px 0;color:var(--text-2)}
article code{font-family:var(--mono);font-size:.88em;background:#1a1a1a;border:1px solid var(--line-2);border-radius:5px;padding:1px 6px}
article pre{position:relative;margin:0 0 22px;padding:16px 18px;background:#0f0f0f;border:1px solid var(--line);border-radius:10px;overflow:auto;font-size:13.5px;line-height:1.65}
article pre code{background:none;border:0;padding:0;font-size:inherit}
.code{position:relative}
.code__lang{position:absolute;top:8px;right:46px;font-family:var(--mono);font-size:11px;color:var(--text-3);z-index:1}
.code__copy{position:absolute;top:6px;right:8px;z-index:1;font:11px var(--mono);color:var(--text-2);background:#1a1a1a;border:1px solid var(--line);border-radius:6px;padding:2px 7px;cursor:pointer}
.code__copy:hover{color:var(--text)}
.tbl{overflow-x:auto;margin:0 0 22px;border:1px solid var(--line);border-radius:10px}
article table{border-collapse:collapse;width:100%;font-size:14px}
article th,article td{padding:10px 14px;border-bottom:1px solid var(--line-2);text-align:left;vertical-align:top}
article th{font-weight:600;background:var(--surface);color:var(--text)}
article tr:last-child td{border-bottom:0}
article details{margin:0 0 18px;border:1px solid var(--line);border-radius:10px;padding:10px 16px;background:var(--surface)}
article summary{cursor:pointer;font-weight:500}
.pager{display:grid;grid-template-columns:1fr 1fr;gap:16px;margin-top:72px;padding-top:32px;border-top:1px solid var(--line)}
.pager a{display:block;padding:16px 18px;border:1px solid var(--line);border-radius:10px;text-decoration:none}
.pager a:hover{border-color:var(--text-2)}
.pager span{display:block;font-family:var(--mono);font-size:11px;letter-spacing:.06em;color:var(--text-3)}
.pager .next{text-align:right;grid-column:2}
.foot{margin-top:40px;font-size:13px;color:var(--text-3)}
/* 代码高亮：黑白灰为主 */
.hl .c,.hl .c1,.hl .cm,.hl .ch,.hl .cs,.hl .cp,.hl .cpf{color:#6b6b6b;font-style:italic}
.hl .k,.hl .kd,.hl .kn,.hl .kr,.hl .kt,.hl .kc,.hl .kp,.hl .ow{color:#ffffff;font-weight:600}
.hl .s,.hl .s1,.hl .s2,.hl .sb,.hl .sc,.hl .sd,.hl .se,.hl .sh,.hl .si,.hl .sx,.hl .sr,.hl .ss,.hl .dl,.hl .sa{color:#86efac}
.hl .m,.hl .mi,.hl .mf,.hl .mh,.hl .mo,.hl .il{color:#fcd34d}
.hl .nf,.hl .fm,.hl .nc,.hl .nn{color:#e5e5e5;font-weight:500}
.hl .nb,.hl .bp,.hl .nd,.hl .na,.hl .nt{color:#bdbdbd}
.hl .o,.hl .p{color:#9a9a9a}
.hl .gd{color:#fca5a5}.hl .gi{color:#86efac}
.hl{color:#d4d4d4}
@media (max-width:960px){.wrap{grid-template-columns:1fr;gap:0}.toc{display:none}article h1.title{font-size:30px}.nav__links a.hide-sm{display:none}}
"""

JS = r"""
document.querySelectorAll('article pre').forEach(function(pre){
  var b=document.createElement('button');b.className='code__copy';b.textContent='复制';
  b.onclick=function(){navigator.clipboard.writeText(pre.innerText).then(function(){b.textContent='已复制';setTimeout(function(){b.textContent='复制'},1200)})};
  pre.parentNode.insertBefore(b,pre);
});
var links=[].slice.call(document.querySelectorAll('.toc a')),map={};
links.forEach(function(a){map[decodeURIComponent(a.getAttribute('href').slice(1))]=a});
var io=new IntersectionObserver(function(es){es.forEach(function(e){if(e.isIntersecting&&map[e.target.id]){links.forEach(function(l){l.classList.remove('on')});map[e.target.id].classList.add('on')}})},{rootMargin:'-80px 0px -70% 0px'});
document.querySelectorAll('article h1[id],article h2[id]').forEach(function(h){io.observe(h)});
"""


def slugify(value: str, separator: str) -> str:
    value = re.sub(r"<[^>]+>", "", value)
    value = re.sub(r"[^\w\u4e00-\u9fff\- ]", "", value).strip().lower()
    return re.sub(r"[\s]+", separator, value) or "s"


def rewrite_links(body: str, src: str) -> str:
    """把课件里的相对链接改成 GitHub 地址，课件之间的链接改成在线文章地址。"""
    base = posixpath.dirname(src)
    md2slug = {p: s for p, s, *_ in LESSONS}

    def fix(m: re.Match) -> str:
        href = m.group(1)
        if re.match(r"^(https?:|mailto:|#)", href):
            return m.group(0)
        path, _, frag = href.partition("#")
        target = posixpath.normpath(posixpath.join(base, path))
        if target in md2slug:
            new = f"{md2slug[target]}.html" + (f"#{frag}" if frag else "")
        else:
            kind = "tree" if not posixpath.splitext(target)[1] else "blob"
            new = f"{REPO}/{kind}/main/{target}" + (f"#{frag}" if frag else "")
        return f'href="{new}"'

    return re.sub(r'href="([^"]+)"', fix, body)


def render(src: str) -> tuple[str, str, str]:
    text = (ROOT / src).read_text(encoding="utf-8")
    first, _, rest = text.partition("\n")
    title = first.lstrip("# ").strip()
    md = markdown.Markdown(
        extensions=["extra", "codehilite", "toc", "sane_lists"],
        extension_configs={
            "codehilite": {"css_class": "hl", "guess_lang": False},
            "toc": {"toc_depth": "1-2", "slugify": slugify},
        },
    )
    body = md.convert(rest)
    body = re.sub(r"<table>", '<div class="tbl"><table>', body)
    body = re.sub(r"</table>", "</table></div>", body)
    body = rewrite_links(body, src)
    return title, body, md.toc


def page(i: int) -> str:
    src, slug, num, short, _ = LESSONS[i]
    title, body, toc = render(src)
    toc = re.sub(r'<div class="toc">\s*', "", toc).rstrip()
    toc = re.sub(r"</div>$", "", toc)
    prev_html = next_html = ""
    if i > 0:
        p = LESSONS[i - 1]
        prev_html = f'<a class="prev" href="{p[1]}.html"><span>← 上一课 {p[2]}</span>{html.escape(p[3])}</a>'
    if i < len(LESSONS) - 1:
        n = LESSONS[i + 1]
        next_html = f'<a class="next" href="{n[1]}.html"><span>下一课 {n[2]} →</span>{html.escape(n[3])}</a>'
    desc = html.escape(title)
    return f"""<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>{desc} · AI Agent 全栈</title>
<meta name="description" content="{desc}。duang777 的 AI Agent 全栈公开学习课件。" />
<style>{CSS}</style>
</head>
<body>
<header class="nav">
  <a class="nav__brand" href="../index.html"><span class="nav__dot"></span>AI Agent 全栈</a>
  <nav class="nav__links">
    <a href="../index.html#progress">课件</a>
    <a class="hide-sm" href="{REPO}/blob/main/ROADMAP.md">路线图</a>
    <a href="{REPO}/blob/main/{src}">在 GitHub 上查看</a>
  </nav>
</header>
<div class="wrap">
  <aside class="toc"><div class="toc__label">本课目录</div>{toc}</aside>
  <article>
    <div class="meta">模块 00 · 基础功底 · 第 {num} 课</div>
    <h1 class="title">{html.escape(title)}</h1>
    {body}
    <nav class="pager">{prev_html}{next_html}</nav>
    <p class="foot">© 2026 duang777 · 课件源文件：<a href="{REPO}/blob/main/{src}">{src}</a></p>
  </article>
</div>
<script>{JS}</script>
</body>
</html>
"""


def main() -> None:
    OUT.mkdir(parents=True, exist_ok=True)
    for i, (_, slug, *_rest) in enumerate(LESSONS):
        (OUT / f"{slug}.html").write_text(page(i), encoding="utf-8")
        print("wrote", OUT / f"{slug}.html")


if __name__ == "__main__":
    main()
