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


def archive_reviewed_html(path, response, source_url, selector, *, fetched_at, schema=None, charset="utf-8", notes=""):
    """归档公开原件并保留正文选取/编码；仅用于契约人工审查语料。"""
    import hashlib
    import html
    import yaml

    root = lxml_html.fromstring(response.decode(charset))
    selected = root.xpath(selector)
    if len(selected) != 1:
        raise ValueError("正文选择器必须准确命中一个元素")
    body = selected[0]
    for element in body.xpath(".//script|.//style|.//noscript"):
        element.drop_tree()
    title = (root.findtext(".//title") or "").strip()
    raw = "<html><body><h1>" + html.escape(title) + "</h1>" + lxml_html.tostring(body, encoding="unicode") + "</body></html>"
    path.mkdir(parents=True, exist_ok=True)
    (path / "response.html").write_bytes(response)
    (path / "raw.html").write_text(raw)
    meta = {"source_id": path.name, "source_url": source_url, "fetched_at": fetched_at,
            "rights_tag": "public-official-review", "review_mode": "manual", "body_selector": selector,
            "response_charset": charset, "notes": notes,
            "response_sha256": hashlib.sha256(response).hexdigest(), "raw_sha256": hashlib.sha256(raw.encode()).hexdigest()}
    if schema:
        meta["schema"] = schema
    (path / "meta.yaml").write_text(yaml.safe_dump(meta, allow_unicode=True, sort_keys=False))
    return ReviewedSample(path)


class ReviewedSample:
    """人工语料标注辅助；复用与检查器相同的文档加载、Span和证据生成。"""

    def __init__(self, path):
        import yaml
        from .corpus import _load_document
        from .extraction import ExtractionContext

        self.path = path
        self.meta = yaml.safe_load((path / "meta.yaml").read_text())
        self.doc = _load_document(path, self.meta)
        self.ctx = ExtractionContext(self.doc, "manual-review@0.1.0")

    def span(self, quote, occurrence=0):
        matches = []
        for unit in self.doc.units():
            start = 0
            while (pos := unit.text.find(quote, start)) >= 0:
                matches.append(unit.span(pos, pos + len(quote)))
                start = pos + max(1, len(quote))
        if not quote or occurrence < 0 or occurrence >= len(matches):
            raise ValueError(f"{self.path.name}: 找不到原文 {quote!r} 第{occurrence}处")
        return matches[occurrence]

    def prov(self, quote, occurrence=0):
        return self.ctx.prov(self.span(quote, occurrence), 1, "manual")

    def table_prov(self, row_label, quote):
        """限定到同一表格行，避免比例/金额在标题或其他行命中。"""
        for block in self.doc.blocks:
            if block.get("kind") != "table":
                continue
            for ri, row in enumerate(block["rows"]):
                if not any(cell["text"].strip() == row_label for cell in row):
                    continue
                for unit in self.doc.units():
                    if unit.block == block["index"] and unit.row == ri and unit.text.strip() == quote:
                        start = unit.text.index(quote)
                        return self.ctx.prov(unit.span(start, start + len(quote)), 1, "manual")
        raise ValueError(f"{self.path.name}: 找不到表格行 {row_label!r} 的值 {quote!r}")

    def text(self, quote):
        return {"value": quote, "prov": self.prov(quote)}

    def org(self, name, uscc=None, address=None):
        value = {"name_raw": name, "prov": self.prov(name)}
        if uscc:
            value.update(uscc=uscc, uscc_prov=self.prov(uscc))
        if address:
            value["address"] = self.text(address)
        return value

    def money(self, quote, value):
        return {"value": value, "currency": "CNY", "raw": quote, "prov": self.prov(quote)}

    def save(self, facts):
        import json
        (self.path / "reviewed.json").write_text(json.dumps(
            {"annotation_method": "manual", "facts": facts, "evidence": self.ctx.evidence},
            ensure_ascii=False, indent=2) + "\n")


def lifecycle_stage(config, fact, current=None):
    """本地契约评测：对JSON Schema条件求值；不执行聚合、持久化或重放。"""
    from jsonschema import Draft202012Validator

    if not Draft202012Validator(config.get("current_projection_filter", {})).is_valid(fact):
        return current
    rules = config["initial"] if current is None else config["transitions"]
    matches = []
    for rule in rules:
        if current is not None:
            origins = rule["from"] if isinstance(rule["from"], list) else [rule["from"]]
            if current not in origins:
                continue
        if fact.get(config["trigger_source"]["field"]) not in rule.get("trigger", []):
            continue
        condition = rule.get("when", {})
        Draft202012Validator.check_schema(condition)
        if Draft202012Validator(condition).is_valid(fact):
            matches.append(rule["to"])
    if len(matches) > 1:
        raise ValueError("生命周期条件重叠，不能隐式按顺序选取")
    return matches[0] if matches else current
