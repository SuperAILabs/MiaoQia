"""集成测试使用真实 PostgreSQL 与 Kafka：MIAOQIA_TEST_PG（超级用户连接串）、MIAOQIA_TEST_KAFKA；未设置时跳过。"""
from __future__ import annotations

import os
import random
import time
import uuid
from urllib.parse import urlsplit, urlunsplit

import psycopg
import pytest
from confluent_kafka.admin import AdminClient, NewTopic


def _dsn(admin: str, user: str, db: str) -> str:
    u = urlsplit(admin)
    return urlunsplit((u.scheme, f"{user}:pw@{u.hostname}:{u.port or 5432}", f"/{db}", u.query, ""))


@pytest.fixture
def database():
    admin = os.environ.get("MIAOQIA_TEST_PG")
    if not admin:
        pytest.skip("未设置 MIAOQIA_TEST_PG")
    name = f"pkt{int(time.time()) % 100000}_{random.randint(0, 9999):04d}"
    with psycopg.connect(admin, autocommit=True) as c:
        c.execute(f"CREATE ROLE {name}_owner LOGIN PASSWORD 'pw'")
        c.execute(f"CREATE ROLE {name}_app LOGIN PASSWORD 'pw' NOBYPASSRLS")
        c.execute(f"CREATE DATABASE {name} OWNER {name}_owner")
    with psycopg.connect(_dsn(admin, f"{name}_owner", name), autocommit=True) as c:
        c.execute(f"GRANT USAGE ON SCHEMA public TO {name}_app")
        c.execute(f"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO {name}_app")
        c.execute(f"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO {name}_app")
    yield {"owner": _dsn(admin, f"{name}_owner", name), "app": _dsn(admin, f"{name}_app", name)}
    with psycopg.connect(admin, autocommit=True) as c:
        c.execute(f"DROP DATABASE IF EXISTS {name} WITH (FORCE)")
        c.execute(f"DROP ROLE IF EXISTS {name}_owner")
        c.execute(f"DROP ROLE IF EXISTS {name}_app")


@pytest.fixture
def kafka():
    brokers = os.environ.get("MIAOQIA_TEST_KAFKA")
    if not brokers:
        pytest.skip("未设置 MIAOQIA_TEST_KAFKA")
    suffix = uuid.uuid4().hex[:8]
    topics = {"public": f"public.pkt-{suffix}", "dlq": f"dlq.pkt-{suffix}"}
    adm = AdminClient({"bootstrap.servers": brokers})
    for f in adm.create_topics([NewTopic(t, num_partitions=3, replication_factor=1) for t in topics.values()]).values():
        f.result(30)
    yield {"brokers": brokers, **topics}
    adm.delete_topics(list(topics.values()))
