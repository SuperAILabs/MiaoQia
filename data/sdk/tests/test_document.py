from miaoqia.sdk.document import Document

DOC = Document({
    "snapshot_ref": "snp_" + "0" * 26, "pipeline_version": "doc.html-dev@0.1.0", "media_type": "text_html",
    "blocks": [
        {"index": 0, "kind": "heading", "level": 1, "text": "某项目采购公告"},
        {"index": 1, "kind": "paragraph", "text": "项目编号：ABC-2026-001 项目名称：给水管网改造"},
        {"index": 2, "kind": "paragraph", "text": "预算金额：320 万元。"},
        {"index": 3, "kind": "table", "rows": [[{"text": "采购人"}, {"text": " 某市水务集团 "}]]},
    ],
})


def test_inline_label_stops_before_next_label():
    s = DOC.find_labeled(["项目编号"])
    assert (s.text, s.block, s.char_start, s.char_end) == ("ABC-2026-001", 1, 5, 17)
    assert DOC.quote(s.locator()) == s.text
    assert DOC.find_labeled(["项目名称"]).text == "给水管网改造"


def test_trailing_punctuation_trimmed():
    assert DOC.find_labeled(["预算金额"]).text == "320 万元"


def test_table_label_cell():
    s = DOC.find_labeled(["采购人名称", "采购人"])
    assert (s.text, s.block, s.row, s.col) == ("某市水务集团", 3, 0, 1)
    assert s.locator() == {"block": 3, "char_start": 1, "char_end": 7, "row": 0, "col": 1}
    assert DOC.quote(s.locator()) == "某市水务集团"


def test_absent_label():
    assert DOC.find_labeled(["代理机构"]) is None


def test_numbered_labels():
    doc = Document({"snapshot_ref": "snp_" + "0" * 26, "pipeline_version": "x@0.0.1", "media_type": "text_html", "blocks": [
        {"index": 0, "kind": "paragraph", "text": "（1）招标人：某项目经理部"},
        {"index": 1, "kind": "paragraph", "text": "6.2投标文件递交截止时间：2026年11月04日10时00分。"},
    ]})
    assert doc.find_labeled(["招标人"]).text == "某项目经理部"
    assert doc.find_labeled(["投标文件递交截止时间"]).text == "2026年11月04日10时00分"


def test_join_blocks_quote():
    doc = Document({"snapshot_ref": "snp_" + "0" * 26, "pipeline_version": "x@0.0.1", "media_type": "text_html", "blocks": [
        {"index": 0, "kind": "paragraph", "text": "某集团某项目"},
        {"index": 1, "kind": "paragraph", "text": "管材采购公开招标"},
    ]})
    s = doc.join_blocks(0, 1)
    assert s.text == "某集团某项目管材采购公开招标"
    assert s.locator() == {"block": 0, "char_start": 0, "char_end": 8, "block_end": 1}
    assert doc.quote(s.locator()) == s.text
