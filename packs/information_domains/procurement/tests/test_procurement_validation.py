"""结构合法仍可能发生语义错误：真实映射与负向变体验证。"""
import copy
from functools import partial
import json
from pathlib import Path

import pytest
import yaml

from miaoqia.sdk.corpus import load_reviewed_sample
from miaoqia.sdk.validation import validate_extraction as validate_integrity
from miaoqia_pack_procurement.validation import validate

ROOT = Path(__file__).resolve().parents[4]
CORPUS = ROOT / "packs/information_domains/procurement/corpus"


def _review(name):
    return load_reviewed_sample(CORPUS / f"official-{name}-20261014")


validate_extraction = partial(validate_integrity, semantic_validator=validate)


def test_awarded_and_failed_lots_are_independent():
    doc, facts, evidence = _review("award")
    assert not validate_extraction(doc, facts, evidence)
    fact = facts[0]
    assert [lot["lot_key"] for lot in fact["lots"] if lot.get("ending")] == ["5"]
    assert len([lot for lot in fact["lots"] if lot.get("bid_results")]) == 4
    fact["lots"][0]["ending"] = copy.deepcopy(fact["lots"][-1]["ending"])
    assert any("互相矛盾" in error for error in validate_extraction(doc, facts, evidence))



def test_duplicate_lot_and_candidate_winner_rejected():
    doc, facts, evidence = _review("candidate")
    facts[0]["lots"][0]["bid_results"][0]["is_winner"] = True
    assert any("候选人" in e for e in validate_extraction(doc, facts, evidence))
    facts[0]["lots"].append(copy.deepcopy(facts[0]["lots"][0]))
    assert any("标包号重复" in e for e in validate_extraction(doc, facts, evidence))



def test_page_and_scanned_attachment_delivery_conflict_is_preserved():
    _, page, _ = _review("contract-forestry")
    _, scanned, _ = _review("contract-attachment")
    assert page[0]["contract"]["conditions"]["delivery_period"]["value"] == "自合同签订之日起30天"
    assert "15个工作日" in scanned[0]["contract"]["conditions"]["delivery_period"]["value"]
    for facts in (page, scanned):
        assert "CONTRACT_TERM_CONFLICT" in {w["code"] for w in facts[0]["quality"]["warnings"]}



def test_real_amount_amendment_checks_target_type_prior_value_and_version(monkeypatch):
    from miaoqia.sdk.corpus import _validator
    from miaoqia_pack_procurement.validation import validate_amendment

    monkeypatch.chdir(CORPUS.parents[3])
    validator, schema = _validator("domains/procurement/notice.schema.json")
    _, before, _ = _review("award-amended-original")
    _, amended, _ = _review("amendment")
    change = amended[0]["amendment"]["changes"][0]
    target = before[0]
    registry = validator._registry
    assert not validate_amendment(target, change, schema, registry, 1, 1)
    assert change["after"]["value"] == "2865430"
    equivalent = copy.deepcopy(target)
    equivalent["award_amount"]["raw"] = "284.863万元"
    assert not validate_amendment(equivalent, change, schema, registry, 1, 1)
    assert validate_amendment(target, change, schema, registry, 1, 2) == ["AMENDMENT_STALE_VERSION"]
    bad = copy.deepcopy(change)
    bad["after"]["value"] = {"arbitrary": "payload"}
    assert validate_amendment(target, bad, schema, registry, 1, 1) == ["AMENDMENT_VALUE_SCHEMA_MISMATCH"]
    bad = copy.deepcopy(change)
    bad["before"]["value"] = "1"
    assert validate_amendment(target, bad, schema, registry, 1, 1) == ["AMENDMENT_BEFORE_MISMATCH"]
    bad = copy.deepcopy(change)
    immutable = copy.deepcopy(change)
    immutable["field_path"] = "/id"
    assert validate_amendment(target, immutable, schema, registry, 1, 1) == ["AMENDMENT_IMMUTABLE_FIELD"]
    bad["field_path"] = "/unknown"
    assert validate_amendment(target, bad, schema, registry, 1, 1) == ["AMENDMENT_UNKNOWN_FIELD"]



def test_nested_deadline_amendment_preserves_timezone_and_schema(monkeypatch):
    from miaoqia.sdk.corpus import _validator
    from miaoqia_pack_procurement.validation import validate_amendment

    monkeypatch.chdir(CORPUS.parents[3])
    validator, schema = _validator("domains/procurement/notice.schema.json")
    _, amended, _ = _review("amendment-deadline")
    change = amended[0]["amendment"]["changes"][0]
    target = {"lots": [{"deadlines": [{"at": change["before"]}]}]}
    assert not validate_amendment(target, change, schema, validator._registry, 1, 1)
    assert change["after"]["value"] == "2026-06-10T01:30:00Z"
    bad = copy.deepcopy(change)
    bad["field_path"] = "/lots/-1/deadlines/0/at"
    assert validate_amendment(target, bad, schema, validator._registry, 1, 1) == ["AMENDMENT_INVALID_POINTER"]



def test_lifecycle_accepts_real_cold_start_stages_without_fake_predecessors():
    config = yaml.safe_load((CORPUS.parent / "lifecycle.yaml").read_text())["lifecycle"]
    from miaoqia.sdk.testing import lifecycle_stage
    for name, want in [("award", "awarded"), ("candidate", "candidate_published"),
                       ("contract", "contracted"), ("acceptance-yantai", "accepted"),
                       ("multiwinner", "shortlisted")]:
        _, facts, _ = _review(name)
        assert lifecycle_stage(config, facts[0]) == want
    for rule in config["transitions"]:
        if "amendment" in rule.get("trigger", []):
            from_states = rule["from"] if isinstance(rule["from"], list) else [rule["from"]]
            assert not {"awarded", "contracted", "accepted"} & set(from_states)



def test_real_retryable_failure_is_not_permanent_termination():
    doc, facts, evidence = _review("failure")
    assert not validate_extraction(doc, facts, evidence)
    assert facts[0]["notice_type"] == "failure"
    assert facts[0]["ending"]["ending_type"] == "failed_bid"
    assert "重新采购" in facts[0]["ending"]["reason"]["value"]



@pytest.mark.parametrize("result,want", [("failed", "acceptance_failed"),
    ("partial", "partially_accepted"), (None, "acceptance_pending")])
def test_synthetic_acceptance_mutations_do_not_report_passed(result, want):
    # 负向变体不是已采集的真实失败验收样本。
    from miaoqia.sdk.testing import lifecycle_stage
    config = yaml.safe_load((CORPUS.parent / "lifecycle.yaml").read_text())["lifecycle"]
    _, facts, _ = _review("acceptance-yantai")
    if result is None:
        facts[0]["acceptance"].pop("result")
    else:
        facts[0]["acceptance"]["result"] = result
    assert lifecycle_stage(config, facts[0]) == want
    assert lifecycle_stage(config, facts[0], "contracted") == want
    facts[0]["acceptance"]["result"] = "passed"
    assert lifecycle_stage(config, facts[0], want) == "accepted"



def test_consortium_and_same_lot_shortlists_preserve_semantics():
    doc, facts, evidence = _review("consortium")
    assert not validate_extraction(doc, facts, evidence)
    result = facts[0]["lots"][0]["bid_results"][0]
    assert result["bidder"]["uscc"] == "91321283MADWT8A11R"
    assert result["consortium_members"][0]["name_raw"] == "江苏移动信息系统集成有限公司"
    result["consortium_members"].append(copy.deepcopy(result["consortium_members"][0]))
    assert any("成员重复" in e for e in validate_extraction(doc, facts, evidence))
    doc, facts, evidence = _review("multiwinner")
    assert not validate_extraction(doc, facts, evidence)
    assert [len(lot["bid_results"]) for lot in facts[0]["lots"]] == [9, 10]
    assert [x["rank"] for x in facts[0]["lots"][1]["bid_results"]].count(5) == 2
    facts[0]["lots"][0]["bid_results"][0]["is_winner"] = True
    assert any("第二阶段成交" in e for e in validate_extraction(doc, facts, evidence))



def test_real_same_purchase_has_five_actual_winners_and_unit_sum_quotes():
    from miaoqia.sdk.testing import lifecycle_stage
    doc, facts, evidence = _review("ordinary-multiwinner")
    assert not validate_extraction(doc, facts, evidence)
    fact = facts[0]
    results = fact["lots"][0]["bid_results"]
    assert len(results) == 5 and all(x["is_winner"] for x in results)
    assert all(x["result_kind"] == "contract_award" for x in results)
    assert all(x["price_kind"] == "annual_unit_sum" for x in results)
    assert results[2]["price"]["value"] == "2858738.75"
    config = yaml.safe_load((CORPUS.parent / "lifecycle.yaml").read_text())["lifecycle"]
    assert lifecycle_stage(config, fact) == "awarded"
    results[1]["id"] = results[0]["id"]
    assert any("标识重复" in e for e in validate_extraction(doc, facts, evidence))



def test_real_judgment_failed_inspection_does_not_override_current_project():
    from miaoqia.sdk.testing import lifecycle_stage
    doc, facts, evidence = _review("failed-acceptance-judgment")
    assert not validate_extraction(doc, facts, evidence)
    fact = facts[0]
    assert fact["acceptance"]["result"] == "failed"
    assert "不能作为认定" in fact["acceptance"]["dispute"]["value"]
    outcomes = fact["acceptance"]["judicial_outcome"]
    assert any("解除双方之间的买卖合同，理由不成立" in item["value"] for item in outcomes)
    assert any("3611400元" in item["value"] and "10%质保金理由不成立" in item["value"] for item in outcomes)
    for item in [fact["acceptance"]["dispute"], *outcomes]:
        assert doc.quote(evidence[item["prov"]["evidence_ref"]]["locator"]) == item["value"]
    assert "不合格" in doc.quote(evidence[fact["acceptance"]["result_prov"]["evidence_ref"]]["locator"])
    codes = {x["code"] for x in fact["quality"]["warnings"]}
    assert "FAILED_ACCEPTANCE_NOT_TERMINATION" in codes
    config = yaml.safe_load((CORPUS.parent / "lifecycle.yaml").read_text())["lifecycle"]
    assert lifecycle_stage(config, fact) is None
    assert lifecycle_stage(config, fact, "contracted") == "contracted"

