"""Native public list/detail HTML protocols for local transaction portals."""
from __future__ import annotations

import json
import re
from datetime import date
from urllib.parse import SplitResult, parse_qs, urlencode, urljoin, urlsplit

from miaoqia.sdk.source_html import HtmlElement, element, parse_document, serialize

from miaoqia.acquisition.api.handlers import DetailFetch, DetailRef, FetchRequest, FetchResponse, ListPage, OffsetPage, SourceBlocked, Window

from . import html_text, json_body
from .ccgp_http import checked_document, checked_html, official_url, require_official, response_html, visible_text, publication_date

NOTICE_ID = re.compile(r"[A-Za-z0-9][A-Za-z0-9._~-]{0,127}")
ENTRY_PAGE_ONLY = "pagination_unverified"


def native_id(value: object) -> str | None:
    if type(value) not in (str, int):
        return None
    token = str(value)
    return token if NOTICE_ID.fullmatch(token) else None


def notice_query_id(address: SplitResult, aliases: tuple[str, ...]) -> str | None:
    query = parse_qs(address.query, keep_blank_values=True)
    values = {value for alias in aliases for value in query.get(alias, ())}
    return native_id(values.pop()) if len(values) == 1 else None


def portal_address(base: str, href: str) -> SplitResult | None:
    """Keep locators on the configured origin, including any explicit port."""
    if not href or any(char.isspace() or ord(char) < 32 or char == "\\" for char in href):
        return None
    expected = urlsplit(base)
    if expected.port is None:
        address = official_url(base, base, href)
        return address._replace(netloc=expected.netloc, fragment="") if address else None
    try:
        address = urlsplit(urljoin(base, href))
    except ValueError:
        return None
    if (address.scheme, address.netloc) != (expected.scheme, expected.netloc):
        return None
    return address._replace(fragment="")


def require_portal(base: str, response: FetchResponse) -> SplitResult:
    if urlsplit(base).port is None:
        return require_official(base, response)
    if response.status in (401, 403):
        raise SourceBlocked(f"http_{response.status}")
    if response.status != 200:
        raise ValueError(f"unexpected HTTP status {response.status}")
    if response.url.startswith(("https://", "http://")) and (address := portal_address(base, response.url)):
        return address
    raise ValueError("response outside source origin")


def notice_markup(root: HtmlElement, heading: str = "") -> str:
    # The normalization pipeline reads paragraph blocks. Keep prose that a
    # portal puts directly inside the document or its main article.
    for container in root.xpath("//body|//article|//main"):
        for child in list(container):
            if child.tail and child.tail.strip():
                paragraph = element("p")
                paragraph.text, child.tail = child.tail, None
                child.addnext(paragraph)
        if container.text and container.text.strip():
            paragraph = element("p")
            paragraph.text, container.text = container.text, None
            container.insert(0, paragraph)
    if heading:
        title = element("h1")
        title.text = heading
        root.find("body").insert(0, title)
    return serialize(root)


def trade_body(root: HtmlElement, title_xpath: str) -> str:
    title = next((text for node in root.xpath(title_xpath) if (text := visible_text(node))), "")
    has_content = root.xpath(
        "//body//*[self::article or self::main or self::p or self::td][normalize-space(.)"
        " and not(ancestor::header or ancestor::nav or ancestor::footer)]"
    )
    if not title or not has_content:
        raise ValueError("announcement heading or substantive body missing")
    return notice_markup(root, "" if root.xpath("//body//h1[normalize-space(.)]") else title)


class TradeHtmlBoard:
    channels = ("published",)
    windowed = False
    source_id: str
    list_url: str
    row_xpath: str
    detail_path: re.Pattern[str]
    title_xpath = "//h1|//title"

    def list_address(self, channel: str) -> str:
        return self.list_url

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or page != 1:
            raise ValueError("only the reference entry page is available")
        return FetchRequest("GET", self.list_address(channel))

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        address = require_portal(self.list_url, response)
        expected = urlsplit(self.list_address(channel))
        expected_query = parse_qs(expected.query)
        query = parse_qs(address.query)
        if (channel not in self.channels or address.path != expected.path
                or any(query.get(field) != values for field, values in expected_query.items())):
            raise ValueError("response outside source list")
        root = response_html(response)
        refs = {}
        for row in root.xpath(self.row_xpath):
            for link in row.xpath("self::a|.//a") or [row]:
                if link.xpath("ancestor-or-self::*[@hidden or @aria-hidden='true']"):
                    continue
                href = self.link_href(row, link)
                detail = self.detail_address(href)
                title, row_date = self.list_fields(row, link)
                if detail is not None and title:
                    key = self.notice_key(detail)
                    locator = detail.geturl()
                    refs.setdefault(key or locator, DetailRef(key, locator, row_date, title))
        if not refs:
            raise ValueError("no source announcement links recognized on the list")
        return ListPage(tuple(refs.values()), False, ENTRY_PAGE_ONLY)

    def link_href(self, row: HtmlElement, link: HtmlElement) -> str:
        return link.get("href", "")

    def list_fields(self, row: HtmlElement, link: HtmlElement) -> tuple[str, date | None]:
        title = (link.get("title") or visible_text(link)).strip()
        return title, publication_date(" ".join(row.xpath(".//text()[not(ancestor::a)]")))

    def detail_address(self, href: str) -> SplitResult | None:
        address = portal_address(self.list_url, href)
        return address if address and self.detail_path.fullmatch(address.path) else None

    def notice_key(self, address: SplitResult) -> str | None:
        return notice_query_id(address, ("id", "noticeId", "docid", "infoid"))

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        address = self.detail_address(ref.source_url)
        if channel not in self.channels or address is None or self.notice_key(address) != ref.source_key:
            raise ValueError("invalid source detail reference")
        return (DetailFetch(f"{ref.source_key or 'pending'}/body", FetchRequest("GET", address.geturl())),)

    def check_detail(self, response: FetchResponse) -> date | None:
        require_portal(self.list_url, response)
        root = response_html(response)
        if self.detail_address(response.url) is None:
            raise ValueError("response outside source details")
        trade_body(root, self.title_xpath)
        return None

    def to_html(self, raw: bytes) -> str:
        return trade_body(checked_html(raw.decode("utf-8")), self.title_xpath)


class BeifangTender(TradeHtmlBoard):
    source_id = "bfjsjt-tender"
    list_url = "http://zb.bfjsjt.com/portal/list?chnlcode=tender"
    row_xpath = "//ul[contains(concat(' ', normalize-space(@class), ' '), ' table_list_detail ')]/li"
    detail_path = re.compile(r"/portal/detail")
    title_xpath = (
        "//h1|//*[contains(concat(' ', normalize-space(@class), ' '), ' tender_detail_title ')"
        " or contains(concat(' ', normalize-space(@class), ' '), ' detail_title ')]"
        "|//*[contains(concat(' ', normalize-space(@class), ' '), ' tender_detail_top ')]"
        "//*[contains(concat(' ', normalize-space(@class), ' '), ' title ')]"
    )

    _page_size = 500
    _page_number = r"[1-9][0-9]{0,8}"
    _published_timestamp = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}")
    _page_input = "//span[contains(concat(' ', normalize-space(@class), ' '), ' page_input ')]"
    _detail_heading = "//*[contains(concat(' ', normalize-space(@class), ' '), ' tender_detail_title ')]"
    _detail_content = "//*[@id='tender_detail_bottom']//*[@id='doc-content']"

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or type(page) is not int or not re.fullmatch(self._page_number, str(page)):
            raise ValueError("invalid Beifang listing request")
        return FetchRequest("GET", f"{self.list_url}&pageIndex={page}&pagestr={page}&pageSize={self._page_size}")

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        address = require_portal(self.list_url, response)
        query = parse_qs(address.query, keep_blank_values=True)
        requested_pages = query.get("pageIndex", [])
        if (channel not in self.channels or address.path != urlsplit(self.list_url).path
                or query.get("chnlcode") != ["tender"] or query.get("pageSize") != [str(self._page_size)]
                or set(query) != {"chnlcode", "pageIndex", "pagestr", "pageSize"}
                or len(requested_pages) != 1 or not re.fullmatch(self._page_number, requested_pages[0])
                or query.get("pagestr") != requested_pages):
            raise ValueError("response outside Beifang list request")
        requested = int(requested_pages[0])
        root = response_html(response)
        counters = root.xpath(self._page_input + "/i/text()")
        totals = root.xpath("//i[contains(concat(' ', normalize-space(@class), ' '), ' page_hint_ptotal ')]/text()")
        sizes = root.xpath("//p[contains(concat(' ', normalize-space(@class), ' '), ' page_select_current ')]/text()")
        if (root.xpath(self._page_input + "/input/@value") != requested_pages
                or len(counters) != 1 or not re.fullmatch(rf"/{self._page_number}", counters[0].strip())
                or len(totals) != 1 or not re.fullmatch(self._page_number, totals[0].strip())
                or [size.strip() for size in sizes] != [str(self._page_size)]
                or len(root.xpath("//*[@dt-type='next']")) != 1):
            raise ValueError("Beifang pager does not match requested page")

        total = int(totals[0])
        if int(counters[0].strip()[1:]) != (total + self._page_size - 1) // self._page_size:
            raise ValueError("inconsistent Beifang page and record totals")
        rows = root.xpath(self.row_xpath)
        offset = OffsetPage(total, self._page_size, len(rows))
        has_next = offset.has_more(requested)
        refs = []
        complete = True
        for row in rows:
            detail = self.detail_address(row.get("data-href", ""))
            titles = row.xpath("./div[1]/a[not(@hidden or @aria-hidden='true')]")
            if detail is None or len(titles) != 1 or row.xpath("ancestor-or-self::*[@hidden or @aria-hidden='true']"):
                complete = False
                continue
            title = (titles[0].get("title") or visible_text(titles[0])).strip()
            timestamps = row.xpath("./div[4]/h3/@title")
            published = (
                publication_date(timestamps[0])
                if len(timestamps) == 1 and self._published_timestamp.fullmatch(timestamps[0]) else None
            )
            if not title or published is None:
                complete = False
            refs.append(DetailRef(self.notice_key(detail), detail.geturl(), published, title))
        if not refs:
            raise ValueError("no Beifang announcement links recognized on the list")
        return ListPage(tuple(refs), has_next, coverage_gap=None if complete else "unrecognized_list_rows",
                        ordered_by_publication=complete, reported_page=requested, offset=offset)

    def notice_key(self, address: SplitResult) -> str | None:
        values = parse_qs(address.query, keep_blank_values=True).get("docid", [])
        return native_id(values[0]) if len(values) == 1 else None

    def detail_address(self, href: str) -> SplitResult | None:
        address = super().detail_address(href)
        if (address is not None and self.notice_key(address) is not None
                and parse_qs(address.query, keep_blank_values=True).get("chnlcode") == ["tender"]):
            return address
        return None

    def check_detail(self, response: FetchResponse) -> None:
        require_portal(self.list_url, response)
        root = response_html(response)
        if self.detail_address(response.url) is None:
            raise ValueError("response outside Beifang notice route")
        headings = root.xpath(self._detail_heading)
        bodies = root.xpath(self._detail_content)
        if (len(headings) != 1 or not visible_text(headings[0]) or len(bodies) != 1
                or not any(text.strip() for text in bodies[0].xpath(".//text()[not(ancestor::script or ancestor::style)]"))):
            raise ValueError("Beifang announcement heading or substantive body missing")


class ChongqingTrade014(TradeHtmlBoard):
    source_id = "chongqing-ggzy-trade-014"
    contract_version = 2
    channels = ("published", "procurement-notices")
    list_url = "https://www.cqggzy.com/trade/014"
    _procurement_path = "/trade/014005"
    _procurement_category = "014005001"
    row_xpath = "//ul[contains(concat(' ', normalize-space(@class), ' '), ' min-h-80 ')]/li"
    detail_path = re.compile(r"/trade/(?:014/[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}|014005/[1-9][0-9]{0,19})")
    _category = re.compile(r"014\d{3,9}")
    _wrapper = "//div[contains(concat(' ', normalize-space(@class), ' '), ' detail-wrapper ')]"
    _heading = "./div[contains(concat(' ', normalize-space(@class), ' '), ' font-bold ')]"
    _rendered_body = "./div[contains(concat(' ', normalize-space(@class), ' '), ' app-detail ')]"

    def list_address(self, channel: str) -> str:
        if channel == "procurement-notices":
            return urljoin(self.list_url, self._procurement_path) + "?categoryNum=" + self._procurement_category
        return self.list_url

    def _check_channel(self, channel: str, ref: DetailRef) -> None:
        procurement = urlsplit(ref.source_url).path.startswith(self._procurement_path + "/")
        if (channel == "procurement-notices") != procurement:
            raise ValueError("Chongqing notice outside requested channel")

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        if response.url != self.list_address(channel):
            raise ValueError("unexpected Chongqing listing address")
        listing = super().parse_list(channel, response)
        refs = listing.refs
        if channel == "published":
            refs = tuple(ref for ref in refs if not urlsplit(ref.source_url).path.startswith(self._procurement_path + "/"))
        if not refs:
            raise ValueError("no Chongqing notices in requested channel")
        for ref in refs:
            self._check_channel(channel, ref)
        if any(ref.published is None for ref in refs):
            raise ValueError("Chongqing list row lacks publication date")
        return ListPage(refs, False, coverage_gap="homepage_current_window")

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        self._check_channel(channel, ref)
        return super().detail_fetches(channel, ref)

    def detail_address(self, href: str) -> SplitResult | None:
        address = super().detail_address(href)
        if address is None:
            return None
        query = parse_qs(address.query, keep_blank_values=True)
        categories = query.get("categoryNum", ())
        if set(query) != {"categoryNum"} or len(categories) != 1 or not self._category.fullmatch(categories[0]):
            return None
        if address.path.startswith(self._procurement_path + "/") and categories[0] != self._procurement_category:
            return None
        return address

    def notice_key(self, address: SplitResult) -> str | None:
        return address.path.rsplit("/", 1)[-1] if self.detail_path.fullmatch(address.path) else None

    @staticmethod
    def _native_record(source: str) -> tuple[str, dict]:
        payload = json.loads(source)

        def at(index: object):
            if type(index) is not int or not 0 <= index < len(payload):
                raise ValueError("invalid Chongqing SSR reference")
            return payload[index]

        try:
            if not isinstance(payload, list) or payload[0] != ["ShallowReactive", 1]:
                raise ValueError("invalid Chongqing SSR payload")
            meta = at(1)
            data_ref = at(meta["data"])
            if data_ref[0] != "ShallowReactive" or at(meta["serverRendered"]) is not True:
                raise ValueError("Chongqing SSR data missing")
            data = at(data_ref[1])
            records = [at(value["currentRecord"]) for index in data.values()
                       if isinstance(value := at(index), dict) and "currentRecord" in value]
            record, = records
            fields = {field: at(record[field]) for field in ("infoId", "categoryNum", "title", "infoDate", "infoContent")}
            path = at(meta["path"])
        except (TypeError, KeyError, IndexError, AttributeError) as exc:
            raise ValueError("invalid Chongqing SSR notice record") from exc
        if not isinstance(path, str) or not all(isinstance(value, str) and value.strip() for value in fields.values()):
            raise ValueError("incomplete Chongqing SSR notice record")
        return path, fields

    def _document(self, source: str, address: SplitResult | None = None) -> tuple[str, date]:
        root = parse_document(source)
        payloads = root.xpath("//script[@id='__NUXT_DATA__' and @type='application/json']/text()")
        checked_document(root)
        if len(payloads) != 1:
            raise ValueError("Chongqing SSR notice record missing or ambiguous")
        path, record = self._native_record(payloads[0])
        native_address = self.detail_address(path)
        if (native_address is None or path != f"{native_address.path}?{native_address.query}"
                or (address is not None and address != native_address)
                or record["infoId"] != self.notice_key(native_address)
                or parse_qs(native_address.query)["categoryNum"] != [record["categoryNum"]]):
            raise ValueError("Chongqing notice identity does not match")
        published = publication_date(record["infoDate"])
        wrappers = root.xpath(self._wrapper)
        headings = wrappers[0].xpath(self._heading) if len(wrappers) == 1 else []
        bodies = wrappers[0].xpath(self._rendered_body) if len(wrappers) == 1 else []
        if (published is None or len(headings) != 1 or len(bodies) != 1
                or visible_text(headings[0]) != " ".join(record["title"].split())):
            raise ValueError("Chongqing notice heading, body, or publication date missing")
        document = checked_html(record["infoContent"])
        content = document.find("body")
        canonical_text = "".join(visible_text(content).split()) if content is not None else ""
        if not canonical_text or canonical_text not in "".join(visible_text(bodies[0]).split()):
            raise ValueError("Chongqing rendered body does not match current notice")
        return notice_markup(document, record["title"]), published

    def check_detail(self, response: FetchResponse) -> date:
        require_portal(self.list_url, response)
        address = self.detail_address(response.url)
        if address is None:
            raise ValueError("response outside Chongqing source details")
        return self._document(html_text(response), address)[1]

    def to_html(self, raw: bytes) -> str:
        return self._document(raw.decode("utf-8"))[0]


class EwbTradeBoard(TradeHtmlBoard):
    """Shared EWB layout. Missing publisher IDs keep discoveries provisional."""

    row_xpath = (
        "//li|//*[contains(concat(' ', normalize-space(@class), ' '), ' ewb-list-node ')]"
        "|//a[@href or @data-href or @data-url or @onclick]"
    )
    detail_path = re.compile(r"/jyxx/\d+/\d+/\d{8}/[^/%]+\.html")
    explicit_link = re.compile(r"""['"](/jyxx/[^'" ]+)['"]""")

    def link_href(self, row: HtmlElement, link: HtmlElement) -> str:
        for element in (link, row):
            for attribute in ("href", "data-href", "data-url"):
                href = element.get(attribute, "")
                if self.detail_address(href) is not None:
                    return href
            if (link_literal := self.explicit_link.search(element.get("onclick", ""))) is not None:
                if self.detail_address(link_literal[1]) is not None:
                    return link_literal[1]
        return ""


class EwbConstructionBoard(EwbTradeBoard):
    _notice_id = r"[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}"
    detail_path = re.compile(
        rf"/jyxx/003001/(?P<category>003001\d{{3}})/20\d{{6}}/(?P<notice_id>{_notice_id})\.html"
    )
    row_xpath = "//tr[contains(concat(' ', normalize-space(@class), ' '), ' ewb-trade-tr ')]"
    article_xpath = "//div[contains(concat(' ', normalize-space(@class), ' '), ' article-info ')]"
    heading_xpath = "./h1"
    body_xpath = "./div[contains(concat(' ', normalize-space(@class), ' '), ' con ')]"
    identity_xpath = "//a[@id='viewGuid']/@value"

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        listing = super().parse_list(channel, response)
        if any(ref.published is None for ref in listing.refs):
            raise ValueError("construction list row lacks publication date")
        return ListPage(listing.refs, False, coverage_gap="homepage_current_window")

    def detail_address(self, href: str) -> SplitResult | None:
        address = super().detail_address(href)
        return address._replace(query="") if address is not None else None

    def notice_key(self, address: SplitResult) -> str | None:
        match = self.detail_path.fullmatch(address.path)
        return match["notice_id"].lower() if match else None

    def check_detail(self, response: FetchResponse) -> date | None:
        address = require_portal(self.list_url, response)
        match = self.detail_path.fullmatch(address.path)
        root = response_html(response)
        if match is None or root.xpath(self.identity_xpath) != [
            f"cms_{match['category']}_{match['notice_id']}"
        ]:
            raise ValueError("construction detail identity does not match the request route")
        self._announcement(root)
        return None

    @classmethod
    def _announcement(cls, root: HtmlElement) -> str:
        articles = root.xpath(cls.article_xpath)
        if len(articles) != 1:
            raise ValueError("construction announcement container missing or ambiguous")
        headings = articles[0].xpath(cls.heading_xpath)
        bodies = articles[0].xpath(cls.body_xpath)
        if (len(headings) != 1 or not visible_text(headings[0]) or len(bodies) != 1
                or not any(text.strip() for text in bodies[0].xpath(".//text()[not(ancestor::script or ancestor::style)]"))):
            raise ValueError("construction announcement heading or body missing")
        return "<html><body>" + "".join(serialize(node, with_tail=False) for node in (*headings, *bodies)) + "</body></html>"

    def to_html(self, raw: bytes) -> str:
        return self._announcement(checked_html(raw.decode("utf-8")))


class SuzhouTrade(EwbConstructionBoard):
    source_id = "suzhou-ggzy-trade"
    contract_version = 1
    list_url = "https://ggzy.suzhou.gov.cn/jyxx/tradeInfo.html"
    query_path = "/EpointWebBuilder/JyxxSearchAction.action"
    jump_path = "/jump.html"
    site_guid = "7eb5f7f1-9041-43ad-8e13-8fcb82ea831a"
    category = "003001"
    page_size = 15
    maximum_public_page = 10
    windowed = True

    def _query(self, command: str, **params: str) -> FetchRequest:
        url = urljoin(self.list_url, self.query_path)
        query = urlencode({"cmd": command, "siteguid": self.site_guid, **params})
        return FetchRequest("GET", f"{url}?{query}", headers=(("Referer", self.list_url),))

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or type(page) is not int or not 1 <= page <= self.maximum_public_page:
            raise ValueError("Suzhou page exceeds the public pagination range")
        return self._query(
            "getList1", categorynum=self.category, diqu="苏州市", xmmc="", zstype="", zblx="", jyfl="",
            pageSize=str(self.page_size), pageIndex=str(page - 1),
            starttime=window.since.isoformat(), endtime=window.until.isoformat(),
        )

    def _custom(self, response: FetchResponse, command: str) -> tuple[SplitResult, str]:
        address = require_portal(self.list_url, response)
        query = parse_qs(address.query, keep_blank_values=True)
        if (address.path != self.query_path or query.get("cmd") != [command]
                or query.get("siteguid") != [self.site_guid]):
            raise ValueError("unexpected Suzhou response route")
        if response.body.lstrip().startswith(b"<"):
            checked_html(response.body.decode("utf-8"))
            raise ValueError("received HTML at a Suzhou JSON endpoint")
        if response.media_type not in ("application/octet-stream", "application/json"):
            raise ValueError("unexpected Suzhou response media type")
        payload = json_body(response)
        status, custom = payload.get("status"), payload.get("custom")
        code = status.get("code") if isinstance(status, dict) else None
        if code in ("401", "403", "429"):
            raise SourceBlocked(f"publisher_{code}")
        if code != "200" or not isinstance(custom, str):
            raise ValueError("unknown Suzhou query response")
        return address, custom

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        from .trade_api import _list_refs

        if channel not in self.channels:
            raise ValueError("unknown Suzhou source channel")
        address, custom = self._custom(response, "getList1")
        query = parse_qs(address.query)
        indexes = query.get("pageIndex", ())
        if (query.get("categorynum") != [self.category] or query.get("diqu") != ["苏州市"]
                or query.get("pageSize") != [str(self.page_size)]
                or len(indexes) != 1 or not re.fullmatch(r"[0-9]", indexes[0])):
            raise ValueError("invalid Suzhou listing scope or offset")
        current = int(indexes[0]) + 1
        data = json.loads(custom)
        if not isinstance(data, dict) or not isinstance(data.get("Table"), list):
            raise ValueError("missing Suzhou list records")
        rows = data["Table"]
        offset = OffsetPage(data.get("TotalCount"), self.page_size, len(rows),
                            maximum_public_page=self.maximum_public_page)
        more = offset.has_more(current)
        refs = _list_refs(rows, self._list_ref)
        complete = len(refs) == len(rows)
        gap = None if complete else "unrecognized_list_rows"
        if complete and current == self.maximum_public_page and offset.total_records > current * self.page_size:
            gap = "pagination_requires_captcha"
        return ListPage(refs, more, coverage_gap=gap, ordered_by_publication=complete,
                        reported_page=current, offset=offset)

    def _list_ref(self, row: dict) -> DetailRef | None:
        key, category = row.get("infoid"), row.get("categorynum")
        if not isinstance(key, str) or not re.fullmatch(self._notice_id, key) or not isinstance(category, str):
            return None
        if not re.fullmatch(r"003001[0-9]{3}", category):
            return None
        raw_date = row.get("postdate")
        if not isinstance(raw_date, str) or not re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}", raw_date):
            return None
        published = publication_date(raw_date)
        text = row.get("title")
        title = visible_text(checked_html(text)) if isinstance(text, str) and text.strip() else ""
        if published is None or not title:
            return None
        query = urlencode({"infoguid": key, "categorynum": category})
        source_url = urljoin(self.list_url, self.jump_path) + "?" + query
        return DetailRef(key.lower(), source_url, published, title)

    def _jump_identity(self, channel: str, ref: DetailRef) -> tuple[str, str]:
        address = portal_address(self.list_url, ref.source_url)
        if channel not in self.channels or address is None or address.path != self.jump_path:
            raise ValueError("unexpected Suzhou detail reference")
        query = parse_qs(address.query, keep_blank_values=True)
        keys, categories = query.get("infoguid", ()), query.get("categorynum", ())
        if (set(query) != {"infoguid", "categorynum"} or len(keys) != 1 or len(categories) != 1
                or not re.fullmatch(self._notice_id, keys[0]) or keys[0].lower() != ref.source_key
                or not re.fullmatch(r"003001[0-9]{3}", categories[0])):
            raise ValueError("invalid Suzhou detail identity")
        return keys[0], categories[0]

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        key, category = self._jump_identity(channel, ref)
        locator = self._query("getDetailPath", infoid=key, categorynum=category, pageIndex="0")
        return (DetailFetch(f"{ref.source_key}/body", None, locator=locator),)

    def resolve_detail(self, channel: str, ref: DetailRef, response: FetchResponse) -> FetchRequest:
        key, category = self._jump_identity(channel, ref)
        address, target = self._custom(response, "getDetailPath")
        query = parse_qs(address.query)
        if query.get("infoid") != [key] or query.get("categorynum") != [category] or query.get("pageIndex") != ["0"]:
            raise ValueError("Suzhou locator response does not match the request identity")
        detail = portal_address(self.list_url, target)
        match = self.detail_path.fullmatch(detail.path) if detail else None
        if (match is None or detail.query or match["category"] != category
                or match["notice_id"].lower() != ref.source_key):
            raise ValueError("Suzhou resolved detail does not match the announcement identity")
        return FetchRequest("GET", detail.geturl())


class JiangsuTradeMirror(EwbConstructionBoard):
    source_id = "js-218-206-153-6-trade"
    list_url = "http://218.206.153.6:8099/jyxx/001001/001001001/tradeInfo.html"
    detail_path = re.compile(
        rf"/jyxx/001001/(?P<category>001001001)/20\d{{6}}/(?P<notice_id>{EwbConstructionBoard._notice_id})\.html"
    )


class XuzhouTrade(EwbConstructionBoard):
    source_id = "xuzhou-ggzy"
    list_url = "https://ggzy.zwb.xz.gov.cn/jyxx/003001/list.html"
    list_path = "/inteligentsearchnew/rest/esinteligentsearch/getFullTextDataNew"
    windowed = True
    page_size = 12

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or type(page) is not int or page < 1:
            raise ValueError("invalid Xuzhou listing request")
        return FetchRequest.post_json(urljoin(self.list_url, self.list_path), {
            "token": "", "pn": (page - 1) * self.page_size, "rn": self.page_size,
            "sdt": "", "edt": "", "wd": "", "inc_wd": "", "exc_wd": "",
            "fields": "title", "cnum": "002", "sort": '{"webdate":"0"}',
            "ssort": "title", "cl": 300, "terminal": "",
            "condition": [{"fieldName": "categorynum", "isLike": True, "likeType": 2, "equal": "003001"}],
            "time": [{"fieldName": "webdate", "startTime": f"{window.since.isoformat()} 00:00:00",
                      "endTime": f"{window.until.isoformat()} 23:59:59"}],
            "highlights": "title", "statistics": None, "unionCondition": None,
            "accuracy": "", "noParticiple": "0", "searchRange": None, "isBusiness": "1",
        }, headers=(("Referer", self.list_url), ("Accept", "application/json")))

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        from .trade_api import _api_response, _list_refs, _object, _rows

        if channel not in self.channels:
            raise ValueError("unknown Xuzhou source channel")
        address, payload = _api_response(self.list_url, response, self.list_path)
        if address.query or address.fragment:
            raise ValueError("response outside Xuzhou list request")
        result = _object(payload, "result")
        rows = _rows(result, "records")
        refs = _list_refs(rows, self._list_ref)
        complete = len(refs) == len(rows)
        return ListPage(refs, False, coverage_gap=None if complete else "unrecognized_list_rows",
                        ordered_by_publication=complete,
                        offset=OffsetPage(result.get("totalcount"), self.page_size, len(rows)))

    def _list_ref(self, row: dict) -> DetailRef | None:
        href = row.get("linkurl")
        address = self.detail_address(href) if isinstance(href, str) else None
        if address is None:
            return None
        match = self.detail_path.fullmatch(address.path)
        key = native_id(row.get("infoid"))
        if key is None or key.lower() != self.notice_key(address) or row.get("categorynum") != match["category"]:
            return None
        timestamp = row.get("webdate")
        if not isinstance(timestamp, str) or not re.fullmatch(r"\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}", timestamp):
            return None
        published = publication_date(timestamp)
        title_markup = row.get("title")
        if published is None or not isinstance(title_markup, str) or not title_markup.strip():
            return None
        title = visible_text(checked_html(title_markup))
        return DetailRef(key.lower(), address.geturl(), published, title) if title else None

beifang_tender_html = BeifangTender().to_html
chongqing_trade_html = ChongqingTrade014().to_html
ewb_construction_html = EwbConstructionBoard().to_html
