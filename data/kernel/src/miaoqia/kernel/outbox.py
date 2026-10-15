"""事务性发件箱：事件与业务写入同事务追加到 kernel_outbox，由 Go 的 outbox-relay 发布。信封与 Go 内核一致（CloudEvents 1.0）。"""
from __future__ import annotations

import json
import re
from urllib.parse import urlsplit
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

from sqlalchemy import Connection, text

from . import ids, reqctx, tracing


class InvalidEnvelope(ValueError):
    pass


def validate(env: Any) -> None:
    """域与租户一致性：租户事件必须带租户与单元，公共域事件必须为空。"""
    if not isinstance(env, dict):
        raise InvalidEnvelope("事件信封必须是 JSON 对象")
    allowed = {"specversion", "id", "type", "source", "time", "subject", "datacontenttype", "dataschema",
               "miaoqiazone", "miaoqiatenant", "miaoqiacell", "traceparent", "data"}
    if set(env) - allowed or any(value is None for value in env.values()):
        raise InvalidEnvelope("未知信封字段或null值")
    for key in ("specversion", "id", "type", "source"):
        if not isinstance(env.get(key), str) or not env[key]:
            raise InvalidEnvelope(f"事件信封属性 {key} 必须是非空字符串")
    for key in ("subject", "miaoqiatenant", "miaoqiacell", "traceparent", "dataschema"):
        if key in env and (not isinstance(env[key], str) or not env[key]):
            raise InvalidEnvelope(f"事件信封属性 {key} 必须是字符串")
    zone = env.get("miaoqiazone")
    if zone == reqctx.PUBLIC and ("miaoqiatenant" in env or "miaoqiacell" in env):
        raise InvalidEnvelope(f"公共域事件 {env.get('id')} 不得携带租户或单元")
    if zone == reqctx.TENANT and not (env.get("miaoqiatenant") and env.get("miaoqiacell")):
        raise InvalidEnvelope(f"租户事件 {env.get('id')} 必须携带租户与单元")
    if zone not in (reqctx.PUBLIC, reqctx.TENANT, reqctx.PLATFORM):
        raise InvalidEnvelope(f"事件 {env.get('id')} 的 miaoqiazone 取值非法：{zone!r}")
    if env["specversion"] != "1.0":
        raise InvalidEnvelope(f"事件 {env['id']} 的 specversion 必须是 1.0")
    patterns = {"id": r"evt_[0-9A-HJKMNP-TV-Z]{26}", "type": r"[a-z_]+\.[a-z_]+\.[a-z_]+\.v[1-9][0-9]*",
                "source": r"miaoqia/[a-z][a-z0-9_-]*", "miaoqiacell": r"[a-z0-9][a-z0-9-]{0,62}",
                "traceparent": r"00-[0-9a-f]{32}-[0-9a-f]{16}-0[01]"}
    for key, pattern in patterns.items():
        if key in env and not re.fullmatch(pattern, env[key]):
            raise InvalidEnvelope(f"信封{key}格式无效")
    if "miaoqiacell" in env and not env.get("miaoqiatenant"):
        raise InvalidEnvelope("单元必须带租户")
    if schema := env.get("dataschema"):
        try:
            if any(c.isspace() or ord(c)>127 for c in schema) or re.search(r"%(?![0-9a-fA-F]{2})", schema):
                raise ValueError("invalid URI")
            urlsplit(schema)
        except ValueError:
            raise InvalidEnvelope("dataschema必须为URI引用") from None
    if env.get("datacontenttype") != "application/json" or not isinstance(env.get("data"), dict):
        raise InvalidEnvelope("事件必须为JSON对象载荷")
    try:
        instant = env["time"]
        if not isinstance(instant, str) or not re.fullmatch(r"\d{4}-\d\d-\d\d[Tt]\d\d:\d\d:\d\d(?:\.\d+)?(?:[Zz]|[+-]\d\d:\d\d)", instant):
            raise ValueError("invalid time")
        datetime.fromisoformat(instant.upper().replace("Z", "+00:00"))
    except (KeyError, ValueError):
        raise InvalidEnvelope("事件time必须为RFC3339时间") from None


def check_topic(topic_name: str, env: dict) -> None:
    validate(env)
    if not re.fullmatch(r"(public|platform|cell-[a-z0-9][a-z0-9-]{0,62})\.[a-z][a-z0-9_-]*", topic_name):
        raise InvalidEnvelope("非法事件主题")
    prefix = topic_name.split(".", 1)[0]
    zone, cell = env["miaoqiazone"], env.get("miaoqiacell", "")
    if ((prefix == "public" and zone != reqctx.PUBLIC) or (prefix == "platform" and zone != reqctx.PLATFORM)
            or (prefix.startswith("cell-") and (zone != reqctx.TENANT or f"cell-{cell}" != prefix))):
        raise InvalidEnvelope("事件域/单元与主题不符")


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
        c.validate()
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
        target_topic = ev.topic or topic(self.zone, c.cell, self.namespace)
        check_topic(target_topic, env)
        key = ev.partition_key or ev.subject
        if self.zone == reqctx.TENANT:
            key = f"{c.tenant}/{key}"
        conn.execute(
            text("INSERT INTO kernel_outbox (event_id, topic, partition_key, envelope) VALUES (:i, :t, :k, CAST(:e AS jsonb))"),
            {"i": env["id"], "t": target_topic, "k": key,
             "e": json.dumps(env, ensure_ascii=False)},
        )
        conn.execute(text("SELECT pg_notify('kernel_outbox', '')"))
        return env
