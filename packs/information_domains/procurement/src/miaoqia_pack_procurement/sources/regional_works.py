"""Staged HTTP protocols for regional construction and trading notices.

Sources without verified pagination support only the observed entry page.
The reference catalog defines protocol inputs, while tests use synthetic notices.
"""

from __future__ import annotations

import json
import re
from collections import Counter
from datetime import date, datetime
from hashlib import sha256
from html import escape
from urllib.parse import SplitResult, parse_qs, urlencode, urlsplit

from miaoqia.sdk.source_html import HtmlElement, serialize

from miaoqia.acquisition.api.handlers import (
    DetailFetch, DetailRef, FetchRequest, FetchResponse, GroupedNotice, GroupSelector, ListPage, NoticeGroup,
    OffsetPage, SourceBlocked, Window,
)

from .ccgp_http import checked_html, official_url, publication_date, require_official, response_html, visible_text
from . import DiscoveryOnly, html_text
from .html_boards import CONTENT_CLASS, HtmlNoticeBoard
from .trade_html import EwbConstructionBoard, native_id

_UUID = r"[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}"
_COMPACT_DATE = re.compile(r"^(20\d{2})(\d{2})(\d{2})(?!\d)")
_NOTICE_FIELDS = ("项目编号", "招标人", "投标人", "中标人", "中标单位", "中标候选人", "变更内容", "更正内容")


def _date(value: object) -> date | None:
    if not isinstance(value, str):
        return None
    try:
        compact = _COMPACT_DATE.match(value.strip())
        return date(*(int(part) for part in compact.groups())) if compact else publication_date(value)
    except ValueError:
        return None


def _json_object(value: object) -> dict:
    if isinstance(value, str):
        value = json.loads(value)
    if not isinstance(value, dict):
        raise ValueError("expected a JSON object")
    return value


def _json_rows(value: object) -> list[dict]:
    if isinstance(value, str):
        value = json.loads(value)
    if not isinstance(value, list) or any(not isinstance(row, dict) for row in value):
        raise ValueError("expected a JSON array of records")
    return value


def _json_response(response: FetchResponse, *, accept_html_label: bool = False) -> dict:
    content = response.body.decode("utf-8-sig")
    if response.media_type in ("text/html", "text/plain") and content.lstrip().startswith("<"):
        checked_html(content)
        raise ValueError("received HTML at a JSON endpoint")
    if response.media_type not in ("application/json", "text/plain") and not (
        accept_html_label and response.media_type == "text/html"
    ):
        raise ValueError("unexpected media type at a JSON endpoint")
    result = _json_object(content)
    for field in ("code", "status"):
        if result.get(field) in (401, 403, "401", "403"):
            raise SourceBlocked(f"api_{result[field]}")
    if result.get("success") is False:
        raise ValueError("source API returned failure")
    return result


def _unique_refs(candidates: list[DetailRef]) -> tuple[DetailRef, ...]:
    by_url: dict[str, list[DetailRef]] = {}
    for ref in candidates:
        by_url.setdefault(ref.source_url, []).append(ref)
    counts = Counter(key for refs in by_url.values() for key in {ref.source_key for ref in refs})
    results = []
    for refs in by_url.values():
        first = refs[0]
        key = first.source_key
        if counts[key] > 1 or any(ref.source_key != key for ref in refs):
            key = None
        results.append(DetailRef(key, first.source_url, first.published, first.title))
    return tuple(results)


class _FirstPage:
    windowed = False
    origin: str
    list_path: str
    coverage_gap = "pagination_unverified"

    def _check_page(self, channel: str, page: int) -> None:
        if channel not in self.channels or page != 1:
            raise ValueError("only the first referenced listing request is supported")

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        self._check_page(channel, page)
        return FetchRequest("GET", self.origin + self.list_path)

    def _check_list(self, channel: str, response: FetchResponse) -> None:
        self._check_page(channel, 1)
        if require_official(self.origin, response).path != self.list_path:
            raise ValueError("response is outside the official listing route")

    def _page(self, refs: list[DetailRef]) -> ListPage:
        return ListPage(_unique_refs(refs), False, self.coverage_gap)


class _HtmlDetail(_FirstPage):
    detail_path: re.Pattern[str]

    def _detail_url(self, base: str, href: str) -> str | None:
        if not href or any(ord(char) <= 32 or ord(char) == 127 for char in href):
            return None
        url = official_url(self.origin, base, href)
        if url is None or url.query or url.fragment or not self.detail_path.fullmatch(url.path):
            return None
        return self.origin + url.path

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        self._check_page(channel, 1)
        url = self._detail_url(self.origin, ref.source_url)
        if url is None:
            raise ValueError("detail reference is outside the official notice route")
        return (DetailFetch(ref.source_key or "body", FetchRequest("GET", url)),)

    def check_detail(self, response: FetchResponse) -> None:
        require_official(self.origin, response)
        doc = response_html(response)
        if self._detail_url(self.origin, response.url) is None:
            raise ValueError("response is outside the official notice route")
        self._check_body(doc)

    def _check_body(self, doc: HtmlElement) -> None:
        # The reference only established delivery of HTML. Require notice markers
        # in visible prose or tables before accepting a detail page as an original.
        headings = doc.xpath("//h1|//h2|//*[contains(concat(' ', normalize-space(@class), ' '), ' article-title ')]")
        blocks = doc.xpath("//p[not(ancestor::nav or ancestor::footer)]|//td[not(ancestor::nav or ancestor::footer)]")
        body = " ".join(map(visible_text, blocks))
        if not any(map(visible_text, headings)) or not any(field in body for field in _NOTICE_FIELDS):
            raise ValueError("detail lacks a visible notice heading and body")


class _DatedLinks(_HtmlDetail):
    list_link_xpath = "//a[@href]"

    def _listing_fields(self, link: HtmlElement) -> tuple[str, date | None]:
        title = (link.get("title") or visible_text(link)).strip()
        dates = link.xpath("ancestor::li[1]//text()[not(ancestor::a)]")
        return title, _date(" ".join(dates))

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        self._check_list(channel, response)
        return self._page(self._dated_refs(response.url, response_html(response)))

    def _dated_refs(self, base: str, doc: HtmlElement) -> list[DetailRef]:
        refs = []
        for link in doc.xpath(self.list_link_xpath):
            if link.xpath("ancestor-or-self::*[@hidden or @aria-hidden='true']"):
                continue
            url = self._detail_url(base, link.get("href", ""))
            title, published = self._listing_fields(link)
            if url is None or not title:
                continue
            key = self.detail_path.fullmatch(urlsplit(url).path)["notice_id"]
            refs.append(DetailRef(key, url, published, title))
        return refs


class HefeiWorks(_DatedLinks):
    source_id = "hefei-ggzy-engineer"
    origin = "https://ggzy.hefei.gov.cn"
    channels = ("construction-tenders",)
    list_path = "/jyxx/002001/002001001/moreinfo_jyxxgg2.html"
    detail_path = re.compile(
        rf"/jyxx/002001/002001001/20\d{{6}}/(?:(?P<notice_id>{_UUID})|[A-Za-z0-9_-]+)\.html"
    )
    _page_number = r"[1-9][0-9]{0,8}"
    _pager_xpath = "//*[contains(concat(' ', normalize-space(@class), ' '), ' ewb-page ')]"

    def _list_path(self, page: int) -> str:
        return self.list_path if page == 1 else f"{self.list_path.rsplit('/', 1)[0]}/{page}.html"

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or type(page) is not int or not re.fullmatch(self._page_number, str(page)):
            raise ValueError("invalid Hefei listing request")
        return FetchRequest("GET", self.origin + self._list_path(page))

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        address = require_official(self.origin, response)
        if channel not in self.channels or address.query or address.fragment:
            raise ValueError("response is outside the Hefei listing route")
        doc = response_html(response)
        refs = _unique_refs(self._dated_refs(response.url, doc))
        if not refs:
            raise ValueError("no Hefei announcements recognized")
        pagers = doc.xpath(self._pager_xpath)
        if not pagers and address.path == self.list_path:
            return ListPage(refs, False, self.coverage_gap, reported_page=1)
        markers = pagers[0].xpath(".//*[@id='index']") if len(pagers) == 1 else []
        counter = re.fullmatch(
            rf"({self._page_number})/({self._page_number})", visible_text(markers[0]) if len(markers) == 1 else ""
        )
        if counter is None:
            raise ValueError("missing or invalid Hefei page counter")
        current, total = map(int, counter.groups())
        if current > total or address.path != self._list_path(current):
            raise ValueError("Hefei page counter does not match listing route")
        next_links = pagers[0].xpath(".//a[starts-with(normalize-space(.), '下页')]")
        has_next = current < total
        if has_next:
            target = None
            if len(next_links) == 1 and not next_links[0].xpath("ancestor-or-self::*[@hidden or @aria-hidden='true']"):
                target = official_url(self.origin, response.url, next_links[0].get("href", ""))
            if target is None or target.path != self._list_path(current + 1) or target.query or target.fragment:
                raise ValueError("invalid Hefei next-page link")
        elif next_links:
            raise ValueError("Hefei terminal page advertises a next page")
        return ListPage(refs, has_next, ordered_by_publication=True, reported_page=current)

    def check_detail(self, response: FetchResponse) -> None:
        address = require_official(self.origin, response)
        route = self.detail_path.fullmatch(address.path)
        if route is None or address.query or address.fragment:
            raise ValueError("response is outside the official notice route")
        doc = response_html(response)
        key = route["notice_id"]
        if key is not None:
            fields = doc.xpath("//input[@id='infoguid']")
            if len(fields) != 1 or fields[0].get("value") != key:
                raise ValueError("Hefei notice identifier does not match the detail route")
        self._check_body(doc)

    def _check_body(self, doc: HtmlElement) -> None:
        _HefeiDocument().to_html(serialize(doc).encode("utf-8"))


class _HefeiDocument(HtmlNoticeBoard):
    body_xpath = (
        f"//*[{CONTENT_CLASS} and not(ancestor-or-self::*[@hidden or @aria-hidden='true' "
        "or contains(concat(' ', normalize-space(@class), ' '), ' hidden ')])]"
    )
    body_class = "ewb-project-notice"


hefei_works_html = _HefeiDocument().to_html


class HenanHomepageWorks(EwbConstructionBoard):
    source_id = "henan-ggzy-homepage-direct"
    origin = "https://hnsggzyjy.henan.gov.cn"
    list_url = origin + "/jyxx/002001/transaction_notice.html"
    channels = ("homepage-tenders",)
    category = "002001001"
    list_path = "/EpointWebBuilder/rest/frontAppCustomAction/getPageInfoListNewYzm"
    detail_path = re.compile(rf"/jyxx/002001/(?P<category>{category})/20\d{{6}}/(?P<notice_id>{_UUID})\.html")
    page_size = 8
    maximum_public_page = 6
    article_xpath = "//div[@class='container']/div[@class='main']"
    heading_xpath = "./div[@class='title']/div[@class='title-text']"
    body_xpath = "./div[contains(concat(' ', normalize-space(@class), ' '), ' detail-list ')]"

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or type(page) is not int or not 1 <= page <= self.maximum_public_page:
            raise ValueError("Henan page exceeds the public pagination range")
        return FetchRequest("POST", self.origin + self.list_path, form=(
            ("siteGuid", "7eb5f7f1-9041-43ad-8e13-8fcb82ea831a"), ("categoryNum", self.category), ("kw", ""),
            ("startDate", ""), ("endDate", ""), ("pageIndex", str(page - 1)), ("pageSize", str(self.page_size)),
            ("jytype", ""), ("xiaqucode", "4100"),
        ))

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        address = require_official(self.origin, response)
        if channel not in self.channels or address.path != self.list_path or address.query or address.fragment:
            raise ValueError("response outside the Henan construction listing route")
        data = _json_object(_json_response(response, accept_html_label=True).get("custom"))
        rows = _json_rows(data.get("infodata"))
        offset = OffsetPage(data.get("count"), self.page_size, len(rows), maximum_public_page=self.maximum_public_page)
        refs = []
        for row in rows:
            href, title = row.get("infourl"), row.get("title")
            detail = self.detail_address(href) if isinstance(href, str) else None
            key = self.notice_key(detail) if detail is not None else None
            published = _date(row.get("infodate"))
            if (key is None or key != row.get("infoid") or published is None or row.get("categorynum") != self.category
                    or not isinstance(title, str) or not title.strip()):
                raise ValueError("invalid Henan construction announcement row")
            refs.append(DetailRef(key, detail.geturl(), published, title.strip()))
        gap = "pagination_requires_captcha" if offset.total_records > self.page_size * self.maximum_public_page else None
        return ListPage(tuple(refs), False, coverage_gap=gap, offset=offset)


henan_works_html = HenanHomepageWorks().to_html


class ShanxiWorks(_DatedLinks):
    source_id = "shanxi-prec"
    contract_version = 2
    origin = "https://prec.sxzwfw.gov.cn"
    channels = ("construction",)
    list_path = "/jyxxgc/index.jhtml"
    detail_path = re.compile(r"/jyxx(?P<category>gczb|gchxr|gcgs|gcgz|zbjh)/(?P<notice_id>[0-9]{1,20})\.jhtml")
    list_link_xpath = "//div[contains(concat(' ', normalize-space(@class), ' '), ' cs_two_content ')]/a"
    page_size = 10
    _list_route = re.compile(r"/jyxxgc/index(?:_(?P<page>[1-9][0-9]{0,8}))?\.jhtml")
    _pager = re.compile(r"\blaypage\.render\(\{(.*?)\bjump\s*:", re.DOTALL)
    _pager_field = re.compile(r"(?m)^\s*,\s*(count|limit|curr)\s*:\s*(0|[1-9][0-9]{0,8})\s*$")
    _legacy_origin = "http://prec.sxzwfw.gov.cn:80"
    _view_count = re.compile(r'''Cms\.viewCount\(\s*["']\s*["']\s*,\s*["']([0-9]{1,20})["']\s*\)''')
    _plan_tables = ("//div[contains(concat(' ', normalize-space(@class), ' '), ' cs_xq_content ')]"
                    "/table[contains(concat(' ', normalize-space(@class), ' '), ' bid_msgTable ')]")

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or type(page) is not int or not 1 <= page <= 999_999_999:
            raise ValueError("invalid Shanxi construction list request")
        path = self.list_path if page == 1 else f"/jyxxgc/index_{page}.jhtml"
        return FetchRequest("GET", self.origin + path)

    def _detail_url(self, base: str, href: str) -> str | None:
        if href.startswith(self._legacy_origin + "/"):
            href = self.origin + href.removeprefix(self._legacy_origin)
        return super()._detail_url(base, href)

    def _listing_fields(self, link: HtmlElement) -> tuple[str, date | None]:
        titles = link.xpath("./p[contains(concat(' ', normalize-space(@class), ' '), ' cs_bz_cont ')]")
        dates = link.xpath(".//span[contains(concat(' ', normalize-space(@class), ' '), ' cs_bz_cont_1_time ')]")
        return (visible_text(titles[0]) if len(titles) == 1 else "",
                _date(visible_text(dates[0])) if len(dates) == 1 else None)

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        address = require_official(self.origin, response)
        route = self._list_route.fullmatch(address.path)
        if channel not in self.channels or not route or address.query or address.fragment:
            raise ValueError("response outside the Shanxi construction list route")
        raw = html_text(response)
        root = checked_html(raw)
        pagers = self._pager.findall(raw)
        fields = self._pager_field.findall(pagers[0]) if len(pagers) == 1 else []
        metadata = {key: int(value) for key, value in fields}
        page = int(route["page"] or 1)
        if (len(fields) != 3 or set(metadata) != {"count", "limit", "curr"}
                or metadata["curr"] != page or metadata["limit"] != self.page_size):
            raise ValueError("Shanxi construction pager does not match the requested page")
        rows = root.xpath(self.list_link_xpath)
        offset = OffsetPage(metadata["count"], self.page_size, len(rows))
        more = offset.has_more(page)
        refs = _unique_refs(self._dated_refs(response.url, root))
        complete = len(refs) == len(rows) and all(ref.source_key and ref.published for ref in refs)
        dates = [self._listing_fields(row)[1] for row in rows]
        ordered = all(day is not None for day in dates) and dates == sorted(dates, reverse=True)
        gap = None if complete else "unrecognized_list_rows"
        return ListPage(refs, more, coverage_gap=gap, ordered_by_publication=ordered,
                        reported_page=page, offset=offset)

    @staticmethod
    def _announcement(root: HtmlElement) -> str:
        bodies = root.xpath("//div[contains(concat(' ', normalize-space(@class), ' '), ' div-article2 ')]")
        if len(bodies) != 1 or not visible_text(bodies[0]):
            raise ValueError("missing Shanxi construction announcement body")
        dates = bodies[0].xpath("../div[contains(concat(' ', normalize-space(@class), ' '), ' div-title2 ')]")
        if len(dates) != 1 or _date(visible_text(dates[0])) is None:
            raise ValueError("missing Shanxi construction publication date")
        titles = bodies[0].xpath("../../p[contains(concat(' ', normalize-space(@class), ' '), ' cs_title_P1 ')]")
        title = visible_text(titles[0]) if len(titles) == 1 else ""
        heading = f"<h1>{escape(title)}</h1>" if title else ""
        publication = f"<p>{escape(visible_text(dates[0]))}</p>"
        return f"<html><body>{heading}{publication}{serialize(bodies[0], with_tail=False)}</body></html>"

    @staticmethod
    def _tender_plan(tables: list[HtmlElement]) -> tuple[str, date, str, HtmlElement]:
        if len(tables) != 1 or not visible_text(tables[0]):
            raise ValueError("missing or ambiguous Shanxi tender plan body")
        body = tables[0]
        wrapper = body.getparent().getparent()
        titles = wrapper.xpath("./p[contains(concat(' ', normalize-space(@class), ' '), ' cs_title_P1 ')]")
        dates = wrapper.xpath("./p[contains(concat(' ', normalize-space(@class), ' '), ' cs_title_P3 ')]")
        title = visible_text(titles[0]) if len(titles) == 1 else ""
        date_label = visible_text(dates[0]) if len(dates) == 1 else ""
        published = _date(date_label) if date_label.startswith(("发布日期：", "发布日期:")) else None
        if not title or published is None:
            raise ValueError("missing Shanxi tender plan title or publication date")
        return title, published, date_label, body

    def _check_detail(self, response: FetchResponse, ref: DetailRef | None) -> date | None:
        address = require_official(self.origin, response)
        route = self.detail_path.fullmatch(address.path)
        html = html_text(response)
        root = checked_html(html)
        if route is None or address.query or address.fragment or self._view_count.findall(html) != [route["notice_id"]]:
            raise ValueError("Shanxi construction announcement identity does not match detail request")
        if ref is not None and ref.source_key is not None and ref.source_key != route["notice_id"]:
            raise ValueError("Shanxi construction announcement identity does not match the listing")
        if route["category"] == "zbjh":
            title, published, _, _ = self._tender_plan(root.xpath(self._plan_tables))
            if ref is not None and title != ref.title:
                raise ValueError("Shanxi tender plan title does not match the listing")
            return published
        self._announcement(root)
        return None

    def check_detail(self, response: FetchResponse) -> date | None:
        return self._check_detail(response, None)

    def check_detail_with_ref(self, ref: DetailRef, response: FetchResponse) -> date | None:
        return self._check_detail(response, ref)

    def to_html(self, raw: bytes) -> str:
        root = checked_html(raw.decode("utf-8"))
        tables = root.xpath(self._plan_tables)
        if not tables:
            return self._announcement(root)
        title, _, date_label, body = self._tender_plan(tables)
        return (f'<html><body data-notice-kind="tender-plan"><h1>{escape(title)}</h1>'
                f'<p>{escape(date_label)}</p>{serialize(body, with_tail=False)}</body></html>')


shanxi_works_html = ShanxiWorks().to_html


class _VerifiedEpointWorks(EwbConstructionBoard):
    requires_list_infoid = False

    def _list_result(self, response: FetchResponse) -> dict:
        return _json_object(_json_response(response).get("result"))

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        address = require_official(self.list_url, response)
        if channel not in self.channels or address.path != self.list_path or address.query or address.fragment:
            raise ValueError("response outside the construction listing route")
        result = self._list_result(response)
        rows = _json_rows(result.get("records"))
        offset = OffsetPage(result.get("totalcount"), self.page_size, len(rows))
        refs = []
        for row in rows:
            href = row.get("linkurl")
            detail = self.detail_address(href) if isinstance(href, str) else None
            route = self.detail_path.fullmatch(detail.path) if detail is not None else None
            key = self.notice_key(detail) if detail is not None else None
            published = _date(row.get("webdate"))
            markup = row.get("title")
            title = visible_text(checked_html(markup)) if isinstance(markup, str) and markup.strip() else ""
            infoid = row.get("infoid")
            if (route is None or row.get("categorynum") != route["category"] or key is None or published is None
                    or not title or ((self.requires_list_infoid or infoid is not None) and native_id(infoid) != key)):
                raise ValueError("invalid construction announcement row")
            refs.append(DetailRef(key, detail.geturl(), published, title))
        return ListPage(tuple(refs), False, ordered_by_publication=self.ordered, offset=offset)


class _ShaanxiTradeNotices(_VerifiedEpointWorks):
    contract_version = 2
    origin = "https://www.sxggzyjy.cn"
    list_url = origin + "/jydt/"
    list_path = "/inteligentsearch_new/rest/esinteligentsearch/getFullTextDataNew"
    windowed = True
    ordered = False
    page_size = 10
    requires_list_infoid = True
    category: str
    _key_pattern = rf"[0-9a-fA-F]{{32}}|{_UUID}"
    article_xpath = "//div[@id='c']"
    heading_xpath = "./h3[contains(concat(' ', normalize-space(@class), ' '), ' article-title ')]"
    _publication_label = re.compile(r"【信息时间[：:]\s*(20[0-9]{2}-[0-9]{2}-[0-9]{2})\s*】")

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or type(page) is not int or page < 1:
            raise ValueError("invalid Shaanxi announcement list request")
        return FetchRequest.post_json(self.origin + self.list_path, {
            "token": "", "pn": (page - 1) * self.page_size, "rn": self.page_size,
            "sdt": "", "edt": "", "wd": "", "inc_wd": "", "exc_wd": "", "fields": "", "cnum": "001",
            "sort": '{"webdate":"0"}', "ssort": "", "cl": 10000, "terminal": "",
            "highlights": "", "statistics": None, "unionCondition": [], "accuracy": "",
            "noParticiple": "1", "searchRange": [], "noWd": True, "isBusiness": "1",
            "time": [{"fieldName": "webdate", "startTime": f"{window.since} 00:00:00",
                      "endTime": f"{window.until} 23:59:59"}],
            "condition": [{"fieldName": "categorynum", "equal": self.category, "notEqual": None,
                           "equalList": None, "notEqualList": None, "isLike": False, "likeType": 2, "noWd": True}],
        })

    def _list_result(self, response: FetchResponse) -> dict:
        envelope = _json_response(response)
        if type(envelope.get("code")) is not int or envelope["code"] != 200:
            raise ValueError("Shaanxi announcement search did not return success")
        return _json_object(_json_object(envelope.get("content")).get("result"))

    def notice_key(self, address: SplitResult) -> str | None:
        match = self.detail_path.fullmatch(address.path)
        return match["notice_id"] if match else None

    def check_detail(self, response: FetchResponse) -> date:
        target = require_official(self.origin, response)
        root = response_html(response)
        key = self.notice_key(target)
        markers = root.xpath("//span[@id='viewGuid']")
        if (key is None or target.query or target.fragment or len(markers) != 1
                or markers[0].get("value") != f"cms_{key}"
                or not self._matches_detail_identity(root, markers[0], key)):
            raise ValueError("Shaanxi announcement detail identity does not match the request route")
        self._announcement(root)
        metadata = root.xpath(self.article_xpath
                              + "/div[contains(concat(' ', normalize-space(@class), ' '), ' info-source ')]")
        dates = self._publication_label.findall(visible_text(metadata[0])) if len(metadata) == 1 else []
        # The date in the route can differ from the day published on the listing and article.
        published = _date(dates[0]) if len(dates) == 1 else None
        if published is None:
            raise ValueError("missing or ambiguous Shaanxi detail publication date")
        return published


class ShaanxiWorks(_ShaanxiTradeNotices):
    source_id = "shaanxi-ggzy-construction"
    channels = ("construction-search",)
    category = "001001001001"
    detail_path = re.compile(rf"/jydt/001001/001001001/(?P<category>{category})/20\d{{6}}/"
                             rf"(?P<notice_id>{_ShaanxiTradeNotices._key_pattern})\.html")
    body_xpath = "./div[@id='mainContent']"

    def _matches_detail_identity(self, root: HtmlElement, marker: HtmlElement, key: str) -> bool:
        return marker.get("data-infoid") == key and marker.get("data-value") == self.category


class ShaanxiGovernmentProcurement(_ShaanxiTradeNotices):
    source_id = "shaanxi-government-procurement"
    channels = ("government-procurement",)
    category = "001001004001"
    detail_path = re.compile(rf"/jydt/001001/001001004/(?P<category>{category})/20\d{{6}}/"
                             rf"(?P<notice_id>{_ShaanxiTradeNotices._key_pattern})\.html")
    body_xpath = (
        ".//div[contains(concat(' ', normalize-space(@class), ' '), ' jynr ')"
        " and contains(concat(' ', normalize-space(@class), ' '), ' news_content ')]"
    )

    def _matches_detail_identity(self, root: HtmlElement, marker: HtmlElement, key: str) -> bool:
        fields = root.xpath("//input[@id='souceinfoid']")
        return len(fields) == 1 and fields[0].get("type") == "hidden" and fields[0].get("value") == key


shaanxi_works_html = ShaanxiWorks().to_html
shaanxi_procurement_html = ShaanxiGovernmentProcurement().to_html


class JiangxiAnnouncements(_VerifiedEpointWorks):
    source_id = "jiangxi-ggzy-announcements"
    origin = "https://www.jxsggzy.cn"
    list_url = origin + "/jyxx/002001/002001001/trade.html"
    list_path = "/XZinterface/rest/esinteligentsearch/getFullTextDataNew"
    channels = ("homepage-trade-announcements",)
    windowed = True
    ordered = False
    page_size = 10
    category = "002001001"
    detail_path = re.compile(rf"/jyxx/002001/(?P<category>{category})/20\d{{6}}/(?P<notice_id>{_UUID})\.html")
    article_xpath = "//div[@class='container']/div[@class='content']"
    heading_xpath = "./p[@class='title']"
    body_xpath = "./div[@class='text']"

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or type(page) is not int or page < 1:
            raise ValueError("invalid Jiangxi construction list request")
        return FetchRequest.post_json(self.origin + self.list_path, {
            "token": "", "pn": (page - 1) * self.page_size, "rn": self.page_size, "sdt": "", "edt": "",
            "wd": "", "inc_wd": "", "exc_wd": "", "fields": "", "cnum": "",
            "sort": '{"webdate":"0","id":"0"}', "ssort": "", "cl": 500, "terminal": "",
            "condition": [{"fieldName": "categorynum", "equal": self.category, "notEqual": None, "equalList": None,
                           "notEqualList": None, "isLike": True, "likeType": 2}],
            "time": [{"fieldName": "webdate", "startTime": f"{window.since} 00:00:00",
                      "endTime": f"{window.until} 23:59:59"}],
            "highlights": "", "statistics": None, "unionCondition": [], "accuracy": "", "noParticiple": "1",
            "searchRange": None, "noWd": True,
        }, headers=(("Content-Type", "text/plain;charset=UTF-8"),))


jiangxi_works_html = JiangxiAnnouncements().to_html


class SichuanWorks(_VerifiedEpointWorks):
    source_id = "sichuan-ggzy-construction"
    origin = "https://ggzyjy.sc.gov.cn"
    list_url = origin + "/"
    list_path = "/inteligentsearch/rest/esinteligentsearch/getFullTextDataNew"
    channels = ("construction-search",)
    windowed = True
    ordered = False
    page_size = 12
    category = "002001001"
    detail_path = re.compile(
        rf"/jyxx/002001/(?P<category>{category})/20\d{{6}}/(?P<notice_id>{_UUID}|[0-9a-fA-F]{{32}})\.html"
    )
    article_xpath = "//div[@id='infoContainer']"
    heading_xpath = "./h2[@id='title']"
    body_xpath = "./div[@id='newsText']"

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or type(page) is not int or page < 1:
            raise ValueError("invalid Sichuan construction list request")
        return FetchRequest.post_json(self.origin + self.list_path, {
            "token": "", "pn": (page - 1) * self.page_size, "rn": self.page_size, "sdt": "", "edt": "",
            "wd": "", "inc_wd": "", "exc_wd": "", "fields": "", "cnum": "",
            "sort": '{"ordernum":"0","webdate":"0"}', "ssort": "", "cl": 10000, "terminal": "",
            "condition": [{"fieldName": "categorynum", "equal": self.category, "notEqual": None, "equalList": None,
                           "notEqualList": None, "isLike": True, "likeType": 2}],
            "time": [{"fieldName": "webdate", "startTime": f"{window.since} 00:00:00",
                      "endTime": f"{window.until} 23:59:59"}],
            "highlights": "", "statistics": None, "unionCondition": None, "accuracy": "", "noParticiple": "1",
            "searchRange": None, "noWd": True,
        })


sichuan_works_html = SichuanWorks().to_html


class XinjiangWorks(_VerifiedEpointWorks):
    source_id = "xinjiang-ggzy-construction"
    channels = ("construction-search",)
    origin = "https://ggzy.xinjiang.gov.cn"
    list_url = origin + "/"
    list_path = "/inteligentsearchnew/rest/esinteligentsearch/getFullTextDataNew"
    category = "001001001"
    ordered = True
    page_size = 10
    detail_path = re.compile(
        rf"/(?:xinjiangggzy_new/)?jyxx/001001/(?P<category>{category})/20\d{{6}}/(?P<notice_id>{_UUID})\.html"
    )
    article_xpath = "//div[contains(concat(' ', normalize-space(@class), ' '), ' public-article ')]"
    heading_xpath = "./h3[contains(concat(' ', normalize-space(@class), ' '), ' public-article-title ')]"
    body_xpath = "./div[contains(concat(' ', normalize-space(@class), ' '), ' public-article-container ')]"
    identity_xpath = "//span[@id='viewGuid']/@value"

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or type(page) is not int or page < 1:
            raise ValueError("invalid Xinjiang construction list request")
        return FetchRequest.post_json(self.origin + self.list_path, {
            "token": "", "pn": (page - 1) * self.page_size, "rn": self.page_size, "sdt": "", "edt": "",
            "wd": "", "inc_wd": "", "exc_wd": "", "fields": "title;projectnum;projectname", "cnum": "002",
            "sort": '{"webdate":"0"}', "ssort": "title", "cl": 200, "terminal": "",
            "condition": [{"fieldName": "categorynum", "isLike": True, "likeType": 2, "equal": self.category}],
            "time": None, "highlights": "", "statistics": None, "unionCondition": [], "accuracy": "100",
            "noParticiple": "0", "searchRange": None, "isBusiness": 1,
        })

xinjiang_works_html = XinjiangWorks().to_html


class HubeiWorks(_FirstPage):
    source_id = "hubei-ggzy-construction"
    origin = "https://www.hbggzyfwpt.cn"
    channels = ("construction-tenders",)
    list_path = "/jyxxAjax/jsgcZbgg"
    detail_path = "/jyxxAjax/jsgcZbggDetail"
    notice_path = "/jyxx/jsgcZbggDetail"
    page_size = 10

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or type(page) is not int or page < 1:
            raise ValueError("invalid Hubei construction list request")
        return FetchRequest("POST", self.origin + self.list_path,
                            form=(("currentPage", str(page)), ("pageSize", str(self.page_size)), ("area", "")))

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        self._check_list(channel, response)
        payload = _json_response(response, accept_html_label=True)
        rows = _json_rows(payload.get("jsgcZbggList"))
        reported = payload.get("currentPage")
        if type(reported) is not int or reported < 1 or payload.get("pageSize") != self.page_size or len(rows) > self.page_size:
            raise ValueError("invalid Hubei construction pagination metadata")
        refs = {}
        for row in rows:
            guid = self._guid(row.get("tenderBulletinGuid"))
            title = row.get("bulletinName")
            timestamp = row.get("bulletinIssueTime")
            if isinstance(timestamp, str) and re.fullmatch(r"[0-9]{14}", timestamp):
                timestamp = timestamp[:8]
            published = _date(timestamp)
            if not guid or not isinstance(title, str) or not title.strip() or published is None or guid in refs:
                raise ValueError("invalid or duplicate Hubei construction announcement row")
            url = self.origin + self.notice_path + "?" + urlencode({"guid": guid})
            refs[guid] = DetailRef(guid, url, published, title.strip())
        return ListPage(tuple(refs.values()), len(rows) == self.page_size,
                        coverage_gap=None if len(rows) == self.page_size else "unverified_archive_boundary",
                        reported_page=reported)

    @staticmethod
    def _guid(value: object) -> str | None:
        return value.lower() if isinstance(value, str) and re.fullmatch(_UUID, value) else None

    def _locator_guid(self, url: str, path: str) -> str | None:
        address = official_url(self.origin, self.origin, url)
        if address is None or address.path != path or address.fragment:
            return None
        parameters = parse_qs(address.query, keep_blank_values=True)
        values = parameters.get("guid", [])
        return self._guid(values[0]) if len(parameters) == len(values) == 1 else None

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        guid = self._locator_guid(ref.source_url, self.notice_path)
        if channel not in self.channels or guid is None or guid != ref.source_key:
            raise ValueError("invalid Hubei construction detail reference")
        url = self.origin + self.detail_path + "?" + urlencode({"guid": guid})
        return (DetailFetch(guid, FetchRequest("GET", url)),)

    @classmethod
    def _announcement(cls, payload: dict, guid: str | None = None) -> str:
        tender = _json_object(payload.get("tender"))
        identity = cls._guid(tender.get("tenderBulletinGuid"))
        if identity is None or (guid is not None and guid != identity):
            raise ValueError("Hubei construction announcement identity does not match detail request")
        title = tender.get("bulletinName")
        markup = tender.get("bulletinContent")
        if not isinstance(title, str) or not title.strip() or not isinstance(markup, str) or not markup.strip():
            raise ValueError("missing Hubei construction announcement title or body")
        body = checked_html(markup)
        if not visible_text(body):
            raise ValueError("missing Hubei construction announcement body")
        return f"<h1>{escape(title.strip())}</h1>" + "".join(map(serialize, body.xpath("//body")))

    def check_detail(self, response: FetchResponse) -> None:
        require_official(self.origin, response)
        guid = self._locator_guid(response.url, self.detail_path)
        if guid is None:
            raise ValueError("response is outside the Hubei construction detail route")
        self._announcement(_json_response(response, accept_html_label=True), guid)

    def to_html(self, raw: bytes) -> str:
        return self._announcement(_json_object(raw.decode("utf-8-sig")))


hubei_works_html = HubeiWorks().to_html


class HunanWorks:
    source_id = "hunan-ggzy-construction"
    contract_version = 2
    origin = "https://www.hnsggzy.com"
    channels = ("open-construction-sections",)
    windowed = False
    list_path = "/tradeApi/constructionTender/listByFile"
    detail_path = "/tradeApi/constructionNotice/getBySectionId"
    coverage_gap = "section_notice_coverage_unverified"
    page_size = 10
    _max_page = 999_999_999
    _max_notices = 256
    _max_response_bytes = 8 * 1024 * 1024
    _selector_kind = "hunan-section-notice"
    _selector_version = 1
    _bulletin_type = re.compile(r"[A-Z]+(?:_[A-Z]+)*_NOTICE")
    _publication_time = re.compile(r"20[0-9]{2}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}")
    # Version 1 covers the publisher's member fields. The selector binds the section separately.
    _record_fields = ("id", "bulletinType", "noticeSendTime", "noticeName", "noticeContent")

    def _list_params(self, page: int) -> dict[str, str]:
        return {"current": str(page), "size": str(self.page_size), "tenderProjectType": "CONSTRUCTION",
                "notice": "", "tenderMode": "OPEN", "status": "", "regionCode": ""}

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or type(page) is not int or not 1 <= page <= self._max_page:
            raise ValueError("invalid Hunan construction section list request")
        query = urlencode(self._list_params(page))
        return FetchRequest("GET", self.origin + self.list_path + "?" + query)

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        address = require_official(self.origin, response)
        if channel not in self.channels or address.path != self.list_path or address.fragment:
            raise ValueError("response is outside the official Hunan listing route")
        data = self._response_data(response.body, response.media_type)
        rows = data.get("records")
        current, size, total, pages = (data.get(field) for field in ("current", "size", "total", "pages"))
        if (type(current) is not int or not 1 <= current <= self._max_page
                or type(size) is not int or size != self.page_size
                or type(total) is not int or not 0 <= total <= self._max_page * size
                or type(pages) is not int or pages != (total + size - 1) // size
                or not isinstance(rows, list)):
            raise ValueError("invalid Hunan section page metadata")
        expected = {key: [value] for key, value in self._list_params(current).items()}
        query = parse_qs(address.query, keep_blank_values=True, strict_parsing=True, max_num_fields=len(expected))
        if query != expected:
            raise ValueError("Hunan section page does not match the listing request")
        offset = OffsetPage(total, size, len(rows))
        has_next = offset.has_more(current)
        groups = []
        seen = set()
        for row in rows:
            section_id = row.get("bidSectionId") if isinstance(row, dict) else None
            if not isinstance(section_id, str) or native_id(section_id) is None:
                raise ValueError("invalid Hunan construction section row")
            if section_id in seen:
                raise ValueError("duplicate Hunan construction section row")
            seen.add(section_id)
            groups.append(NoticeGroup(section_id, self.group_request(channel, section_id)))
        return ListPage((), has_next, coverage_gap=self.coverage_gap, reported_page=current,
                        offset=offset, groups=tuple(groups))

    def group_request(self, channel: str, group_key: str) -> FetchRequest:
        if channel not in self.channels:
            raise ValueError("invalid Hunan construction section channel")
        if not isinstance(group_key, str) or native_id(group_key) is None:
            raise ValueError("invalid Hunan construction section key")
        return FetchRequest("GET", self.origin + self.detail_path + "?" + urlencode({"sectionId": group_key}))

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        raise ValueError("Hunan announcements require section expansion")

    def check_detail(self, response: FetchResponse) -> None:
        raise ValueError("Hunan announcements require section expansion")

    def expand_group(self, channel: str, group: NoticeGroup, response: FetchResponse) -> tuple[GroupedNotice, ...]:
        require_official(self.origin, response)
        expected = self.group_request(channel, group.group_key)
        if group.request != expected or response.url != expected.url:
            raise ValueError("Hunan section response does not match the official group request")
        data = self._response_data(response.body, response.media_type)
        return tuple(member for member, _ in self._section_records(data, group.group_key, expected.url).values())

    def to_selected_html(self, raw: bytes, selector: GroupSelector, *, published_on: date,
                         record_digest: str) -> str:
        """Project one checked notice from the full original for Evidence."""
        if (selector.kind, selector.version) != (self._selector_kind, self._selector_version):
            raise ValueError("unsupported Hunan notice selector")
        url = self.group_request(self.channels[0], selector.group_key).url
        data = self._response_data(raw, "application/json")
        selected = self._section_records(data, selector.group_key, url).get(selector.notice_key)
        if selected is None:
            raise ValueError("Hunan notice selector did not match a record")
        member, record = selected
        if member.ref.published != published_on or member.record_digest != record_digest:
            raise ValueError("Hunan selected record binding does not match")
        title = escape(member.ref.title)
        published = escape(record["noticeSendTime"])
        body = self._notice_body(record["noticeContent"])
        visible_body = escape(body.text or "") + "".join(map(serialize, body))
        return f"<h1>{title}</h1><p>公告时间：{published}</p><div>{visible_body}</div>"

    def _section_records(self, data: dict, section_id: str, url: str) -> dict[str, tuple[GroupedNotice, dict[str, str]]]:
        notices = data.get("noticeList")
        if not isinstance(notices, list) or not 1 <= len(notices) <= self._max_notices:
            raise ValueError("invalid Hunan section announcement count")
        records = {}
        for notice in notices:
            if not isinstance(notice, dict) or notice.get("constructionSectionId") != section_id:
                raise ValueError("invalid Hunan construction notice section")
            record = {field: notice.get(field) for field in self._record_fields}
            key, kind, published, title, content = record.values()
            if (not all(isinstance(value, str) and value.strip() for value in record.values())
                    or native_id(key) is None or key == section_id
                    or len(kind) > 64 or not self._bulletin_type.fullmatch(kind)
                    or not self._publication_time.fullmatch(published)):
                raise ValueError("invalid Hunan construction notice in section response")
            if key in records:
                raise ValueError("duplicate Hunan construction notice key")
            published_on = datetime.fromisoformat(published).date()
            self._notice_body(content)
            selector = GroupSelector(self._selector_kind, self._selector_version, section_id, key)
            canonical = json.dumps([selector.kind, selector.version, record], ensure_ascii=False, sort_keys=True,
                                   separators=(",", ":")).encode("utf-8")
            member = GroupedNotice(DetailRef(key, url, published_on, title.strip()), selector,
                                   "sha256:" + sha256(canonical).hexdigest())
            records[key] = member, record
        return records

    @staticmethod
    def _notice_body(content: str) -> HtmlElement:
        bodies = checked_html(content).xpath("//body")
        if len(bodies) != 1:
            raise ValueError("invalid Hunan construction notice body")
        body = bodies[0]
        for node in body.xpath(".//*[@hidden or @aria-hidden='true']"):
            node.drop_tree()
        if body.xpath("ancestor-or-self::*[@hidden or @aria-hidden='true']") or not visible_text(body):
            raise ValueError("invalid Hunan construction notice body")
        return body

    def _response_data(self, raw: bytes, media_type: str) -> dict:
        if not 1 <= len(raw) <= self._max_response_bytes:
            raise ValueError("invalid Hunan JSON response size")
        content = raw.decode("utf-8-sig")
        if content.lstrip().startswith("<"):
            checked_html(content)
            raise ValueError("received HTML at a Hunan JSON endpoint")
        if media_type not in ("application/json", "text/plain"):
            raise ValueError("unexpected media type at a Hunan JSON endpoint")
        try:
            envelope = json.loads(content, object_pairs_hook=self._unique_object, parse_constant=self._invalid_constant)
        except RecursionError as exc:
            raise ValueError("Hunan JSON response is too deeply nested") from exc
        if not isinstance(envelope, dict):
            raise ValueError("invalid Hunan JSON response envelope")
        for field in ("code", "status"):
            if envelope.get(field) in (401, 403, "401", "403"):
                raise SourceBlocked(f"api_{envelope[field]}")
        if type(envelope.get("code")) is not int or envelope["code"] != 200 or envelope.get("success") is not True:
            raise ValueError("Hunan API response did not report success")
        data = envelope.get("data")
        if not isinstance(data, dict):
            raise ValueError("invalid Hunan API response data")
        return data

    @staticmethod
    def _unique_object(pairs: list[tuple[str, object]]) -> dict:
        result = dict(pairs)
        if len(result) != len(pairs):
            raise ValueError("duplicate JSON key in Hunan response")
        return result

    @staticmethod
    def _invalid_constant(value: str) -> None:
        raise ValueError(f"invalid Hunan JSON constant {value}")


hunan_works_selected_html = HunanWorks().to_selected_html


class TibetWorks(DiscoveryOnly, _FirstPage):
    source_id = "tibet-ggzy-construction"
    origin = "http://ggzy.xizang.gov.cn"
    channels = ("construction",)
    list_path = "/p22/jyxxgc.html"
    coverage_gap = "project_code_followup_unavailable"
    _opener = re.compile(r"""^\s*window\.open\(\s*(['"])([^'"]+\.jhtml)\1""")
    _detail_path = re.compile(r"/jyxxgcgg/(\d{1,20})\.jhtml")

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        self._check_list(channel, response)
        refs = []
        for link in response_html(response).xpath("//li//p[@onclick]"):
            opener = self._opener.match(link.get("onclick"))
            if opener is None:
                continue
            url = official_url(self.origin, response.url, opener[2])
            if url is None or url.query or url.fragment:
                continue
            match = self._detail_path.fullmatch(url.path)
            title = visible_text(link)
            if match and title:
                date_text = link.xpath("string(following-sibling::p[1])")
                refs.append(DetailRef(match[1], self.origin + url.path, _date(date_text), title))
        return self._page(refs)


tibet_works_html = TibetWorks().to_html
