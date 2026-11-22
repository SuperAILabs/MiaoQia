"""Procurement homepage anchors, limited to the documented public detail locators."""
from __future__ import annotations

import re
from datetime import date
from html import escape
from urllib.parse import SplitResult, parse_qs

from miaoqia.acquisition.api.handlers import DetailFetch, DetailRef, FetchRequest, FetchResponse, ListPage
from miaoqia.sdk.source_html import HtmlElement, serialize

from . import DiscoveryOnly
from .ccgp_http import checked_html, publication_date, response_html, visible_text
from .trade_html import TradeHtmlBoard, require_portal

ARTICLE_ID = re.compile(r"[A-Za-z0-9+/=_-]{1,192}")


def _field(url: SplitResult, name: str) -> str | None:
    values = parse_qs(url.query, keep_blank_values=True).get(name, ())
    return values[0] if len(values) == 1 and values[0] else None


class HenanProcurementHomepage(TradeHtmlBoard):
    source_id = "henan-government-procurement-home"
    list_url = "https://zfcg.henan.gov.cn/"
    row_xpath = "//li[a[contains(@href, '/henan/content?') and contains(@href, 'channelCode=H600101')]]"
    detail_path = re.compile(r"/henan/content")
    _body_prefix = "/cmsweb35rc67w/henan/cgxx/cggg/webinfo/"
    _body_path = re.compile(rf"{_body_prefix}20\d{{2}}/\d{{2}}/\d{{2}}/[0-9]{{1,32}}\.htm")

    def detail_address(self, href: str) -> SplitResult | None:
        url = super().detail_address(href)
        if url and _field(url, "channelCode") == "H600101" and _field(url, "bz") == "1" and self.notice_key(url):
            return url
        return None

    def notice_key(self, address: SplitResult) -> str | None:
        identifier = _field(address, "infoId")
        return identifier if identifier and re.fullmatch(r"[0-9]{1,32}", identifier) else None

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        listing = super().parse_list(channel, response)
        if any(ref.published is None for ref in listing.refs):
            raise ValueError("Henan procurement list row lacks publication date")
        return ListPage(listing.refs, False, coverage_gap="homepage_current_window")

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        address = self.detail_address(ref.source_url)
        if (channel not in self.channels or address is None or not ref.source_key
                or self.notice_key(address) != ref.source_key or ref.published is None):
            raise ValueError("invalid Henan procurement announcement identity or date")
        published = ref.published.isoformat().replace("-", "/")
        url = self.list_url.rstrip("/") + f"{self._body_prefix}{published}/{ref.source_key}.htm"
        return (DetailFetch(ref.source_key, FetchRequest("GET", url)),)

    def check_detail(self, response: FetchResponse) -> None:
        address = require_portal(self.list_url, response)
        if address.query or self._body_path.fullmatch(address.path) is None:
            raise ValueError("response outside the Henan procurement body route")
        self._announcement(response_html(response))

    @staticmethod
    def _announcement(root: HtmlElement) -> str:
        tables = root.xpath("//body/table[contains(concat(' ', normalize-space(@class), ' '), ' Content ')]")
        if len(tables) != 1 or "项目编号" not in visible_text(tables[0]):
            raise ValueError("missing Henan procurement announcement body")
        return serialize(tables[0], with_tail=False)

    def to_html(self, raw: bytes) -> str:
        return self._announcement(checked_html(raw.decode("utf-8")))


class TibetProcurementHomepage(TradeHtmlBoard):
    source_id = "tibet-government-procurement-home"
    contract_version = 3
    list_url = "http://www.ccgp-xizang.gov.cn/"
    row_xpath = (
        "//*[contains(concat(' ', normalize-space(@class), ' '), ' projectAnnouncement ')]"
        "//*[contains(concat(' ', normalize-space(@class), ' '), ' notice_list ')]/ul/li"
    )
    detail_path = re.compile(r"/freecms/site/xizang/xzcggg/info/(20[0-9]{2})/([0-9]{1,32})\.html")
    _article_xpath = "//*[@id='print-content']"

    def list_fields(self, row: HtmlElement, link: HtmlElement) -> tuple[str, date | None]:
        title = " ".join(" ".join(link.xpath("text()")).split())
        dates = [published for node in link.xpath("./span") if (published := publication_date(visible_text(node)))]
        return title, dates[0] if len(dates) == 1 else None

    def detail_address(self, href: str) -> SplitResult | None:
        address = super().detail_address(href)
        return address if address and not address.query else None

    def notice_key(self, address: SplitResult) -> str | None:
        match = self.detail_path.fullmatch(address.path)
        return f"{match[1]}:{match[2]}" if match else None

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        listing = super().parse_list(channel, response)
        if any(not ref.source_key or ref.published is None or str(ref.published.year) != ref.source_key.split(":")[0]
               for ref in listing.refs):
            raise ValueError("Tibet procurement list row lacks a matching publication date")
        return ListPage(listing.refs, False, coverage_gap="homepage_current_window")

    def _detail(self, response: FetchResponse) -> tuple[str, str, date | None]:
        address = require_portal(self.list_url, response)
        locator = self.detail_address(address.geturl())
        source_key = self.notice_key(locator) if locator is not None else None
        if source_key is None:
            raise ValueError("response outside the Tibet procurement detail route")
        title, published, _, _ = self._announcement(response_html(response))
        if published is not None and str(published.year) != source_key.split(":")[0]:
            raise ValueError("mismatched Tibet procurement publication date")
        return source_key, title, published

    def check_detail(self, response: FetchResponse) -> date:
        _, _, published = self._detail(response)
        if published is None:
            raise ValueError("missing Tibet procurement publication date")
        return published

    def check_detail_with_ref(self, ref: DetailRef, response: FetchResponse) -> date | None:
        source_key, title, published = self._detail(response)
        if source_key != ref.source_key:
            raise ValueError("Tibet procurement announcement identity does not match the listing")
        if published is None and (ref.published is None or str(ref.published.year) != source_key.split(":")[0]
                                  or title != ref.title):
            raise ValueError("missing Tibet procurement publication date with matching list evidence")
        return published

    @classmethod
    def _announcement(cls, root: HtmlElement) -> tuple[str, date | None, str, HtmlElement]:
        articles = root.xpath(cls._article_xpath)
        if len(articles) != 1:
            raise ValueError("missing Tibet procurement announcement container")
        article = articles[0]
        headers = article.xpath(".//*[contains(concat(' ', normalize-space(@class), ' '), ' notice-hear ')]")
        titles = headers[0].xpath(".//h2") if len(headers) == 1 else []
        bodies = article.xpath(".//*[contains(concat(' ', normalize-space(@class), ' '), ' notice-con ')]")
        if len(titles) != 1 or not (title := visible_text(titles[0])):
            raise ValueError("missing Tibet procurement announcement title")
        if len(bodies) != 1 or not visible_text(bodies[0]):
            raise ValueError("missing Tibet procurement announcement body")
        fields = [match[1] for node in headers[0].xpath(".//span[not(ancestor::h2)]")
                  if (match := re.fullmatch(r"发布时间\s*[:：]\s*(.*)", visible_text(node)))]
        if len(fields) != 1:
            raise ValueError("missing or ambiguous Tibet procurement publication date field")
        value = fields[0]
        published = publication_date(value) if value else None
        if value and published is None:
            raise ValueError("invalid Tibet procurement publication date")
        return title, published, value, bodies[0]

    def to_html(self, raw: bytes) -> str:
        title, _, value, body = self._announcement(checked_html(raw.decode("utf-8")))
        publication = f"<p>公告时间：{escape(value)}</p>" if value else ""
        return f"<h1>{escape(title)}</h1>{publication}{serialize(body, with_tail=False)}"


class XinjiangProcurementHomepage(DiscoveryOnly, TradeHtmlBoard):
    contract_version = 2
    source_id = "xinjiang-government-procurement-home"
    list_url = "http://www.ccgp-xinjiang.gov.cn/"
    row_xpath = "//a[contains(@href, '/site/detail?')]"
    detail_path = re.compile(r"/site/detail")

    def detail_address(self, href: str) -> SplitResult | None:
        url = super().detail_address(href)
        if url and _field(url, "parentId") and self.notice_key(url):
            return url
        return None

    def notice_key(self, address: SplitResult) -> str | None:
        identifier = _field(address, "articleId")
        return identifier if identifier and ARTICLE_ID.fullmatch(identifier) else None


henan_procurement_html = HenanProcurementHomepage().to_html
tibet_procurement_html = TibetProcurementHomepage().to_html
xinjiang_procurement_html = XinjiangProcurementHomepage().to_html
