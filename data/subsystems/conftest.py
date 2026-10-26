"""子系统集成测试使用真实 PostgreSQL：MIAOQIA_TEST_PG（超级用户连接串）；未设置时跳过。

每个夹具建一个独立数据库与 owner/app 两个账号（app 为 NOBYPASSRLS，只经默认权限获得表的读写），与 kernel 测试一致。
"""
from __future__ import annotations

import os
import random
import time
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit

import psycopg
import pytest

REPO = Path(__file__).resolve().parents[2]
CORPUS = REPO / "packs/information_domains/procurement/corpus"


def _dsn(admin: str, user: str, db: str) -> str:
    u = urlsplit(admin)
    return urlunsplit((u.scheme, f"{user}:pw@{u.hostname}:{u.port or 5432}", f"/{db}", u.query, ""))


def _create(admin: str):
    name = f"pst{int(time.time()) % 100000}_{random.randint(0, 99999):05d}"
    with psycopg.connect(admin, autocommit=True) as c:
        c.execute(f"CREATE ROLE {name}_owner LOGIN PASSWORD 'pw'")
        c.execute(f"CREATE ROLE {name}_app LOGIN PASSWORD 'pw' NOBYPASSRLS")
        c.execute(f"CREATE DATABASE {name} OWNER {name}_owner")
    with psycopg.connect(_dsn(admin, f"{name}_owner", name), autocommit=True) as c:
        c.execute(f"GRANT USAGE ON SCHEMA public TO {name}_app")
        c.execute(f"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO {name}_app")
        c.execute(f"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO {name}_app")
    return name, {"owner": _dsn(admin, f"{name}_owner", name), "app": _dsn(admin, f"{name}_app", name)}


def _drop(admin: str, name: str) -> None:
    with psycopg.connect(admin, autocommit=True) as c:
        c.execute(f"DROP DATABASE IF EXISTS {name} WITH (FORCE)")
        c.execute(f"DROP ROLE IF EXISTS {name}_owner")
        c.execute(f"DROP ROLE IF EXISTS {name}_app")


@pytest.fixture
def pg_factory():
    admin = os.environ.get("MIAOQIA_TEST_PG")
    if not admin:
        pytest.skip("未设置 MIAOQIA_TEST_PG")
    created = []

    def make():
        name, dsns = _create(admin)
        created.append(name)
        return dsns

    yield make
    for name in created:
        _drop(admin, name)
