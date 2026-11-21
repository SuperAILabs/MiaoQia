"""Hunan's public procurement homepage, dated category, and JSON notice originals."""
from __future__ import annotations

import json
from datetime import datetime
from urllib.parse import SplitResult, parse_qs

from miaoqia.acquisition.api.handlers import (
    DetailFetch, DetailRef, FetchRequest, FetchResponse, ListPage, ListRequestPlan, OffsetPage, SourceBlocked, Window,
)

from . import china_date, json_body
from .anhui_ccgp import portal_data, portal_detail_url
from .ccgp_http import checked_html, official_url, publication_date, require_official, visible_text

_ORIGIN = "http://www.ccgp-hunan.gov.cn"
_PARENT_ID = "622707"
_SEARCH = "/portal/searchHome"
_CATEGORY = "/portal/category"
_PREFLIGHT = "/portal/sensitiveWords/check"


def _portal_response(response: FetchResponse) -> SplitResult:
    final = require_official(_ORIGIN, response)
    if response.body.lstrip().startswith(b"<"):
        checked_html(response.body.decode("utf-8"))  # Access gates may use the API's status or media type.
    if response.media_type != "application/json":
        raise ValueError("expected an application/json portal response")
    return final


def _response_data(response: FetchResponse) -> tuple[SplitResult, dict]:
    return _portal_response(response), portal_data(response)


def _detail_query(url: SplitResult) -> tuple[str, str]:
    if url.path != "/portal/detail" or url.fragment:
        raise ValueError("response is outside the Hunan detail route")
    query = parse_qs(url.query, keep_blank_values=True)
    articles, parents = query.get("articleId", []), query.get("parentId", [])
    if set(query) != {"articleId", "parentId"} or len(articles) != 1 or len(parents) != 1:
        raise ValueError("unexpected Hunan detail query")
    if not articles[0].strip() or not parents[0].isascii() or not parents[0].isdigit():
        raise ValueError("invalid Hunan article or parent identifier")
    return articles[0], parents[0]


def _notice_refs(rows: list, parent: str) -> tuple[DetailRef, ...]:
    refs = []
    for row in rows:
        if not isinstance(row, dict):
            raise ValueError("invalid Hunan list row")
        article, title = row.get("articleId"), row.get("title")
        if not isinstance(article, str) or not article.strip() or not isinstance(title, str) or not title.strip():
            raise ValueError("Hunan list row lacks articleId or title")
        timestamp, label = row.get("publishDate"), row.get("publishDateString")
        published = publication_date(label) if isinstance(label, str) else None
        if published is None and type(timestamp) is int:
            try:
                published = china_date(timestamp)
            except (OverflowError, OSError) as exc:
                raise ValueError("invalid Hunan publication timestamp") from exc
        refs.append(DetailRef(article, portal_detail_url(_ORIGIN, article, parent), published, title.strip()))
    return tuple(refs)


class HunanGovernmentProcurement:
    source_id = "hunan-government-procurement"
    channels = ("homepage-procurement",)
    windowed = False

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or page != 1:
            raise ValueError("Hunan discovery is limited to the first homepage list")
        return FetchRequest.post_json(_ORIGIN + _SEARCH, {
            "code": "160-396330",
            "subCodes": ["160-945988"],
            "districtCode": "430000",
            "pageSize": 10,
            "needTotal": True,
            "needValidCount": False,
            "needNewCnt": True,
        })

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        final, data = _response_data(response)
        if final.path != _SEARCH:
            raise ValueError("response is outside the Hunan homepage API")
        rows = data.get("children")
        if not isinstance(rows, list):
            raise ValueError("Hunan homepage has no children list")
        parent = str(data.get("firstId", _PARENT_ID))
        if not parent.isascii() or not parent.isdigit():
            raise ValueError("invalid Hunan list parent identifier")
        refs: dict[str, DetailRef] = {}
        for ref in _notice_refs(rows, parent):
            refs.setdefault(ref.source_key, ref)
        return ListPage(tuple(refs.values()), False, coverage_gap="homepage_current_window")

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        target = official_url(_ORIGIN, _ORIGIN, ref.source_url)
        if target is None:
            raise ValueError("detail reference is outside the official Hunan origin")
        article, parent = _detail_query(target)
        if article != ref.source_key:
            raise ValueError("Hunan detail reference has a mismatched articleId")
        return (DetailFetch(article, FetchRequest("GET", portal_detail_url(_ORIGIN, article, parent))),)

    def check_detail(self, response: FetchResponse) -> None:
        final, data = _response_data(response)
        article, _ = _detail_query(final)
        if data.get("articleId") != article:
            raise ValueError("Hunan detail articleId is missing or does not match the request")
        if data.get("title") is not None and not isinstance(data["title"], str):
            raise ValueError("Hunan detail title is not text")
        content = data.get("content")
        if not isinstance(content, str) or not content.strip() or not visible_text(checked_html(content)):
            raise ValueError("Hunan detail has no announcement body")


class HunanProcurementCategory(HunanGovernmentProcurement):
    """The dated notice category linked from the homepage's procurement section."""

    channels = ("public-procurement",)
    windowed = True
    category_code = "160-945988"
    page_size = 100

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        if channel not in self.channels or type(page) is not int or page < 1:
            raise ValueError("invalid Hunan category page")
        return FetchRequest.post_json(_ORIGIN + _CATEGORY, {
            "categoryCode": self.category_code,
            "pageNo": page,
            "pageSize": self.page_size,
            "publishDateBegin": window.since.isoformat(),
            "publishDateEnd": window.until.isoformat(),
        })

    def list_plan(self, channel: str, window: Window, page: int, now: datetime) -> ListRequestPlan:
        identity = self.list_request(channel, window, page)
        body = json.loads(identity.json_body)
        body["_t"] = int(now.timestamp() * 1000)
        return ListRequestPlan(identity=identity, request=FetchRequest.post_json(identity.url, body),
                               preflight=FetchRequest.post_json(_ORIGIN + _PREFLIGHT, body))

    def check_list_preflight(self, response: FetchResponse) -> None:
        final = _portal_response(response)
        if final.path != _PREFLIGHT or final.query or final.fragment:
            raise ValueError("response is outside the Hunan preflight route")
        data = json_body(response)
        result = data.get("result")
        if data.get("success") is not True or not isinstance(result, dict) or "block" not in result:
            raise ValueError("Hunan preflight returned an unrecognized verdict")
        if result["block"] is not None:
            raise SourceBlocked("publisher_query_blocked")

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        final, data = _response_data(response)
        if channel not in self.channels or final.path != _CATEGORY or final.query or final.fragment:
            raise ValueError("response is outside the Hunan category route")
        rows, total = data.get("data"), data.get("total")
        if not isinstance(rows, list) or data.get("empty") is not (total == 0):
            raise ValueError("Hunan category row set does not match its reported state")
        refs = _notice_refs(rows, _PARENT_ID)
        return ListPage(refs, False, ordered_by_publication=True, offset=OffsetPage(total, self.page_size, len(rows)))
