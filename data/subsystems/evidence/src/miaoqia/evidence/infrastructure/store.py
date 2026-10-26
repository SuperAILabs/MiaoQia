"""解析修订的只读查询（api.Extractions 的实现）。公共实例数据，不设租户上下文，使用 evidence 自己的数据库账号。"""
from __future__ import annotations

from sqlalchemy import create_engine, text

from ..api import Revision


class SqlExtractions:
    def __init__(self, dsn: str):
        url = dsn.replace("postgres://", "postgresql+psycopg://", 1).replace("postgresql://", "postgresql+psycopg://", 1)
        self.engine = create_engine(url, pool_size=2, pool_pre_ping=True)

    def close(self) -> None:
        self.engine.dispose()

    def revision(self, revision_ref: str) -> Revision | None:
        with self.engine.connect() as conn:
            row = conn.execute(text("""
                SELECT r.id, s.id, s.source_id, s.source_key, s.observed_at, s.rights_tags, r.pack_id, r.pack_version,
                       r.extractor, r.quality_status, r.facts
                FROM evd_extraction_revisions r JOIN evd_snapshots s ON s.id = r.snapshot_id
                WHERE r.id = :id"""), {"id": revision_ref}).first()
        if row is None:
            return None
        return Revision(id=row[0], snapshot_ref=row[1], source_id=row[2], source_key=row[3], observed_at=row[4],
                        rights_tags=tuple(row[5]), pack_id=row[6], pack_version=row[7], extractor=row[8],
                        quality_status=row[9], facts=tuple(row[10]))
