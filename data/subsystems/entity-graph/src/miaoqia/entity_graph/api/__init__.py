"""entity-graph 的公开接口（其他子系统唯一可导入的部分）。"""

NAME = "entity-graph"

from dataclasses import dataclass
from typing import Literal, Protocol


@dataclass(frozen=True)
class FixtureFieldProof:
    fact_id: str
    version: int
    field_path: str
    evidence_ref: str


@dataclass(frozen=True)
class FixtureOrganization:
    fixture_subject_key: str
    org_id: str
    name: str
    aliases: tuple[str, ...]
    proofs: tuple[FixtureFieldProof, ...]
    confidence: float


@dataclass(frozen=True)
class FixtureRegistry:
    """已验证的不可变证据投影；调用者须先授权全部依赖，类型本身不授予权限。"""
    namespace: str
    reference_name: str
    reference_version: str
    reference_hash: str
    records: tuple[FixtureOrganization, ...]


@dataclass(frozen=True)
class FixtureResolution:
    fixture_only: bool
    namespace: str
    reference_name: str
    reference_version: str
    reference_hash: str
    status: Literal["resolved", "ambiguous", "unknown", "conflict"]
    method: Literal["exact_strong_key", "exact_name", "exact_alias"]
    candidates: tuple[str, ...]
    selected_id: str | None
    confidence: float
    consulted_proofs: tuple[FixtureFieldProof, ...]


class FixtureOrganizationResolver(Protocol):
    def resolve(self, name: str, *, fixture_subject_key: str | None = None) -> FixtureResolution: ...
