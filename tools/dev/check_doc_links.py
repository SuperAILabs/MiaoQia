"""检查仓库内 Markdown 文档的相对链接与锚点。

用法：python3 tools/dev/check_doc_links.py [文件 ...]
      python3 tools/dev/check_doc_links.py --self-test
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


def strip_code(text: str, keep_inline: bool = False) -> list[str]:
    """返回去掉代码块内容的行（行号保持不变）。

    扫描链接时连同行内代码一起去掉，避免把代码示例当作链接；生成标题锚点时传 keep_inline=True，
    保留行内代码的文字（GitHub 的锚点包含这些文字，只去掉反引号）。
    """
    lines, in_fence = [], False
    for line in text.splitlines():
        if FENCE.match(line):
            in_fence = not in_fence
            lines.append("")
            continue
        if in_fence:
            lines.append("")
        else:
            lines.append(line if keep_inline else re.sub(r"`[^`]*`", "", line))
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
        for line in strip_code(f.read(), keep_inline=True):
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


def self_test() -> int:
    """回归用例：标题含行内代码的合法锚点通过；不存在的锚点和目标被拒绝；代码中的链接不检查。"""
    import tempfile

    cases = [
        ("# Use `foo`\n\n[ok](#use-foo)\n", 0),
        ("# 2. 时间模型\n\n[ok](#2-时间模型)\n", 0),
        ("# A\n# A\n\n[ok](#a-1)\n", 0),
        ("# Use `foo`\n\n[bad](#use)\n", 1),
        ("# A\n\n[bad](#missing)\n", 1),
        ("[bad](missing.md)\n", 1),
        ("```\n[ignored](missing.md)\n```\n`[ignored](missing.md)`\n", 0),
    ]
    failed = 0
    with tempfile.TemporaryDirectory() as d:
        for i, (text, expected) in enumerate(cases):
            path = os.path.join(d, f"case{i}.md")
            with open(path, "w", encoding="utf-8") as f:
                f.write(text)
            anchors.cache_clear()
            got = len(check(path))
            if got != expected:
                failed += 1
                print(f"自测用例 {i} 失败：期望 {expected} 处问题，实际 {got}：{text!r}")
    print(f"自测 {len(cases)} 个用例，{failed} 个失败")
    return 1 if failed else 0


def main(argv: list[str]) -> int:
    if argv == ["--self-test"]:
        return self_test()
    files = argv or subprocess.check_output(["git", "ls-files", "*.md"], text=True).split()
    problems = [p for f in files if os.path.exists(f) for p in check(f)]
    for p in problems:
        print(p)
    print(f"已检查 {len(files)} 个文件，{len(problems)} 处问题")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
