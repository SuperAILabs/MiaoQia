import sys
from pathlib import Path

import yaml
import pytest

from miaoqia.sdk.corpus import check

HTML = """<html><head><title>某项目采购公告</title></head><body>
<h1>某项目采购公告（合成样本）</h1>
<p>项目名称：给水管网改造工程管材采购</p>
<p>预算金额：320 万元</p>
<table><tr><td>采购人</td><td>某市水务集团</td></tr></table>
<p>投标截止时间：2026年11月20日 09:30</p>
<p>联系人：张三丰 联系电话：0571-88886666 手机 13912345678 邮箱 zhang@example.com</p>
</body></html>"""


def _sample(root: Path, html: str) -> Path:
    s = root / "synthetic-0001"
    s.mkdir(parents=True)
    (s / "raw.html").write_text(html, encoding="utf-8")
    (s / "meta.yaml").write_text(yaml.safe_dump({"source_url": "https://example.invalid/1", "rights_tag": "synthetic"}), encoding="utf-8")
    (s / "expected.yaml").write_text(yaml.safe_dump({"facts": [{"fact_type": "tendering.notice", "fields": {
        "/title/value": "给水管网改造工程管材采购",
        "/buyer/name_raw": "某市水务集团",
        "/budget/value": "3200000",
        "/lots/0/deadlines/0/at/value": "2026-11-20T01:30:00Z",
    }}]}, allow_unicode=True), encoding="utf-8")
    return root


def test_check_pipeline_validates_schema_and_scores_fields(tmp_path, monkeypatch, capsys):
    monkeypatch.chdir(Path(__file__).resolve().parents[3])          # 仓库根目录，用于加载 contracts/schemas
    sys.path.insert(0, str(Path(__file__).parent))
    corpus = _sample(tmp_path, HTML)
    code = check(corpus, "demo.extractor:DemoNoticeExtractor", "domains/procurement/notice.schema.json", 1.0)
    out = capsys.readouterr().out
    assert code == 0, out
    assert "字段准确率 100.0%（4/4）" in out


def test_merged_cells_expand_to_grid_and_resolve_to_origin():
    from miaoqia.sdk.document import Document
    from miaoqia.sdk.testing import html_to_document
    html = """<table><tr><td>序号</td><td>包件号</td><td>数量</td><td>保证金（万元）</td></tr>
    <tr><td>1</td><td rowspan="2">SH-01</td><td>5</td><td rowspan="2">20</td></tr>
    <tr><td>2</td><td>834</td></tr>
    <tr><td colspan="2">合计</td><td>839</td><td></td></tr></table>"""
    data = html_to_document(html, "snp_" + "0" * 26)
    rows = data["blocks"][0]["rows"]
    assert all(len(r) == 4 for r in rows)                                   # 完整网格
    assert rows[2][1] == {"text": "", "covered_by": {"row": 1, "col": 1}}
    grid = Document(data).table_grid(0)
    assert grid[2][1].text == "SH-01" and (grid[2][1].row, grid[2][1].col) == (1, 1)   # 解析到起始单元格
    assert grid[2][2].text == "834" and grid[3][3] is None


def test_real_procurement_contact_evidence_points_to_each_value(monkeypatch):
    """公开联系人各字段可独立回原文，验证原文定位及哈希。"""
    import hashlib

    from miaoqia.sdk import Document, ExtractionContext
    from miaoqia.sdk.testing import html_to_document

    root = Path(__file__).resolve().parents[3]
    pack = root / "packs/information_domains/procurement"
    monkeypatch.syspath_prepend(str(pack / "src"))
    from miaoqia_pack_procurement.extractors.tender_notice import TenderNoticeExtractor

    raw = (pack / "corpus/crcc18-g228-concrete-202610/raw.html").read_text()
    doc = Document(html_to_document(raw, "snp_" + "0" * 26))
    extractor = TenderNoticeExtractor()
    ctx = ExtractionContext(doc, f"{extractor.id}@{extractor.version}")
    contacts = extractor.extract(doc, ctx).facts[0]["contacts"]
    checked = set()
    for contact in contacts:
        for field in ("name", "phone", "email"):
            if field not in contact:
                continue
            evidence = ctx.evidence[contact[field + "_prov"]["evidence_ref"]]
            quote = doc.quote(evidence["locator"])
            assert quote == contact[field]
            assert evidence["quote_hash"] == "sha256:" + hashlib.sha256(quote.encode()).hexdigest()
            checked.add(field)
    assert checked == {"name", "phone", "email"}


def test_reviewed_corpus_has_all_requested_stages_and_no_fabricated_dates(monkeypatch):
    import json

    root = Path(__file__).resolve().parents[3]
    monkeypatch.chdir(root)
    corpus = root / "packs/information_domains/procurement/corpus"
    assert check(corpus, None, "domains/procurement/notice.schema.json", None, reviewed=True) == 0
    types = set()
    for sample in corpus.glob("official-*"):
        annotation = json.loads((sample / "reviewed.json").read_text())
        types.update(f["notice_type"] for f in annotation["facts"])
    assert {"award", "candidate", "amendment", "contract", "acceptance"} <= types
    missing_date = json.loads((corpus / "official-acceptance-yantai-20261014/reviewed.json").read_text())["facts"][0]
    assert "accepted_at" not in missing_date["acceptance"]
    assert missing_date["acceptance"]["result"] == "passed"
    split_page = json.loads((corpus / "official-acceptance-20261014/reviewed.json").read_text())["facts"]
    assert len(split_page) == 6
    assert len({f["acceptance"]["supplier"]["name_raw"] for f in split_page}) == 6


def test_reviewed_corpus_rejects_modified_original(monkeypatch, tmp_path, capsys):
    import shutil

    root = Path(__file__).resolve().parents[3]
    monkeypatch.chdir(root)
    source = root / "packs/information_domains/procurement/corpus/official-contract-20261014"
    target = tmp_path / source.name
    shutil.copytree(source, target)
    with (target / "response.html").open("ab") as f:
        f.write(b"changed")
    assert check(tmp_path, None, "domains/procurement/notice.schema.json", None, reviewed=True) == 1
    assert "原件哈希不符" in capsys.readouterr().out


def test_empty_corpus_and_empty_gold_cannot_pass(monkeypatch, tmp_path):
    root = Path(__file__).resolve().parents[3]
    monkeypatch.chdir(root)
    monkeypatch.syspath_prepend(str(Path(__file__).parent))
    assert check(tmp_path, "demo.extractor:DemoNoticeExtractor", "domains/procurement/notice.schema.json", 1.0) == 1
    corpus = _sample(tmp_path, HTML)
    (corpus / "synthetic-0001/expected.yaml").write_text("facts: []\n")
    assert check(corpus, "demo.extractor:DemoNoticeExtractor", "domains/procurement/notice.schema.json", 1.0) == 1


def test_multi_schema_review_checks_every_group_and_propagates_failure(monkeypatch, tmp_path):
    from miaoqia.sdk import corpus as module
    seen = []
    def validate(path, extractor, schema, accuracy, reviewed, export):
        seen.append(schema)
        return 1 if schema == "change" else 0
    monkeypatch.setattr(module, "check", validate)
    assert module.main(["check", str(tmp_path), "--reviewed", "--schema", "registration", "--schema", "change", "--schema", "abnormal"]) == 1
    assert seen == ["registration", "change", "abnormal"]


def test_domain_semantics_cannot_be_skipped_by_corpus(monkeypatch, tmp_path, capsys):
    """重复标包的结构仍合法，但正式corpus路径必须执行采购包语义校验。"""
    import copy
    import json
    import shutil
    root = Path(__file__).resolve().parents[3]
    monkeypatch.chdir(root)
    source = root / "packs/information_domains/procurement/corpus/official-award-20261014"
    target = tmp_path / source.name
    shutil.copytree(source, target)
    review = json.loads((target / "reviewed.json").read_text())
    review["facts"][0]["lots"].append(copy.deepcopy(review["facts"][0]["lots"][0]))
    (target / "reviewed.json").write_text(json.dumps(review))
    assert check(tmp_path, None, "domains/procurement/notice.schema.json", None, reviewed=True) == 1
    assert "标包号重复" in capsys.readouterr().out


def test_semantic_registration_missing_or_ambiguous_fails_closed(monkeypatch, tmp_path):
    import pytest
    from miaoqia.sdk import corpus as module
    monkeypatch.setattr(module, "_repo_root", lambda: tmp_path)
    pack = tmp_path / "packs/information_domains/example"
    (pack / "src").mkdir(parents=True)
    registration = {"pack": {"provides": {"schemas": ["contracts/schemas/example.json"]}}}
    (pack / "pack.yaml").write_text(yaml.safe_dump(registration))
    with pytest.raises(ValueError, match="未登记"):
        module._semantic_validator("example.json")
    registration["pack"]["provides"]["semantic_validator"] = "absent.module:validate"
    (pack / "pack.yaml").write_text(yaml.safe_dump(registration))
    with pytest.raises(ValueError, match="入口不存在"):
        module._semantic_validator("example.json")
    other = pack.parent / "other"
    other.mkdir()
    (other / "pack.yaml").write_text(yaml.safe_dump(registration))
    with pytest.raises(ValueError, match="多个领域包"):
        module._semantic_validator("example.json")


def test_invalid_evidence_does_not_reach_domain_callback():
    from miaoqia.sdk import Document
    from miaoqia.sdk.testing import html_to_document
    from miaoqia.sdk.validation import validate_extraction
    from unittest.mock import Mock
    doc = Document(html_to_document("<p>通用文本</p>", "snp_" + "0" * 26))
    callback = Mock(return_value=[])
    assert validate_extraction(doc, [{"evidence_ref": "ev_missing"}], {}, semantic_validator=callback)
    callback.assert_not_called()


def _registered_validator(tmp_path, monkeypatch, source):
    from miaoqia.sdk import corpus as module
    pack = tmp_path / 'packs/information_domains/example'
    (pack / 'src').mkdir(parents=True)
    # Each test has a distinct import name, so module caches cannot mask failures.
    name = 'validator_' + tmp_path.name.replace('-', '_')
    (pack / 'src' / (name + '.py')).write_text(source)
    (pack / 'pack.yaml').write_text(yaml.safe_dump({'pack': {'provides': {
        'schemas': ['contracts/schemas/domains/example.json'],
        'semantic_validator': name + ':validate',
    }}}))
    monkeypatch.setattr(module, '_repo_root', lambda: tmp_path)
    return module, name


def test_validator_signature_and_origin_are_checked(monkeypatch, tmp_path):
    import pytest
    from types import ModuleType
    module, name = _registered_validator(tmp_path, monkeypatch, 'def validate(doc):\n    return []\n')
    with pytest.raises(ValueError, match='document/facts/evidence'):
        module._semantic_validator('domains/example.json')
    # 签名合规后才会导入模块，来源校验需用合规入口单独验证。
    (tmp_path / 'packs/information_domains/example/src' / (name + '.py')).write_text(
        'def validate(document, facts, evidence):\n    return []\n')
    shadow = ModuleType(name)
    shadow.__file__ = str(tmp_path / 'shadow.py')
    monkeypatch.setitem(sys.modules, name, shadow)
    with pytest.raises(ValueError, match='来源'):
        module._semantic_validator('domains/example.json')


def test_semantic_callback_must_return_error_list():
    import pytest
    from miaoqia.sdk import Document
    from miaoqia.sdk.testing import html_to_document
    from miaoqia.sdk.validation import validate_extraction
    doc = Document(html_to_document('<p>valid</p>', 'snp_' + '0' * 26))
    for result in (None, False, {}, '', [None]):
        with pytest.raises(ValueError, match='错误字符串列表'):
            validate_extraction(doc, [], {}, semantic_validator=lambda *_: result)
    assert validate_extraction(doc, [], {}, semantic_validator=lambda *_: []) == []


def test_unregistered_domain_schema_fails_closed(monkeypatch, tmp_path):
    import pytest
    from miaoqia.sdk import corpus as module
    monkeypatch.setattr(module, '_repo_root', lambda: tmp_path)
    with pytest.raises(ValueError, match='没有登记所有者'):
        module._semantic_validator('domains/unowned.json')
    assert module._semantic_validator('common/types.schema.json') is None


@pytest.mark.parametrize('source,message', [
    ('''def wrap(fn):
    async def replacement(document, facts, evidence):
        raise AssertionError("入口加载不应执行函数")
    return replacement

@wrap
def validate(document, facts, evidence):
    return []
''', '同步函数'),
    ('''from functools import wraps

def wrap(fn):
    @wraps(fn)
    def replacement(document):
        raise AssertionError("入口加载不应执行函数")
    return replacement

@wrap
def validate(document, facts, evidence):
    return []
''', 'document/facts/evidence'),
    ('''def validate(document, facts, evidence):
    return []

async def replacement(document, facts, evidence):
    raise AssertionError("入口加载不应执行函数")

validate = replacement
''', '同步函数'),
    ('''def validate(document, facts, evidence):
    return []

def replacement(document):
    raise AssertionError("入口加载不应执行函数")

validate = replacement
''', 'document/facts/evidence'),
    ('''def validate(document, facts, evidence):
    return []

async def replacement(document, facts, evidence):
    yield "入口加载不应执行函数"

validate = replacement
''', '同步函数'),
])
def test_loaded_entry_rejects_decorators_and_rebindings(monkeypatch, tmp_path, source, message):
    from miaoqia.sdk.entrypoints import resolve_semantic_validator
    module, name = _registered_validator(tmp_path, monkeypatch, source)
    pack = tmp_path / 'packs/information_domains/example'
    # 静态登记有效；真正调用的函数必须在加载后被拒绝，而非等到运行语料才失败。
    resolve_semantic_validator(pack, name + ':validate')
    with pytest.raises(ValueError, match=message):
        module._semantic_validator('domains/example.json')


def test_loaded_entry_allows_compatible_sync_wrapper(monkeypatch, tmp_path):
    source = '''from functools import wraps

def wrap(fn):
    @wraps(fn)
    def replacement(*args, **kwargs):
        return fn(*args, **kwargs)
    return replacement

@wrap
def validate(document, facts, evidence):
    return []
'''
    module, _ = _registered_validator(tmp_path, monkeypatch, source)
    callback = module._semantic_validator('domains/example.json')
    assert callback(None, [], {}) == []
