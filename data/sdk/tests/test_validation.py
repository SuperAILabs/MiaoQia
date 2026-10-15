"""通用文档与证据完整性的负向验证。"""
import pytest
from miaoqia.sdk import Document, ExtractionContext
from miaoqia.sdk.testing import html_to_document
from miaoqia.sdk.validation import validate_extraction


def _review(_name):
    doc = Document(html_to_document("<p>通用证据</p>", "snp_" + "0" * 26))
    ctx = ExtractionContext(doc, "generic@1.0.0")
    ref = ctx.text(doc.block_span(0))
    return doc, [{"value": ref}], ctx.evidence


@pytest.mark.parametrize("fault", ["hash", "snapshot", "pipeline", "bounds", "range", "missing_ref"])
def test_bad_evidence_never_passes(fault):
    doc, facts, evidence = _review("contract")
    ref = next(iter(evidence.values()))
    if fault == "hash":
        ref["quote_hash"] = "sha256:" + "0" * 64
    elif fault == "snapshot":
        ref["snapshot_ref"] = "snp_" + "1" * 26
    elif fault == "pipeline":
        ref["pipeline_version"] = "different@1.0.0"
    elif fault == "bounds":
        ref["locator"]["block"] = -1
    elif fault == "range":
        ref["locator"]["char_end"] = 999999
    else:
        evidence.clear()
    assert validate_extraction(doc, facts, evidence)

