"""结构合法仍可能发生语义错误：真实映射与负向变体验证。"""
import copy
from functools import partial
from pathlib import Path


from miaoqia.sdk.corpus import load_reviewed_sample
from miaoqia.sdk.validation import validate_extraction as validate_integrity
from miaoqia_pack_organizations.validation import validate

ROOT = Path(__file__).resolve().parents[4]
CORPUS = ROOT / "packs/information_domains/organizations/corpus"


validate_extraction = partial(validate_integrity, semantic_validator=validate)


def test_enterprise_real_fields_do_not_invent_status_or_confuse_shareholder():
    sample = CORPUS / "official-enterprise-henan-20261014"
    doc, facts, evidence = load_reviewed_sample(sample)
    reviewed = {"facts": facts, "evidence": evidence}
    fact = reviewed["facts"][0]
    assert not validate_extraction(doc, [fact], reviewed["evidence"])
    assert "status" not in fact
    assert fact["subject"]["name_raw"] == "河南垂天智能制造有限公司"
    assert fact["shareholders"][0]["org"]["name_raw"] == "河南垂天科技有限公司"
    assert fact["registered_capital"]["value"] == "100000000"
    for shareholder in fact["shareholders"]:
        ref = reviewed["evidence"][shareholder["share_ratio_prov"]["evidence_ref"]]
        assert "row" in ref["locator"]
        row = doc.blocks[ref["locator"]["block"]]["rows"][ref["locator"]["row"]]
        assert any(cell["text"] == shareholder["org"]["name_raw"] for cell in row)
    fact["legal_representative"]["name"] = "错误姓名"
    assert any("证据不一致" in e for e in validate_extraction(doc, [fact], reviewed["evidence"]))



def test_real_branch_approval_does_not_imply_completed_cancellation():
    root = CORPUS
    def read(name):
        path = root / f"official-{name}-20261014"
        doc, facts, evidence = load_reviewed_sample(path)
        review = {"facts": facts, "evidence": evidence}
        assert not validate_extraction(doc, review["facts"], review["evidence"])
        return doc, review
    _, review = read("branch")
    assert len(review["facts"]) == 4
    assert all("status" not in fact for fact in review["facts"])
    assert all(fact["parent"]["uscc"] == "91440101MA5CXB8B85" for fact in review["facts"])
    doc, cancelled = read("cancelled")
    assert cancelled["facts"][0]["status"] == "cancelled"
    fact = cancelled["facts"][0]
    fact["parent"] = copy.deepcopy(fact["subject"])
    assert any("自身" in e for e in validate_extraction(doc, [fact], cancelled["evidence"]))
    _, change = read("change")
    assert "changed_on" not in change["facts"][0]
    assert change["facts"][0]["before"]["value"] == "新疆帕戈郎食品有限公司"
    _, abnormal = read("abnormal")
    assert len(abnormal["facts"]) == 3
    assert all(fact["reason_code"] == "unreachable_address" for fact in abnormal["facts"])
    assert all("removed_on" not in fact for fact in abnormal["facts"])

