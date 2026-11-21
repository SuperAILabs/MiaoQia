"""HTML parsing for source handlers with site-specific selection rules."""
from lxml import html
from lxml.etree import ParserError

HtmlElement = html.HtmlElement


def parse_document(source: str) -> HtmlElement:
    try:
        return html.document_fromstring(source)
    except ParserError as exc:
        raise ValueError("response has no HTML document") from exc


def element(tag: str) -> HtmlElement:
    return html.Element(tag)


def serialize(node: HtmlElement, *, with_tail: bool = True) -> str:
    return html.tostring(node, encoding="unicode", with_tail=with_tail)
