"""国家公共资源交易平台-交易公告（national-public-resources），试点只采集“央企招投标”来源的工程建设公告。

列表：POST /information/pubTradingInfo/getTradList（JSON，每页 20 条），按日期区间过滤；单次查询最多 1 000 条，
运行时按天切分窗口。服务端判定时返回 code 829 与验证码：来源停止，不处理验证码。
正文：详情路径 /information/deal/html/a/… 是外壳，正文在把 /a/ 换成 /b/ 的 HTML 页。
"""
from __future__ import annotations

import logging
from datetime import date

from miaoqia.acquisition.api.handlers import (DetailFetch, DetailRef, FetchRequest, FetchResponse, ListPage,
                                              OffsetPage, SourceBlocked, Window)

from . import html_text, json_body
from .ccgp_http import checked_html, visible_text

BASE = "https://www.ggzy.gov.cn"
LIST_URL = f"{BASE}/information/pubTradingInfo/getTradList"
SOURCE_CENTRAL_SOE = "2"
CLASSIFY_CONSTRUCTION = "01"
DETAIL_PREFIX = "/information/deal/html/a/"
CAPTCHA, REFINE = 829, 804
RESULT_CAP = 1000
DETAIL_METADATA_ONLY = "national_detail_metadata_only"
log = logging.getLogger(__name__)


class NationalPublicResources:
    source_id = "national-public-resources"
    channels = (f"{SOURCE_CENTRAL_SOE}:{CLASSIFY_CONSTRUCTION}",)
    windowed = True

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        source, classify = channel.split(":")
        return FetchRequest("POST", LIST_URL, form=(
            ("SOURCE_TYPE", source), ("DEAL_TIME", "06"), ("TIMEBEGIN", window.since.isoformat()),
            ("TIMEEND", window.until.isoformat()), ("DEAL_CLASSIFY", classify), ("PAGENUMBER", str(page))),
            headers=(("Referer", f"{BASE}/deal/dealList.html"),))

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        body = json_body(response)
        code = body.get("code")
        if code == CAPTCHA:
            raise SourceBlocked("captcha_challenge")
        if code == REFINE:
            raise ValueError("query exceeds the platform result cap")
        data = body.get("data")
        if code != 200 or not isinstance(data, dict):
            raise ValueError(f"unexpected list response code {code!r}")
        records = data.get("records")
        page, pages = data.get("current"), data.get("pages")
        if not isinstance(records, list) or type(page) is not int or type(pages) is not int:
            raise ValueError("invalid national pagination metadata")
        offset = OffsetPage(data.get("total"), data.get("size"), len(records))
        if pages != (offset.total_records + offset.page_size - 1) // offset.page_size:
            raise ValueError("national page count disagrees with the total")
        continuation = offset.has_more(page)
        refs = []
        for record in records:
            if not isinstance(record, dict) or not isinstance(record.get("publishTime"), str):
                raise ValueError("invalid national list record")
            path, rid = record.get("url"), record.get("id")
            if not isinstance(path, str) or not path.startswith(DETAIL_PREFIX):
                raise ValueError("record lacks detail path")
            source_key = rid if isinstance(rid, str) and rid.strip() else None
            refs.append(DetailRef(source_key=source_key, source_url=BASE + path,
                                  published=date.fromisoformat(record["publishTime"]), title=record.get("title") or ""))
        gap = "publisher_result_cap" if offset.total_records >= RESULT_CAP else None
        return ListPage(tuple(refs), continuation, coverage_gap=gap, reported_page=page, offset=offset)

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        body = ref.source_url.replace(DETAIL_PREFIX, "/information/deal/html/b/", 1)
        return (DetailFetch(ref.source_key or "body", FetchRequest("GET", body)),)

    def check_detail(self, response: FetchResponse) -> None:
        root = checked_html(html_text(response))
        content = root.xpath("//*[@id='mycontent']//*[contains(concat(' ', normalize-space(@class), ' '), ' detail_content ')]")
        if len(content) == 1 and visible_text(content[0]):
            return
        if root.xpath("//*[@id='mycontent']//table[.//tr[th and td]]"):
            log.warning("source=%s reason=%s", self.source_id, DETAIL_METADATA_ONLY)
            raise ValueError(DETAIL_METADATA_ONLY)
        raise ValueError("detail body missing")
