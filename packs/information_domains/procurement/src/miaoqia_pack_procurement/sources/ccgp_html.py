"""Static HTTP discovery for the central CCGP list and Hubei's homepage tender tab."""
from __future__ import annotations

import re

from miaoqia.sdk.source_html import HtmlElement, parse_document, serialize

from miaoqia.acquisition.api.handlers import DetailFetch, DetailRef, FetchRequest, FetchResponse, ListPage, Window

from . import html_text
from .ccgp_http import (checked_document, checked_html, official_url, publication_date, require_official,
                        response_html, visible_text)

_CENTRAL_LIST = "/cggg/zygg/gkzb/"
_CENTRAL_PAGER = re.compile(rf"{_CENTRAL_LIST}index(?:_([1-9]\d*))?\.htm")
_CENTRAL_DETAIL = re.compile(rf"{_CENTRAL_LIST}20\d{{4}}/t20\d{{6}}_(\d+)\.htm")
_HUBEI_DETAIL = re.compile(r"/notice/20\d{4}/notice_([0-9a-fA-F]{32})\.html")
_CENTRAL_ROWS = '//ul[contains(concat(" ", normalize-space(@class), " "), " c_list_bid ")]/li'
_HAS_CLASS = 'contains(concat(" ", normalize-space(@class), " "), $token)'
_CENTRAL_PAGER_SCRIPT = re.compile(
    r"\s*Pager\(\{\s*size\s*:\s*([1-9]\d{0,4})\s*,\s*current\s*:\s*(0|[1-9]\d{0,4})\s*,"
    r"\s*prefix\s*:\s*'index'\s*,\s*suffix\s*:\s*'htm'\s*\}\)\s*;?\s*"
)


def _central_main(doc: HtmlElement) -> HtmlElement:
    main = doc.xpath(f"//*[{_HAS_CLASS}]", token=" vF_detail_main ")
    if len(main) != 1:
        raise ValueError("central detail lacks a unique announcement container")
    heading = main[0].xpath(f".//*[{_HAS_CLASS}]//h2", token=" vF_detail_header ")
    body = main[0].xpath(f".//*[{_HAS_CLASS}]", token=" vF_detail_content ")
    if not any(map(visible_text, heading)) or len(body) != 1 or not visible_text(body[0]):
        raise ValueError("central detail lacks announcement title or body")
    return main[0]


def central_tender_html(raw: bytes) -> str:
    """Keep the announcement heading, summary table and body for normalization."""
    main = _central_main(checked_html(raw.decode("utf-8")))
    return serialize(main, with_tail=False)


class _StaticNotices:
    windowed = False
    origin: str
    detail_path: re.Pattern[str]

    def _identity(self, base: str, href: str) -> tuple[str, str] | None:
        if not href:
            return None
        parsed = official_url(self.origin, base, href)
        if parsed is None or parsed.query or parsed.fragment:
            return None
        match = self.detail_path.fullmatch(parsed.path)
        if not match:
            return None
        return match[1], self.origin + parsed.path

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        identity = self._identity(self.origin, ref.source_url)
        if identity is None or identity[0] != ref.source_key:
            raise ValueError("detail reference does not match its official notice route")
        key, url = identity
        return (DetailFetch(key, FetchRequest("GET", url)),)

    def _detail(self, response: FetchResponse) -> HtmlElement:
        require_official(self.origin, response)
        doc = response_html(response)
        if self._identity(self.origin, response.url) is None:
            raise ValueError("response is outside the official notice route")
        return doc


class CentralPublicTenders(_StaticNotices):
    source_id = "ccgp-central-public-tenders"
    origin = "https://www.ccgp.gov.cn"
    channels = ("central-open-tenders",)
    detail_path = _CENTRAL_DETAIL

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or page < 1:
            raise ValueError("invalid central list channel or page")
        suffix = "" if page == 1 else f"index_{page - 1}.htm"
        return FetchRequest("GET", self.origin + _CENTRAL_LIST + suffix)

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        final = require_official(self.origin, response)
        doc = parse_document(html_text(response))
        scripts = doc.xpath(f"//*[{_HAS_CLASS}]//script[not(@src)]/text()", token=" pager ")
        pager_calls = [match for script in scripts if (match := _CENTRAL_PAGER_SCRIPT.fullmatch(script))]
        checked_document(doc)
        pager = _CENTRAL_PAGER.fullmatch(final.path)
        if final.path != _CENTRAL_LIST and pager is None:
            raise ValueError("response is outside the central tender list")
        current = int(pager[1]) if pager and pager[1] else 0
        refs: dict[str, DetailRef] = {}
        for row in doc.xpath(_CENTRAL_ROWS):
            anchors = row.xpath("./a[@href]")
            if not anchors:
                continue
            anchor = anchors[0]
            identity = self._identity(response.url, anchor.get("href"))
            if identity is None:
                continue
            key, url = identity
            if key in refs:
                continue
            title = visible_text(anchor)
            published = re.search(r"发布时间\s*[：:]\s*(\d{4}-\d{2}-\d{2})", visible_text(row))
            if not title or published is None:
                raise ValueError("central list row lacks title or published date")
            refs[key] = DetailRef(key, url, publication_date(published[1]), title)
        if not refs:
            raise ValueError("central list contains no recognized notices")

        if pager_calls:
            if len(pager_calls) != 1:
                raise ValueError("central list contains conflicting page metadata")
            size, advertised = map(int, pager_calls[0].groups())
            if advertised != current or current >= size:
                raise ValueError("central list page does not match the published pager")
            if current + 1 < size:
                return ListPage(tuple(refs.values()), True)
            return ListPage(tuple(refs.values()), False, coverage_gap="rolling_archive_boundary")

        for href in doc.xpath("//a[@href]/@href"):
            link = official_url(self.origin, response.url, href)
            if link is None or link.query or link.fragment:
                continue
            next_page = _CENTRAL_PAGER.fullmatch(link.path)
            if next_page and next_page[1] == str(current + 1):
                return ListPage(tuple(refs.values()), True)
        return ListPage(tuple(refs.values()), False, coverage_gap="pagination_unverified")

    def check_detail(self, response: FetchResponse) -> None:
        _central_main(self._detail(response))


class HubeiGovernmentProcurement(_StaticNotices):
    source_id = "hubei-government-procurement-homepage"
    origin = "https://www.ccgp-hubei.gov.cn"
    channels = ("homepage-tenders",)
    detail_path = _HUBEI_DETAIL

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or page != 1:
            raise ValueError("Hubei discovery is limited to the homepage tender tab")
        return FetchRequest("GET", self.origin + "/")

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        final = require_official(self.origin, response)
        doc = response_html(response)
        if final.path != "/":
            raise ValueError("response is outside the Hubei homepage")
        refs: dict[str, DetailRef] = {}
        for anchor in doc.xpath('//*[@id="area-1-2"]//a[@href]'):
            identity = self._identity(response.url, anchor.get("href"))
            if identity is None:
                continue
            key, url = identity
            title = visible_text(anchor)
            if not title:
                raise ValueError("Hubei list row lacks title")
            row_text = " ".join(anchor.xpath("ancestor::li[1]//text()[not(ancestor::a)]"))
            published = publication_date(row_text)
            refs.setdefault(key, DetailRef(key, url, published, title))
        if not refs:
            raise ValueError("Hubei homepage tender tab contains no recognized notices")
        return ListPage(tuple(refs.values()), False, coverage_gap="homepage_current_window")

    def check_detail(self, response: FetchResponse) -> None:
        _hubei_detail(self._detail(response))


def _hubei_detail(doc: HtmlElement) -> HtmlElement:
    details = doc.xpath(f"//*[{_HAS_CLASS}]", token=" con_info ")
    if len(details) != 1:
        raise ValueError("Hubei detail lacks a unique announcement container")
    bodies = details[0].xpath(f".//*[{_HAS_CLASS}]", token=" art_con ")
    if len(bodies) != 1 or not any(map(visible_text, details[0].xpath(".//h1|.//h2"))) or not any(
        marker in visible_text(bodies[0]) for marker in ("采购方式", "项目编号")
    ):
        raise ValueError("Hubei detail lacks announcement title or body")
    return details[0]


def hubei_tender_html(raw: bytes) -> str:
    return serialize(_hubei_detail(checked_html(raw.decode("utf-8"))), with_tail=False)
