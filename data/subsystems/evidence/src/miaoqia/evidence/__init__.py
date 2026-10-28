"""evidence 子系统：证据与内容理解。接口、事件与数据随所属里程碑实现（docs/05-delivery/01-roadmap.md）。"""
from __future__ import annotations

from importlib import resources

from miaoqia.kernel.consumer import Consumer
from miaoqia.kernel.runtime import Env, Module

from .api import NAME
from .application import probe

MIGRATIONS = str(resources.files(__name__) / "migrations")  # Alembic 迁移随包分发，安装后同样可定位


def extractions(dsn: str):
    """api.Extractions 的本地绑定：数据单元内以 evidence 自己的数据库账号只读解析修订（由部署单元组装注入）。"""
    from .infrastructure.store import SqlExtractions

    return SqlExtractions(dsn)


def module() -> Module:
    def consumers(env: Env) -> list[Consumer]:
        # 租户文档业务管线尚未实现；不能复用公共探针主题/发件箱。
        if env.data_scope == "tenant":
            return []
        return [Consumer(name=f"{NAME}.probe", subsystem=NAME, brokers=env.brokers, topics=["public.acquisition"],
                         handlers={probe.EMITTED: probe.on_emitted}, db=env.db)]

    return Module(name=NAME, database="evd", migrations=MIGRATIONS, consumers=consumers)


def resolver(database, store, *, display=None, enrichments=None, authorize=None):
    """api.EvidenceResolver 的本地绑定；必须注入按当前请求上下文判定展示级别的策略。"""
    from .application.queries import Resolver

    return Resolver(database, store, display, enrichments=enrichments, authorize=authorize)


def enrichments(database, store, *, facts, display=None, enable_test_recipe=False, recipes=(), authorize=None, fact_requirements=None):
    """公开本地派生登记/读取装配，默认无recipe且拒绝访问。"""
    from .application.enrichments import EnrichmentStore
    return EnrichmentStore(database, store, facts=facts, display=display, enable_test_recipe=enable_test_recipe, recipes=recipes, authorize=authorize, fact_requirements=fact_requirements)


def load_recipe(pack_dir, entrypoint, packs_dir):
    """本地显式recipe装配；复用包入口来源检查，不自动启用所有行业包。"""
    from .extensions.packs import _load_module
    module, name = entrypoint.split(":")
    return getattr(_load_module(pack_dir, module), name)(packs_dir)


def source_requirements(database):
    """Owner metadata-only port; no content objects or caller source assertions."""
    from .application.provenance import Provenance
    return Provenance(database)
