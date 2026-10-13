"""解析器 SPI 与字段溯源。

信息领域包实现 Extractor；evidence 的 extraction_orchestrator 为每次解析提供 ExtractionContext。
字段只能通过 ExtractionContext 的构造方法产生，构造时必须给出原文 Span：上下文据此登记证据引用（ev_），
并把 provenance（证据、方法、置信度、解析器版本）写入字段。
"""
from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from typing import Callable, Protocol

from . import zh
from .document import Document, Span

CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"


def _ulid_like(prefix: str, seed: str) -> str:
    """确定性的 26 位 Crockford 标识（用于本地与测试；生产环境由 evidence 分配 ULID）。"""
    n = int.from_bytes(hashlib.sha256(seed.encode()).digest()[:17], "big")
    chars = []
    for _ in range(26):
        chars.append(CROCKFORD[n % 32])
        n //= 32
    return f"{prefix}_{''.join(chars)}"


@dataclass
class ExtractionResult:
    facts: list[dict] = field(default_factory=list)
    warnings: list[dict] = field(default_factory=list)


class Extractor(Protocol):
    """信息领域包的解析器：输入规范化文档，输出领域事实（不含 public_fact_meta，由平台补齐）。"""

    id: str
    version: str
    fact_types: tuple[str, ...]

    def applies(self, doc: Document, source: dict) -> bool: ...

    def extract(self, doc: Document, ctx: "ExtractionContext") -> ExtractionResult: ...


@dataclass
class ExtractionContext:
    doc: Document
    extractor: str                      # 形如 proc.cn-ccgp@0.1.0
    region_resolver: Callable[[str], str | None] = lambda name: None
    evidence: dict[str, dict] = field(default_factory=dict)   # ev_ → EvidenceRef

    # ---- 证据 ----
    def evidence_ref(self, span: Span) -> str:
        loc = span.locator()
        quote = self.doc.quote(loc)
        if quote != span.text:
            raise ValueError(f"定位与原文不一致：{quote!r} != {span.text!r}")
        key = f"{self.doc.snapshot_ref}|{self.doc.pipeline_version}|{sorted(loc.items())}"
        ev = _ulid_like("ev", key)
        self.evidence[ev] = {
            "id": ev,
            "snapshot_ref": self.doc.snapshot_ref,
            "pipeline_version": self.doc.pipeline_version,
            "locator": loc,
            "quote_hash": "sha256:" + hashlib.sha256(quote.encode("utf-8")).hexdigest(),
        }
        return ev

    def prov(self, span: Span, confidence: float, method: str = "rule") -> dict:
        return {"evidence_ref": self.evidence_ref(span), "method": method,
                "confidence": round(confidence, 3), "extractor": self.extractor}

    # ---- 带证据字段的构造（与 contracts/schemas/common/types.schema.json 对应）----
    def text(self, span: Span, confidence: float = 0.95) -> dict:
        return {"value": span.text, "prov": self.prov(span, confidence)}

    def money(self, span: Span, confidence: float = 0.95, default_unit: str | None = None) -> dict | None:
        parsed = zh.parse_money(span.text, default_unit)
        if parsed is None:
            return None
        value, currency = parsed
        return {"value": value, "currency": currency, "raw": span.text, "prov": self.prov(span, confidence)}

    def instant(self, span: Span, confidence: float = 0.95, tz: str = "+08:00") -> dict | None:
        parsed = zh.parse_datetime(span.text, tz)
        if parsed is None:
            return None
        value, precision, tz_raw = parsed
        return {"value": value, "precision": precision, "tz_raw": tz_raw, "prov": self.prov(span, confidence)}

    def quantity(self, span: Span, unit_span: Span | None = None, confidence: float = 0.95) -> dict | None:
        value = span.text.replace(",", "")
        try:
            float(value)
        except ValueError:
            return None
        out = {"value": value, "prov": self.prov(span, confidence)}
        if unit_span is not None:
            out["unit_raw"] = unit_span.text
        return out

    def org(self, span: Span, confidence: float = 0.95) -> dict:
        """组织引用只保留原文名称；解析为 org_id 由 entity-graph 完成。"""
        return {"name_raw": span.text, "prov": self.prov(span, confidence)}

    def region(self, span: Span, confidence: float = 0.9) -> dict | None:
        code = self.region_resolver(span.text)
        if code is None:
            return None
        return {"code": code, "name_raw": span.text, "prov": self.prov(span, confidence)}

    def new_id(self, prefix: str, *parts: str) -> str:
        return _ulid_like(prefix, "|".join([self.doc.snapshot_ref, prefix, *parts]))
