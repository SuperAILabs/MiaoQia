"""mqc check 的回归测试：向仓库副本注入各类错误，确认都被检出。"""
import json

import pytest

import mqc.cli as cli

CREDIT = "contracts/schemas/domains/credit/record.schema.json"


def run_check(root, monkeypatch, capsys):
    monkeypatch.setattr(cli, "find_root", lambda start=None: root)
    code = cli.main(["check"])
    return code, capsys.readouterr()


def test_repo_passes(sandbox, monkeypatch, capsys):
    code, _ = run_check(sandbox, monkeypatch, capsys)
    assert code == 0


@pytest.mark.parametrize("rule,mutate", [
    ("S3", lambda d: d["properties"].update({"x": {"type": ["string", "null"]}})),
    ("S5", lambda d: d["properties"].update({"x": {"type": "string", "pattern": "^qqq_[0-9A-HJKMNP-TV-Z]{26}$"}})),
    ("S6", lambda d: d["properties"]["record_type"]["enum"].append("Bad-Value")),
    ("S7", lambda d: d["properties"].update({"x": {"$ref": "../../common/types.schema.json#/$defs/nope"}})),
    ("S9", lambda d: d.pop("x-version")),
])
def test_schema_rules(sandbox, monkeypatch, capsys, rule, mutate):
    p = sandbox / CREDIT
    d = json.loads(p.read_text(encoding="utf-8"))
    mutate(d)
    p.write_text(json.dumps(d, ensure_ascii=False), encoding="utf-8")
    code, out = run_check(sandbox, monkeypatch, capsys)
    assert code == 1 and f" {rule} " in out.err


@pytest.mark.parametrize("rule,rel,old,new", [
    ("Y1", "packs/information_domains/procurement/lifecycle.yaml", "trigger:\n    - contract", "on:\n    - contract"),
    ("P2", "packs/information_domains/procurement/lifecycle.yaml", "to: accepted", "to: archived"),
    ("P0", "packs/application_domains/tendering/policy/templates/a01-direct-bid.yaml", "weight: 15", "weight: 0"),
    ("P0", "packs/application_domains/tendering/policy/templates/a01-direct-bid.yaml", "mode: immediate", "mode: hourly"),
    ("P4", "packs/application_domains/tendering/policy/templates/a01-direct-bid.yaml", "reason: BLOCKED_BUYER", "reason: NOT_A_CODE"),
    ("P6", "packs/application_domains/tendering/policy/views.yaml", "- tendering.deadline.changed.v1", "- tendering.deadline.moved.v1"),
    ("E2", "contracts/events/org/events.asyncapi.yaml", "name: org.change.recorded.v1", "name: credit.change.recorded.v1"),
    ("P8", "packs/information_domains/procurement/relationships.yaml", "to_path: lots[*].bid_results[*].awarded_items[*].brand", "to_path: lots[*].bid_results[*].awarded_items[*].brand_required"),
    ("P8", "packs/information_domains/procurement/relationships.yaml", "    to: brand\n", "    to: lot\n"),
    ("P8", "packs/information_domains/procurement/relationships.yaml", "        resolve: { from_path: part_of }\n", ""),
    ("P8", "packs/information_domains/investment_projects/relationships.yaml", "        to_path: investment_project_id\n\n", "        to_path: investment_project_id\n        resolve: { to_path: part_of }\n\n"),
    ("P8", "packs/information_domains/procurement/relationships.yaml", "qualifiers: [domain_fact, lot]\n    validity: event\n    sources:\n      - schema: contracts/schemas/domains/procurement/notice.schema.json\n        from_path: project_id", "qualifiers: [domain_fact]\n    validity: event\n    sources:\n      - schema: contracts/schemas/domains/procurement/notice.schema.json\n        from_path: project_id"),
    ("P8", "packs/information_domains/investment_projects/relationships.yaml", "- type: constructed_by", "- type: designed_by"),
    ("P8", "packs/information_domains/investment_projects/relationships.yaml", "    from: project\n    to: organization\n    qualifiers: [domain_fact]\n    validity: event\n    sources:\n      - schema: contracts/schemas/domains/investment/record.schema.json\n        from_path: investment_project_id\n        to_path: construction_permit.designer", "    from: domain_fact\n    to: organization\n    qualifiers: [domain_fact]\n    validity: event\n    sources:\n      - schema: contracts/schemas/domains/investment/record.schema.json\n        from_path: investment_project_id\n        to_path: construction_permit.designer"),
    ("P9", "packs/application_domains/tendering/coverage.yaml", "stage: M2, task: M2-P4, note: 首条验证旅程", "stage: M2, task: M2-P99, note: 首条验证旅程"),
    ("P9", "packs/application_domains/tendering/coverage.yaml", "stage: M2, task: M2-P4, note: 首条验证旅程", "stage: M3, task: M2-P4, note: 首条验证旅程"),
    ("P9", "packs/application_domains/tendering/pack.yaml", "    coverage: coverage.yaml\n", ""),
    ("P9", "packs/application_domains/tendering/coverage.yaml", "frameworks: [AF-TENDER-SUPPLY]\n", "frameworks: [AF-TENDER-NOPE]\n"),
    ("P9", "packs/application_domains/tendering/coverage.yaml", "entry: { defined: [TPL-TENDER-A02-SUPPLY] }", "entry: { defined: [TPL-TENDER-B03-COMPETE] }"),
    ("P9", "packs/application_domains/tendering/coverage.yaml", "research_templates: [RT-TENDER-MARKET]", "research_templates: []"),
    ("P9", "packs/application_domains/tendering/coverage.yaml", "objective: APP-TENDER.D02", "objective: APP-TENDER.D01"),
    ("P9", "packs/application_domains/tendering/coverage.yaml", "entry: { none: 由 A01 资格缺口衔接或用户发起，不由信号直接进入 }", "entry: { none: '' }"),
])
def test_text_rules(sandbox, monkeypatch, capsys, rule, rel, old, new):
    p = sandbox / rel
    s = p.read_text(encoding="utf-8")
    assert old in s
    p.write_text(s.replace(old, new, 1), encoding="utf-8")
    code, out = run_check(sandbox, monkeypatch, capsys)
    assert code == 1 and f" {rule} " in out.err


def test_bad_fixture_detected(sandbox, monkeypatch, capsys):
    p = sandbox / "contracts/tests/credit.record.yaml"
    s = p.read_text(encoding="utf-8")
    p.write_text(s.replace("  - name: 失信缺执行法院\n    valid: false", "  - name: 失信缺执行法院\n    valid: true", 1), encoding="utf-8")
    code, out = run_check(sandbox, monkeypatch, capsys)
    assert code == 1 and "应通过，实际被拒绝" in out.err


def test_missing_event_fixture_detected(sandbox, monkeypatch, capsys):
    (sandbox / "contracts/events/entity/fixtures.yaml").unlink()
    code, out = run_check(sandbox, monkeypatch, capsys)
    assert code == 1 and "E5" in out.err and "缺少事件载荷正反例" in out.err


def test_datetime_format_is_active():
    from jsonschema import Draft202012Validator
    v = Draft202012Validator({'type': 'string', 'format': 'date-time'},
                             format_checker=Draft202012Validator.FORMAT_CHECKER)
    assert v.is_valid('2026-10-14T08:00:00Z')
    assert not v.is_valid('not-a-time')
    assert not v.is_valid('2026-02-30T08:00:00Z')


def test_bad_api_fixture_detected(sandbox, monkeypatch, capsys):
    import yaml
    p = sandbox / 'contracts/apis/identity/fixtures.yaml'
    d = yaml.safe_load(p.read_text())
    d['fixtures'][0]['base']['tenant'] = 'other'
    p.write_text(yaml.safe_dump(d))
    code, out = run_check(sandbox, monkeypatch, capsys)
    assert code == 1 and '应通过，实际被拒绝' in out.err


def test_replace_missing_fixture_path_is_not_a_negative_control():
    from mqc.fixtures import _apply
    with pytest.raises(ValueError, match='目标不存在'):
        _apply({}, {'op':'replace','path':'/missing','value':1})


T = "packs/application_domains/tendering/"
IND = "packs/industries/building-materials/"


def test_repo_definitions_are_checked(sandbox, monkeypatch, capsys):
    """正例：M2-P2 交付的定义文件全部登记并通过 P9、P10 与行业包检查。"""
    code, out = run_check(sandbox, monkeypatch, capsys)
    assert code == 0, out.err
    assert "行业包检查：1 个包" in out.out


@pytest.mark.parametrize("rule,rel,old,new", [
    # P9 判断框架与目标、角色
    ("P9", T + "frameworks/af-tender-elig.yaml", "      when: features.deadline_days >= 0\n", ""),             # 硬门槛缺条件
    ("P9", T + "frameworks/af-tender-elig.yaml", "manual: [input, override]", "manual: [input, override]\n      kind: hard_gate"),
    ("P9", T + "frameworks/af-tender-elig.yaml", "id: AF-TENDER-ELIG", "id: AF-TENDER-BID"),             # 矩阵标为已定义的框架没有定义文件
    ("P9", T + "frameworks/af-tender-elig.yaml", "'!features.qualification_gap'", "'!features.no_such_feature'"),
    ("P9", T + "frameworks/af-tender-elig.yaml", "params.min_prep_days\n", "params.min_amount_cny\n"),  # A01 模板没有的参数
    ("P9", T + "frameworks/af-tender-supply.yaml", "has(fact.winner.org_id)", "has(fact.bid_deadline)"),  # A02 事实类型没有的字段
    ("P9", T + "frameworks/af-tender-supply.yaml", "tendering.contract.published.v1", "tendering.contract.signed.v1"),
    ("P9", T + "coverage.yaml", "AF-TENDER-ELIG:    { defined: [AF-TENDER-ELIG]", "AF-TENDER-ELIG:    { pending: { stage: M2, task: M2-P2 }, x: [AF-TENDER-ELIG]"),
    ("P9", T + "coverage.yaml", "release: { batch: R1, stage: M2 }", "release: { batch: R1, stage: M1 }"),
    ("P9", T + "coverage.yaml", "    PB-TENDER-A04:\n      objective: APP-TENDER.A04\n      form: transactional\n      release: { batch: R2, stage: M4 }",
     "    PB-TENDER-A04:\n      objective: APP-TENDER.A04\n      form: transactional\n      release: { batch: R2, stage: M3 }"),  # 进入策略待定到 M4，却在 M3 开放
    ("P9", T + "objectives/a02.yaml", "form: transactional", "form: continuous"),
    ("P9", T + "objectives/a02.yaml", "policy_templates: [TPL-TENDER-A02-SUPPLY]", "policy_templates: [TPL-TENDER-A01-BID]"),
    ("P9", T + "roles/dealer.yaml", "default_objectives: [APP-TENDER.A01", "default_objectives: [APP-TENDER.B03, APP-TENDER.A01"),  # B03 模板不含经销商
    ("P9", T + "roles/prime.yaml", "APP-TENDER.C03]", "APP-TENDER.E01]"),
    # P10 卡片
    ("P10", T + "cards/a02-signal.yaml", "investment.construction_permit: record.project_name.value", "investment.construction_permit: record.title.value"),
    ("P10", T + "cards/a02-signal.yaml", "source: fact.amount", "source: fact.budget"),
    ("P10", T + "cards/a02-signal.yaml", "source: signal.unknown_count", "source: signal.unknown_items"),
    ("P10", T + "cards/a02-signal.yaml", "{ action: create_pursuit, permission: can_assign }", "{ action: create_pursuit, permission: can_feedback }"),
    ("P10", T + "cards/a02-signal.yaml", "permission: can_feedback, reasons: quick }", "permission: can_feedback }"),
    ("P10", T + "cards/a02-signal.yaml", "    - { fact_type: investment.construction_permit, label: 施工许可 }\n", ""),
    ("P10", T + "cards/a02-row.yaml", "fact_types: [tendering.bid_result, tendering.contract_record, investment.construction_permit]",
     "fact_types: [tendering.bid_result, tendering.contract_record]"),
    ("P10", T + "cards/b03-signal.yaml", "    - { action: view, permission: can_view_signals }\n", "    - { action: view, permission: can_view_signals }\n    - { action: create_pursuit, permission: can_assign }\n"),
    ("P10", T + "cards/a01-signal.yaml", "when: 'fact.stage == \"intent\"'", "when: 'fact.winner.org_id == \"x\"'"),
    # P10 简报、反馈、结果、指标
    ("P10", T + "briefing.yaml", "quota: { default: 10, min: 5, max: 20 }, merge_window: 30m", "quota: { default: 30, min: 5, max: 20 }, merge_window: 30m"),
    ("P10", T + "briefing.yaml", "order: 3", "order: 2"),
    ("P10", T + "briefing.yaml", "card: CARD-TENDER-B03-SIGNAL", "card: CARD-TENDER-B03-ROW"),
    ("P10", T + "feedback/a02.yaml", "rules: [R-CAT]", "rules: [R-CATEGORY]"),
    ("P10", T + "feedback/a02.yaml", "lists: [blocked]", "lists: [partners]"),
    ("P10", T + "feedback/a02.yaml", "  quick: 5", "  quick: 9"),
    ("P10", T + "feedback/a02.yaml", "    - { code: OTHER, say: 其他, note_required: true }\n  relevant:", "  relevant:"),
    ("P10", T + "feedback/a01.yaml", "vocab: [exclude_terms]", "vocab: [focus_terms]"),
    ("P10", T + "outcomes/a02.yaml", "- { code: PAYMENT_RISK, say: 回款风险, relates_to: risk }", "- { code: TOO_SMALL, say: 回款风险, relates_to: risk }"),
    ("P10", T + "outcomes/a02.yaml", "objective: APP-TENDER.A02", "objective: APP-TENDER.B03"),
    ("P10", T + "metrics.yaml", "segment_by: [objective, value_nature, amount_type, currency, contribution]", "segment_by: [objective, amount_type, currency, contribution]"),
    ("P10", T + "metrics.yaml", "segment_by: [objective, value_nature, amount_type, currency, contribution]", "segment_by: [objective, value_nature, currency, contribution]"),
    ("P10", T + "metrics.yaml", "segment_by: [objective, value_nature, amount_type, currency, contribution]", "segment_by: [objective, value_nature, amount_type, contribution]"),
    ("P10", T + "metrics.yaml", "numerator_filter: { result_type: [success] }\n      denominator: 由跟进候选信号创建且已关闭的跟进数（成功、失败、放弃都计入）\n      source: [pursuit, outcome]\n      window: 30d\n      segment_by: [region, category]",
     "numerator_filter: { result_type: [success] }\n      denominator: 由跟进候选信号创建且已关闭的跟进数（成功、失败、放弃都计入）\n      source: [pursuit, outcome]\n      window: 30d\n      segment_by: [result_type]"),  # 按分子固定的维度分组
    ("P10", T + "metrics.yaml", "denominator_filter: { result_type: [success, failure] }\n      source: [outcome]\n      window: 30d\n      segment_by: [region, category]",
     "denominator_filter: { result_type: [success, failure] }\n      source: [outcome]\n      window: 30d\n      segment_by: [region, result_type]"),
    ("P10", T + "metrics.yaml", "      pending:\n        stage: M4\n", "      pending:\n        stage: M3\n"),          # 待定阶段与开始计算阶段不一致
    ("P10", T + "metrics.yaml", "      pending:\n        stage: M4\n", "      pending:\n        stage: M4\n        task: M2-P4\n"),  # 任务与阶段不一致
    ("P10", T + "metrics.yaml", "        basis: 跟进进入", "        why: 跟进进入"),                      # 缺计数依据
    ("P10", T + "metrics.yaml", "baseline: { pending: { stage: M2, task: M2-P10 } }", "baseline: { pending: { stage: M2, task: M2-P77 } }"),
    ("P10", T + "pack.yaml", "      - feedback/b03.yaml\n", ""),                   # M2 目标缺反馈原因
    ("P10", T + "pack.yaml", "      - outcomes/a01.yaml\n", ""),                   # M2 交易型目标缺结果原因
    # I1–I5 行业包
    ("I1", IND + "pack.yaml", "    defaults: defaults.yaml", "    defaults: defaults.yaml\n    policies: x.yaml"),
    ("I1", IND + "categories.yaml", "scheme: industry:building-materials", "scheme: industry:materials"),
    ("I1", IND + "qualifications.yaml", "kind: product_permit", "kind: permit"),
    ("I2", IND + "categories.yaml", "    - { code: CAT-PIPE-PE, name: PE 管, say: 聚乙烯管，含 HDPE、PE80、PE100 }\n", ""),
    ("I2", IND + "categories.yaml", "    - { code: CAT-PIPE-CU, name: 铜管 }", "    - { code: CAT-PIPE-CU, name: 铜管 }\n    - { code: CAT-PIPE-CU, name: 紫铜管 }"),
    ("I3", IND + "vocabulary.yaml", "category: CAT-PIPE-CU,", "category: CAT-PIPE-COPPER,"),
    ("I3", IND + "vocabulary.yaml", "synonyms: [紫铜管, 铜水管]", "synonyms: [紫铜管, 铜水管, 镀锌管]"),
    ("I3", IND + "vocabulary.yaml", "  brands_pending: { stage: M2, task: M2-P13, note: 品牌词表与带证据的归属声明由领域负责人逐条核对后交付 }\n", ""),
    ("I3", IND + "categories.yaml", "code: CAT-PIPE-CU, target: C3251", "code: CAT-PIPE-AL, target: C3251"),
    ("I4", IND + "qualifications.yaml", "code: Q-BM-ISO14001", "code: Q-BM-ISO9001"),
    ("I5", IND + "defaults.yaml", "    exclude_terms:", "    excluded_terms:"),
    ("I5", T + "policy/templates/a02-supply.yaml", "code: CAT-PIPE-PE }],\n                amount: { value: \"18000000\", currency: CNY }, winner: { org_id: org_p1",
     "code: CAT-PIPE-HDPE }],\n                amount: { value: \"18000000\", currency: CNY }, winner: { org_id: org_p1"),
])
def test_definition_rules(sandbox, monkeypatch, capsys, rule, rel, old, new):
    p = sandbox / rel
    s = p.read_text(encoding="utf-8")
    assert old in s, old
    p.write_text(s.replace(old, new, 1), encoding="utf-8")
    code, out = run_check(sandbox, monkeypatch, capsys)
    assert code == 1 and f" {rule} " in out.err, out.err


def test_ratio_metric_worked_example(sandbox):
    """比例指标的算例：2 条中标、2 条未中标、1 条放弃 → 中标率 50%；按分子固定的 result_type 分组会退化为 0%/100%。"""
    import yaml
    m = {x["id"]: x for x in yaml.safe_load((sandbox / T / "metrics.yaml").read_text(encoding="utf-8"))["metrics"]["items"]}
    closed = ["success", "success", "failure", "failure", "abandoned"]
    def rate(metric, rows):
        num = [x for x in rows if x in metric["numerator_filter"]["result_type"]]
        den = [x for x in rows if x in metric.get("denominator_filter", {}).get("result_type", rows)]
        return len(num) / len(den)
    assert rate(m["a01_win_rate"], closed) == 0.5
    assert rate(m["a02_contact_conversion"], closed[:4]) == 0.5
    assert {rate(m["a01_win_rate"], [x for x in closed if x == t]) for t in ("success", "failure")} == {1.0, 0.0}
    assert "result_type" not in m["a01_win_rate"]["segment_by"]
    assert m["a01_bid_count"]["pending"]["stage"] == "M4" and "a01_pursuits_created" in m


def test_industry_codes_unique_across_packs(sandbox, monkeypatch, capsys):
    """category.any_match 只比较代码：另一个行业包不能定义同名或上下级代码。"""
    d = sandbox / "packs/industries/water"
    d.mkdir()
    (d / "pack.yaml").write_text("pack:\n  id: IND-WATER\n  kind: industry\n  version: 0.1.0\n  platform_api: \">=1.0 <2.0\"\n"
                                 "  owners: [x]\n  provides:\n    taxonomy: categories.yaml\n", encoding="utf-8")
    (d / "categories.yaml").write_text("taxonomy:\n  scheme: industry:water\n  scheme_version: 0.1.0\n  categories:\n"
                                       "    - { code: CAT-WATER, name: 水务设备 }\n", encoding="utf-8")
    code, out = run_check(sandbox, monkeypatch, capsys)
    assert code == 0, out.err
    s = (d / "categories.yaml").read_text(encoding="utf-8")
    (d / "categories.yaml").write_text(s + "    - { code: CAT-PIPE-PE-HOT, name: 热水管 }\n", encoding="utf-8")
    code, out = run_check(sandbox, monkeypatch, capsys)
    assert code == 1 and " I2 " in out.err
