"""由子系统索引（docs/03-architecture/subsystems/README.md 第 1 节，唯一登记处）生成本地环境的数据库初始化脚本。

  python3 tools/dev/gen_local.py           生成 deploy/local/postgres/10-subsystems.sql
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
ROW = re.compile(r"^\| [^|]+ \| \[`([a-z-]+)`\][^|]*\|[^|]*\| (Go|Python) \| `([a-z]+)`[^|]*\| `([a-z]{3})` \|$")


def subsystems() -> list[tuple[str, str, str, str]]:
    rows = [m.groups() for line in INDEX.read_text(encoding="utf-8").splitlines() if (m := ROW.match(line))]
    if len(rows) < 17:
        sys.exit(f"子系统索引解析结果异常：{len(rows)} 行")
    return rows


def render() -> str:
    out = ["-- 由 tools/dev/gen_local.py 根据子系统索引生成，请勿手改。", "-- 仅用于本地开发环境：密码与账号名相同。", "",
           "CREATE ROLE outbox_relay LOGIN PASSWORD 'outbox_relay';", ""]
    for name, lang, unit, db in subsystems():
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


def main() -> int:
    if sys.argv[1:2] == ["topics"]:
        print("\n".join(topics()))
        return 0
    text = render()
    if "--check" in sys.argv:
        if not OUT.exists() or OUT.read_text(encoding="utf-8") != text:
            print(f"{OUT.relative_to(ROOT)} 与子系统索引不一致，请运行 python3 tools/dev/gen_local.py", file=sys.stderr)
            return 1
        return 0
    OUT.write_text(text, encoding="utf-8")
    print(f"已生成 {OUT.relative_to(ROOT)}（{len(subsystems())} 个子系统数据库）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
