"""国家公共资源交易平台-交易公告（national-public-resources），试点只采集“央企招投标”来源的工程建设公告。

列表：POST /information/pubTradingInfo/getTradList（JSON，每页 20 条），按日期区间过滤；单次查询最多 1 000 条，
运行时按天切分窗口。服务端判定时返回 code 829 与验证码：来源停止，不处理验证码。
正文：详情路径 /information/deal/html/a/… 是外壳，正文在把 /a/ 换成 /b/ 的 HTML 页。
"""
from __future__ import annotations

from datetime import date

from miaoqia.acquisition.api.handlers import (DetailFetch, DetailRef, FetchRequest, FetchResponse, ListPage,
                                              SourceBlocked, Window)

from . import html_text, json_body

BASE = "https://www.ggzy.gov.cn"
LIST_URL = f"{BASE}/information/pubTradingInfo/getTradList"
SOURCE_CENTRAL_SOE = "2"
CLASSIFY_CONSTRUCTION = "01"
DETAIL_PREFIX = "/information/deal/html/a/"
CAPTCHA, REFINE = 829, 804


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
        refs = []
        for record in data.get("records") or []:
            path, rid = record.get("url"), record.get("id")
            if not isinstance(path, str) or not path.startswith(DETAIL_PREFIX) or not rid:
                raise ValueError("record lacks id or detail path")
            refs.append(DetailRef(source_key=rid, source_url=BASE + path,
                                  published=date.fromisoformat(record["publishTime"]), title=record.get("title") or ""))
        return ListPage(tuple(refs), int(data.get("current") or 0) < int(data.get("pages") or 0))

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        body = ref.source_url.replace(DETAIL_PREFIX, "/information/deal/html/b/", 1)
        return (DetailFetch(ref.source_key, FetchRequest("GET", body)),)

    def check_detail(self, response: FetchResponse) -> None:
        text = html_text(response)
        if "detail_content" not in text and "发布时间" not in text:
            raise ValueError("detail body missing")
