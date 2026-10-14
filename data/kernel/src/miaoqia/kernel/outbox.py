"""事务性发件箱：事件与业务写入同事务追加到 kernel_outbox，由 Go 的 outbox-relay 发布。信封与 Go 内核一致（CloudEvents 1.0）。"""
from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

from sqlalchemy import Connection, text

from . import ids, reqctx, tracing


class InvalidEnvelope(ValueError):
    pass


def validate(env: dict) -> None:
    """域与租户一致性：租户事件必须带租户与单元，公共域事件必须为空。"""
    zone = env.get("miaoqiazone")
    if zone == reqctx.PUBLIC and (env.get("miaoqiatenant") or env.get("miaoqiacell")):
        raise InvalidEnvelope(f"公共域事件 {env.get('id')} 不得携带租户或单元")
    if zone == reqctx.TENANT and not (env.get("miaoqiatenant") and env.get("miaoqiacell")):
        raise InvalidEnvelope(f"租户事件 {env.get('id')} 必须携带租户与单元")
    if zone not in (reqctx.PUBLIC, reqctx.TENANT, reqctx.PLATFORM):
        raise InvalidEnvelope(f"事件 {env.get('id')} 的 miaoqiazone 取值非法：{zone!r}")
    if env.get("specversion") != "1.0" or not all(env.get(k) for k in ("id", "type", "source")):
        raise InvalidEnvelope(f"事件 {env.get('id')} 缺少 CloudEvents 必填属性")


def topic(zone: str, cell: str, namespace: str) -> str:
    if zone == reqctx.TENANT:
        return f"cell-{cell}.{namespace}"
    if zone == reqctx.PLATFORM:
        return f"platform.{namespace}"
    return f"public.{namespace}"


@dataclass
class Event:
    type: str
    subject: str
    data: Any = field(default_factory=dict)
    partition_key: str = ""
    topic: str = ""


@dataclass(frozen=True)
class Writer:
    subsystem: str
    namespace: str
    zone: str = reqctx.PUBLIC

    def append(self, conn: Connection, ev: Event) -> dict:
        c = reqctx.current()
        env = {
            "specversion": "1.0", "id": ids.new("evt"), "source": f"miaoqia/{self.subsystem}", "type": ev.type,
            "time": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"), "subject": ev.subject,
            "datacontenttype": "application/json", "miaoqiazone": self.zone,
        }
        if self.zone != reqctx.PUBLIC:
            if c.tenant:
                env["miaoqiatenant"] = c.tenant
            if c.cell:
                env["miaoqiacell"] = c.cell
        if tp := tracing.traceparent():
            env["traceparent"] = tp
        env["data"] = ev.data
        validate(env)
        key = ev.partition_key or ev.subject
        if self.zone == reqctx.TENANT:
            key = f"{c.tenant}/{key}"
        conn.execute(
            text("INSERT INTO kernel_outbox (event_id, topic, partition_key, envelope) VALUES (:i, :t, :k, CAST(:e AS jsonb))"),
            {"i": env["id"], "t": ev.topic or topic(self.zone, c.cell, self.namespace), "k": key,
             "e": json.dumps(env, ensure_ascii=False)},
        )
        conn.execute(text("SELECT pg_notify('kernel_outbox', '')"))
        return env
