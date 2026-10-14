"""开发与测试用的最小 HTML 规范化：把 HTML 转为 NormalizedDocument。

生产环境的规范化由 evidence 的 document_pipeline 负责（正文定位、版面、附件、OCR 等）；
本实现只为让解析器能在本地语料上运行与评测，不作为生产依据。
"""
from __future__ import annotations

import re

from lxml import html as lxml_html

BLOCK_TAGS = {"h1": 1, "h2": 2, "h3": 3, "h4": 4, "h5": 5, "h6": 6}
SKIP = {"script", "style", "noscript", "head", "nav", "footer"}


def _text(el) -> str:
    return re.sub(r"[ \t\r\f\v ]+", " ", el.text_content()).strip()


def _span(td, attr: str) -> int:
    v = (td.get(attr) or "").strip()
    return int(v) if v.isdigit() and int(v) > 1 else 1


def _table_grid(table) -> list[list[dict]]:
    """展开 rowspan / colspan 为完整网格：覆盖位置 text 为空，covered_by 指向起始单元格。"""
    grid: dict[tuple[int, int], dict] = {}
    trs = table.xpath("./tr|./thead/tr|./tbody/tr|./tfoot/tr")
    for r, tr in enumerate(trs):
        c = 0
        for td in tr.xpath("./td|./th"):
            while (r, c) in grid:
                c += 1
            rs, cs = _span(td, "rowspan"), _span(td, "colspan")
            cell = {"text": _text(td)}
            if td.tag == "th":
                cell["header"] = True
            if rs > 1:
                cell["row_span"] = rs
            if cs > 1:
                cell["col_span"] = cs
            grid[(r, c)] = cell
            for dr in range(rs):
                for dc in range(cs):
                    if (dr, dc) != (0, 0):
                        grid[(r + dr, c + dc)] = {"text": "", "covered_by": {"row": r, "col": c}}
            c += cs
    if not grid:
        return []
    nrows = max(r for r, _ in grid) + 1
    ncols = max(c for _, c in grid) + 1
    return [[grid.get((r, c), {"text": ""}) for c in range(ncols)] for r in range(nrows)]


def html_to_document(html: str, snapshot_ref: str, source_url: str | None = None,
                     pipeline_version: str = "doc.html-dev@0.1.0") -> dict:
    root = lxml_html.fromstring(html)
    for bad in root.xpath("//script|//style|//noscript"):
        bad.drop_tree()
    blocks: list[dict] = []

    def add(block: dict) -> None:
        block["index"] = len(blocks)
        blocks.append(block)

    def walk(el) -> None:
        tag = el.tag if isinstance(el.tag, str) else ""
        if tag in SKIP:
            return
        if tag in BLOCK_TAGS:
            t = _text(el)
            if t:
                add({"kind": "heading", "level": BLOCK_TAGS[tag], "text": t})
            return
        if tag == "table":
            rows = _table_grid(el)
            if rows:
                add({"kind": "table", "rows": rows})
            return
        if tag in ("p", "li", "div") and not el.xpath("./p|./div|./table|./ul|./ol|./h1|./h2|./h3|./h4"):
            for line in re.split(r"\n+", el.text_content()):
                t = re.sub(r"[ \t\r\f\v ]+", " ", line).strip()
                if t:
                    add({"kind": "list_item" if tag == "li" else "paragraph", "text": t})
            return
        for child in el:
            walk(child)

    body = root.find("body")
    walk(body if body is not None else root)
    title = root.findtext(".//title")
    doc = {"snapshot_ref": snapshot_ref, "pipeline_version": pipeline_version, "media_type": "text_html", "blocks": blocks}
    if source_url:
        doc["source_url"] = source_url
    if title:
        doc["title"] = title.strip()
    return doc
