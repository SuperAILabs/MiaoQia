"""Public HTML bulletin boards with explicit source-owned listing and body selectors."""
from __future__ import annotations

import re
from datetime import date
from html import escape
from urllib.parse import urljoin, urlsplit

from miaoqia.acquisition.api.handlers import DetailFetch, DetailRef, FetchRequest, FetchResponse, ListPage, Window
from miaoqia.sdk.source_html import HtmlElement, parse_document, serialize

from . import html_text
from .ccgp_http import checked_document, checked_html, official_url, publication_date, require_official, visible_text

CONTENT_CLASS = "contains(concat(' ', normalize-space(@class), ' '), $class_token)"


class HtmlNoticeBoard:
    channels = ("published",)
    windowed = False
    source_id: str
    board_url: str
    list_xpath: str
    notice_path: re.Pattern
    body_xpath: str
    body_class = ""

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or page != 1:
            raise ValueError("only the verified entry page is available")
        return FetchRequest("GET", self.board_url)

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        root = checked_html(html_text(response))
        origin = urlsplit(self.board_url)
        refs = {}
        for container in root.xpath(self.list_xpath):
            links = [container] if container.tag == "a" else container.xpath(".//a[@href]")
            for link in links:
                address = urlsplit(urljoin(self.board_url, link.get("href", "")))
                if (address.scheme, address.netloc) != (origin.scheme, origin.netloc):
                    continue
                match = self.notice_path.fullmatch(address.path)
                title = " ".join((link.get("title") or link.text_content()).split())
                if match is None or not title:
                    continue
                key = match.groupdict().get("notice_id")
                locator = address._replace(query="", fragment="").geturl()
                refs[locator] = DetailRef(key, locator, publication_date(container.text_content()), title)
        if not refs:
            raise ValueError("no recognized announcement links on the bulletin page")
        return ListPage(tuple(refs.values()), False, coverage_gap="verified_entry_page_only")

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        return (DetailFetch(ref.source_key or "body", FetchRequest("GET", ref.source_url)),)

    def _body(self, content: str) -> str:
        root = checked_html(content)
        nodes = root.xpath(self.body_xpath, class_token=f" {self.body_class} ")
        if len(nodes) != 1 or not nodes[0].xpath("normalize-space(.)"):
            raise ValueError("publisher detail body missing or ambiguous")
        body = nodes[0]
        lead = self._lead(root, body)
        title = f"<h1>{escape(lead)}</h1>" if lead else ""
        return f"<html><body>{title}{serialize(body)}</body></html>"

    def _lead(self, root: HtmlElement, body: HtmlElement) -> str:
        if body.xpath(".//h1"):
            return ""
        return root.xpath("normalize-space((//h1)[1])") or root.xpath("normalize-space((//title)[1])")

    def check_detail(self, response: FetchResponse) -> date | None:
        self._body(html_text(response))
        return None

    def to_html(self, raw: bytes) -> str:
        return self._body(raw.decode("utf-8"))


class ChemicalEngineeringBoard(HtmlNoticeBoard):
    source_id = "cncecyc-ywgg1gc"
    origin = "https://bid.cncecyc.com"
    board_url = origin + "/cms/channel/ywgg1gc/index.htm"
    list_xpath = "//*[@id='list1']//li"
    notice_path = re.compile(r"/cms/channel/ywgg1gc/(?P<notice_id>\d+)\.htm")
    body_xpath, body_class = f"//*[{CONTENT_CLASS}]", "ninfo-con"
    _page_number = r"[1-9][0-9]{0,8}"
    _page_counter = re.compile(rf"当前\s*({_page_number})\s*页\s*/\s*共计\s*({_page_number})\s*页")

    def _lead(self, root: HtmlElement, body: HtmlElement) -> str:
        headings = root.xpath(f"//*[{CONTENT_CLASS}]/h2", class_token=" ninfo-title ")
        if len(headings) == 1 and (heading := headings[0].xpath("normalize-space(.)")):
            return heading
        return super()._lead(root, body)

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or type(page) is not int or not re.fullmatch(self._page_number, str(page)):
            raise ValueError("invalid CNCE listing request")
        return FetchRequest("GET", self.board_url if page == 1 else f"{self.board_url}?pageNo={page}")

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        address = require_official(self.origin, response)
        if channel not in self.channels or address.path != urlsplit(self.board_url).path or address.fragment:
            raise ValueError("response is outside the CNCE listing route")
        requested = 1
        if address.query:
            page_param = re.fullmatch(rf"pageNo=({self._page_number})", address.query)
            if page_param is None:
                raise ValueError("invalid CNCE listing query")
            requested = int(page_param[1])
        root = checked_html(html_text(response))
        pagers = root.xpath(f"//*[{CONTENT_CLASS}]", class_token=" pagination ")
        counters = pagers[0].xpath(f".//*[{CONTENT_CLASS}]", class_token=" pag-txt ") if len(pagers) == 1 else []
        counter = self._page_counter.fullmatch(counters[0].text_content().strip()) if len(counters) == 1 else None
        if counter is None:
            raise ValueError("missing or invalid CNCE page counter")
        current, total = map(int, counter.groups())
        if current != requested or current > total:
            raise ValueError("CNCE page counter does not match requested page")
        has_next = current < total
        next_links = pagers[0].xpath(".//a[normalize-space(.)='下页']")
        if len(next_links) != 1:
            raise ValueError("missing or ambiguous CNCE next-page control")
        next_link = next_links[0]
        classes = next_link.get("class", "").split()
        if has_next:
            if next_link.get("page") != str(current + 1) or "pageItem" not in classes or "disabled" in classes:
                raise ValueError("invalid CNCE next-page control")
        elif next_link.get("page") is not None or "disabled" not in classes:
            raise ValueError("CNCE terminal page advertises a next page")
        refs = []
        complete_rows = True
        for row in root.xpath(self.list_xpath):
            dates = row.xpath(f".//*[{CONTENT_CLASS}]", class_token=" bidDate ")
            published = publication_date(dates[0].text_content()) if len(dates) == 1 else None
            row_refs = []
            for link in row.xpath(".//a[@href]"):
                address = official_url(self.origin, self.board_url, link.get("href"))
                match = self.notice_path.fullmatch(address.path) if address else None
                title = " ".join((link.get("title") or link.text_content()).split())
                if match and title:
                    locator = address._replace(query="", fragment="").geturl()
                    row_refs.append((match["notice_id"], locator, title))
            if len(row_refs) != 1:
                published = None
            if published is None:
                complete_rows = False
            refs.extend(DetailRef(key, locator, published, title) for key, locator, title in row_refs)
        if not refs:
            raise ValueError("no recognized announcement links on the bulletin page")
        gap = None if complete_rows else "unrecognized_list_rows"
        return ListPage(tuple(refs), has_next, coverage_gap=gap, ordered_by_publication=complete_rows, reported_page=current)

    def check_detail(self, response: FetchResponse) -> None:
        address = require_official(self.origin, response)
        if not self.notice_path.fullmatch(address.path) or address.query or address.fragment:
            raise ValueError("response is outside the CNCE notice route")
        super().check_detail(response)


class RailwayMaterialsBoard(HtmlNoticeBoard):
    source_id = "crecgec-home"
    contract_version = 2
    board_url = "https://www.crecgec.com/"
    list_xpath = "//a[@href]"
    notice_path = re.compile(r"/notice/(?P<notice_id>\d+)\.html")

    def _body(self, content: str) -> str:
        raise ValueError("the publisher's procurement notice body is not verified")


class SichuanBiddingBoard(HtmlNoticeBoard):
    source_id = "scbid-zbxx"
    origin = "https://www.scbid.com"
    board_url = origin + "/zbxx"
    list_xpath = f"./div[{CONTENT_CLASS}]"
    notice_path = re.compile(r"/bx/detail/[0-9]+_(?P<notice_id>[0-9]+)\.html")
    body_xpath, body_class = f"//*[{CONTENT_CLASS}]", "content"
    contract_version = 2
    page_size = 50
    _verified_pages = 2
    _date_label = re.compile(r"发布日期\s*[：:]?\s*[0-9]{4}年\s*[0-9]{1,2}月\s*[0-9]{1,2}日")
    _detail_nid = re.compile(r'(?m)^[ \t]*data: \{nid: "([0-9]+)"\},?[ \t\r]*$')

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or type(page) is not int or not 1 <= page <= self._verified_pages:
            raise ValueError("invalid SCBid public listing request")
        return FetchRequest("GET", self.board_url if page == 1 else self._page_url(page))

    def _page_url(self, page: int) -> str:
        return f"{self.board_url}/p{page}.html"

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        require_official(self.origin, response)
        routes = {self.board_url: 1, self._page_url(2): 2}
        current = routes.get(response.url)
        if channel not in self.channels or current is None:
            raise ValueError("response is outside the SCBid listing routes")
        root = checked_html(html_text(response))
        sections = root.xpath(f"//*[{CONTENT_CLASS}]", class_token=" bidding-sections-lists ")
        if len(sections) != 1:
            raise ValueError("missing or ambiguous SCBid listing section")
        section = sections[0]
        pagers = section.xpath(f".//ul[{CONTENT_CLASS}]", class_token=" pagination-ul ")
        if len(pagers) != 1:
            raise ValueError("missing or ambiguous SCBid pager")
        pager = pagers[0]
        current_items = pager.xpath(f"./li[{CONTENT_CLASS}]", class_token=" current ")
        if len(current_items) != 1 or self._page_link(current_items[0], current, response.url) != self._page_url(current):
            raise ValueError("SCBid pager does not match the requested page")
        has_next = current < self._verified_pages
        if has_next and self._page_link(pager, current + 1, response.url) != self._page_url(current + 1):
            raise ValueError("missing or invalid SCBid next page")

        containers = section.xpath(f".//div[{CONTENT_CLASS}]", class_token=" rows-container ")
        if len(containers) != 1:
            raise ValueError("missing or ambiguous SCBid list rows")
        refs = []
        for row in containers[0].xpath(self.list_xpath, class_token=" row "):
            links = row.xpath(f"./div[{CONTENT_CLASS}]/a[@href]", class_token=" title ")
            dates = row.xpath(f"./div[{CONTENT_CLASS}]", class_token=" stime ")
            if len(links) != 1 or len(dates) != 1:
                raise ValueError("missing or ambiguous SCBid notice fields")
            address = official_url(self.origin, response.url, links[0].get("href"))
            match = self.notice_path.fullmatch(address.path) if address else None
            title = " ".join(links[0].text_content().split())
            month_day = dates[0].text_content().strip()
            if (match is None or address.query or address.fragment
                    or not title or not re.fullmatch(r"[0-9]{2}-[0-9]{2}", month_day)):
                raise ValueError("invalid SCBid announcement row")
            refs.append(DetailRef(match["notice_id"], self.origin + address.path, None, title))
        if not refs:
            raise ValueError("SCBid listing contains no notice rows")
        if len(refs) > self.page_size:
            raise ValueError("SCBid listing exceeds the verified page size")
        gap = "list_publication_date_unavailable" if has_next else "pagination_unverified"
        return ListPage(tuple(refs), has_next, coverage_gap=gap, reported_page=current)

    def _page_link(self, container: HtmlElement, page: int, base: str) -> str | None:
        links = container.xpath(".//a[@href and normalize-space(.)=$page]", page=str(page))
        if len(links) != 1:
            return None
        address = official_url(self.origin, base, links[0].get("href"))
        return address.geturl() if address is not None else None

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        address = official_url(self.origin, self.origin, ref.source_url)
        match = self.notice_path.fullmatch(address.path) if address else None
        if (channel not in self.channels or match is None or ref.source_key != match["notice_id"]
                or ref.source_url != self.origin + address.path):
            raise ValueError("invalid SCBid detail reference")
        return super().detail_fetches(channel, ref)

    def check_detail(self, response: FetchResponse) -> date:
        address = require_official(self.origin, response)
        match = self.notice_path.fullmatch(address.path)
        if match is None or response.url != self.origin + address.path:
            raise ValueError("response is outside the SCBid notice route")
        key, published, _ = self._detail_body(html_text(response))
        if key != match["notice_id"]:
            raise ValueError("SCBid detail identity does not match the notice route")
        return published

    def _body(self, content: str) -> str:
        return self._detail_body(content)[2]

    def _detail_body(self, content: str) -> tuple[str, date, str]:
        root = parse_document(content)
        scripts = root.xpath(f"//script[not(@src) and not(ancestor::*[{CONTENT_CLASS}])]/text()",
                             class_token=f" {self.body_class} ")
        # Only the page template's standalone notice metadata binds this body to a publisher nid.
        keys = {key for script in scripts for key in self._detail_nid.findall(script)}
        checked_document(root)
        if len(keys) != 1:
            raise ValueError("missing or conflicting SCBid detail identity")
        titles = root.xpath(f"//div[{CONTENT_CLASS}]", class_token=" scbid-printer-part-title ")
        bodies = root.xpath(self.body_xpath, class_token=f" {self.body_class} ")
        fields = root.xpath(f"//span[{CONTENT_CLASS}]/span", class_token=" scbid-printer-part-time ")
        dates = [match[0] for node in fields if (match := self._date_label.match(visible_text(node)))]
        published = publication_date(dates[0]) if len(dates) == 1 else None
        title = visible_text(titles[0]) if len(titles) == 1 else ""
        if published is None:
            raise ValueError("missing or invalid SCBid detail publication date")
        if not title or len(bodies) != 1 or not visible_text(bodies[0]):
            raise ValueError("missing or ambiguous SCBid title or notice body")
        body = serialize(bodies[0], with_tail=False)
        return keys.pop(), published, f"<html><body><h1>{escape(title)}</h1><p>公告时间：{published.isoformat()}</p>{body}</body></html>"


class ShangraoProcurementBoard(HtmlNoticeBoard):
    source_id = "ygcgfwpt-transaction"
    origin = "https://www.ygcgfwpt.com"
    _category = "002001003"
    _list_path = f"/jyxx/002001/{_category}/"
    board_url = origin + _list_path + "transaction_inquiry.html"
    _notice_id = r"[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}"
    notice_path = re.compile(rf"{_list_path}[0-9]{{8}}/(?P<notice_id>{_notice_id})\.html")
    list_xpath = "//ul[@id='infolist']/li"
    body_xpath = "//*[@id='content-main']"
    _static_pages = 6
    _page_size = 10
    _pager_fields = re.compile(
        r"(?m)^\s*var\s+(pageIndex|pageSize|pagetotalcount|total)\s*=\s*([^;\r\n]+);"
    )

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or type(page) is not int or not 1 <= page <= self._static_pages:
            raise ValueError("invalid Shangrao public listing request")
        return FetchRequest("GET", self._list_url(page))

    def _list_url(self, page: int) -> str:
        return self.board_url if page == 1 else f"{self.origin}{self._list_path}{page}.html"

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        from miaoqia.acquisition.api.handlers import OffsetPage
        from miaoqia.sdk.source_html import parse_document

        from .ccgp_http import checked_document, visible_text

        address = require_official(self.origin, response)
        root = parse_document(html_text(response))
        scripts = root.xpath("//script[not(@src)]/text()")
        checked_document(root)
        fields = [field for script in scripts for field in self._pager_fields.findall(script)]
        values = dict(fields)
        if set(values) != {"pageIndex", "pageSize", "pagetotalcount", "total"} or len(fields) != 4:
            raise ValueError("missing or ambiguous Shangrao page metadata")
        index = re.fullmatch(r"([0-9])\s*\+\s*1", values["pageIndex"].strip())
        total_value = values["total"].strip()
        if (index is None or int(index[1]) >= self._static_pages
                or values["pageSize"].strip() != str(self._page_size)
                or values["pagetotalcount"].strip() != str(self._static_pages)
                or not re.fullmatch(r"[1-9][0-9]{0,8}", total_value)):
            raise ValueError("invalid Shangrao page metadata")
        current, total = int(index[1]) + 1, int(total_value)
        if (channel not in self.channels or self.origin + address.path != self._list_url(current)
                or address.query or address.fragment):
            raise ValueError("Shangrao page metadata does not match its listing route")

        if len(root.xpath("//ul[@id='infolist']")) != 1:
            raise ValueError("missing or ambiguous Shangrao listing container")
        rows = root.xpath(self.list_xpath)
        offset = OffsetPage(total, self._page_size, len(rows))
        more_records = offset.has_more(current)
        refs = []
        for row in rows:
            links = row.xpath("./a[@href]")
            if len(links) != 1:
                raise ValueError("Shangrao listing row lacks a unique link")
            link = links[0]
            locator = official_url(self.origin, response.url, link.get("href"))
            match = self.notice_path.fullmatch(locator.path) if locator else None
            titles = link.xpath(f"./p[{CONTENT_CLASS}]", class_token=" title ")
            dates = link.xpath(f"./span[{CONTENT_CLASS}]", class_token=" date ")
            title = visible_text(titles[0]) if len(titles) == 1 else ""
            date_text = visible_text(dates[0]) if len(dates) == 1 else ""
            published = publication_date(date_text) if re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}", date_text) else None
            if match is None or locator.query or locator.fragment or not title or published is None:
                raise ValueError("invalid Shangrao announcement row")
            refs.append(DetailRef(match["notice_id"], self.origin + locator.path, published, title))
        if more_records and current == self._static_pages:
            # The publisher routes later pages through its captcha flow.
            return ListPage(tuple(refs), False, coverage_gap="pagination_requires_captcha",
                            ordered_by_publication=True, reported_page=current)
        return ListPage(tuple(refs), more_records, ordered_by_publication=True, reported_page=current, offset=offset)

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        address = official_url(self.origin, self.origin, ref.source_url)
        match = self.notice_path.fullmatch(address.path) if address else None
        if (channel not in self.channels or match is None or match["notice_id"] != ref.source_key
                or ref.source_url != self.origin + address.path):
            raise ValueError("Shangrao detail reference does not match its official notice route")
        return super().detail_fetches(channel, ref)

    def check_detail(self, response: FetchResponse) -> None:
        address = require_official(self.origin, response)
        match = self.notice_path.fullmatch(address.path)
        if match is None or address.query or address.fragment:
            raise ValueError("response is outside the Shangrao notice route")
        key, _ = self._detail_body(html_text(response))
        if key != match["notice_id"]:
            raise ValueError("Shangrao detail identity does not match its notice route")

    def _body(self, content: str) -> str:
        return self._detail_body(content)[1]

    def _detail_body(self, content: str) -> tuple[str, str]:
        root = checked_html(content)
        keys = root.xpath("//*[@id='infoid']")
        categories = root.xpath("//*[@id='cnumnow']")
        key = keys[0].text_content().strip() if len(keys) == 1 else ""
        if (not re.fullmatch(self._notice_id, key) or len(categories) != 1
                or categories[0].text_content().strip() != self._category):
            raise ValueError("missing or invalid Shangrao detail identity")
        bodies = root.xpath(self.body_xpath)
        headings = root.xpath(f"//h3[{CONTENT_CLASS}]", class_token=" title ")
        title = " ".join(headings[0].text_content().split()) if len(headings) == 1 else ""
        if len(bodies) != 1 or not bodies[0].xpath("normalize-space(.)") or not title:
            raise ValueError("missing or ambiguous Shangrao announcement title or body")
        return key, f"<html><body><h1>{escape(title)}</h1>{serialize(bodies[0], with_tail=False)}</body></html>"


chemical_engineering_html = ChemicalEngineeringBoard().to_html
railway_materials_html = RailwayMaterialsBoard().to_html
sichuan_bidding_html = SichuanBiddingBoard().to_html
shangrao_procurement_html = ShangraoProcurementBoard().to_html
