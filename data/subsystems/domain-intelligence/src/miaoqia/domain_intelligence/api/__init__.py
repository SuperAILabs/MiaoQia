"""domain-intelligence 的公开接口（其他子系统唯一可导入的部分）。"""

from dataclasses import dataclass
from datetime import datetime
from typing import Literal, Protocol


NAME = "domain-intelligence"

FACT_UPSERTED = "domain.fact.upserted.v1"
TOPIC = "public.domain"


@dataclass(frozen=True)
class Basis:
    """只支持当前记录值与当时已知口径；固定版本也必须受记录截止时点约束。"""

    view: Literal["current", "known_at"]
    recorded_cutoff: datetime | None = None

    def __post_init__(self):
        if self.view not in ("current", "known_at"):
            raise ValueError("不支持的时间口径")
        if self.view == "current" and self.recorded_cutoff is not None:
            raise ValueError("当前口径不能带记录截止时点")
        if self.view == "known_at" and (self.recorded_cutoff is None or self.recorded_cutoff.utcoffset() is None):
            raise ValueError("当时已知口径必须带有时区的记录截止时点")


@dataclass(frozen=True)
class FactRead:
    basis: Basis
    fact: dict


@dataclass(frozen=True)
class Page:
    basis: Basis
    items: tuple[dict, ...]
    next_cursor: str | None = None


class Facts(Protocol):
    def get_fact(self, fact_id: str, basis: Basis, *, version: int | None = None) -> FactRead | None: ...

    def get_fact_history(self, fact_id: str, basis: Basis, *, after: int = 0, limit: int = 50) -> Page: ...

    def query_facts(self, info_domain: str, object_type: str, basis: Basis, *, after: str = "", limit: int = 50) -> Page: ...

    def bid_result_units(self, fact_id: str, basis: Basis, *, version: int | None = None) -> Page: ...
