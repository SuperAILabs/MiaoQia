"""evidence 的公开接口（其他子系统唯一可导入的部分）。"""
from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from typing import Protocol

NAME = "evidence"

SNAPSHOT_CREATED = "evidence.snapshot.created.v1"
EXTRACTION_REVISED = "evidence.extraction.revised.v1"
TOPIC = "public.evidence"


@dataclass(frozen=True)
class Revision:
    """一次解析修订（GetExtraction）：快照与来源对象、解析器与领域包、质量状态与解析出的领域字段（不含公共字段）。"""

    id: str
    snapshot_ref: str
    source_id: str
    source_key: str
    observed_at: datetime
    rights_tags: tuple[str, ...]
    pack_id: str
    pack_version: str
    extractor: str
    quality_status: str
    facts: tuple[dict, ...]


class Extractions(Protocol):
    """解析修订的读取。修订不可变；不存在时返回 None。"""

    def revision(self, revision_ref: str) -> Revision | None: ...


@dataclass(frozen=True)
class ResolvedEvidence:
    id: str
    display_level: str
    snapshot_ref: str | None = None
    source_id: str | None = None
    source_key: str | None = None
    observed_at: datetime | None = None
    locator: dict | None = None
    snippet: str | None = None
    context: str | None = None
    derivation: dict | None = None


class EvidenceResolver(Protocol):
    def resolve(self, evidence_ref: str) -> ResolvedEvidence | None: ...


class Enrichments(Protocol):
    """已验证不可变结果的读取；无权返回None，完整性错误拒绝，当前权限每次重验。"""
    def require_full(self, rights_tags: list[str]) -> None: ...

    def get(self, run_key: str) -> dict | None: ...
