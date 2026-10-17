"""检查仓库内 Markdown 文档的相对链接与锚点。

用法：python3 tools/dev/check_doc_links.py [文件 ...]
不带参数时检查 git 跟踪的全部 .md 文件。外部链接（http、mailto）不检查。
锚点按 GitHub 规则生成：小写，去掉字母、数字、汉字、连字符、下划线和空格以外的字符，空格换成连字符；
同名标题依次加 -1、-2 后缀。
"""

from __future__ import annotations

import os
import re
import subprocess
import sys
from functools import lru_cache

LINK = re.compile(r"(?<!!)\[[^\]]*\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")
HEADING = re.compile(r"^(#{1,6})\s+(.*?)\s*#*\s*$")
FENCE = re.compile(r"^\s*(```|~~~)")


def strip_code(text: str) -> list[str]:
    """返回去掉代码块内容的行（行号保持不变），并去掉行内代码。"""
    lines, in_fence = [], False
    for line in text.splitlines():
        if FENCE.match(line):
            in_fence = not in_fence
            lines.append("")
            continue
        lines.append("" if in_fence else re.sub(r"`[^`]*`", "", line))
    return lines


def slug(title: str) -> str:
    title = re.sub(r"<[^>]+>", "", title)  # 内嵌 HTML
    title = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", title)  # 标题中的链接只取文字
    title = title.replace("`", "").strip().lower()
    title = re.sub(r"[^\w一-鿿\- ]", "", title)
    return title.replace(" ", "-")


@lru_cache(maxsize=None)
def anchors(path: str) -> frozenset[str]:
    seen: dict[str, int] = {}
    result = set()
    with open(path, encoding="utf-8") as f:
        for line in strip_code(f.read()):
            m = HEADING.match(line)
            if not m:
                continue
            base = slug(m.group(2))
            n = seen.get(base, 0)
            seen[base] = n + 1
            result.add(base if n == 0 else f"{base}-{n}")
    return frozenset(result)


def check(path: str) -> list[str]:
    problems = []
    with open(path, encoding="utf-8") as f:
        lines = strip_code(f.read())
    for lineno, line in enumerate(lines, 1):
        for m in LINK.finditer(line):
            url = m.group(1)
            if re.match(r"^[a-z][a-z0-9+.-]*:", url, re.I):  # http:、mailto: 等
                continue
            target, _, anchor = url.partition("#")
            target = target.split("?", 1)[0]
            resolved = os.path.normpath(os.path.join(os.path.dirname(path), target)) if target else path
            if not os.path.exists(resolved):
                problems.append(f"{path}:{lineno}: 目标不存在：{url}")
                continue
            if anchor and resolved.endswith(".md") and anchor not in anchors(resolved):
                problems.append(f"{path}:{lineno}: 锚点不存在：{url}")
    return problems


def main(argv: list[str]) -> int:
    files = argv or subprocess.check_output(["git", "ls-files", "*.md"], text=True).split()
    problems = [p for f in files if os.path.exists(f) for p in check(f)]
    for p in problems:
        print(p)
    print(f"已检查 {len(files)} 个文件，{len(problems)} 处问题")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
