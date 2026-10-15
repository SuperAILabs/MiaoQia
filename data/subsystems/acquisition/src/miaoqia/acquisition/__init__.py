"""acquisition 子系统：来源采集。接口、事件与数据随所属里程碑实现（docs/05-delivery/01-roadmap.md）。"""
from __future__ import annotations

from importlib import resources

from miaoqia.kernel.consumer import Consumer
from miaoqia.kernel.runtime import Env, Module

from .api import NAME
from .application import probe

MIGRATIONS = str(resources.files(__name__) / "migrations")  # Alembic 迁移随包分发，安装后同样可定位


def module() -> Module:
    def consumers(env: Env) -> list[Consumer]:
        return [Consumer(name=f"{NAME}.probe", subsystem=NAME, brokers=env.brokers, topics=["platform.operations"],
                         handlers={probe.REQUESTED: probe.on_requested}, db=env.db)]

    return Module(name=NAME, database="acq", migrations=MIGRATIONS, consumers=consumers)
