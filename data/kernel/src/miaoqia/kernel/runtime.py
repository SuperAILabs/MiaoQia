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

from . import db, scheduler, tracing
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


@dataclass(frozen=True)
class Worker:
    """A long-lived process task that returns after its stop event is set."""

    name: str
    run: Callable[[threading.Event], None]


@dataclass
class Module:
    name: str
    database: str = ""
    migrations: str | None = None                    # Alembic 迁移目录
    consumers: Callable[[Env], list[Consumer]] | None = None
    # Periodic jobs (kernel.scheduler); run only in roles whose roles.yaml entry sets jobs: true.
    jobs: Callable[[Env], list[scheduler.Job]] | None = None
    extra: dict = field(default_factory=dict)
    workers: Callable[[Env], list[Worker]] | None = None


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
    jobs: list[scheduler.Job] = []
    workers: list[Worker] = []
    run_consumers = spec.get("consumers") is not False
    run_jobs = spec.get("jobs") is True
    run_workers = spec.get("workers") is True
    for module in modules:
        if module.name not in want:
            continue
        consumer_factory = module.consumers if run_consumers else None
        job_factory = module.jobs if run_jobs else None
        worker_factory = module.workers if run_workers else None
        if not (consumer_factory or job_factory or worker_factory):
            continue
        database = db.Database(os.environ[env_key(module.name, "DB_DSN")]) if module.database else None
        env = Env(role=role, brokers=brokers, db=database, subsystem=module.name, data_scope=scopes[module.name],
                  cell=spec.get("cell", ""))
        if consumer_factory:
            consumers.extend(consumer_factory(env))
        if job_factory:
            jobs.extend(job_factory(env))
        if worker_factory:
            workers.extend(worker_factory(env))
    runner = scheduler.Runner(jobs) if jobs else None
    log.info("进程角色启动 unit=%s role=%s subsystems=%s consumers=%d jobs=%d workers=%d",
             unit, role, sorted(want), len(consumers), len(jobs), len(workers))
    stop = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: stop.set())
    signal.signal(signal.SIGINT, lambda *_: stop.set())
    failures: queue.SimpleQueue[tuple[str, str, Exception]] = queue.SimpleQueue()

    def supervise(label: str, name: str, task: Callable, *args) -> None:
        try:
            task(*args)
            if not stop.is_set():
                raise RuntimeError(f"{label}意外退出")
        except Exception as error:
            failures.put((label, name, error))
            stop.set()

    threads = [threading.Thread(target=supervise, args=("消费者", consumer.name, consumer.run),
                                name=consumer.name, daemon=True) for consumer in consumers]
    threads.extend(threading.Thread(target=supervise, args=("工作线程", worker.name, worker.run, stop),
                                    name=worker.name, daemon=True) for worker in workers)
    if runner is not None:
        threads.append(threading.Thread(target=runner.run, args=(stop,), name="scheduler", daemon=True))
    try:
        for t in threads:
            t.start()
        stop.wait()
    finally:
        stop.set()
        for c in consumers:
            c.stop()
        for t in threads:
            if t.ident is not None:
                t.join(10)
        tracing.shutdown()
    if not failures.empty():
        label, name, error = failures.get_nowait()
        raise RuntimeError(f"{label} {name} 失败") from error


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
