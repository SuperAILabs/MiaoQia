"""结果公告解析器：真实官方原件上的语义边界与反例（准确率评测见 mq-corpus check --gold reviewed）。"""
from functools import partial
from pathlib import Path

from miaoqia.sdk import ExtractionContext
from miaoqia.sdk.corpus import load_reviewed_sample
from miaoqia.sdk.document import Document
from miaoqia.sdk.testing import html_to_document
from miaoqia.sdk.validation import validate_extraction as validate_integrity
from miaoqia_pack_procurement.extractors.result_notice import ResultNoticeExtractor
from miaoqia_pack_procurement.validation import validate

ROOT = Path(__file__).resolve().parents[4]
CORPUS = ROOT / "packs/information_domains/procurement/corpus"
validate_extraction = partial(validate_integrity, semantic_validator=validate)


def _run(doc: Document):
    ex = ResultNoticeExtractor()
    ctx = ExtractionContext(doc, f"{ex.id}@{ex.version}")
    result = ex.extract(doc, ctx)
    assert not validate_extraction(doc, result.facts, ctx.evidence)
    return result.facts[0], ctx.evidence


def _sample(name: str):
    doc, _, _ = load_reviewed_sample(CORPUS / f"official-{name}-20261014")
    return doc


def _results(fact):
    return [r for lot in fact["lots"] for r in lot.get("bid_results", [])]


def test_award_lots_keep_each_winner_and_void_only_the_failed_lot():
    fact, evidence = _run(_sample("award"))
    assert fact["notice_type"] == "award"
    assert [r["bidder"]["name_raw"] for r in _results(fact)] == [
        "软通计算机有限公司", "紫光计算机科技有限公司", "联硕电脑（重庆）有限公司", "华为终端有限公司"]
    assert all(r["is_winner"] and r["price_kind"] == "total" for r in _results(fact))
    voided = [lot for lot in fact["lots"] if lot.get("ending")]
    assert [lot["lot_key"] for lot in voided] == ["5"] and "bid_results" not in voided[0]
    # 每个字段都回到原文：证据引用指向的片段与取值一致
    price = _results(fact)[0]["price"]
    assert evidence[price["prov"]["evidence_ref"]]["locator"]["block"] == 22


def test_candidates_are_ranked_but_never_winners():
    fact, _ = _run(_sample("candidate"))
    assert fact["notice_type"] == "candidate"
    assert [(r["rank"], r["stage"]) for r in _results(fact)] == [(1, "candidate"), (2, "candidate"), (3, "candidate")]
    assert not any("is_winner" in r for r in _results(fact))


def test_framework_shortlist_is_not_a_contract_award_and_keeps_tied_ranks():
    fact, _ = _run(_sample("multiwinner"))
    results = _results(fact)
    assert fact["method"] == "framework"
    assert all(r["result_kind"] == "framework_shortlist" and r["is_winner"] is False for r in results)
    assert "price" not in results[0]                     # 最高入围价格不是该供应商的报价
    lot2 = [r["rank"] for r in fact["lots"][1]["bid_results"]]
    assert lot2.count(5) == 2


def test_annual_unit_sum_is_not_a_total_amount():
    fact, _ = _run(_sample("ordinary-multiwinner"))
    results = _results(fact)
    assert len(results) == 5 and fact["lots"][0]["lot_key"] == "unpartitioned"
    assert {r["price_kind"] for r in results} == {"annual_unit_sum"}
    assert fact["quality"]["warnings"][0]["code"] == "UNIT_SUM_NOT_TOTAL"


def test_winner_only_in_attachment_is_not_fabricated():
    fact, _ = _run(_sample("award-amended-original"))
    assert fact["result_details_pending"] is True
    assert _results(fact) == []
    assert fact["award_amount"]["value"] == "2848630"     # 汇总金额不分摊到标包
    assert fact["quality"]["warnings"][0]["code"] == "RESULT_DETAILS_PENDING"


def test_consortium_members_attach_to_the_named_winner():
    fact, _ = _run(_sample("consortium"))
    (result,) = _results(fact)
    assert result["bidder"]["uscc"] == "91321283MADWT8A11R"
    assert [m["name_raw"] for m in result["consortium_members"]] == ["江苏移动信息系统集成有限公司"]


def test_result_amendments_and_other_stages_are_not_parsed_as_results():
    ex = ResultNoticeExtractor()
    for name in ("amendment", "contract", "failure", "acceptance"):
        doc = _sample(name)
        assert not ex.applies(doc, {}), name


def test_supplier_lines_outside_a_result_section_do_not_create_winners():
    """合成反例：没有中标金额与包头的“供应商名称：详见附件”不产生中标人；
    金额出现在供应商之前时不把它挂到后面的供应商上。"""
    html = ("<h1>某项目中标公告（合成样本）</h1><p>供应商名称：详见附件</p><p>中标（成交）金额：12.5 万元</p>"
            "<p>第一包：</p><p>中标（成交）金额：99 万元</p><p>供应商名称：某某建材有限公司</p>")
    doc = Document(html_to_document(html, "snp_" + "0" * 26))
    fact, _ = _run(doc)
    (result,) = _results(fact)
    assert result["bidder"]["name_raw"] == "某某建材有限公司" and "price" not in result
    assert fact["award_amount"]["value"] == "125000" and fact["result_details_pending"] is True


def test_text_value_must_equal_the_quoted_original():
    """反例：文本字段取值被改写（定位与证据哈希仍有效）时，领域校验报告与原文不一致。"""
    doc = _sample("award")
    ex = ResultNoticeExtractor()
    ctx = ExtractionContext(doc, f"{ex.id}@{ex.version}")
    fact = ex.extract(doc, ctx).facts[0]
    fact["title"]["value"] = "改写后的标题"
    assert any("文本字段与证据原文不一致" in e for e in validate_extraction(doc, [fact], ctx.evidence))


def test_official_building_pair_has_the_same_code_and_percentage_is_not_money():
    from miaoqia_pack_procurement.extractors.tender_notice import TenderNoticeExtractor
    pair = {}
    for kind in ("tender", "award"):
        doc, reviewed, _ = load_reviewed_sample(CORPUS / f"official-building-{kind}-20261026")
        ex = TenderNoticeExtractor() if kind == "tender" else ResultNoticeExtractor()
        assert ex.applies(doc, {})
        ctx = ExtractionContext(doc, f"{ex.id}@{ex.version}")
        result = ex.extract(doc, ctx)
        assert not validate_extraction(doc, result.facts, ctx.evidence)
        pair[kind] = result.facts[0]
        assert pair[kind]["project_code"] == {"value": "XCXCG-2025-008", "prov": pair[kind]["project_code"]["prov"]}
        assert pair[kind]["buyer"]["name_raw"] == reviewed[0]["buyer"]["name_raw"] == "江山市水务有限公司"
    award = pair["award"]
    assert award["lots"][0]["bid_results"][0]["bidder"]["name_raw"] == "新兴铸管股份有限公司"
    assert award["lots"][0]["conditions"]["contract_pricing"]["value"] == "76.5000%"
    assert "price" not in award["lots"][0]["bid_results"][0] and "award_amount" not in award
