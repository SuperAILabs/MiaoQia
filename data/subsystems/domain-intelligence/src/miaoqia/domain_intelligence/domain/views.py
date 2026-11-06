"""A02 采购结果视图投影：公告事实版本 → 求值单位（packs/application_domains/tendering/policy/views.yaml 的 tendering.bid_result）。

形状与 detection 的求值单位一致（services/subsystems/detection/api/ports.go 的 Unit），供 data-api 的视图读取原样返回。
规则（与 views.yaml 一致，不推断）：
  - 每包每条结果独立求值：候选人公示的每名候选人；中标公告中 is_winner 为真、且非框架入围的每家中标人；
  - 框架第一阶段入围、未中标的投标人、只在附件中的结果不产生求值单位；
  - amount 只取 price_kind=total 的报价，年单价合计、单价、估算与未标明含义的报价都不进入金额；
  - 来源字段为空时视图中不出现该字段（表达式读取为未知）：采购项目、标的与品类来自项目聚合与品类增强，location 来自
    行政区划解析，org_id 来自 entity-graph 组织解析。固定标题主题只在查询已验证receipt/pp范围后回退，明确notice_subject，不表示实际供货。
"""
from __future__ import annotations

from decimal import Decimal, InvalidOperation

FACT_TYPE = "tendering.bid_result"
EVENT = "tendering.result.published.v1"
DOMAIN = "INF-PROC"


def _evidence(prov: dict | None) -> dict | None:
    if not prov:
        return None
    return {"confidence": prov.get("confidence", 0), "method": prov.get("method", "rule"), "suspect": False}


def _org(ref: dict) -> dict:
    out = {"name_raw": ref["name_raw"]}
    for key in ("org_id", "uscc"):
        if key in ref:
            out[key] = ref[key]
    return out


def _cny(money: dict) -> int | None:
    if money.get("currency") != "CNY":
        return None
    try:
        value = Decimal(money["value"])
    except (InvalidOperation, KeyError):
        return None
    return int(value) if value == value.to_integral_value() else None


def bid_result_units(fact: dict, topic_context: dict | None = None) -> list[dict]:
    notice_type = fact.get("notice_type")
    if notice_type not in ("award", "candidate"):
        return []
    all_results = [r for lot in fact.get("lots", []) for r in lot.get("bid_results", [])]
    topic = (topic_context if notice_type == "award" and len(fact.get("lots", [])) == 1 and len(all_results) == 1
             and all_results[0].get("stage") == "award" and all_results[0].get("is_winner") is True
             and all_results[0].get("result_kind", "contract_award") == "contract_award"
             and not fact.get("ending") and not fact.get("result_details_pending")
             and not any(lot.get("ending") for lot in fact.get("lots", [])) else None)
    units = []
    for lot in fact.get("lots", []):
        for result in lot.get("bid_results", []):
            if notice_type == "award":
                if result.get("is_winner") is not True or result.get("result_kind", "contract_award") != "contract_award":
                    continue
            elif result.get("stage") != "candidate":
                continue
            fields: dict = {"result_type": "candidate_published" if notice_type == "candidate" else "awarded",
                            "winner": _org(result["bidder"])}
            evidence = {}
            if (e := _evidence(result["bidder"].get("prov"))) is not None:
                evidence["fact.winner"] = e
            display = {"title": fact.get("title", {}).get("value", ""), "winner_name": result["bidder"]["name_raw"],
                       "lot_key": lot["lot_key"]}
            if "org_id" in result["bidder"]:
                display["winner_org_id"] = result["bidder"]["org_id"]
            price = result.get("price")
            if price is not None and result.get("price_kind") == "total":
                fields["amount"] = {"value": price["value"], "currency": price["currency"]}
                if (e := _evidence(price.get("prov"))) is not None:
                    evidence["fact.amount"] = e
                display.update(amount_value=price["value"], amount_currency=price["currency"])
                if (cny := _cny(price)) is not None:
                    display["amount_cny"] = cny
            if "buyer" in fact:
                fields["buyer"] = _org(fact["buyer"])
                display["buyer_name"] = fact["buyer"]["name_raw"]
                if "org_id" in fact["buyer"]:
                    display["buyer_org_id"] = fact["buyer"]["org_id"]
            if "location" in fact:
                fields["location"] = fact["location"]["code"]
                display["location"] = fact["location"]["code"]
            if "project_id" in fact:
                fields["project_id"] = fact["project_id"]
                display["procurement_project_id"] = fact["project_id"]
            if result.get("awarded_items"):
                fields["awarded_items"] = result["awarded_items"]
            fields["candidates"] = [{"stage": r["stage"], "bidder": _org(r["bidder"]), **({"rank": r["rank"]} if "rank" in r else {})}
                                    for r in lot.get("bid_results", [])]
            org_ids = [o["org_id"] for o in [fact.get("buyer", {}), result["bidder"], *result.get("consortium_members", [])]
                       if o.get("org_id")]
            if org_ids:
                fields["org_ids"] = org_ids
            refs = [{"name": "fact", "id": fact["id"], "version": fact["version"], "domain": DOMAIN}]
            if topic and not lot.get("items") and not result.get("awarded_items"):
                categories = topic["categories"]
                fields["categories"] = [c["category"]["code"] for c in categories]
                fields["categories_scope"] = "notice_subject"
                evidence["fact.categories"] = {"confidence": min(c["prov"]["confidence"] for c in categories), "method": "rule", "suspect": False}
                project = topic["project"]
                fields["project_id"] = project["id"]
                display["procurement_project_id"] = project["id"]
                refs.append({"name": "project", "id": project["id"], "version": project["version"], "domain": DOMAIN})
            units.append({
                "key": f"{FACT_TYPE}:{fact['id']}:{result['id']}", "fact_type": FACT_TYPE, "domain": DOMAIN,
                "subject": {"id": fact["id"], "version": fact["version"], "part_id": result["id"]}, "event": EVENT,
                "fields": fields, "items": {}, "item_domains": {}, "item_refs": refs, "evidence": evidence, "display": display,
            })
    return units
