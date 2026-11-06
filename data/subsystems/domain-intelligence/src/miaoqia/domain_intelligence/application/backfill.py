"""显式目标的本地项目补物化；不重放consumer inbox或改写Notice历史。"""
from __future__ import annotations

import re

from sqlalchemy import text

from miaoqia.contracts import models

from .source import validate_source
from .facts import fact_version
from .projects import aggregate_notice, lock_identities


def backfill(database, extractions, notice_ids):
    """单个有界批次原子执行。服务调用上下文由本地装配提供；任一目标无效则全部回滚。"""
    selected = sorted(set(notice_ids))
    if not 1 <= len(notice_ids) <= 100 or any(not re.fullmatch(r"ntc_[0-9A-HJKMNP-TV-Z]{26}", n) for n in selected):
        raise ValueError("必须显式指定1–100个有效ntc_公告标识")
    with database.tx() as conn:
        rows = []
        for notice_id in selected:
            row = conn.execute(text("SELECT source_id,source_key,ordinal,info_domain,object_type FROM dom_facts WHERE id=:n"),
                               {"n": notice_id}).first()
            if row is None or row.info_domain != "INF-PROC" or row.object_type != "tendering.notice":
                raise ValueError(f"目标不是已物化INF-PROC公告：{notice_id}")
            rows.append((notice_id, row))
        # 与正常物化相同的锁层次；先锁全部来源，再按稳定顺序锁全部项目，避免批次交叉死锁。
        for key in sorted({f"{r.info_domain}|{r.source_id}|{r.source_key}" for _, r in rows}):
            conn.execute(text("SELECT pg_advisory_xact_lock(hashtext(:k))"), {"k": key})
        pending, results = [], []
        for notice_id, row in rows:
            notice = fact_version(conn, notice_id)  # 持来源锁后固定当前版本
            models.Notice.model_validate(notice)
            existing = conn.execute(text("SELECT status,project_id FROM dom_notice_links WHERE notice_id=:n AND notice_version=:v"),
                                    {"n": notice_id, "v": notice["version"]}).first()
            if existing:
                results.append({"notice_id": notice_id, "notice_version": notice["version"],
                                "action": "unchanged", "status": existing.status, "project_id": existing.project_id})
                continue
            revision = extractions.revision(notice["revision_ref"])
            validate_source(notice, row, revision)
            pending.append((notice, revision))
        lock_identities(conn, *(revision for _, revision in pending))
        for notice, revision in pending:
            aggregate_notice(conn, revision, notice)
            status, project_id = conn.execute(text("SELECT status,project_id FROM dom_notice_links WHERE notice_id=:n AND notice_version=:v"),
                                             {"n": notice["id"], "v": notice["version"]}).one()
            results.append({"notice_id": notice["id"], "notice_version": notice["version"],
                            "action": "processed", "status": status, "project_id": project_id})
        return sorted(results, key=lambda r: r["notice_id"])
