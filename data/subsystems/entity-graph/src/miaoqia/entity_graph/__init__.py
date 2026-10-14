"""entity-graph 子系统：实体与关系图谱。接口、事件与数据随所属里程碑实现（docs/05-delivery/01-roadmap.md）。"""
from __future__ import annotations

from pathlib import Path

from miaoqia.kernel.runtime import Module

from .api import NAME

MIGRATIONS = str(Path(__file__).resolve().parents[3] / "migrations")


def module() -> Module:
    return Module(name=NAME, database="ent", migrations=MIGRATIONS)
