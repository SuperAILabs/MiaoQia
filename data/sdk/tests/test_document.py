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
