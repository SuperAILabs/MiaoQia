"""链路探针：收到 operations 发起的探针后，以采集单元的身份发布 acquisition.probe.emitted.v1。"""
from __future__ import annotations

from sqlalchemy import Connection

from miaoqia.kernel import outbox, reqctx

from ..api import NAME

REQUESTED = "operations.probe.requested.v1"
EMITTED = "acquisition.probe.emitted.v1"

writer = outbox.Writer(subsystem=NAME, namespace=NAME, zone=reqctx.PUBLIC)


def on_requested(conn: Connection, env: dict) -> None:
    pid = env["data"]["probe_id"]
    writer.append(conn, outbox.Event(type=EMITTED, subject=pid, data={"probe_id": pid, "hops": ["operations", NAME]}))
