"""由子系统索引（docs/03-architecture/subsystems/README.md 第 1 节，唯一登记处）生成部署使用的机器可读清单与本地环境的数据库初始化脚本。

  python3 tools/dev/gen_local.py           生成 deploy/subsystems.yaml 与 deploy/local/postgres/10-subsystems.sql
  python3 tools/dev/gen_local.py --check   生成物与索引不一致时退出码为 1（CI 使用）
  python3 tools/dev/gen_local.py topics    列出本地需要创建的 Kafka 主题：contracts/events 中声明的通道、
                                           审计主题 platform.audit，以及每个子系统的死信主题 dlq.<子系统>

每个子系统数据库两个角色：<db>_owner 拥有库并执行迁移；<db>_app 是应用连接账号（非超级用户、无 BYPASSRLS，
行级安全对其生效），由默认权限获得 owner 所建表的读写权限。outbox_relay 只被授予各库发件箱表的读与标记权限
（由 kernel 迁移授予，见 contracts/storage/kernel.sql）。
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
INDEX = ROOT / "docs/03-architecture/subsystems/README.md"
OUT = ROOT / "deploy/local/postgres/10-subsystems.sql"
REGISTRY = ROOT / "deploy/subsystems.yaml"
ROLES = {u: ROOT / f"deploy/{u}/roles.yaml" for u in ("acquisition", "data", "platform")}
ROW = re.compile(r"^\| ([^|]+?) \| \[`([a-z-]+)`\][^|]*\|[^|]*\| (Go|Python) \| `([a-z]+)`[^|]*\| (?:`([a-z]{3})`|（[^）]+）) \|$")


PLANES = {"数据供应": "data_supply", "客户智能": "customer_intelligence", "交互": "interaction", "平台": "platform"}


def rows() -> list[tuple[str, str, str, str, str | None]]:
    """(平面, 子系统, 语言, 部署单元, 数据库代码或 None)。"""
    return [(PLANES[m.group(1)], *m.groups()[1:]) for line in INDEX.read_text(encoding="utf-8").splitlines() if (m := ROW.match(line))]


def subsystems() -> list[tuple[str, str, str, str | None]]:
    """(子系统, 语言, 部署单元, 数据库代码或 None)。"""
    out = [r[1:] for r in rows()]
    if len(out) != 18:
        sys.exit(f"子系统索引解析结果异常：{len(out)} 行")
    return out


def render() -> str:
    out = ["-- 由 tools/dev/gen_local.py 根据子系统索引生成，请勿手改。", "-- 仅用于本地开发环境：密码与账号名相同。", "",
           "CREATE ROLE outbox_relay LOGIN PASSWORD 'outbox_relay';", ""]
    for name, lang, unit, db in subsystems():
        if db is None:
            continue
        out += [
            f"-- {name}（{lang}，{unit}）",
            f"CREATE ROLE {db}_owner LOGIN PASSWORD '{db}_owner';",
            f"CREATE ROLE {db}_app LOGIN PASSWORD '{db}_app' NOBYPASSRLS;",
            f"CREATE DATABASE {db} OWNER {db}_owner;",
            f"REVOKE ALL ON DATABASE {db} FROM PUBLIC;",
            f"GRANT CONNECT ON DATABASE {db} TO {db}_app, outbox_relay;",
            f"\\connect {db}",
            f"ALTER SCHEMA public OWNER TO {db}_owner;",
            f"GRANT USAGE ON SCHEMA public TO {db}_app;",
            f"ALTER DEFAULT PRIVILEGES FOR ROLE {db}_owner IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO {db}_app;",
            f"ALTER DEFAULT PRIVILEGES FOR ROLE {db}_owner IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO {db}_app;",
            "\\connect postgres",
            "",
        ]
    return "\n".join(out)


def topics() -> list[str]:
    import yaml

    out = {"platform.audit"}
    for f in sorted((ROOT / "contracts/events").glob("*/*.asyncapi.yaml")):
        doc = yaml.safe_load(f.read_text(encoding="utf-8"))
        out |= {c.get("address", k) for k, c in (doc.get("channels") or {}).items()}
    out |= {f"dlq.{name}" for name, *_ in subsystems()}
    return sorted(out)


def render_registry() -> str:
    out = ["# 由 tools/dev/gen_local.py 根据子系统索引（docs/03-architecture/subsystems/README.md）生成，请勿手改。", "subsystems:"]
    for plane, name, lang, unit, db in rows():
        out.append(f"  {name}: {{ plane: {plane}, language: {lang.lower()}, unit: {unit}" + (f", database: {db} }}" if db else " }"))
    return "\n".join(out) + "\n"


def check_roles() -> list[str]:
    import yaml

    units = {name: unit for name, _, unit, _ in subsystems()}
    errs = []
    for unit, path in ROLES.items():
        doc = yaml.safe_load(path.read_text(encoding="utf-8"))
        for role, spec in doc["roles"].items():
            for sub in spec.get("subsystems", []):
                if units.get(sub) != unit:
                    errs.append(f"{path.relative_to(ROOT)}：角色 {role} 中的 {sub} 不属于 {unit} 单元")
    return errs


def main() -> int:
    if sys.argv[1:2] == ["topics"]:
        print("\n".join(topics()))
        return 0
    outputs = {OUT: render(), REGISTRY: render_registry()}
    if "--check" in sys.argv:
        bad = [p for p, t in outputs.items() if not p.exists() or p.read_text(encoding="utf-8") != t]
        for p in bad:
            print(f"{p.relative_to(ROOT)} 与子系统索引不一致，请运行 python3 tools/dev/gen_local.py", file=sys.stderr)
        errs = check_roles()
        for e in errs:
            print(e, file=sys.stderr)
        return 1 if bad or errs else 0
    for p, t in outputs.items():
        p.write_text(t, encoding="utf-8")
    print(f"已生成 {OUT.relative_to(ROOT)}（{sum(1 for *_, d in subsystems() if d)} 个子系统数据库）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
