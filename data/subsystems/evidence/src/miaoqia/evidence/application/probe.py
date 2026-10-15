"""链路探针：收到采集单元的探针后，以数据单元的身份发布 evidence.probe.forwarded.v1。"""
from __future__ import annotations

from sqlalchemy import Connection

from miaoqia.kernel import outbox, reqctx

from ..api import NAME

EMITTED = "acquisition.probe.emitted.v1"
FORWARDED = "evidence.probe.forwarded.v1"

writer = outbox.Writer(subsystem=NAME, namespace=NAME, zone=reqctx.PUBLIC)


def on_emitted(conn: Connection, env: dict) -> None:
    d = env["data"]
    if env['source'] != 'miaoqia/acquisition' or env['subject'] != d['probe_id'] or d['hops'] != ['operations', 'acquisition']:
        raise ValueError('probe lineage invalid')
    writer.append(conn, outbox.Event(type=FORWARDED, subject=d["probe_id"],
                                     data={"probe_id": d["probe_id"], "hops": [*d["hops"], NAME]}))
