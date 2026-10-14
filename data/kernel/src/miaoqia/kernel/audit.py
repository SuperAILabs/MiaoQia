"""审计 SDK：审计条目与业务写入同事务进入本子系统发件箱，经中继投递到 platform.audit（与 Go 内核一致）。"""
from __future__ import annotations

from datetime import datetime, timezone

from sqlalchemy import Connection

from . import outbox, reqctx

EVENT_TYPE = "audit.entry.recorded.v1"
TOPIC = "platform.audit"


def record(conn: Connection, subsystem: str, action: str, resource: str, result: str, basis: str = "") -> None:
    c = reqctx.current()
    data = {"actor": c.principal.id, "actor_kind": c.principal.kind, "subsystem": subsystem, "action": action,
            "resource": resource, "result": result,
            "occurred_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")}
    if c.tenant:
        data["tenant"] = c.tenant
    if basis:
        data["basis"] = basis
    outbox.Writer(subsystem=subsystem, namespace="audit", zone=reqctx.PLATFORM).append(
        conn, outbox.Event(type=EVENT_TYPE, subject=resource, data=data, topic=TOPIC,
                           partition_key=f"{c.tenant}/{resource}"))
