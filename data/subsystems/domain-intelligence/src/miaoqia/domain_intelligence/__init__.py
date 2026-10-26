"""domain-intelligence 子系统：领域情报。接口、事件与数据随所属里程碑实现（docs/05-delivery/01-roadmap.md）。"""
from __future__ import annotations

from importlib import resources
from typing import Callable

from miaoqia.evidence import api as evidence_api
from miaoqia.kernel.consumer import Consumer
from miaoqia.kernel.runtime import Env, Module

from .api import NAME

MIGRATIONS = str(resources.files(__name__) / "migrations")  # Alembic 迁移随包分发，安装后同样可定位


def module(extractions: Callable[[], evidence_api.Extractions] | None = None) -> Module:
    """extractions 由部署单元组装注入（数据单元内为 evidence 的本地绑定）；未注入时消费角色拒绝启动，不以空数据运行。"""

    def consumers(env: Env) -> list[Consumer]:
        if env.role != "data-consumer":
            return []
        if extractions is None:
            raise SystemExit("domain-intelligence：未装配解析修订读取（evidence api.Extractions）")
        from .application.facts import EXTRACTION_REVISED, Materializer

        materializer = Materializer(extractions())
        return [Consumer(name=f"{NAME}.facts", subsystem=NAME, brokers=env.brokers, topics=[evidence_api.TOPIC],
                         handlers={EXTRACTION_REVISED: materializer.on_extraction_revised}, db=env.db)]

    return Module(name=NAME, database="dom", migrations=MIGRATIONS, consumers=consumers)
