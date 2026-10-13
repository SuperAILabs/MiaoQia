import pytest

from miaoqia.sdk.document import Document, Span
from miaoqia.sdk.extraction import ExtractionContext

DOC = Document({"snapshot_ref": "snp_" + "1" * 26, "pipeline_version": "doc.html-dev@0.1.0", "media_type": "text_html",
                "blocks": [{"index": 0, "kind": "paragraph", "text": "预算金额：320 万元"}]})


def test_field_carries_provenance_and_registers_evidence():
    ctx = ExtractionContext(DOC, "proc.demo@0.0.1")
    span = DOC.find_labeled(["预算金额"])
    m = ctx.money(span, confidence=0.9)
    assert m["value"] == "3200000" and m["raw"] == "320 万元"
    ev = m["prov"]["evidence_ref"]
    assert m["prov"] == {"evidence_ref": ev, "method": "rule", "confidence": 0.9, "extractor": "proc.demo@0.0.1"}
    ref = ctx.evidence[ev]
    assert ref["locator"] == {"block": 0, "char_start": 5, "char_end": 11}
    assert ref["quote_hash"].startswith("sha256:")


def test_evidence_ids_are_deterministic_and_well_formed():
    a = ExtractionContext(DOC, "x@0.0.1").evidence_ref(DOC.find_labeled(["预算金额"]))
    b = ExtractionContext(DOC, "x@0.0.1").evidence_ref(DOC.find_labeled(["预算金额"]))
    assert a == b and len(a) == 29 and a.startswith("ev_")


def test_span_must_match_source_text():
    ctx = ExtractionContext(DOC, "x@0.0.1")
    with pytest.raises(ValueError):
        ctx.text(Span("400 万元", 0, 5, 11))


def test_unparseable_money_returns_none():
    ctx = ExtractionContext(DOC, "x@0.0.1")
    assert ctx.money(Span("预算", 0, 0, 2)) is None
