"""acquisition 子系统：来源采集。接口、事件与数据随所属里程碑实现（docs/05-delivery/01-roadmap.md）。"""
from __future__ import annotations

import logging
import os
from collections.abc import Callable
from importlib import resources
from typing import TYPE_CHECKING

from miaoqia.kernel import scheduler
from miaoqia.kernel.consumer import Consumer
from miaoqia.kernel.runtime import Env, Module, Worker

from .api import NAME
from .application import probe

if TYPE_CHECKING:
    from .application.browser_jobs import BrowserRenderer

MIGRATIONS = str(resources.files(__name__) / "migrations")  # Alembic 迁移随包分发，安装后同样可定位
log = logging.getLogger(__name__)


def module(*, browser_factory: Callable[[], BrowserRenderer] | None = None) -> Module:
    def consumers(env: Env) -> list[Consumer]:
        return [Consumer(name=f"{NAME}.probe", subsystem=NAME, brokers=env.brokers, topics=["platform.operations"],
                         handlers={probe.REQUESTED: probe.on_requested}, db=env.db)]

    def collection_store(env: Env) -> str:
        objects = env.config("OBJECT_STORE_DIR")
        if not objects:
            log.warning("collection disabled role=%s: MIAOQIA_ACQUISITION_OBJECT_STORE_DIR not set", env.role)
        return objects

    def jobs(env: Env) -> list[scheduler.Job]:
        if not collection_store(env):
            return []
        from .application import dispatch, sources

        declared = sources.load(os.environ.get("MIAOQIA_PACKS_DIR", "packs"))
        return [dispatch.planner_job(env.db, declared)]

    def workers(env: Env) -> list[Worker]:
        objects = collection_store(env)
        if not objects:
            return []
        from miaoqia.kernel.objectstore import open_object_store

        from .application import discovery, dispatch, sources
        from .application.fetch import Fetcher
        from .infrastructure.archive_spool import ArchiveSpool
        from .infrastructure.transport import UrllibTransport

        browser_only = env.role == "acq-browser"
        declared = sources.load(os.environ.get("MIAOQIA_PACKS_DIR", "packs"),
                                transport="browser" if browser_only else "http")
        if browser_only and not declared:
            return []
        store = open_object_store(objects)
        spool = ArchiveSpool.configured(env.config)
        browser = None
        if browser_only:
            if browser_factory is None or spool is None:
                raise ValueError("acq-browser requires a confined renderer and private spool")
            browser = browser_factory()
            if not callable(getattr(browser, "render", None)):
                raise ValueError("acq-browser renderer is invalid")
        fetcher = Fetcher(env.db, store, UrllibTransport(), spool=spool, browser=browser)
        maintenance = [scheduler.Job(name=f"{NAME}.expire-discovery", every=600, timeout=30, database=env.db,
                                     run=lambda deadline: discovery.expire(env.db, store, deadline=deadline))]
        if spool is not None:
            def expire_spool(deadline):
                spool.expire()
                return 0

            # A separate host may share the acquisition database but not this volume.
            maintenance.append(scheduler.Job(name=f"{NAME}.expire-archive-spool", every=600, timeout=30,
                                             run=expire_spool))
        return [*dispatch.workers(fetcher, declared, browser_only=browser_only),
                Worker(f"{NAME}.maintenance", scheduler.Runner(maintenance).run)]

    return Module(name=NAME, database="acq", migrations=MIGRATIONS, consumers=consumers, jobs=jobs, workers=workers)
