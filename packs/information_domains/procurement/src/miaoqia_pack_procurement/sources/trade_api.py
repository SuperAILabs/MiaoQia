"""JSON and form protocols for local and specialist procurement notice sources."""
from __future__ import annotations

import json
import re
from collections.abc import Callable
from datetime import date
from urllib.parse import SplitResult, parse_qs, urlencode, urlsplit

from miaoqia.acquisition.api.handlers import (
    DetailFetch, DetailRef, FetchRequest, FetchResponse, ListPage, OffsetPage, SourceBlocked, Window,
)

from . import json_body
from .ccgp_http import checked_html, require_official, response_html, visible_text
from .ccgp_http import publication_date
from .trade_html import ENTRY_PAGE_ONLY, native_id, notice_markup, notice_query_id, portal_address, trade_body

ACCEPT_JSON = (("Accept", "application/json"),)


def _check_api_status(body: dict) -> None:
    for field in ("code", "status"):
        code = body.get(field)
        if code in (401, "401", 403, "403"):
            raise SourceBlocked(f"api_{code}")
    if body.get("success") is False:
        raise ValueError("source API returned failure")


def _api_response(origin: str, response: FetchResponse, path: str | None = None) -> tuple[SplitResult, dict]:
    address = require_official(origin, response)
    if response.body.lstrip().startswith(b"<"):
        checked_html(response.body.decode("utf-8"))
        raise ValueError("received HTML at a JSON endpoint")
    if response.media_type != "application/json":
        if response.media_type in ("text/html", "text/plain"):
            response_html(response)
        raise ValueError("expected application/json")
    if path is not None and address.path != path:
        raise ValueError("unexpected source API response path")
    body = json_body(response)
    _check_api_status(body)
    return address, body


def _object(parent: dict, field: str) -> dict:
    value = parent.get(field)
    if not isinstance(value, dict):
        raise ValueError(f"missing API object {field}")
    return value


def _first_text(row: dict, *fields: str) -> str:
    for field in fields:
        value = row.get(field)
        if isinstance(value, str) and value.strip():
            return value.strip()
    return ""


def _first_id(row: dict, *fields: str) -> str | None:
    for field in fields:
        if row.get(field) not in (None, ""):
            return native_id(row[field])
    return None


def _rows(parent: dict, *fields: str) -> list:
    for field in fields:
        value = parent.get(field)
        if isinstance(value, list):
            return value
    raise ValueError("missing API list")


def _list_refs(rows: list, build_ref: Callable[[dict], DetailRef | None]) -> tuple[DetailRef, ...]:
    return tuple(ref for row in rows if isinstance(row, dict) and (ref := build_ref(row)) is not None)


def _first_page(rows: list, build_ref: Callable[[dict], DetailRef | None]) -> ListPage:
    accepted = _list_refs(rows, build_ref)
    refs = {}
    for ref in accepted:
        refs.setdefault(ref.source_key or ref.source_url, ref)
    gap = "unrecognized_list_rows" if len(accepted) < len(rows) else ENTRY_PAGE_ONLY
    return ListPage(tuple(refs.values()), False, gap)


def _locator(origin: str, path: str, **query: str) -> str:
    return f"{origin}{path}?{urlencode(query)}"


def _detail_id(address: SplitResult, body: dict, query_name: str, *id_fields: str, required: bool = False) -> None:
    requested = notice_query_id(address, (query_name,))
    returned = _first_id(body, *id_fields)
    if requested is None or ((required or returned is not None) and returned != requested):
        raise ValueError("detail identity does not match request locator")


class _JsonNotice:
    title_fields: tuple[str, ...]
    content_fields: tuple[str, ...]

    @staticmethod
    def document_payload(envelope: dict) -> dict:
        return envelope

    def render_document(self, payload: dict) -> str:
        title = _first_text(payload, *self.title_fields)
        content = _first_text(payload, *self.content_fields)
        if not title or not content:
            raise ValueError("announcement title or body missing")
        root = checked_html(content)
        body = root.find("body")
        if body is None or not visible_text(body):
            raise ValueError("announcement body has no visible content")
        return notice_markup(root, title)

    def to_html(self, raw: bytes) -> str:
        envelope = json_body(FetchResponse(200, "application/json", raw, ""))
        _check_api_status(envelope)
        return self.render_document(self.document_payload(envelope))


def _page_index(page: int) -> int:
    if page < 1:
        raise ValueError("page must be one-based")
    return page


def _require_channel(channel: str, channels: tuple[str, ...]) -> None:
    if channel not in channels:
        raise ValueError("unknown source channel")


def _query_for(origin: str, expected_path: str, ref: DetailRef) -> SplitResult:
    address = portal_address(origin, ref.source_url)
    if address is None or address.path != expected_path:
        raise ValueError("invalid source detail locator")
    return address


class CotendersNews(_JsonNotice):
    source_id = "cotenders-news"
    origin = "https://www.cotenders.cn"
    channels = ("4", "1")
    windowed = False
    page_size = 10
    list_path = "/PublicServer/publicAnnouncement/getList.do"
    detail_path = "/PublicServer/publicAnnouncement/selectById.do"
    public_path = "/PublicServer2.0/public/comDetail/comDetail.html"
    title_fields = ("title",)
    content_fields = ("content",)

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        _require_channel(channel, self.channels)
        return FetchRequest("POST", self.origin + self.list_path, form=(
            ("page", str(_page_index(page))), ("rows", str(self.page_size)), ("colType", channel), ("title", ""),
        ), headers=ACCEPT_JSON)

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        _require_channel(channel, self.channels)
        _, body = _api_response(self.origin, response, self.list_path)

        def build(row: dict) -> DetailRef | None:
            key, title = _first_id(row, "id"), _first_text(row, "title")
            published = publication_date(_first_text(row, "publishTime"))
            if key is None or not title or published is None:
                return None
            locator = _locator(self.origin, self.public_path, id=key, colType=channel)
            return DetailRef(f"{channel}:{key}", locator, published, title)

        data = _object(body, "data")
        rows = _rows(data, "list")
        page = data.get("pageNum")
        if type(page) is not int or data.get("pageSize") != self.page_size or data.get("size") != len(rows):
            raise ValueError("invalid Cotenders pagination metadata")
        offset = OffsetPage(data.get("total"), self.page_size, len(rows))
        continuation = offset.has_more(page)
        if data.get("hasNextPage") is not continuation:
            raise ValueError("Cotenders continuation disagrees with the total")
        refs = _list_refs(rows, build)
        complete = len(refs) == len(rows)
        return ListPage(refs, continuation, None if complete else "unrecognized_list_rows",
                        ordered_by_publication=complete, reported_page=page, offset=offset)

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        _require_channel(channel, self.channels)
        address = _query_for(self.origin, self.public_path, ref)
        key = notice_query_id(address, ("id",))
        if key is None or parse_qs(address.query).get("colType") != [channel] or ref.source_key != f"{channel}:{key}":
            raise ValueError("invalid source detail identity")
        url = _locator(self.origin, self.detail_path, id=key, colType=channel)
        request = FetchRequest("GET", url, headers=ACCEPT_JSON)
        return (DetailFetch(f"{ref.source_key}/body", request),)

    def check_detail(self, response: FetchResponse) -> None:
        address, body = _api_response(self.origin, response, self.detail_path)
        channel = notice_query_id(address, ("colType",))
        _require_channel(channel, self.channels)
        detail = self.document_payload(body)
        _detail_id(address, detail, "id", "id", required=True)
        self.render_document(detail)

    @staticmethod
    def document_payload(envelope: dict) -> dict:
        return _object(_object(envelope, "data"), "pojo")


class CrccepBiddingMaterials(_JsonNotice):
    source_id = "crccep-bidding-mat"
    origin = "https://www.crccep.cn"
    channels = ("MAT",)
    windowed = True
    list_path = "/crcc-purportal-manage/portal/specialtopic/findNoticesBynotices"
    detail_path = "/crcc-purportal-manage/portal/specialtopic/findNoticesDetails"
    public_path = "/findNotices"
    column = "BiddingAnnouncement"
    notice_types = ("1", "2")
    sourcing_mode = "ZBCG"
    attachment_origin = "https://epur.crccep.cn"
    attachment_path = "/expertHelp/expertLink"
    attachment_id = re.compile(r"(?:[A-Za-z0-9_-]+/)+[A-Za-z0-9_-]+\.[A-Za-z0-9]+")
    page_size = 15
    title_fields = ("noticeTitle",)
    content_fields = ("noticeContent",)
    headers = ACCEPT_JSON + (("Origin", origin), ("Referer", f"{origin}/findNoticesList?index={column}&type=MAT"))

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        _require_channel(channel, self.channels)
        return FetchRequest.post_json(self.origin + self.list_path, {
            "publishTimeBegin": f"{window.since.isoformat()} 00:00:00", "publishTimeEnd": f"{window.until.isoformat()} 23:59:59",
            "likeKey": "", "purCompanyName": "",
            "orgLevel": None, "noticeContent": "", "subtypeIdList": [channel], "pageNum": _page_index(page),
            "pageSize": self.page_size, "noticeType": ",".join(self.notice_types), "sourcingModeID": self.sourcing_mode,
        }, self.headers)

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        _require_channel(channel, self.channels)
        _, body = _api_response(self.origin, response, self.list_path)

        def build(row: dict) -> DetailRef | None:
            key, title = _first_id(row, "id"), _first_text(row, "noticeTitle")
            if (key is None or not title or not self._in_scope(row)
                    or row.get("subtypeId") != channel or row.get("sourcingModeID") != self.sourcing_mode):
                return None
            locator = _locator(self.origin, self.public_path, columnId=self.column, noticeId=key)
            return DetailRef(key, locator, publication_date(_first_text(row, "publishTime")), title)

        rows = _rows(body, "list")
        listing = _first_page(rows, build)
        fields = ("pageNum", "pageSize", "currentPageSize", "pages", "total")
        if not any(key in body for key in fields):
            return listing
        numbers = tuple(body.get(key) for key in fields)
        if any(type(value) is not int or value < 0 for value in numbers):
            raise ValueError("invalid material notice pagination metadata")
        number, size, current_size, pages, total = numbers
        if (size != self.page_size or not 1 <= number <= max(1, pages)
                or pages != (total + size - 1) // size
                or current_size != len(rows) or current_size != min(size, total - (number - 1) * size)):
            raise ValueError("material notice pagination counts disagree")
        gap = "unrecognized_list_rows" if len(listing.refs) != current_size else None
        return ListPage(listing.refs, number < pages, gap, reported_page=number)

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        _require_channel(channel, self.channels)
        address = _query_for(self.origin, self.public_path, ref)
        key = notice_query_id(address, ("noticeId",))
        if key is None or key != ref.source_key or parse_qs(address.query).get("columnId") != [self.column]:
            raise ValueError("invalid source detail identity")
        locator = _locator(self.origin, self.detail_path, id=key, columnId=self.column)
        return (DetailFetch(f"{key}/body", FetchRequest("GET", locator, headers=self.headers)),)

    def check_detail(self, response: FetchResponse) -> None:
        self.render_document(self._checked_detail(response))

    def _checked_detail(self, response: FetchResponse) -> dict:
        address, body = _api_response(self.origin, response, self.detail_path)
        _detail_id(address, body, "id", "id", required=True)
        if parse_qs(address.query).get("columnId") != [self.column] or not self._in_scope(body):
            raise ValueError("detail outside the material bidding notice channel")
        return body

    def attachment_fetches(self, channel: str, ref: DetailRef, body: FetchResponse) -> tuple[DetailFetch, ...]:
        _require_channel(channel, self.channels)
        payload = self._checked_detail(body)
        if _first_id(payload, "id") != ref.source_key:
            raise ValueError("attachment manifest belongs to another notice")
        root = checked_html(_first_text(payload, *self.content_fields))
        parts = {}
        for anchor in root.xpath("./body//a[@href]"):
            href = anchor.get("href")
            if not href.startswith(f"{self.attachment_origin}/") or "#" in href:
                continue
            address = portal_address(self.attachment_origin, href)
            if address is None or address.path != self.attachment_path:
                continue
            try:
                query = parse_qs(address.query, keep_blank_values=True, strict_parsing=True,
                                 errors="strict", max_num_fields=2)
            except ValueError:
                continue
            ids, names = query.get("fileId", ()), query.get("fileName", ())
            if len(ids) != 1 or len(names) != 1:
                continue
            file_id, name = ids[0], names[0]
            if len(file_id) > 512 or not self.attachment_id.fullmatch(file_id):
                continue
            label = name if 0 < len(name) <= 512 and not re.search(r"[/\\\x00-\x1f\x7f]", name) else None
            request = None if label is None else FetchRequest(
                "GET", _locator(self.attachment_origin, self.attachment_path, fileId=file_id, fileName=label),
            )
            part = DetailFetch(file_id, request, kind="attachment", name=label)
            if file_id in parts and parts[file_id] != part:
                raise ValueError("conflicting declaration for attachment key")
            parts[file_id] = part
        return tuple(parts.values())

    def _in_scope(self, data: dict) -> bool:
        return str(data.get("noticeType")) in self.notice_types


class RudongNoticeBoard(_JsonNotice):
    source_id = "jsjyztb-web-list"
    contract_version = 1
    origin = "http://rudong.net"
    channels = ("2",)
    windowed = True
    page_size = 10
    maximum_public_page = 6
    list_path = "/admin-api/web/notice/page-all"
    detail_path = "/admin-api/web/notice/detail-all"
    public_path = "/web/filedetail"
    title_fields = ("proname",)
    content_fields = ("ggcontent",)
    headers = ACCEPT_JSON + (("Referer", f"{origin}/web/list?dbtype=2"),)

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        _require_channel(channel, self.channels)
        if not 1 <= page <= self.maximum_public_page:
            raise ValueError("page outside Rudong public pagination range")
        locator = _locator(self.origin, self.list_path, pageNo=str(page), pageSize=str(self.page_size),
                           proname="", zbtype="", starttime=f"{window.since} 00:00:00",
                           endtime=f"{window.until} 23:59:59")
        return FetchRequest("GET", locator, headers=self.headers)

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        _require_channel(channel, self.channels)
        address, body = _api_response(self.origin, response, self.list_path)
        query = parse_qs(address.query, keep_blank_values=True)
        pages = query.get("pageNo", ())
        if len(pages) != 1 or not re.fullmatch(r"[1-9]", pages[0]) or int(pages[0]) > self.maximum_public_page:
            raise ValueError("invalid Rudong list page")
        page = int(pages[0])
        if query.get("pageSize") != [str(self.page_size)] or body.get("code") != 0:
            raise ValueError("Rudong list did not return a successful page")
        data = _object(body, "data")
        rows = _rows(data, "list")

        def build(row: dict) -> DetailRef | None:
            key, title = _first_id(row, "id"), _first_text(row, "proname")
            tenant = _first_id(row, "tenantId")
            package = _first_id(row, "probdid")
            published = publication_date(_first_text(row, "fbtime"))
            if key is None or tenant is None or package is None or not title or published is None:
                return None
            locator = _locator(self.origin, self.public_path, dbtype=channel, id=key,
                               probdid=package, tenantId=tenant)
            return DetailRef(f"{channel}:{tenant}:{key}", locator, published, title)

        refs = _list_refs(rows, build)
        if len({ref.source_key for ref in refs}) != len(rows):
            raise ValueError("Rudong list has missing or duplicated notice identity or date")
        offset = OffsetPage(data.get("total"), self.page_size, len(rows), maximum_public_page=self.maximum_public_page)
        blocked_tail = page == self.maximum_public_page and offset.total_records > page * self.page_size
        gap = "pagination_requires_captcha" if blocked_tail else None
        return ListPage(refs, offset.has_more(page), coverage_gap=gap, reported_page=page, offset=offset)

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        _require_channel(channel, self.channels)
        address = _query_for(self.origin, self.public_path, ref)
        key = notice_query_id(address, ("id",))
        tenant = notice_query_id(address, ("tenantId",))
        package = notice_query_id(address, ("probdid",))
        stable = f"{channel}:{tenant}:{key}"
        if (key is None or tenant is None or package is None
                or parse_qs(address.query).get("dbtype") != [channel] or stable != ref.source_key):
            raise ValueError("invalid source detail identity")
        locator = _locator(self.origin, self.detail_path, dbtype=channel, id=key, probdid=package, tenantId=tenant)
        return (DetailFetch(f"{stable}/body", FetchRequest("GET", locator, headers=self.headers)),)

    def check_detail(self, response: FetchResponse) -> date:
        address, body = _api_response(self.origin, response, self.detail_path)
        if body.get("code") != 0 or parse_qs(address.query).get("dbtype") != list(self.channels):
            raise ValueError("Rudong detail did not return a successful notice")
        detail = self.document_payload(body)
        _detail_id(address, detail, "id", "id", required=True)
        tenant = _first_id(detail, "tenantId")
        if tenant is None or tenant != notice_query_id(address, ("tenantId",)):
            raise ValueError("Rudong detail tenant does not match request locator")
        _detail_id(address, detail, "probdid", "probdid", required=True)
        published = publication_date(_first_text(detail, "fbtime"))
        if published is None:
            raise ValueError("Rudong detail lacks publication date")
        self.render_document(detail)
        return published

    @staticmethod
    def document_payload(envelope: dict) -> dict:
        return _object(_object(envelope, "data"), "baseRespDTO")


class LuzhouNotices:
    source_id = "luzhou-ggzy-tzgg"
    origin = "https://www.lzsggzy.com"
    channels = ("010001",)
    windowed = False
    page_size = 10
    list_path = "/inteligentsearchlz/rest/esinteligentsearch/getFullTextDataNew"
    detail_path = re.compile(r"/tzgg/(?P<channel>[0-9]+)/(?:[A-Za-z0-9_-]+/)*(?P<key>[A-Za-z0-9_-]+)\.html")

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        _require_channel(channel, self.channels)
        return FetchRequest.post_json(self.origin + self.list_path, {
            "token": "", "pn": (_page_index(page) - 1) * self.page_size, "rn": self.page_size, "sdt": "", "edt": "",
            "wd": " ", "inc_wd": "", "exc_wd": "", "fields": "title", "cnum": "006",
            "sort": '{"webdate":0}', "ssort": "title", "cl": 200, "terminal": "",
            "condition": [{"fieldName": "categorynum", "equal": channel, "notEqual": None,
                           "equalList": None, "notEqualList": None, "isLike": True, "likeType": 2}],
            "time": None, "highlights": "", "statistics": None, "unionCondition": None,
            "accuracy": "", "noParticiple": "0", "searchRange": None, "isBusiness": "1",
        }, ACCEPT_JSON + (("Referer", f"{self.origin}/tzgg/{channel}/list.html"),))

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        _require_channel(channel, self.channels)
        _, body = _api_response(self.origin, response, self.list_path)
        content = body.get("content")
        if isinstance(content, str):
            content = json.loads(content)
        if not isinstance(content, dict):
            raise ValueError("missing search content")
        _check_api_status(content)

        def build(row: dict) -> DetailRef | None:
            detail = self._detail_address(_first_text(row, "linkurl"))
            key = _first_id(row, "infoid")
            title = _first_text(row, "titlenew", "title")
            # The public list displays webdate. infodate can be months or years later.
            timestamp = re.fullmatch(r"\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}", _first_text(row, "webdate"))
            published = publication_date(timestamp[0]) if timestamp else None
            if detail is None or row.get("categorynum") != channel or not title or published is None:
                return None
            address, locator_key = detail
            if key != locator_key:
                return None
            return DetailRef(locator_key, address.geturl(), published, title)

        result = _object(content, "result")
        rows = _rows(result, "records")
        refs = _list_refs(rows, build)
        complete = len(refs) == len(rows)
        # The service does not echo pn. The runtime checks requested-page geometry and cross-page overlap.
        return ListPage(refs, False, None if complete else "unrecognized_list_rows", ordered_by_publication=complete,
                        offset=OffsetPage(result.get("totalcount"), self.page_size, len(rows)))

    def _detail_address(self, locator: str) -> tuple[SplitResult, str] | None:
        address = portal_address(self.origin, locator)
        if address is None:
            return None
        match = self.detail_path.fullmatch(address.path)
        if match is None or match["channel"] not in self.channels:
            return None
        return address, match["key"]

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        _require_channel(channel, self.channels)
        detail = self._detail_address(ref.source_url)
        if detail is None or detail[1] != ref.source_key:
            raise ValueError("invalid source detail identity")
        address, key = detail
        return (DetailFetch(f"{key}/body", FetchRequest("GET", address.geturl())),)

    def check_detail(self, response: FetchResponse) -> None:
        require_official(self.origin, response)
        if self._detail_address(response.url) is None:
            raise ValueError("unexpected source detail path")
        trade_body(response_html(response), "//h1|//title")


def luzhou_html(raw: bytes) -> str:
    return trade_body(checked_html(raw.decode("utf-8")), "//h1|//title")


class M2BidList(_JsonNotice):
    source_id = "m2-bid-list"
    origin = "https://ugw.m2.com.cn"
    portal = "https://gzc.m2.com.cn"
    api = "/opportunity/api/tenderInfo/open"
    channels = ("notice",)
    windowed = True
    page_size = 100
    title_fields = ("noticeName", "projectName")
    content_fields = ("context", "content")
    headers = ACCEPT_JSON + (("Referer", f"{portal}/bid-list?platformKey=M2"),)
    attachment_host = "guangzc-pd-file.obs.cn-north-1.myhuaweicloud.com"
    attachment_path = re.compile(r"/upload1/[0-9a-f]{32}/20[0-9]{2}(?:0[1-9]|1[0-2])/[0-9a-f]{32}\.(docx|xls|pdf)", re.I)

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        _require_channel(channel, self.channels)
        return FetchRequest.post_json(f"{self.origin}{self.api}/{channel}/list/v1.0", {
            "currentPage": _page_index(page), "size": self.page_size, "provinceName": "", "keyword": "",
            "startDate": window.since.isoformat(), "endDate": window.until.isoformat(),
        }, self.headers)

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        _require_channel(channel, self.channels)
        _, body = _api_response(self.origin, response, f"{self.api}/{channel}/list/v1.0")

        def build(row: dict) -> DetailRef | None:
            key, title = _first_id(row, "tenderInfoId"), _first_text(row, "noticeName", "projectName")
            published = publication_date(_first_text(row, "publishedAt"))
            if key is None or not title or published is None:
                return None
            locator = _locator(self.portal, f"/detail/{channel}", id=key, platformKey="M2")
            return DetailRef(key, locator, published, title)

        data = _object(body, "data")
        total = data.get("total")
        rows = [] if total == 0 and "infoList" in data and data["infoList"] is None else _rows(data, "infoList")
        refs = _list_refs(rows, build)
        gap = None if len(refs) == len(rows) else "unrecognized_list_rows"
        return ListPage(refs, False, gap, offset=OffsetPage(total, self.page_size, len(rows)))

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        _require_channel(channel, self.channels)
        address = _query_for(self.portal, f"/detail/{channel}", ref)
        key = notice_query_id(address, ("id",))
        if key is None or key != ref.source_key or parse_qs(address.query).get("platformKey") != ["M2"]:
            raise ValueError("invalid source detail identity")
        url = f"{self.origin}{self.api}/detail/v1.0/{key}/{channel}"
        return (DetailFetch(f"{key}/body", FetchRequest("GET", url, headers=self.headers)),)

    def check_detail(self, response: FetchResponse) -> None:
        detail = self._checked_detail(response)
        if _first_text(detail, *self.content_fields):
            self.render_document(detail)
        elif not _first_text(detail, *self.title_fields) or not any(
            part.request for part in self._file_parts(detail)
        ):
            raise ValueError("announcement has no body or accepted attachment declaration")

    def _checked_detail(self, response: FetchResponse) -> dict:
        address, body = _api_response(self.origin, response)
        prefix = f"{self.api}/detail/v1.0/"
        path = address.path.removeprefix(prefix).split("/")
        if not address.path.startswith(prefix) or len(path) != 2 or path[1] != "notice" or native_id(path[0]) is None:
            raise ValueError("unexpected source detail path")
        detail = self.document_payload(body)
        identifier = _first_id(detail, "tenderInfoId")
        if identifier != path[0]:
            raise ValueError("detail identity does not match request path")
        return detail

    def attachment_fetches(self, channel: str, ref: DetailRef, response: FetchResponse) -> tuple[DetailFetch, ...]:
        _require_channel(channel, self.channels)
        detail = self._checked_detail(response)
        if _first_id(detail, "tenderInfoId") != ref.source_key:
            raise ValueError("attachment manifest belongs to another notice")
        return self._file_parts(detail)

    def _file_parts(self, detail: dict) -> tuple[DetailFetch, ...]:
        files = detail.get("files")
        if files is None:
            return ()
        if not isinstance(files, list):
            raise ValueError("invalid M2 attachment manifest")
        parts = {}
        for item in files:
            key = _first_id(item, "id") if isinstance(item, dict) else None
            if key is None or not re.fullmatch(r"[0-9]{1,32}", key) or key in parts:
                raise ValueError("invalid or repeated M2 file ID")
            name = _first_text(item, "name")
            label = name if 0 < len(name) <= 512 and not re.search(r"[/\\\x00-\x1f\x7f]", name) else None
            request = self._file_request(_first_text(item, "url"), label) if label is not None else None
            parts[key] = DetailFetch(key, request, kind="attachment", name=label)
        return tuple(parts.values())

    def _file_request(self, url: str, name: str) -> FetchRequest | None:
        if not url or any(char.isspace() or ord(char) < 32 or char == "\\" for char in url):
            return None
        try:
            address = urlsplit(url)
            query = parse_qs(address.query, keep_blank_values=True, strict_parsing=True, errors="strict", max_num_fields=3)
        except ValueError:
            return None
        if (address.scheme != "https" or address.netloc not in (self.attachment_host, f"{self.attachment_host}:443")
                or address.fragment or set(query) != {"AccessKeyId", "Expires", "Signature"}
                or any(len(values) != 1 or not values[0] for values in query.values())
                or re.fullmatch(r"[0-9]{1,12}", query["Expires"][0]) is None):
            return None
        match = self.attachment_path.fullmatch(address.path)
        if match is None or not name.lower().endswith(f".{match[1].lower()}"):
            return None
        return FetchRequest("GET", url)

    @staticmethod
    def document_payload(envelope: dict) -> dict:
        return _object(envelope, "data")


class YmzecTradeInfo(_JsonNotice):
    """Parse protected response shape offline. Live transport requires a separate rights integration."""

    source_id = "ymzec-trade-info"
    origin = "https://www.ymzec.com"
    channels = ("15:bidding",)
    windowed = False
    list_path = "/bid-api/api/trans/trade/pageEs"
    detail_path = "/bid-api/api/trans/trade/getByTradeId"
    title_fields = ("tradeName", "title", "projectName")
    content_fields = ("noticeContent", "content", "tradeContent")

    def list_request(self, channel: str, window: Window, page: int) -> FetchRequest:
        raise SourceBlocked("authorization_required")

    def parse_list(self, channel: str, response: FetchResponse) -> ListPage:
        _require_channel(channel, self.channels)
        _, body = _api_response(self.origin, response, self.list_path)
        rows = _rows(body if isinstance(body.get("records"), list) else self.document_payload(body), "records")

        def build(row: dict) -> DetailRef | None:
            key, title = _first_id(row, "tradeId", "id"), _first_text(row, "tradeName", "title", "projectName")
            if key is None or not title:
                return None
            locator = _locator(self.origin, "/bid/web-outportal/index.html#/trade-info-detail", tradeId=key)
            return DetailRef(key, locator, publication_date(_first_text(row, "publishTime")), title)

        return _first_page(rows, build)

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        raise SourceBlocked("authorization_required")

    def check_detail(self, response: FetchResponse) -> None:
        address, body = _api_response(self.origin, response, self.detail_path)
        detail = self.document_payload(body)
        _detail_id(address, detail, "tradeId", "tradeId", "id")
        self.render_document(detail)

    @staticmethod
    def document_payload(envelope: dict) -> dict:
        return _object(envelope, "data") if "data" in envelope else envelope


cotenders_html = CotendersNews().to_html
crccep_html = CrccepBiddingMaterials().to_html
rudong_html = RudongNoticeBoard().to_html
m2_html = M2BidList().to_html
ymzec_html = YmzecTradeInfo().to_html
