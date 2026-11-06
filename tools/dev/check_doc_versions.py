"""检查 Markdown 文档不复述版本号（文档体系第5节“引用不带版本与状态值”）。

用法：python3 tools/dev/check_doc_versions.py [文件 ...]
      python3 tools/dev/check_doc_versions.py --self-test
不带参数时检查 git 跟踪的全部 .md 文件。规则：
  V1 元信息行（以“> 状态：”开头）不写版本号与更新日期，变更历史由版本库追溯；
  V2 正文不出现 x.y.z 版本号字面量：引用契约、扩展包或文档只给链接，版本以被引用处为准。
     代码块、行内代码、链接地址、IP 地址不检查；第三方组件的版本（如“Kafka 4.1.0”）是部署事实，
     组件名紧邻版本号时允许。
"""

from __future__ import annotations

import os
import re
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from check_doc_links import strip_code  # noqa: E402

META = re.compile(r"^> 状态：.*(版本|更新)：")
SEMVER = re.compile(r"(?<![\d.])v?\d+\.\d+\.\d+(?![\d.])")
URL = re.compile(r"\]\([^)\s]+\)|https?://\S+")
IPV4 = re.compile(r"\d+\.\d+\.\d+\.\d+(/\d+)?")
THIRD_PARTY = ("Keycloak", "OpenFGA", "Kafka", "PostgreSQL", "Temporal", "OpenSearch", "Valkey",
               "OpenBao", "Jev", "CloudEvents", "Go", "Python", "Node", "uv", "pnpm")
ALLOWED = re.compile(r"(?:%s)\s?$" % "|".join(map(re.escape, THIRD_PARTY)))


def check_text(text: str) -> list[tuple[int, str]]:
    problems = []
    raw = text.splitlines()
    for n, line in enumerate(strip_code(text), 1):
        if META.match(raw[n - 1]):
            problems.append((n, "V1 元信息行不写版本号与更新日期"))
            continue
        body = IPV4.sub("", URL.sub("", line))
        for m in SEMVER.finditer(body):
            if not ALLOWED.search(body[:m.start()]):
                problems.append((n, f"V2 版本号字面量 {m.group()}：改为链接到被引用的契约或文档"))
    return problems


def tracked_markdown() -> list[str]:
    out = subprocess.run(["git", "ls-files", "*.md"], capture_output=True, text=True, check=True).stdout
    return [p for p in out.splitlines() if p]


def self_test() -> None:
    sample = "\n".join([
        "> 状态：评审中 · 版本：1.0 · 更新：2026-10-12 · 负责人：x",
        "> 状态：评审中 · 负责人：x",
        "identity 契约 1.5.0 实现审批",
        "真实 Kafka 4.1.0 与 OpenFGA 1.10.0",
        "地址 `10.89.14.250` 与 10.89.14.0/24",
        "见 [规范](https://example.com/spec/v1.0.2/x.md)",
        "`proc.tender-notice@0.3.0` 在行内代码中",
        "```", "version: 2.1.0", "```",
    ])
    got = check_text(sample)
    assert [n for n, _ in got] == [1, 3], got
    print("自测通过")


def main(argv: list[str]) -> int:
    if argv == ["--self-test"]:
        self_test()
        return 0
    files = argv or tracked_markdown()
    failed = 0
    for path in files:
        with open(path, encoding="utf-8") as f:
            for n, msg in check_text(f.read()):
                print(f"{path}:{n}: {msg}")
                failed += 1
    if failed:
        print(f"\n{failed} 个问题", file=sys.stderr)
        return 1
    print("通过")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
