import sys
from pathlib import Path

import yaml

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
