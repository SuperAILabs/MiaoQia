"""部署单元的进程角色组装（与 services/kernel/runtime 对等）：每个子系统提供一个 Module，进程角色按部署清单加载。

配置（环境变量）：MIAOQIA_DEPLOY_DIR（默认 deploy）、MIAOQIA_KAFKA_BROKERS、MIAOQIA_<子系统>_DB_DSN、
MIAOQIA_<子系统>_DB_OWNER_DSN（迁移）、MIAOQIA_<子系统>_<KEY>、OTEL_EXPORTER_OTLP_ENDPOINT。
"""
from __future__ import annotations

import argparse
import logging
import os
import signal
import sys
import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Callable

import yaml

from . import db, tracing
from .consumer import Consumer

log = logging.getLogger("miaoqia.runtime")


def env_key(sub: str, key: str) -> str:
    return f"MIAOQIA_{sub.upper().replace('-', '_')}_{key}"


@dataclass
class Env:
    role: str
    brokers: str
    db: db.Database | None
    subsystem: str

    def config(self, key: str) -> str:
        return os.environ.get(env_key(self.subsystem, key), "")


@dataclass
class Module:
    name: str
    database: str = ""
    migrations: str | None = None                    # Alembic 迁移目录
    consumers: Callable[[Env], list[Consumer]] | None = None
    extra: dict = field(default_factory=dict)


def _deploy_dir() -> Path:
    return Path(os.environ.get("MIAOQIA_DEPLOY_DIR", "deploy"))


def migrate(modules: list[Module]) -> None:
    for m in modules:
        if not m.database:
            continue
        dsn = os.environ.get(env_key(m.name, "DB_OWNER_DSN"))
        if not dsn:
            raise SystemExit(f"未配置 {env_key(m.name, 'DB_OWNER_DSN')}")
        db.migrate(dsn, m.migrations)
        log.info("迁移完成 subsystem=%s database=%s", m.name, m.database)


def run(unit: str, role: str, modules: list[Module]) -> None:
    roles = yaml.safe_load((_deploy_dir() / unit / "roles.yaml").read_text(encoding="utf-8"))["roles"]
    if role not in roles:
        raise SystemExit(f"未知角色 {role!r}")
    want = set(roles[role].get("subsystems", []))
    tracing.setup(unit, role)
    brokers = os.environ.get("MIAOQIA_KAFKA_BROKERS", "")
    consumers: list[Consumer] = []
    for m in modules:
        if m.name not in want:
            continue
        d = db.Database(os.environ[env_key(m.name, "DB_DSN")]) if m.database and m.consumers else None
        env = Env(role=role, brokers=brokers, db=d, subsystem=m.name)
        if m.consumers:
            consumers += m.consumers(env)
    log.info("进程角色启动 unit=%s role=%s subsystems=%s consumers=%d", unit, role, sorted(want), len(consumers))
    stop = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: stop.set())
    signal.signal(signal.SIGINT, lambda *_: stop.set())
    threads = [threading.Thread(target=c.run, name=c.name, daemon=True) for c in consumers]
    for t in threads:
        t.start()
    stop.wait()
    for c in consumers:
        c.stop()
    for t in threads:
        t.join(10)
    tracing.shutdown()


def main(unit: str, modules: list[Module]) -> None:
    logging.basicConfig(level=logging.INFO, format='{"level":"%(levelname)s","logger":"%(name)s","msg":"%(message)s"}')
    p = argparse.ArgumentParser(prog=f"mq-{unit}")
    p.add_argument("command", nargs="?", choices=["migrate"])
    p.add_argument("--role")
    a = p.parse_args()
    if a.command == "migrate":
        migrate(modules)
    elif a.role:
        run(unit, a.role, modules)
    else:
        p.print_usage()
        sys.exit(2)
