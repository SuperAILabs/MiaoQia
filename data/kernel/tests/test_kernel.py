from __future__ import annotations

import json
import threading
import time
from pathlib import Path

import pytest
from confluent_kafka import Consumer as KC
from confluent_kafka import Producer
from sqlalchemy import text

from miaoqia.kernel import consumer, db, outbox, reqctx

REPO = Path(__file__).resolve().parents[3]


def test_kernel_sql_matches_contract():
    assert db.kernel_sql() == (REPO / "contracts/storage/kernel.sql").read_text(encoding="utf-8")


def tenant(t: str) -> reqctx.Context:
    return reqctx.Context(principal=reqctx.Principal("usr_test", "user"), tenant=t, cell="01")


def test_row_level_security(database):
    db.migrate(database["owner"])
    import psycopg

    with psycopg.connect(database["owner"], autocommit=True) as c:
        c.execute("""CREATE TABLE notes (id text PRIMARY KEY, tenant_id text NOT NULL DEFAULT current_setting('app.tenant_id'), body text);
                     ALTER TABLE notes ENABLE ROW LEVEL SECURITY;
                     CREATE POLICY t ON notes USING (tenant_id = current_setting('app.tenant_id', true))
                       WITH CHECK (tenant_id = current_setting('app.tenant_id', true));""")
    d = db.Database(database["app"])
    for t, i in (("tnt_a", "n1"), ("tnt_b", "n2")):
        with reqctx.use(tenant(t)), d.tx() as conn:
            conn.execute(text("INSERT INTO notes (id, body) VALUES (:i, 'x')"), {"i": i})
    for t in ("tnt_a", "tnt_b"):
        with reqctx.use(tenant(t)), d.tx() as conn:
            assert conn.execute(text("SELECT count(*) FROM notes")).scalar() == 1
    with pytest.raises(Exception), reqctx.use(tenant("tnt_a")), d.tx() as conn:
        conn.execute(text("INSERT INTO notes (id, tenant_id, body) VALUES ('n3', 'tnt_b', 'x')"))
    with pytest.raises(reqctx.MissingContext), d.tx():
        pass
    d.close()


def test_envelope_rules():
    w = outbox.Writer(subsystem="kt", namespace="kt", zone=reqctx.PUBLIC)
    assert outbox.topic(reqctx.TENANT, "01", "detection") == "cell-01.detection"
    with pytest.raises(outbox.InvalidEnvelope):
        outbox.validate({"specversion": "1.0", "id": "e", "type": "t", "source": "s", "miaoqiazone": reqctx.TENANT,
                         "miaoqiatenant": "tnt_a"})
    with pytest.raises(outbox.InvalidEnvelope):
        consumer.check_envelope("public.kt", {"specversion": "1.0", "id": "e", "type": "t", "source": "s",
                                              "miaoqiazone": reqctx.TENANT, "miaoqiatenant": "tnt_a", "miaoqiacell": "01"})
    assert w.zone == reqctx.PUBLIC


def _relay_once(dsn_owner: str, brokers: str) -> int:
    """测试用的发件箱发布（生产环境由 Go 的 outbox-relay 执行）。"""
    import psycopg

    p = Producer({"bootstrap.servers": brokers})
    with psycopg.connect(dsn_owner) as c:
        rows = c.execute("SELECT seq, topic, partition_key, envelope::text FROM kernel_outbox WHERE published_at IS NULL ORDER BY seq").fetchall()
        for _, topic, key, env in rows:
            p.produce(topic, key=key, value=env)
        p.flush(10)
        c.execute("UPDATE kernel_outbox SET published_at = now() WHERE published_at IS NULL")
    return len(rows)


def _read_one(brokers: str, topic: str) -> dict:
    kc = KC({"bootstrap.servers": brokers, "group.id": f"reader-{time.time()}", "auto.offset.reset": "earliest"})
    kc.subscribe([topic])
    deadline = time.time() + 30
    try:
        while time.time() < deadline:
            m = kc.poll(0.5)
            if m is not None and not m.error():
                return {"value": json.loads(m.value()), "headers": dict(m.headers() or [])}
        raise AssertionError(f"读取 {topic} 超时")
    finally:
        kc.close()


def test_outbox_consumer_dedupe_and_dead_letter(database, kafka):
    db.migrate(database["owner"])
    d = db.Database(database["app"])
    w = outbox.Writer(subsystem="pkt", namespace="pkt", zone=reqctx.PUBLIC)
    with reqctx.use(reqctx.service("pkt")), d.tx() as conn:
        w.append(conn, outbox.Event(type="pkt.probe.emitted.v1", subject="prb_1", data={"fail": False}, topic=kafka["public"]))
        w.append(conn, outbox.Event(type="pkt.probe.emitted.v1", subject="prb_2", data={"fail": True}, topic=kafka["public"]))
    assert _relay_once(database["owner"], kafka["brokers"]) == 2

    seen: list[str] = []
    attempts = {"n": 0}

    def handle(conn, env):
        if env["data"]["fail"]:
            attempts["n"] += 1
            raise RuntimeError("模拟处理失败")
        assert reqctx.current().principal.id == "svc:pkt"
        seen.append(env["subject"])

    c = consumer.Consumer(name="pkt.probe", subsystem="pkt", brokers=kafka["brokers"], topics=[kafka["public"]],
                          handlers={"pkt.probe.emitted.v1": handle}, payload_schemas={"pkt.probe.emitted.v1": {"type":"object", "required":["fail"], "additionalProperties":False, "properties":{"fail":{"type":"boolean"}}}}, db=d, dead_letter=kafka["dlq"], max_attempts=3, backoff=0.01)
    th = threading.Thread(target=c.run, daemon=True)
    th.start()
    deadline = time.time() + 45
    while time.time() < deadline and not (seen and attempts["n"] == 3):
        time.sleep(0.1)
    assert seen == ["prb_1"] and attempts["n"] == 3
    dl = _read_one(kafka["brokers"], kafka["dlq"])
    assert dl["value"]["subject"] == "prb_2" and dl["headers"].get("dlq_error")

    # 重复投递同一事件不再处理；随后的新事件作为栅栏
    first = _read_one(kafka["brokers"], kafka["public"])["value"]
    p = Producer({"bootstrap.servers": kafka["brokers"]})
    p.produce(kafka["public"], value=json.dumps(first))
    p.flush(10)
    with reqctx.use(reqctx.service("pkt")), d.tx() as conn:
        w.append(conn, outbox.Event(type="pkt.probe.emitted.v1", subject="prb_3", data={"fail": False}, topic=kafka["public"]))
    _relay_once(database["owner"], kafka["brokers"])
    deadline = time.time() + 45
    while time.time() < deadline and len(seen) < 2:
        time.sleep(0.1)
    c.stop()
    th.join(10)
    assert seen == ["prb_1", "prb_3"], seen
    d.close()
