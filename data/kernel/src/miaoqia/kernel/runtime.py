"""部署单元的进程角色组装（与 services/kernel/runtime 对等）：每个子系统提供一个 Module，进程角色按部署清单加载。

配置（环境变量）：MIAOQIA_DEPLOY_DIR（默认 deploy）、MIAOQIA_KAFKA_BROKERS、MIAOQIA_<子系统>_DB_DSN、
MIAOQIA_<子系统>_DB_OWNER_DSN（迁移）、MIAOQIA_<子系统>_<KEY>、OTEL_EXPORTER_OTLP_ENDPOINT。
"""
from __future__ import annotations

import argparse
import logging
import os
import queue
import signal
import sys
import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Callable

import yaml

from . import db, tracing
from .consumer import Consumer
from .scopes import instance_scope, role_scopes

log = logging.getLogger("miaoqia.runtime")


def env_key(sub: str, key: str) -> str:
    return f"MIAOQIA_{sub.upper().replace('-', '_')}_{key}"


@dataclass
class Env:
    role: str
    brokers: str
    db: db.Database | None
    subsystem: str
    data_scope: str = "public"
    cell: str = ""

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


def prepare_role(unit: str, role: str, modules: list[Module]):
    roles = yaml.safe_load((_deploy_dir() / unit / "roles.yaml").read_text())["roles"]
    if role not in roles:
        raise ValueError(f"未知角色 {role!r}")
    spec = roles[role]
    registry = yaml.safe_load((_deploy_dir() / "subsystems.yaml").read_text())["subsystems"]
    scopes = role_scopes(spec, registry, unit=unit, available={m.name for m in modules})
    return spec, scopes


def migrate(unit: str, role: str, modules: list[Module]) -> None:
    spec, scopes = prepare_role(unit, role, modules)
    for m in modules:
        if m.name not in scopes or not m.database:
            continue
        dsn = os.environ.get(env_key(m.name, "DB_OWNER_DSN"))
        if not dsn:
            raise SystemExit(f"未配置 {env_key(m.name, 'DB_OWNER_DSN')}")
        db.migrate(dsn, m.migrations)
        log.info("迁移完成 subsystem=%s scope=%s cell=%s", m.name, scopes[m.name], spec.get("cell", ""))


def run(unit: str, role: str, modules: list[Module]) -> None:
    spec, scopes = prepare_role(unit, role, modules)
    want = set(scopes)
    tracing.setup(unit, role)
    brokers = os.environ.get("MIAOQIA_KAFKA_BROKERS", "")
    consumers: list[Consumer] = []
    for m in modules:
        if m.name not in want:
            continue
        d = db.Database(os.environ[env_key(m.name, "DB_DSN")]) if m.database and m.consumers else None
        env = Env(role=role, brokers=brokers, db=d, subsystem=m.name, data_scope=scopes[m.name], cell=spec.get("cell", ""))
        if m.consumers:
            consumers += m.consumers(env)
    log.info("进程角色启动 unit=%s role=%s subsystems=%s consumers=%d", unit, role, sorted(want), len(consumers))
    stop = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: stop.set())
    signal.signal(signal.SIGINT, lambda *_: stop.set())
    failures: queue.SimpleQueue[tuple[str, Exception]] = queue.SimpleQueue()

    def consume(c: Consumer) -> None:
        try:
            c.run()
            if not stop.is_set():
                raise RuntimeError("消费者意外退出")
        except Exception as e:  # noqa: BLE001 — 将线程失败交给主线程，结束进程以便外部重启
            failures.put((c.name, e))
            stop.set()

    threads = [threading.Thread(target=consume, args=(c,), name=c.name, daemon=True) for c in consumers]
    try:
        for t in threads:
            t.start()
        stop.wait()
    finally:
        for c in consumers:
            c.stop()
        for t in threads:
            if t.ident is not None:
                t.join(10)
        tracing.shutdown()
    if not failures.empty():
        name, error = failures.get_nowait()
        raise RuntimeError(f"消费者 {name} 失败") from error


def main(unit: str, modules: list[Module]) -> None:
    logging.basicConfig(level=logging.INFO, format='{"level":"%(levelname)s","logger":"%(name)s","msg":"%(message)s"}')
    p = argparse.ArgumentParser(prog=f"mq-{unit}")
    p.add_argument("command", nargs="?", choices=["migrate"])
    p.add_argument("--role")
    a = p.parse_args()
    if a.command == "migrate":
        migrate(unit, a.role or {"acquisition": "acq-admin", "data": "data-api"}[unit], modules)
    elif a.role:
        run(unit, a.role, modules)
    else:
        p.print_usage()
        sys.exit(2)
