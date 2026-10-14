"""请求上下文：租户、单元、主体、用途。进程内以 contextvars 传递，进程间经事件信封传递。"""
from __future__ import annotations

import contextlib
import contextvars
from dataclasses import dataclass, field, replace
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
    extra: dict = field(default_factory=dict)


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
    token = _current.set(c)
    try:
        yield c
    finally:
        _current.reset(token)


def service(name: str, base: Context | None = None) -> Context:
    """以服务身份发起的上下文（事件消费、工作流、定时任务）。"""
    p = Principal(id=f"svc:{name}", kind="service")
    return replace(base, principal=p) if base else Context(principal=p)
