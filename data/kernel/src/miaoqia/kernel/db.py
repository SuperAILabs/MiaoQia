"""子系统数据访问：独立连接池与数据库账号，事务内按请求上下文设置行级安全会话变量（与 Go 内核一致）。"""
from __future__ import annotations

import contextlib
import time
from importlib import resources
from typing import Iterator

from sqlalchemy import Connection, create_engine, text
from sqlalchemy.engine import Engine

from . import reqctx


def kernel_sql() -> str:
    """内核表定义（与 contracts/storage/kernel.sql 一致，测试校验）。"""
    return resources.files(__package__).joinpath("kernel.sql").read_text(encoding="utf-8")


def _url(dsn: str) -> str:
    return dsn.replace("postgres://", "postgresql+psycopg://", 1).replace("postgresql://", "postgresql+psycopg://", 1)


class Database:
    def __init__(self, dsn: str, pool_size: int = 5, *, pool_timeout: float = 5.0):
        if type(pool_timeout) not in (int, float) or not 0 < pool_timeout <= 30:
            raise ValueError("invalid pool timeout")
        self.pool_timeout = float(pool_timeout)
        self.engine: Engine = create_engine(_url(dsn), pool_size=pool_size,
                                             pool_timeout=pool_timeout, pool_pre_ping=True)

    def close(self) -> None:
        self.engine.dispose()

    @contextlib.contextmanager
    def tx(self, *, deadline: float | None = None, statement_cap_ms: int = 5000) -> Iterator[Connection]:
        """事务：开始时把请求上下文写入事务级会话变量；没有请求上下文时拒绝执行。"""
        c = reqctx.current()
        c.validate()
        if deadline is not None:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("request deadline expired")
            # SQLAlchemy's QueuePool timeout is configured at engine creation;
            # fail before checkout when the request cannot afford that wait.
            if remaining < self.pool_timeout:
                raise TimeoutError("request deadline shorter than pool checkout bound")
        with self.engine.begin() as conn:
            if deadline is not None:
                remaining_ms = max(1, min(statement_cap_ms, int((deadline - time.monotonic()) * 1000)))
                conn.execute(text("SET LOCAL statement_timeout = :ms"), {"ms": f"{remaining_ms}ms"})
                conn.execute(text("SET LOCAL idle_in_transaction_session_timeout = :ms"), {"ms": f"{remaining_ms}ms"})
                conn.execute(text("SET TRANSACTION READ ONLY"))
            conn.execute(
                text("SELECT set_config('app.tenant_id', :t, true), set_config('app.cell', :c, true), "
                     "set_config('app.principal', :p, true)"),
                {"t": c.tenant, "c": c.cell, "p": c.principal.id},
            )
            yield conn


def migrate(owner_dsn: str, script_location: str | None = None) -> None:
    """以 owner 账号创建内核表，再执行子系统的 Alembic 迁移（script_location 为迁移目录，env.py 调用 run_alembic_env）。"""
    eng = create_engine(_url(owner_dsn))
    try:
        with eng.begin() as conn:
            conn.exec_driver_sql(kernel_sql())
    finally:
        eng.dispose()
    if script_location:
        from alembic import command
        from alembic.config import Config

        cfg = Config()
        cfg.set_main_option("script_location", script_location)
        cfg.set_main_option("sqlalchemy.url", _url(owner_dsn).replace("%", "%%"))
        command.upgrade(cfg, "head")


def run_alembic_env() -> None:
    """子系统迁移目录的 env.py 只需调用本函数（在线迁移，owner 账号）。"""
    from alembic import context

    eng = create_engine(context.config.get_main_option("sqlalchemy.url"))
    with eng.connect() as conn:
        context.configure(connection=conn, version_table="alembic_version")
        with context.begin_transaction():
            context.run_migrations()
    eng.dispose()
