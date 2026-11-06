"""mqc：契约检查与代码生成。

  mqc check                     Schema 方言、样例、事件契约、扩展包一致性、YAML 键名
  mqc generate [--check] [--lang go|python|typescript ...]
  mqc fixtures --export DIR     导出全部正例为 JSON，供生成代码的往返测试
  mqc compat --base REF [-v]    与基线比较的契约兼容性检查
"""
from __future__ import annotations

import argparse
import sys

from .compat import compat
from .events import check_events
from .fixtures import check_fixtures, export_valid
from .generate import TARGETS, generate
from .lint import lint_schemas, lint_yaml_keys
from .packs import check_packs
from .integration import check_integration
from .repo import Repo, find_root
from .report import Report


def _finish(r: Report) -> int:
    for n in r.notes:
        print(f"  {n}")
    if r.errors:
        print(f"\n{len(r.errors)} 个问题：", file=sys.stderr)
        for e in r.errors:
            print(f"  ✗ {e}", file=sys.stderr)
        return 1
    print("通过")
    return 0


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="mqc", description="妙洽契约检查与代码生成")
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("check", help="检查 contracts/ 与 packs/")
    g = sub.add_parser("generate", help="生成 Go、Python、TypeScript 类型")
    g.add_argument("--check", action="store_true", help="只检查生成物是否与契约一致")
    g.add_argument("--lang", action="append", choices=sorted(TARGETS), help="只处理指定语言（可重复）")
    cp = sub.add_parser("compat", help="与基线比较的契约兼容性检查")
    cp.add_argument("--base", required=True, help="基线 git ref，例如 origin/main")
    cp.add_argument("-v", "--verbose", action="store_true", help="列出全部变更（默认只列 breaking）")
    fx = sub.add_parser("fixtures", help="导出样例")
    fx.add_argument("--export", required=True, metavar="DIR")
    args = ap.parse_args(argv)

    repo = Repo(find_root())
    r = Report()
    if args.cmd == "check":
        yamls = sorted(list((repo.root / "contracts").rglob("*.yaml")) + list((repo.root / "packs").rglob("*.yaml")))
        stages = [("Schema 方言", lambda: lint_schemas(repo)), ("样例", lambda: check_fixtures(repo)),
                  ("事件契约", lambda: check_events(repo)), ("扩展包", lambda: check_packs(repo)),
                  ("工程横切", lambda: check_integration(repo)),
                  ("YAML 键名", lambda: lint_yaml_keys(yamls, repo.root))]
        for name, stage in stages:
            try:
                r.extend(stage())
            except Exception as e:  # noqa: BLE001  一个阶段出错不影响其他阶段报告
                r.error(name, f"检查中断：{type(e).__name__}: {str(e)[:200]}")
    elif args.cmd == "compat":
        r.extend(compat(repo, args.base, args.verbose))
    elif args.cmd == "fixtures":
        from pathlib import Path
        r.note(f"已导出 {export_valid(repo, Path(args.export).resolve())} 个正例到 {args.export}")
    else:
        r.extend(generate(repo, args.check, args.lang or sorted(TARGETS)))
    return _finish(r)


if __name__ == "__main__":
    raise SystemExit(main())
