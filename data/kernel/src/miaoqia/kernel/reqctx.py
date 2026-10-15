"""请求上下文：租户、单元、主体、用途。进程内以 contextvars 传递，进程间经事件信封传递。"""
from __future__ import annotations

import contextlib
import contextvars
from dataclasses import dataclass, replace
import re
import base64
import json
from typing import Iterator

PUBLIC, TENANT, PLATFORM = "public", "tenant", "platform"


@dataclass(frozen=True)
class Principal:
    id: str
    kind: str = "service"  # user / service / service_account


@dataclass(frozen=True)
class Context:
    principal: Principal
    tenant: str = ""
    cell: str = ""
    purpose: str = ""

    def validate(self):
        if not isinstance(self.principal.id, str) or not self.principal.id or self.principal.kind not in {"user", "service", "service_account"}:
            raise ValueError("请求主体无效")
        if not all(isinstance(value, str) for value in (self.tenant, self.cell, self.purpose)):
            raise ValueError("上下文字段必须为字符串")
        if bool(self.tenant) != bool(self.cell) or (self.cell and not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,62}", self.cell)):
            raise ValueError("租户与单元无效")

    @classmethod
    def from_dict(cls, value):
        if not isinstance(value, dict) or set(value) - {"principal", "tenant", "cell", "purpose"}:
            raise ValueError("未知上下文字段或非对象")
        for key in ("tenant", "cell", "purpose"):
            if key in value and (not isinstance(value[key], str) or not value[key]):
                raise ValueError("空上下文字段")
        principal = value.get("principal")
        if not isinstance(principal, dict) or set(principal) != {"id", "kind"}:
            raise ValueError("主体字段无效")
        context = cls(**{**value, "principal": Principal(**principal)})
        context.validate()
        return context

    def encode(self):
        self.validate()
        value = {"principal": {"id": self.principal.id, "kind": self.principal.kind}}
        value.update({key: getattr(self, key) for key in ("tenant", "cell", "purpose") if getattr(self, key)})
        return base64.urlsafe_b64encode(json.dumps(value, separators=(",", ":")).encode()).decode().rstrip("=")

    @classmethod
    def decode(cls, header):
        if not isinstance(header, str) or len(header) > 8192 or not re.fullmatch(r"[A-Za-z0-9_-]+", header):
            raise ValueError("无效内部上下文头")
        body = base64.urlsafe_b64decode(header + "=" * (-len(header) % 4))
        if base64.urlsafe_b64encode(body).decode().rstrip("=") != header:
            raise ValueError("非规范Base64URL")
        return cls.from_dict(json.loads(body))


class MissingContext(RuntimeError):
    """需要请求上下文而未提供。"""


_current: contextvars.ContextVar[Context | None] = contextvars.ContextVar("miaoqia_reqctx", default=None)


def current() -> Context:
    c = _current.get()
    if c is None:
        raise MissingContext("缺少请求上下文")
    return c


def get() -> Context | None:
    return _current.get()


@contextlib.contextmanager
def use(c: Context) -> Iterator[Context]:
    c.validate()
    token = _current.set(c)
    try:
        yield c
    finally:
        _current.reset(token)


def service(name: str, base: Context | None = None) -> Context:
    """以服务身份发起的上下文（事件消费、工作流、定时任务）。"""
    p = Principal(id=f"svc:{name}", kind="service")
    return replace(base, principal=p) if base else Context(principal=p)
