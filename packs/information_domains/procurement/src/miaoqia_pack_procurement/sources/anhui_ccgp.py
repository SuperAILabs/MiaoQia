"""安徽政府采购网（anhui-government-procurement-spa）。

列表：POST /portal/category（JSON，按发布时间倒序），按公告类别分别分页。
正文：GET /portal/detail 返回 JSON，正文 HTML 在 result.data.content；原件按接口响应原样归档。
站点在连续请求后返回 429，限速与退避见 sources.yaml。
"""
from __future__ import annotations

from urllib.parse import urlencode

from miaoqia.acquisition.api.handlers import DetailFetch, DetailRef, FetchRequest, FetchResponse, ListPage, Window

from . import china_date, json_body

BASE = "https://www.ccgp-anhui.gov.cn"
PAGE_SIZE = 15
# 采购意向、采购公告、更正公告、结果公告、合同公告
CATEGORIES = tuple(f"ZcyAnnouncement{n}" for n in (1, 2, 3, 4, 5))
PARENT_ID = "541080"


def _data(response: FetchResponse) -> dict:
    body = json_body(response)
    data = (body.get("result") or {}).get("data")
    if body.get("success") is not True or not isinstance(data, dict):
        raise ValueError("unsuccessful portal response")
    return data


class AnhuiGovernmentProcurement:
    source_id = "anhui-government-procurement-spa"
    channels = CATEGORIES
    windowed = False

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        return FetchRequest.post_json(f"{BASE}/portal/category",
                                      {"categoryCode": channel, "pageNo": page, "pageSize": PAGE_SIZE})

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        data = _data(response)
        items = data.get("data") or []
        refs = []
        for item in items:
            article = item.get("articleId")
            if not isinstance(article, str) or not article or not isinstance(item.get("publishDate"), int):
                raise ValueError("list item lacks articleId or publishDate")
            refs.append(DetailRef(source_key=article, source_url=self._detail_url(article),
                                  published=china_date(item["publishDate"]), title=item.get("title") or ""))
        return ListPage(tuple(refs), len(items) == PAGE_SIZE)

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        return (DetailFetch(ref.source_key, FetchRequest("GET", ref.source_url)),)

    def check_detail(self, response: FetchResponse) -> None:
        content = _data(response).get("content")
        if not isinstance(content, str) or not content.strip():
            raise ValueError("detail has no content")

    @staticmethod
    def _detail_url(article: str) -> str:
        return f"{BASE}/portal/detail?{urlencode({'articleId': article, 'parentId': PARENT_ID})}"
