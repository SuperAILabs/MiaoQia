"""evidence 子系统：证据与内容理解。接口、事件与数据随所属里程碑实现（docs/05-delivery/01-roadmap.md）。"""
from __future__ import annotations

from importlib import resources

from miaoqia.kernel.consumer import Consumer
from miaoqia.kernel.runtime import Env, Module

from .api import NAME
from .application import probe

MIGRATIONS = str(resources.files(__name__) / "migrations")  # Alembic 迁移随包分发，安装后同样可定位


def module() -> Module:
    def consumers(env: Env) -> list[Consumer]:
        # 租户文档业务管线尚未实现；不能复用公共探针主题/发件箱。
        if env.data_scope == "tenant":
            return []
        return [Consumer(name=f"{NAME}.probe", subsystem=NAME, brokers=env.brokers, topics=["public.acquisition"],
                         handlers={probe.EMITTED: probe.on_emitted}, db=env.db)]

    return Module(name=NAME, database="evd", migrations=MIGRATIONS, consumers=consumers)
