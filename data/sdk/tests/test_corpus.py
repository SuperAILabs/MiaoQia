import sys
from pathlib import Path

import yaml

from miaoqia.sdk.corpus import check, lint, redact

HTML = """<html><head><title>某项目采购公告</title></head><body>
<h1>某项目采购公告（合成样本）</h1>
<p>项目名称：给水管网改造工程管材采购</p>
<p>预算金额：320 万元</p>
<table><tr><td>采购人</td><td>某市水务集团</td></tr></table>
<p>投标截止时间：2026年11月20日 09:30</p>
<p>联系人：张三丰 联系电话：0571-88886666 手机 13912345678 邮箱 zhang@example.com</p>
</body></html>"""


def test_redact_masks_personal_information_but_keeps_amounts():
    out, n = redact(HTML)
    assert n >= 4
    for leaked in ("张三丰", "0571-88886666", "13912345678", "zhang@example.com"):
        assert leaked not in out
    assert "320 万元" in out and "2026年11月20日 09:30" in out
    assert redact(out) == (out, 0)                                  # 幂等：已脱敏文本不再命中


def _sample(root: Path, html: str, redacted: bool = True) -> Path:
    s = root / "synthetic-0001"
    s.mkdir(parents=True)
    (s / "raw.html").write_text(html, encoding="utf-8")
    (s / "meta.yaml").write_text(yaml.safe_dump({"source_url": "https://example.invalid/1", "redacted": redacted,
                                                 "rights_tag": "synthetic"}), encoding="utf-8")
    (s / "expected.yaml").write_text(yaml.safe_dump({"facts": [{"fact_type": "tendering.notice", "fields": {
        "/title/value": "给水管网改造工程管材采购",
        "/buyer/name_raw": "某市水务集团",
        "/budget/value": "3200000",
        "/lots/0/deadlines/0/at/value": "2026-11-20T01:30:00Z",
    }}]}, allow_unicode=True), encoding="utf-8")
    return root


def test_lint_blocks_unredacted_samples(tmp_path):
    assert lint([_sample(tmp_path / "a", HTML)])
    assert lint([_sample(tmp_path / "b", redact(HTML)[0])]) == []
    assert lint([_sample(tmp_path / "c", redact(HTML)[0], redacted=False)])


def test_check_pipeline_validates_schema_and_scores_fields(tmp_path, monkeypatch, capsys):
    monkeypatch.chdir(Path(__file__).resolve().parents[3])          # 仓库根目录，用于加载 contracts/schemas
    sys.path.insert(0, str(Path(__file__).parent))
    corpus = _sample(tmp_path, redact(HTML)[0])
    code = check(corpus, "demo.extractor:DemoNoticeExtractor", "domains/procurement/notice.schema.json", 1.0)
    out = capsys.readouterr().out
    assert code == 0, out
    assert "字段准确率 100.0%（4/4）" in out
