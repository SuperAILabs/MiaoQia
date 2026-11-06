"""entity-graph 子系统：实体与关系图谱。接口、事件与数据随所属里程碑实现（docs/05-delivery/01-roadmap.md）。"""
from __future__ import annotations

from importlib import resources

from miaoqia.kernel.runtime import Module

from .api import NAME

MIGRATIONS = str(resources.files(__name__) / "migrations")  # Alembic 迁移随包分发，安装后同样可定位


def module() -> Module:
    return Module(name=NAME, database="ent", migrations=MIGRATIONS)


def fixture_resolver(registry):
    """本地装配入口；registry 须由证据所有者端口验证并先授权全部输入。"""
    from .domain.fixtures import FixtureResolver
    return FixtureResolver(registry)


def trusted_fixture_resolver(*, manifest_path, handles, facts, extractions, evidence, provenance, references):
    """Local fixed registry binding; every call proves and authorizes all owner inputs."""
    from .application.fixtures import TrustedFixtureResolver
    return TrustedFixtureResolver(manifest_path=manifest_path, handles=handles, facts=facts, extractions=extractions,
                                  evidence=evidence, provenance=provenance, references=references)
