"""Canonical inert notice documents for browser projection v1."""

import json
from collections import Counter

from lxml import etree, html

from miaoqia.sdk.derivation import canonical
from miaoqia.sdk.document import Document
from miaoqia.sdk.source_html import serialize
from miaoqia.sdk.testing import html_to_document
from miaoqia.sdk.validation import validate_extraction

PROJECTION_VERSION = 1
_SNAPSHOT_REF = "snp_" + "0" * 26
_PIPELINE_VERSION = "doc.browser@1.0.0"
_MEDIA_TYPE = "text_html"
MAX_SELECTED_HTML_BYTES = 256 * 1024
MAX_PROJECTION_BYTES = 1024 * 1024
_MAX_ELEMENTS = 4096
_MAX_DEPTH = 64
_MAX_BLOCKS = 4096
_MAX_SPAN = 128
_MAX_GRID_CELLS = 16_384

_DROP = frozenset({
    "applet", "audio", "base", "button", "canvas", "datalist", "embed", "footer", "form",
    "frame", "frameset", "iframe", "input", "link", "math", "meta", "nav", "noscript", "object",
    "optgroup", "option", "plaintext", "portal", "script", "select", "style", "svg", "template",
    "textarea", "video", "xmp",
})
_SPAN_ATTRS = ("rowspan", "colspan")
_DOCUMENT_KEYS = {"snapshot_ref", "pipeline_version", "media_type", "blocks"}
_BLOCK_KEYS = {
    "heading": {"index", "kind", "level", "text"},
    "paragraph": {"index", "kind", "text"},
    "list_item": {"index", "kind", "text"},
    "table": {"index", "kind", "rows"},
}


def _inert_tree(selected_html: str):
    if not isinstance(selected_html, str) or len(selected_html) > MAX_SELECTED_HTML_BYTES:
        raise ValueError("invalid or oversized selected HTML")
    source = selected_html.encode("utf-8")
    if len(source) > MAX_SELECTED_HTML_BYTES:
        raise ValueError("selected HTML exceeds byte cap")
    parser = html.HTMLParser(encoding="utf-8", no_network=True, remove_comments=True, remove_pis=True)
    try:
        root = html.document_fromstring(source, parser=parser)
    except etree.ParserError as exc:
        raise ValueError("invalid selected HTML") from exc
    if parser.error_log.filter_from_fatals():
        raise ValueError("incomplete HTML parse")

    depth = 0
    elements = []
    for event, node in etree.iterwalk(root, events=("start", "end")):
        if event == "end":
            depth -= 1
            continue
        depth += 1
        if depth > _MAX_DEPTH or len(elements) == _MAX_ELEMENTS:
            raise ValueError("selected HTML exceeds element or depth cap")
        elements.append(node)

    for node in reversed(elements):
        if node.tag in _DROP:
            node.drop_tree()
            continue
        spans = {}
        if node.tag in {"th", "td"}:
            for attr in _SPAN_ATTRS:
                digits = (node.get(attr) or "").strip().lstrip("0")
                if digits.isascii() and digits.isdecimal():
                    if len(digits) > len(str(_MAX_SPAN)) or int(digits) > _MAX_SPAN:
                        raise ValueError("HTML table span exceeds cap")
                    spans[attr] = digits
        node.attrib.clear()
        node.attrib.update(spans)
    return root


def _grid_bound(table) -> int:
    # Use the SDK's direct rows and cells. Previous row spans can occupy at most
    # their combined widths in each row, even if their placements leave gaps.
    rows = table.xpath("./tr|./thead/tr|./tbody/tr|./tfoot/tr")
    width = height = active_width = writes = 0
    ending_width = Counter()
    for row_index, row in enumerate(rows):
        active_width -= ending_width[row_index]
        row_width = active_width
        for cell in row.xpath("./td|./th"):
            rowspan = int(cell.get("rowspan", "1"))
            colspan = int(cell.get("colspan", "1"))
            height = max(height, row_index + rowspan)
            row_width += colspan
            writes += rowspan * colspan
            if rowspan > 1:
                active_width += colspan
                ending_width[row_index + rowspan] += colspan
        width = max(width, row_width)
    return max(writes, height * width)


def _bounded_integer(value, minimum: int, maximum: int) -> bool:
    return type(value) is int and minimum <= value <= maximum


def _validate_table(rows, remaining_cells: int) -> int:
    if not isinstance(rows, list) or not rows or not isinstance(rows[0], list) or not rows[0]:
        raise ValueError("invalid projection table")
    height, width = len(rows), len(rows[0])
    if height * width > remaining_cells:
        raise ValueError("projection exceeds table grid cap")
    covered = Counter()
    expected = {}
    has_text = False
    for r, row in enumerate(rows):
        if not isinstance(row, list) or len(row) != width:
            raise ValueError("invalid projection table row")
        for c, cell in enumerate(row):
            if not isinstance(cell, dict) or not isinstance(cell.get("text"), str):
                raise ValueError("invalid projection cell")
            if "covered_by" in cell:
                origin = cell["covered_by"]
                if (cell.keys() != {"text", "covered_by"} or cell["text"] != ""
                        or not isinstance(origin, dict) or origin.keys() != {"row", "col"}
                        or not _bounded_integer(origin["row"], 0, r)
                        or not _bounded_integer(origin["col"], 0, c)):
                    raise ValueError("invalid projection cell origin")
                covered[origin["row"], origin["col"]] += 1
                continue
            if cell.keys() - {"text", "header", "row_span", "col_span"} or cell.get("header", True) is not True:
                raise ValueError("invalid projection cell fields")
            has_text = has_text or bool(cell["text"].strip())
            for key, limit in (("row_span", height - r), ("col_span", width - c)):
                if key in cell and not _bounded_integer(cell[key], 2, min(limit, _MAX_SPAN)):
                    raise ValueError("invalid projection cell span")
            area = cell.get("row_span", 1) * cell.get("col_span", 1)
            if area > 1:
                expected[r, c] = area - 1
    # The SDK checks pointer geometry. Counts also require every spanned slot to exist.
    if covered != expected:
        raise ValueError("incomplete projection table spans")
    if not has_text:
        raise ValueError("projection table has no text")
    return height * width


def _validate(document) -> None:
    if (not isinstance(document, dict) or document.keys() - {"title"} != _DOCUMENT_KEYS
            or document["snapshot_ref"] != _SNAPSHOT_REF or document["pipeline_version"] != _PIPELINE_VERSION
            or document["media_type"] != _MEDIA_TYPE or not isinstance(document.get("title", ""), str)
            or not isinstance(document["blocks"], list) or not 1 <= len(document["blocks"]) <= _MAX_BLOCKS):
        raise ValueError("invalid browser projection document")
    remaining_cells = _MAX_GRID_CELLS
    for index, block in enumerate(document["blocks"]):
        if not isinstance(block, dict) or not isinstance(block.get("kind"), str):
            raise ValueError("invalid projection block")
        kind = block["kind"]
        if block.keys() != _BLOCK_KEYS.get(kind) or not _bounded_integer(block["index"], index, index):
            raise ValueError("invalid projection block fields")
        if kind == "table":
            remaining_cells -= _validate_table(block["rows"], remaining_cells)
        elif not isinstance(block["text"], str) or not block["text"].strip():
            raise ValueError("invalid projection block text")
        elif kind == "heading" and not _bounded_integer(block["level"], 1, 6):
            raise ValueError("invalid projection heading level")
    if validate_extraction(Document(document), [], {}):
        raise ValueError("invalid normalized projection structure")


def build_projection(selected_html: str) -> bytes:
    """Normalize a bounded, caller-selected notice into canonical UTF-8 JSON."""
    root = _inert_tree(selected_html)
    if sum(_grid_bound(table) for table in root.iter("table")) > _MAX_GRID_CELLS:
        raise ValueError("selected HTML exceeds table grid cap")
    document = html_to_document(serialize(root), _SNAPSHOT_REF, pipeline_version=_PIPELINE_VERSION)
    _validate(document)
    data = canonical(document)
    if len(data) > MAX_PROJECTION_BYTES:
        raise ValueError("projection exceeds byte cap")
    return data


def read_projection(data: bytes) -> dict:
    """Verify projection v1 bytes before exposing the normalized document."""
    if not isinstance(data, bytes) or len(data) > MAX_PROJECTION_BYTES:
        raise ValueError("invalid or oversized browser projection")
    try:
        document = json.loads(data.decode("utf-8"))
    except (ValueError, RecursionError) as exc:
        raise ValueError("invalid projection JSON") from exc
    _validate(document)
    # Round-trip byte equality also rejects duplicate keys at any nesting level.
    if canonical(document) != data:
        raise ValueError("noncanonical projection JSON")
    return document
