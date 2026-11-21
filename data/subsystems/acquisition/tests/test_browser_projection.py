import json

import pytest

from miaoqia.acquisition.api import browser_projection
from miaoqia.acquisition.api.browser_projection import build_projection, read_projection


NOTICE = """<html><head><title>采购公告</title></head><body><article>
<h1>采购</h1><p>正文 <a href="https://example.invalid/attachment">附件标签</a></p>
<ul><li>第一项</li></ul>
<table><tr><th rowspan="2" colspan="2">品目</th><td>甲</td><td></td></tr>
<tr><td>乙</td></tr></table></article></body></html>"""


def test_structured_notice_round_trips_as_canonical_utf8():
    data = build_projection(NOTICE)
    document = read_projection(data)
    assert browser_projection.PROJECTION_VERSION == 1
    assert document == {
        "snapshot_ref": "snp_" + "0" * 26,
        "pipeline_version": "doc.browser@1.0.0",
        "media_type": "text_html",
        "title": "采购公告",
        "blocks": [
            {"index": 0, "kind": "heading", "level": 1, "text": "采购"},
            {"index": 1, "kind": "paragraph", "text": "正文 附件标签"},
            {"index": 2, "kind": "list_item", "text": "第一项"},
            {"index": 3, "kind": "table", "rows": [
                [{"text": "品目", "header": True, "row_span": 2, "col_span": 2},
                 {"text": "", "covered_by": {"row": 0, "col": 0}}, {"text": "甲"}, {"text": ""}],
                [{"text": "", "covered_by": {"row": 0, "col": 0}},
                 {"text": "", "covered_by": {"row": 0, "col": 0}}, {"text": "乙"}, {"text": ""}],
            ]},
        ],
    }
    assert data == json.dumps(document, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()
    equivalent = NOTICE.replace('rowspan="2"', 'rowspan="002"').replace("附件标签", "&#38468;件标签")
    assert build_projection(equivalent) == data


def test_active_subtrees_and_attributes_are_discarded():
    source = """<base href="https://example.invalid/"><meta http-equiv="refresh" content="0">
    <div onclick="handler()" style="display:none" data-locator="secret">
    <p>开始<script>discard-script</script><style>discard-style</style>
    <a href="https://example.invalid/" target="_blank">正文</a><iframe src="/embedded">discard-frame</iframe>
    <svg><title>discard-svg</title></svg><img src="/image" onerror="handler()">
    <template>discard-template</template><textarea>discard-textarea</textarea>
    <object data="/object">discard-object</object><!-- discard-comment -->
    </p><form action="/submit"><p>discard-form</p></form><nav><p>discard-nav</p></nav>
    <table><tr><td style="color:red">表格<script>discard-cell-script</script></td></tr></table>
    <p>&lt;b&gt;作为文本&lt;/b&gt;</p></div>"""
    data = build_projection(source)
    clean = "<div><p>开始\n正文</p><table><tr><td>表格</td></tr></table><p>&lt;b&gt;作为文本&lt;/b&gt;</p></div>"
    assert data == build_projection(clean)
    assert read_projection(data)["blocks"][-1]["text"] == "<b>作为文本</b>"


@pytest.mark.parametrize("source, reason", [
    ("<p>" + "汉" * 100_000 + "</p>", "byte cap"),
    ("<div>" + "<p>x</p>" * 4096 + "</div>", "element or depth cap"),
    ("<div>" * 65 + "<p>x</p>" + "</div>" * 65, "element or depth cap"),
    ("<div>" * 1000 + "x" + "</div>" * 1000, "incomplete HTML parse"),
    ('<table><tr><td rowspan="129">x</td></tr></table>', "span exceeds cap"),
    ('<table><tr><td colspan="' + "9" * 5000 + '">x</td></tr></table>', "span exceeds cap"),
    ('<table><tr><td colspan="128">x</td></tr>' + "<tr><td>y</td></tr>" * 128 + "</table>", "grid cap"),
    ('<table><tr><td rowspan="128" colspan="128">x</td></tr></table>' * 2, "grid cap"),
])
def test_input_budget_rejects_before_normalization(source, reason, monkeypatch):
    def unexpected_normalization(*args, **kwargs):
        pytest.fail("normalization ran before input budget rejection")

    monkeypatch.setattr(browser_projection, "html_to_document", unexpected_normalization)
    with pytest.raises(ValueError, match=reason):
        build_projection(source)


def test_overlapping_spans_cannot_produce_an_inconsistent_grid():
    source = ('<table><tr><td>A</td><td rowspan="2">B</td></tr>'
              '<tr><td colspan="2">C</td></tr></table>')
    with pytest.raises(ValueError, match="projection table spans"):
        build_projection(source)


@pytest.mark.parametrize("source, blocks, reason", [
    ("<title>采购公告</title><article><p> </p><script>ignored</script></article>", [],
     "invalid browser projection document"),
    ("<table><tr><td></td><td>&nbsp;</td></tr></table>",
     [{"index": 0, "kind": "table", "rows": [[{"text": ""}, {"text": " "}]]}], "table has no text"),
])
def test_rejects_documents_without_body_content(source, blocks, reason):
    with pytest.raises(ValueError, match=reason):
        build_projection(source)

    document = read_projection(build_projection(NOTICE))
    document["blocks"] = blocks
    data = json.dumps(document, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()
    with pytest.raises(ValueError, match=reason):
        read_projection(data)


@pytest.mark.parametrize("mutate", [
    lambda data: b" " + data,
    lambda data: data + b"\n",
    lambda data: data[:-1],
    lambda data: b"\xff" + data[1:],
    lambda data: data.decode().encode("utf-16"),
    lambda data: data.replace(b'"index":0', b'"index":0,"index":0', 1),
    lambda data: data.replace(b'"index":0', b'"index":-0', 1),
    lambda data: data.replace("采购".encode(), rb"\u91c7\u8d2d", 1),
    lambda data: b"[" * 2000 + b"]" * 2000,
    lambda data: b" " * (1024 * 1024 + 1),
])
def test_rejects_invalid_or_noncanonical_bytes(mutate):
    with pytest.raises(ValueError):
        read_projection(mutate(build_projection(NOTICE)))


@pytest.mark.parametrize("modify", [
    lambda doc: doc.update(source_url="https://example.invalid/"),
    lambda doc: doc.update(attachments=[]),
    lambda doc: doc.update(snapshot_ref="snp_" + "1" * 26),
    lambda doc: doc.update(pipeline_version="doc.browser@2.0.0"),
    lambda doc: doc.update(media_type="plain_text"),
    lambda doc: doc.update(title=None),
    lambda doc: doc["blocks"][0].update(index=2),
    lambda doc: doc["blocks"][0].update(index=False),
    lambda doc: doc["blocks"][0].update(level=1.0),
    lambda doc: doc["blocks"][0].update(page=1),
    lambda doc: doc["blocks"][1].update(kind="html"),
    lambda doc: doc["blocks"][3]["rows"][0].pop(),
    lambda doc: doc["blocks"][3]["rows"][0][0].update(row_span=129),
    lambda doc: doc["blocks"][3]["rows"][0][1].pop("covered_by"),
    lambda doc: doc["blocks"][3]["rows"][0][1]["covered_by"].update(col=1),
    lambda doc: doc["blocks"][3]["rows"][0][1].update(header=True),
    lambda doc: doc["blocks"][3]["rows"][0][2].update(text=float("nan")),
    lambda doc: doc["blocks"][3].update(rows=[[{"text": ""}] * 129] * 129),
    lambda doc: doc.update(blocks=doc["blocks"] * 1025),
])
def test_rejects_manipulated_projection_structure(modify):
    document = read_projection(build_projection(NOTICE))
    modify(document)
    data = json.dumps(document, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()
    with pytest.raises(ValueError):
        read_projection(data)
