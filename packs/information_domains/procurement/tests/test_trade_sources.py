"""Synthetic protocol responses for the eleven assigned local and specialist portals."""
from __future__ import annotations

import hashlib
import json
from dataclasses import replace
from datetime import date
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

import pytest
import yaml

from miaoqia.acquisition.api.handlers import FetchResponse, SourceBlocked, Window
from miaoqia.acquisition.application import sources
from miaoqia.evidence.extensions.packs import load_documents
from miaoqia.sdk.entrypoints import load_pack_module
from miaoqia.sdk.testing import html_to_document

PACK = Path(__file__).resolve().parents[1]
api = load_pack_module(PACK, "miaoqia_pack_procurement.sources.trade_api")
web = load_pack_module(PACK, "miaoqia_pack_procurement.sources.trade_html")
DAY = date(2026, 11, 19)
WINDOW = Window(DAY, DAY)
TITLE = "【合成夹具】工程公告"
CONTENT = "<table><tr><td>合成正文</td></tr></table>"
CQ_NOTICE_ID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
CQ_CATEGORY = "014001001001"
CQ_PATH = f"/trade/014/{CQ_NOTICE_ID}?categoryNum={CQ_CATEGORY}"
JSON_CASES = [
    (
        api.CotendersNews, "cotenders_html", "4",
        {"data": {"pageNum": 1, "pageSize": 10, "size": 1, "total": 1, "hasNextPage": False,
                  "list": [{"id": "co-17", "title": TITLE, "publishTime": "2026-11-19 11:00:00"}]}},
        {"data": {"pojo": {"id": "co-17", "title": TITLE, "content": CONTENT}}},
        "4:co-17", "/PublicServer2.0/public/comDetail/comDetail.html",
        "/PublicServer/publicAnnouncement/selectById.do",
    ),
    (
        api.CrccepBiddingMaterials, "crccep_html", "MAT",
        {"list": [{"id": "cr-17", "noticeTitle": TITLE, "publishTime": "2026-11-19", "noticeType": "1",
                   "subtypeId": "MAT", "sourcingModeID": "ZBCG"}]},
        {"id": "cr-17", "noticeTitle": TITLE, "noticeContent": CONTENT, "noticeType": "1"},
        "cr-17", "/findNotices", "/crcc-purportal-manage/portal/specialtopic/findNoticesDetails",
    ),
    (
        api.RudongNoticeBoard, "rudong_html", "2",
        {"code": 0, "data": {"total": 1, "list": [{"id": "rd-17", "proname": TITLE, "fbtime": "2026-11-19",
                                                    "probdid": "package-a", "tenantId": 42}]}},
        {"code": 0, "data": {"baseRespDTO": {"id": "rd-17", "tenantId": 42, "probdid": "package-a",
                                              "proname": TITLE, "fbtime": "2026-11-19", "ggcontent": CONTENT}}},
        "2:42:rd-17", "/web/filedetail", "/admin-api/web/notice/detail-all",
    ),
    (
        api.M2BidList, "m2_html", "notice",
        {"data": {"total": 1, "infoList": [{"tenderInfoId": "m2-17", "noticeName": TITLE,
                                "publishedAt": "2026-11-19T08:00:00+08:00"}]}},
        {"data": {"tenderInfoId": "m2-17", "noticeName": TITLE, "context": CONTENT}},
        "m2-17", "/detail/notice", "/opportunity/api/tenderInfo/open/detail/v1.0/m2-17/notice",
    ),
]


def _json(payload, url):
    return FetchResponse(200, "application/json", json.dumps(payload, ensure_ascii=False).encode(), url)


def _html(markup, url):
    return FetchResponse(200, "text/html", markup.encode(), url)


def _listing(handler, request, channel, payload):
    response = _json(payload, request.url)
    listing = handler.parse_list(channel, response).for_request(1, window=WINDOW if handler.windowed else None)
    assert not listing.has_next
    assert listing.coverage_gap == (None if listing.offset else "pagination_unverified")
    ref, = listing.refs
    assert ref.published == DAY
    return ref


def test_crccep_checks_filtered_pages_last_page_and_confirmed_empty():
    handler = api.CrccepBiddingMaterials()
    channel, = handler.channels
    request = handler.list_request(channel, WINDOW, 2)
    query = json.loads(request.json_body)
    assert handler.windowed and query["pageNum"] == 2 and query["pageSize"] == 15
    assert query["publishTimeBegin"] == f"{DAY} 00:00:00"
    assert query["publishTimeEnd"] == f"{DAY} 23:59:59"
    row = {"id": "9007199254740993", "noticeTitle": TITLE, "publishTime": f"{DAY} 16:10:00", "noticeType": "1",
           "subtypeId": "MAT", "sourcingModeID": "ZBCG"}
    payload = {"pageNum": 2, "pageSize": 15, "currentPageSize": 1, "pages": 2, "total": 16, "list": [row]}
    final = handler.parse_list(channel, _json(payload, request.url))
    assert final.reported_page == 2 and not final.has_next and final.coverage_gap is None
    assert final.refs[0].source_key == "9007199254740993"
    final.verify_request(2, window=WINDOW)
    with pytest.raises(ValueError, match="does not match request"):
        final.verify_request(1, window=WINDOW)
    undated = handler.parse_list(channel, _json({**payload, "list": [{**row, "publishTime": ""}]}, request.url))
    with pytest.raises(ValueError, match="outside the request window"):
        undated.verify_request(2, window=WINDOW)

    empty = handler.parse_list(channel, _json({**payload, "pageNum": 1, "pages": 0, "total": 0,
                                               "currentPageSize": 0, "list": []}, request.url))
    assert empty.reported_page == 1 and empty.refs == () and not empty.has_next and empty.coverage_gap is None
    with pytest.raises(ValueError, match="counts disagree"):
        handler.parse_list(channel, _json({**payload, "total": 17}, request.url))


def _chongqing_detail(*, key=CQ_NOTICE_ID, category=CQ_CATEGORY, path=CQ_PATH,
                      rendered=CONTENT, content=CONTENT):
    payload = [
        ["ShallowReactive", 1], {"data": 2, "path": 4, "serverRendered": 5},
        ["ShallowReactive", 3], {"$fixture": 6}, path, True, {"currentRecord": 7},
        {"infoId": 8, "categoryNum": 9, "title": 10, "infoDate": 11, "infoContent": 12},
        key, category, TITLE, f"{DAY.isoformat()} 23:59:59", content,
    ]
    return f"""
        <div class="detail-wrapper">
            <div class="font-bold text-2xl">{TITLE}</div>
            <div class="app-detail">{rendered}<a href="/services/resource.pdf">页面服务资源</a></div>
        </div>
        <footer><a href="https://outside.example/shared.pdf">其他站点公共资源</a></footer>
        <script id="__NUXT_DATA__" type="application/json">{json.dumps(payload)}</script>
    """


def test_chongqing_pins_current_notice_and_keeps_only_publisher_body():
    handler = web.ChongqingTrade014()
    request = handler.list_request("published", WINDOW, 1)
    assert request.method == "GET" and request.url == handler.list_url
    with pytest.raises(ValueError):
        handler.list_request("published", WINDOW, 2)

    markup = f"""
        <ul class="min-h-80">
            <li><a href="{CQ_PATH}">{TITLE}</a><div>{DAY.isoformat()}</div></li>
            <li><a href="https://foreign.example{CQ_PATH}">异站</a></li>
        </ul>
        <a href="{CQ_PATH.replace(CQ_NOTICE_ID, 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb')}">其他栏目</a>
    """
    first = handler.parse_list("published", _html(markup, request.url))
    (ref,) = first.refs
    assert not first.has_next and first.coverage_gap == "homepage_current_window"
    assert ref.source_key == CQ_NOTICE_ID and ref.published == DAY
    (detail,) = handler.detail_fetches("published", ref)
    assert detail.artifact_key == f"{CQ_NOTICE_ID}/body"
    assert detail.request.url == ref.source_url == "https://www.cqggzy.com" + CQ_PATH
    with pytest.raises(ValueError):
        handler.detail_fetches("published", replace(ref, source_url=f"https://foreign.example{CQ_PATH}"))
    with pytest.raises(ValueError, match="publication date"):
        handler.parse_list("published", _html(markup.replace(DAY.isoformat(), ""), request.url))

    response = _html(_chongqing_detail(), ref.source_url)
    assert handler.check_detail(response) == DAY
    html = web.chongqing_trade_html(response.body)
    assert TITLE in html and "<table>" in html and "合成正文" in html
    assert "页面服务资源" not in html and "其他站点公共资源" not in html

    for markup in (_chongqing_detail(key="other"), _chongqing_detail(category="014002"),
                   _chongqing_detail(path=CQ_PATH.replace(CQ_CATEGORY, "014002"))):
        with pytest.raises(ValueError, match="identity does not match"):
            handler.check_detail(_html(markup, ref.source_url))
    with pytest.raises(ValueError, match="does not match current notice"):
        handler.check_detail(_html(_chongqing_detail(rendered="<p>加载中</p>"), ref.source_url))
    with pytest.raises(ValueError, match="missing or ambiguous"):
        handler.check_detail(_html(f"<h1>{TITLE}</h1><main>{CONTENT}</main>", ref.source_url))
    with pytest.raises(SourceBlocked, match="captcha_challenge"):
        handler.check_detail(_html("<title>人机验证</title>", ref.source_url))


def test_chongqing_procurement_keeps_decimal_id_category_and_body_scope():
    handler = web.ChongqingTrade014()
    channel = "procurement-notices"
    identifier = "1699866936856588288"
    category = "014005001"
    path = f"/trade/014005/{identifier}?categoryNum={category}"
    request = handler.list_request(channel, WINDOW, 1)
    assert request.url == "https://www.cqggzy.com/trade/014005?categoryNum=014005001"
    listing = f'<ul class="min-h-80"><li><a href="{path}">{TITLE}</a><time>{DAY}</time></li></ul>'
    ref, = handler.parse_list(channel, _html(listing, request.url)).refs
    assert ref.source_key == identifier
    detail, = handler.detail_fetches(channel, ref)
    response = _html(_chongqing_detail(key=identifier, category=category, path=path), detail.request.url)
    assert handler.check_detail(response) == DAY
    assert "页面服务资源" not in handler.to_html(response.body)
    with pytest.raises(ValueError):
        handler.detail_fetches("published", ref)
    mixed = listing.replace("</ul>", f'<li><a href="{CQ_PATH}">{TITLE}</a><time>{DAY}</time></li></ul>')
    assert [item.source_key for item in handler.parse_list("published", _html(mixed, handler.list_url)).refs] == [CQ_NOTICE_ID]
    with pytest.raises(ValueError, match="outside requested channel"):
        handler.parse_list(channel, _html(mixed, request.url))
    with pytest.raises(ValueError):
        handler.parse_list(channel, _html(listing.replace(category, "014005004"), request.url))
    with pytest.raises(ValueError):
        handler.check_detail(replace(response, url=response.url.replace(identifier, "1699866936856588289")))


def _beifang_row(key, *, published="2026-11-19 13:00:00", query="&chnlcode=tender&objtype=2"):
    return f"""
        <li data-href="/portal/detail?docid={key}{query}">
            <div><a href="javascript:void(0)" title="{TITLE}">{TITLE}</a></div>
            <div><a href="javascript:void(0)" title="PROJECT-1">PROJECT-1</a></div>
            <div><h3 title="合成采购人">合成采购人</h3></div>
            <div><h3 title="{published}"></h3></div>
            <div><h3><script>document.write('2026-11-29 12:00:00');</script></h3></div>
        </li>
    """


def _beifang_list(rows, *, page=1, pages=1, total=1):
    return f"""
        <ul class="table_list_detail">{rows}</ul>
        <div class="page_operate">
            <p class="page_select_current">500</p>
            <span class="page_item page_input"><input value="{page}"><i>/{pages}</i></span>
            <span class="page_btn_icon page_next" dt-type="next"></span>
        </div>
        <i class="page_hint_ptotal">{total}</i>
    """


def test_xuzhou_native_list_and_identity_matched_announcement_body():
    handler = web.XuzhouTrade()
    notice_id = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
    path = f"/jyxx/003001/003001014/20261119/{notice_id}.html"
    row = {"infoid": notice_id, "id": f"{notice_id}_002", "linkurl": f"{path}?tracking=fixture#{notice_id}",
           "title": f"<em>{TITLE}</em>", "webdate": "2026-11-19 00:00:00",
           "infodate": "2026-11-20 00:00:00", "categorynum": "003001014"}
    request = handler.list_request("published", WINDOW, 1)
    ref = _listing(handler, request, "published", {"result": {"totalcount": 1, "records": [row]}})
    assert ref.source_key == notice_id and ref.published == DAY and ref.title == TITLE
    assert ref.source_url == "https://ggzy.zwb.xz.gov.cn" + path
    (detail,) = handler.detail_fetches("published", ref)
    assert detail.artifact_key == notice_id + "/body" and detail.kind == "body"
    markup = f"""<a id="viewGuid" value="cms_003001014_{notice_id}">分类</a>
      <div class="article-info"><h1>{TITLE}</h1><div class="con">{CONTENT}</div></div>
      <footer><a href="/download/public.pdf">公共资源文件</a></footer>"""
    response = _html(markup, detail.request.url)
    handler.check_detail(response)
    assert not hasattr(handler, "attachment_fetches")
    normalized = web.ewb_construction_html(response.body)
    assert "合成正文" in normalized and "公共资源文件" not in normalized
    with pytest.raises(ValueError, match="identity"):
        handler.check_detail(_html(markup.replace(notice_id, "foreign"), detail.request.url))
    with pytest.raises(ValueError, match="heading or body"):
        handler.check_detail(_html(markup.replace(CONTENT, "<style>p {color:red}</style>"), detail.request.url))


def test_xuzhou_windowed_offsets_validate_terminal_empty_and_invalid_rows():
    handler = web.XuzhouTrade()
    since = date(2026, 10, 21)
    window = Window(since, DAY)
    first_request = handler.list_request("published", window, 1)
    second_request = handler.list_request("published", window, 2)
    assert handler.windowed and first_request.method == second_request.method == "POST"
    assert urlsplit(first_request.url).path == "/inteligentsearchnew/rest/esinteligentsearch/getFullTextDataNew"
    first_query = json.loads(first_request.json_body)
    second_query = json.loads(second_request.json_body)
    assert first_query["pn"] == 0 and first_query["rn"] == 12 and second_query == first_query | {"pn": 12}
    assert first_query["condition"] == [{"fieldName": "categorynum", "equal": "003001",
                                         "isLike": True, "likeType": 2}]
    assert first_query["time"] == [{"fieldName": "webdate", "startTime": "2026-10-21 00:00:00",
                                   "endTime": "2026-11-19 23:59:59"}]

    def row(index, published):
        key = f"00000000-aaaa-bbbb-cccc-{index:012}"
        return {"infoid": key, "categorynum": "003001001", "title": TITLE,
                "webdate": f"{published.isoformat()} 23:59:59",
                "linkurl": f"/jyxx/003001/003001001/{published:%Y%m%d}/{key}.html"}

    def parse(total, records):
        response = _json({"result": {"totalcount": total, "records": records}}, first_request.url)
        return handler.parse_list("published", response)

    first = parse(13, [row(index, DAY) for index in range(12)]).for_request(1, window=window)
    last = parse(13, [row(12, since)]).for_request(2, window=window)
    assert first.has_next and not last.has_next
    assert first.coverage_gap is last.coverage_gap is None and last.refs[0].published == since
    assert len({ref.source_key for ref in first.refs + last.refs}) == 13
    empty = parse(0, []).for_request(1, window=window)
    assert not empty.has_next and empty.refs == () and empty.coverage_gap is None
    invalid = parse(1, [row(1, DAY) | {"infoid": "bad"}]).for_request(1, window=window)
    assert invalid.refs == () and invalid.coverage_gap == "unrecognized_list_rows"
    with pytest.raises(ValueError, match="outside the request window"):
        parse(1, [row(1, date(2026, 10, 20))]).for_request(1, window=window)
    with pytest.raises(ValueError, match="row count does not match"):
        parse(14, [row(12, since)]).for_request(2, window=window)
    with pytest.raises(SourceBlocked, match="captcha_challenge"):
        handler.parse_list("published", _html("<title>访问验证</title>", first_request.url))


def test_beifang_native_page_counters_and_disjoint_keys():
    handler = web.BeifangTender()
    first_request = handler.list_request("published", WINDOW, 1)
    second_request = handler.list_request("published", WINDOW, 2)
    assert first_request.method == second_request.method == "GET"
    assert second_request.url == handler.list_url + "&pageIndex=2&pagestr=2&pageSize=500"
    first_markup = _beifang_list("".join(_beifang_row(f"bf-{index}") for index in range(500)), pages=2, total=501)
    second_markup = _beifang_list(_beifang_row("bf-500", published="2026-10-21 16:00:00"), page=2, pages=2, total=501)
    first = handler.parse_list("published", _html(first_markup, first_request.url)).for_request(1, window=None)
    second = handler.parse_list("published", _html(second_markup, second_request.url)).for_request(2, window=None)
    assert first.has_next and not second.has_next
    assert first.coverage_gap is second.coverage_gap is None
    assert first.ordered_by_publication and second.ordered_by_publication
    assert len({ref.source_key for ref in first.refs + second.refs}) == 501
    assert (first.refs[0].published, second.refs[0].published) == (DAY, date(2026, 10, 21))
    assert second.refs[0].title == TITLE
    with pytest.raises(ValueError, match="pager does not match"):
        handler.parse_list("published", _html(first_markup, second_request.url))
    with pytest.raises(ValueError, match="row count does not match"):
        handler.parse_list("published", _html(second_markup.replace("501", "502"), second_request.url))
    with pytest.raises(ValueError, match="outside Beifang list request"):
        handler.parse_list("published", _html(second_markup, second_request.url.replace("pagestr=2", "pagestr=1")))


def test_beifang_missing_publication_and_invalid_docid_create_coverage_gap():
    handler = web.BeifangTender()
    request = handler.list_request("published", WINDOW, 1)
    rows = (_beifang_row("bf-1", published="") + _beifang_row("bf-2", query="&chnlcode=tender&docid=bf-2")
            + _beifang_row("bf-3", query="&chnlcode=result"))
    listing = handler.parse_list("published", _html(_beifang_list(rows, total=3), request.url))
    assert listing.coverage_gap == "unrecognized_list_rows" and not listing.ordered_by_publication
    (ref,) = listing.refs
    assert ref.source_key == "bf-1" and ref.published is None


def test_beifang_detail_pins_docid_and_requires_native_announcement_body():
    handler = web.BeifangTender()
    request = handler.list_request("published", WINDOW, 1)
    listing = handler.parse_list("published", _html(_beifang_list(_beifang_row("bf-17")), request.url))
    (ref,) = listing.refs
    (detail,) = handler.detail_fetches("published", ref)
    assert detail.request.url == ref.source_url and detail.request.method == "GET"
    assert detail.artifact_key == "bf-17/body"
    with pytest.raises(ValueError, match="invalid source detail reference"):
        handler.detail_fetches("published", replace(ref, source_key="bf-elsewhere"))

    heading = f"<h3 class='tender_detail_title'>{TITLE}</h3>"
    body = f"<div id='tender_detail_bottom'><div id='doc-content'>{CONTENT}</div></div>"
    response = _html(heading + body, detail.request.url)
    handler.check_detail(response)
    assert TITLE in web.beifang_tender_html(response.body) and "合成正文" in web.beifang_tender_html(response.body)
    with pytest.raises(ValueError, match="heading or substantive body"):
        handler.check_detail(_html(heading + f"<main>{CONTENT}</main>", detail.request.url))
    with pytest.raises(ValueError, match="heading or substantive body"):
        handler.check_detail(_html(heading + body.replace(CONTENT, "<script>body();</script>"), detail.request.url))
    with pytest.raises(SourceBlocked, match="captcha_challenge"):
        handler.check_detail(_html("<title>人机验证</title>", detail.request.url))


def test_direct_article_text_survives_normalization():
    text = "合成正文只含文本"
    article = f"<main>{text}</main>"
    html_original = _chongqing_detail(rendered=article, content=article).encode()
    json_original = _json({"data": {"pojo": {"title": TITLE, "content": text}}}, "").body
    for normalized in (web.chongqing_trade_html(html_original), api.cotenders_html(json_original)):
        document = html_to_document(normalized, "synthetic")
        assert any(block.get("text") == text for block in document["blocks"])


def test_jiangsu_current_window_uses_dated_native_rows():
    handler = web.JiangsuTradeMirror()
    channel, = handler.channels
    list_url = handler.list_request(channel, WINDOW, 1).url
    notice_id = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
    route = f"/jyxx/001001/001001001/20261119/{notice_id}.html"
    origin = urlsplit(list_url)._replace(path="").geturl()
    markup = f"""
        <table><tr class="ewb-trade-tr"><td><a href="{route}?infoid=ignored">合成公告</a></td>
          <td>2026-11-19</td></tr>
          <tr class="ewb-trade-tr"><td><a href="{origin}:8000{route}">错误端口</a></td></tr>
          <tr class="ewb-trade-tr"><td><a href="https://foreign.example{route}">非原站</a></td></tr></table>
    """
    page = handler.parse_list(channel, _html(markup, list_url))
    (ref,) = page.refs
    assert not page.has_next and page.coverage_gap == "homepage_current_window"
    assert ref.source_key == notice_id and ref.published == DAY and ref.source_url == origin + route
    with pytest.raises(ValueError, match="outside source origin"):
        handler.parse_list(channel, _html(markup, list_url.replace(":8099", "")))


def _suzhou_response(custom, url):
    response = _json({"status": {"code": "200", "state": "error"}, "custom": custom}, url)
    return replace(response, media_type="application/octet-stream")


def _suzhou_rows(count):
    return [
        {"infoid": f"aaaaaaaa-bbbb-cccc-dddd-{index:012d}", "categorynum": "003001009",
         "postdate": DAY.isoformat(), "title": TITLE}
        for index in range(count)
    ]


def test_suzhou_dated_engineering_pagination_respects_public_boundary():
    handler = web.SuzhouTrade()

    def parse(page, total, rows):
        request = handler.list_request("published", WINDOW, page)
        custom = json.dumps({"Table": rows, "TotalCount": total})
        return handler.parse_list("published", _suzhou_response(custom, request.url)).for_request(page, window=WINDOW)

    request = handler.list_request("published", WINDOW, 1)
    query = parse_qs(urlsplit(request.url).query)
    assert query["cmd"] == ["getList1"] and query["categorynum"] == ["003001"]
    assert query["diqu"] == ["苏州市"] and query["pageIndex"] == ["0"] and query["pageSize"] == ["15"]
    assert query["starttime"] == query["endtime"] == [DAY.isoformat()]
    rows = _suzhou_rows(15)
    first = parse(1, 16, rows)
    assert first.has_next and first.coverage_gap is None
    assert first.refs[0].published == DAY and first.refs[0].source_key == rows[0]["infoid"]
    assert first.refs[0].title == TITLE
    for final in (parse(2, 16, _suzhou_rows(16)[-1:]), parse(1, 0, []), parse(10, 150, rows)):
        assert not final.has_next and final.coverage_gap is None
    capped = parse(10, 151, rows)
    assert not capped.has_next and capped.coverage_gap == "pagination_requires_captcha"
    with pytest.raises(ValueError, match="public pagination range"):
        handler.list_request("published", WINDOW, 11)
    with pytest.raises(ValueError, match="row count does not match"):
        parse(2, 16, rows)
    with pytest.raises(ValueError, match="outside the request window"):
        parse(1, 1, [rows[0] | {"postdate": "2026-11-18"}])
    unrecognized = parse(1, 1, [rows[0] | {"categorynum": "003002001"}])
    assert unrecognized.refs == () and unrecognized.coverage_gap == "unrecognized_list_rows"


def test_suzhou_locator_and_body_bind_to_publisher_uuid_and_category():
    handler = web.SuzhouTrade()
    request = handler.list_request("published", WINDOW, 1)
    row, = _suzhou_rows(1)
    listing = _suzhou_response(json.dumps({"Table": [row], "TotalCount": 1}), request.url)
    (ref,) = handler.parse_list("published", listing).refs
    (detail,) = handler.detail_fetches("published", ref)
    key = row["infoid"]
    assert detail.request is None and detail.artifact_key == f"{key}/body"
    assert urlsplit(ref.source_url).path == "/jump.html"
    locator_url = detail.locator.url
    query = parse_qs(urlsplit(locator_url).query)
    assert query["cmd"] == ["getDetailPath"] and query["infoid"] == [key]
    assert query["categorynum"] == ["003001009"] and query["pageIndex"] == ["0"]
    route = f"/jyxx/003001/003001009/20261119/{key}.html"
    body_request = handler.resolve_detail("published", ref, _suzhou_response(route, locator_url))
    assert body_request.url == "https://ggzy.suzhou.gov.cn" + route
    markup = (f"<a id='viewGuid' value='cms_003001009_{key}'></a>"
              f"<div class='article-info'><h1>{TITLE}</h1><div class='con'>{CONTENT}</div></div>")
    handler.check_detail(_html(markup, body_request.url))
    with pytest.raises(ValueError, match="identity does not match"):
        handler.check_detail(_html(markup.replace("003001009", "003001008"), body_request.url))
    with pytest.raises(ValueError, match="request identity"):
        handler.resolve_detail("published", ref, _suzhou_response(route, locator_url.replace(key, key[:-1] + "1")))
    for changed in ("https://foreign.example" + route, route.replace("003001009", "003001008"),
                    route.replace(key, key[:-1] + "1")):
        with pytest.raises(ValueError, match="announcement identity"):
            handler.resolve_detail("published", ref, _suzhou_response(changed, locator_url))


def test_suzhou_handler_revision_changes_collection_contract(monkeypatch):
    source = sources.catalog(PACK.parent.parent)["suzhou-ggzy-trade"].declaration
    current = sources.config_fingerprint(source)
    monkeypatch.setattr(source.handler, "contract_version", source.handler.contract_version + 1)
    assert sources.config_fingerprint(source) != current


@pytest.mark.parametrize("source,adapter,channel,payload,document,key,public_path,detail_path", JSON_CASES)
def test_public_api_protocol(source, adapter, channel, payload, document, key, public_path, detail_path):
    handler = source()
    request = handler.list_request(channel, WINDOW, 1)
    ref = _listing(handler, request, channel, payload)
    assert ref.source_key == key and urlsplit(ref.source_url).path == public_path
    (detail,) = handler.detail_fetches(channel, ref)
    assert detail.artifact_key == f"{key}/body" and urlsplit(detail.request.url).path == detail_path
    assert detail.request.url != ref.source_url
    response = _json(document, detail.request.url)
    handler.check_detail(response)
    original_hash = hashlib.sha256(response.body).hexdigest()
    html = getattr(api, adapter)(response.body)
    assert "<h1>" + TITLE + "</h1>" in html and "<td>合成正文</td>" in html
    updated = replace(response, body=response.body.replace("合成正文".encode(), "修改正文".encode()))
    handler.check_detail(updated)
    assert hashlib.sha256(updated.body).hexdigest() != original_hash
    assert handler.detail_fetches(channel, replace(ref, title="修改标题")) == (detail,)

    with pytest.raises(ValueError):
        handler.check_detail(_json({}, detail.request.url))
    with pytest.raises(ValueError):
        handler.check_detail(replace(response, url="https://foreign.example" + detail_path))
    with pytest.raises(SourceBlocked, match="http_403"):
        handler.check_detail(replace(response, status=403))
    with pytest.raises(SourceBlocked, match="api_401"):
        handler.check_detail(_json({"code": "401"}, detail.request.url))
    with pytest.raises(SourceBlocked, match="login_required"):
        handler.parse_list(channel, _html("<form><input type='password'></form>", request.url))
    with pytest.raises(SourceBlocked, match="captcha_challenge"):
        challenge = replace(response, body="<title>访问验证</title>".encode())
        handler.check_detail(challenge)


def test_cotenders_form_and_notice_namespace():
    handler = api.CotendersNews()
    request = handler.list_request("4", WINDOW, 1)
    assert request.method == "POST"
    assert dict(request.form) == {"page": "1", "rows": "10", "colType": "4", "title": ""}
    assert dict(request.headers)["Accept"] == "application/json"
    payload = JSON_CASES[0][3]
    first = _listing(handler, request, "4", payload)
    other = _listing(handler, handler.list_request("1", WINDOW, 1), "1", payload)
    assert first.source_key == "4:co-17" and other.source_key == "1:co-17"
    (detail,) = handler.detail_fetches("4", first)
    assert detail.request.method == "GET"
    assert parse_qs(urlsplit(detail.request.url).query) == {"id": ["co-17"], "colType": ["4"]}
    with pytest.raises(ValueError, match="identity"):
        handler.check_detail(_json({"data": {"pojo": {"id": "other", "title": TITLE, "content": CONTENT}}},
                                   detail.request.url))
    with pytest.raises(ValueError):
        handler.detail_fetches("1", first)


def test_crccep_material_notice_filters_and_get_identity_check():
    handler = api.CrccepBiddingMaterials()
    request = handler.list_request("MAT", WINDOW, 1)
    payload = json.loads(request.json_body)
    assert request.method == "POST" and payload["pageNum"] == 1 and payload["pageSize"] == 15
    assert payload["subtypeIdList"] == ["MAT"] and payload["sourcingModeID"] == "ZBCG"
    assert payload["noticeType"] == "1,2" and dict(request.headers)["Origin"] == handler.origin
    ref = _listing(handler, request, "MAT", JSON_CASES[1][3])
    (detail,) = handler.detail_fetches("MAT", ref)
    assert detail.request.method == "GET"
    assert parse_qs(urlsplit(detail.request.url).query) == {"id": ["cr-17"], "columnId": ["BiddingAnnouncement"]}
    document = JSON_CASES[1][4] | {"id": "other-id"}
    with pytest.raises(ValueError, match="identity"):
        handler.check_detail(_json(document, detail.request.url))
    with pytest.raises(ValueError, match="identity"):
        handler.check_detail(_json({key: value for key, value in document.items() if key != "id"}, detail.request.url))
    with pytest.raises(ValueError, match="outside the material bidding notice channel"):
        handler.check_detail(_json(JSON_CASES[1][4] | {"noticeType": "3"}, detail.request.url))
    with pytest.raises(ValueError, match="outside the material bidding notice channel"):
        handler.check_detail(_json({key: value for key, value in JSON_CASES[1][4].items() if key != "noticeType"},
                                   detail.request.url))


@pytest.mark.parametrize("wrong_scope", [{"subtypeId": "ENG"}, {"sourcingModeID": "OTHER"}, {"noticeType": None}])
def test_crccep_rejects_list_rows_outside_confirmed_material_scope(wrong_scope):
    handler = api.CrccepBiddingMaterials()
    request = handler.list_request("MAT", WINDOW, 1)
    row, = JSON_CASES[1][3]["list"]
    payload = {"pageNum": 1, "pageSize": 15, "currentPageSize": 1, "pages": 1, "total": 1,
               "list": [row | wrong_scope]}
    listing = handler.parse_list("MAT", _json(payload, request.url))
    assert listing.refs == () and listing.coverage_gap == "unrecognized_list_rows"


def test_crccep_type_2_preserves_body_and_declares_only_native_attachment_identity():
    handler = api.CrccepBiddingMaterials()
    request = handler.list_request("MAT", WINDOW, 1)
    row, = JSON_CASES[1][3]["list"]
    ref = _listing(handler, request, "MAT", {"list": [row | {"noticeType": "2"}]})
    detail, = handler.detail_fetches("MAT", ref)
    path = "https://epur.crccep.cn/expertHelp/expertLink"
    native = f"{path}?fileId=public%2Ffixture-17.pdf&amp;fileName=附件%201.pdf"
    body = CONTENT + f"""
        <a href="#section">本文段落</a><a name="section">正文</a>
        <a href="https://other.example/attachment.pdf" download>外部链接</a>
        <a href="https://www.crccep.cn/findNotices?noticeId=parent">相关公告链接</a>
        <a href="https://epur.crccep.cn/other?fileId=public/other.pdf&amp;fileName=other.pdf">其他路径</a>
        <a href="{path}?fileId=../fixture-17.pdf&amp;fileName=other.pdf">无效键</a>
        <a href="{path}?fileId=public/fixture-17.pdf&amp;fileId=public/other.pdf">重复参数</a>
        <img src="{native}"><a href="{native}">附件</a><a href="{native}">附件副链接</a>
    """
    layout_link = native.replace("fixture-17.pdf", "portal-help.pdf")
    payload = JSON_CASES[1][4] | {
        "noticeType": "2", "noticeContent": body, "tenderExceptionCode": "99",
        "siteFooter": f'<a href="{layout_link}">站点帮助</a>',
    }
    response = _json(payload, detail.request.url)
    original = response.body
    handler.check_detail(response)
    attachment, = handler.attachment_fetches("MAT", ref, response)
    assert (attachment.artifact_key, attachment.name, attachment.kind) == (
        "public/fixture-17.pdf", "附件 1.pdf", "attachment",
    )
    assert attachment.request.method == "GET"
    address = urlsplit(attachment.request.url)
    assert address.netloc == "epur.crccep.cn" and address.path == "/expertHelp/expertLink"
    assert parse_qs(address.query) == {"fileId": ["public/fixture-17.pdf"], "fileName": ["附件 1.pdf"]}
    assert response.body == original and "合成正文" in handler.to_html(original)
    with pytest.raises(ValueError, match="another notice"):
        handler.attachment_fetches("MAT", replace(ref, source_key="other"), response)
    conflicting = body + f'<a href="{native.replace("附件%201.pdf", "other.pdf")}">冲突文件名</a>'
    with pytest.raises(ValueError, match="conflicting declaration"):
        handler.attachment_fetches("MAT", ref, _json(payload | {"noticeContent": conflicting}, detail.request.url))


def test_rudong_lookup_params_and_tenant_partition():
    handler = api.RudongNoticeBoard()
    request = handler.list_request("2", WINDOW, 1)
    assert request.method == "GET"
    assert parse_qs(urlsplit(request.url).query, keep_blank_values=True) == {
        "pageNo": ["1"], "pageSize": ["10"], "proname": [""], "zbtype": [""],
        "starttime": ["2026-11-19 00:00:00"], "endtime": ["2026-11-19 23:59:59"],
    }
    row, = JSON_CASES[2][3]["data"]["list"]
    first = _listing(handler, request, "2", _rudong_list(1, [row]))
    moved = _listing(handler, request, "2", _rudong_list(1, [row | {"probdid": "package-b"}]))
    tenant = _listing(handler, request, "2", _rudong_list(1, [row | {"tenantId": "another-tenant"}]))
    assert moved.source_key == first.source_key and moved.source_url != first.source_url
    assert tenant.source_key != first.source_key
    with pytest.raises(ValueError, match="missing or duplicated"):
        _listing(handler, request, "2", _rudong_list(1, [row | {"tenantId": None}]))
    (detail,) = handler.detail_fetches("2", first)
    assert parse_qs(urlsplit(detail.request.url).query) == {
        "dbtype": ["2"], "id": ["rd-17"], "probdid": ["package-a"], "tenantId": ["42"],
    }
    wrong_tenant = {"code": 0, "data": {"baseRespDTO": JSON_CASES[2][4]["data"]["baseRespDTO"] | {"tenantId": 43}}}
    with pytest.raises(ValueError, match="tenant"):
        handler.check_detail(_json(wrong_tenant, detail.request.url))


def _rudong_list(total, rows):
    return {"code": 0, "data": {"list": rows, "total": total}}


def test_rudong_checks_date_window_and_stops_at_public_page_boundary():
    handler = api.RudongNoticeBoard()
    row, = JSON_CASES[2][3]["data"]["list"]
    rows = [row | {"id": f"rd-{number}"} for number in range(10)]

    def page(number, total, records):
        request = handler.list_request("2", WINDOW, number)
        response = _json(_rudong_list(total, records), request.url)
        return handler.parse_list("2", response).for_request(number, window=WINDOW)

    assert page(1, 11, rows).has_next
    assert not page(2, 11, [row]).has_next
    assert page(1, 0, []).refs == ()
    boundary = page(6, 62, rows)
    assert not boundary.has_next and boundary.coverage_gap == "pagination_requires_captcha"
    with pytest.raises(ValueError, match="public pagination range"):
        handler.list_request("2", WINDOW, 7)
    with pytest.raises(ValueError, match="outside the request window"):
        page(1, 1, [row | {"fbtime": "2026-11-18"}])
    with pytest.raises(ValueError, match="missing or duplicated"):
        page(1, 2, [row, row])


@pytest.fixture
def luzhou_row():
    return {"infoid": "lz-17", "id": "010001lz-17_006", "categorynum": "010001", "titlenew": TITLE,
            "webdate": "2026-11-19 17:13:00", "infodate": "2026-11-20 09:12:59",
            "linkurl": "/tzgg/010001/20261120/lz-17.html"}


@pytest.mark.parametrize("inline_json", [False, True])
def test_luzhou_search_envelope_and_html_details(inline_json, luzhou_row):
    handler = api.LuzhouNotices()
    channel, = handler.channels
    request = handler.list_request(channel, WINDOW, 1)
    payload = json.loads(request.json_body)
    assert request.method == "POST" and payload["pn"] == 0 and payload["rn"] == 10
    assert payload["condition"][0]["equal"] == "010001"
    content = {"result": {"records": [luzhou_row], "totalcount": 1}}
    payload = {"content": json.dumps(content) if inline_json else content}
    listing = handler.parse_list(channel, _json(payload, request.url)).for_request(1, window=None)
    assert not listing.has_next and listing.coverage_gap is None
    ref, = listing.refs
    assert ref.source_key == "lz-17"
    (detail,) = handler.detail_fetches(channel, ref)
    assert detail.artifact_key == "lz-17/body"
    assert detail.request.method == "GET" and detail.request.url == ref.source_url
    with pytest.raises(ValueError, match="detail identity"):
        handler.detail_fetches(channel, replace(ref, source_key="different-notice"))
    response = _html(f"<h1>{TITLE}</h1>{CONTENT}", detail.request.url)
    handler.check_detail(response)
    assert "<td>合成正文</td>" in api.luzhou_html(response.body)


@pytest.mark.parametrize("invalid", [
    {"linkurl": "https://foreign.example/tzgg/010001/lz-17.html"},
    {"linkurl": "/tzgg/010002/lz-17.html"},
    {"linkurl": "/tzgg/010001/other.html"},
    {"categorynum": "010002"},
    {"infoid": None},
    {"webdate": ""},
    {"webdate": "2026-11-31 17:13:00"},
    {"webdate": "updated on 2026-11-19 17:13:00"},
])
def test_luzhou_rejects_unverified_list_identity_or_publication_date(invalid, luzhou_row):
    handler = api.LuzhouNotices()
    channel, = handler.channels
    request = handler.list_request(channel, WINDOW, 1)
    malformed = {"content": {"result": {"records": [luzhou_row | invalid], "totalcount": 1}}}
    gap = handler.parse_list(channel, _json(malformed, request.url))
    assert gap.refs == () and not gap.has_next and gap.coverage_gap == "unrecognized_list_rows"


@pytest.mark.parametrize(("page", "total", "rows", "has_next"), [
    (1, 0, 0, False), (1, 11, 10, True), (2, 11, 1, False), (2, 20, 10, False),
    (1, 11, 1, None), (2, 11, 10, None), (2, 10, 0, None),
])
def test_luzhou_resolves_offset_using_requested_page(page, total, rows, has_next, luzhou_row):
    handler = api.LuzhouNotices()
    channel, = handler.channels
    request = handler.list_request(channel, WINDOW, page)
    assert json.loads(request.json_body)["pn"] == (page - 1) * 10
    records = [luzhou_row | {"infoid": f"row-{i}", "linkurl": f"/tzgg/010001/row-{i}.html"}
               for i in range(rows)]
    listing = handler.parse_list(channel, _json({"content": {"result": {"totalcount": total, "records": records}}},
                                               request.url))
    assert listing.reported_page is None
    if has_next is None:
        with pytest.raises(ValueError, match="row count does not match requested page"):
            listing.for_request(page, window=None)
    else:
        verified = listing.for_request(page, window=None)
        assert verified.has_next is has_next
        assert len(verified.refs) == rows and verified.coverage_gap is None


def test_m2_api_origin_is_distinct_from_notice_locator():
    handler = api.M2BidList()
    request = handler.list_request("notice", WINDOW, 1)
    payload = json.loads(request.json_body)
    assert request.method == "POST" and payload["currentPage"] == 1 and payload["size"] == 100
    assert payload["startDate"] == payload["endDate"] == DAY.isoformat()
    ref = _listing(handler, request, "notice", JSON_CASES[3][3])
    (detail,) = handler.detail_fetches("notice", ref)
    assert urlsplit(detail.request.url).netloc == "ugw.m2.com.cn"
    assert urlsplit(ref.source_url).netloc == "gzc.m2.com.cn"
    assert parse_qs(urlsplit(ref.source_url).query)["platformKey"] == ["M2"]
    for invalid_id in ("wrong", None):
        with pytest.raises(ValueError, match="identity"):
            handler.check_detail(_json({"data": JSON_CASES[3][4]["data"] | {"tenderInfoId": invalid_id}},
                                       detail.request.url))


def test_m2_verifies_offset_counts_and_filters_the_publication_window():
    handler = api.M2BidList()
    request = handler.list_request("notice", WINDOW, 2)
    assert json.loads(request.json_body)["currentPage"] == 2
    row, = JSON_CASES[3][3]["data"]["infoList"]

    def response(rows, total):
        return _json({"data": {"infoList": rows, "total": total}}, request.url)

    final = handler.parse_list("notice", response([row], 101))
    assert not final.for_request(2, window=WINDOW).has_next
    with pytest.raises(ValueError, match="row count"):
        final.for_request(1, window=WINDOW)
    out_of_window = handler.parse_list("notice", response([row | {"publishedAt": "2026-11-20"}], 101))
    with pytest.raises(ValueError, match="outside the request window"):
        out_of_window.for_request(2, window=WINDOW)
    empty = handler.parse_list("notice", response(None, 0))
    assert not empty.for_request(1, window=WINDOW).has_next
    with pytest.raises(ValueError, match="missing API list"):
        handler.parse_list("notice", response(None, 101))


def test_m2_does_not_follow_manifest_files_outside_the_approved_origin_and_path():
    handler = api.M2BidList()
    ref = _listing(handler, handler.list_request("notice", WINDOW, 1), "notice", JSON_CASES[3][3])
    part, = handler.detail_fetches("notice", ref)
    file_path = f"https://{handler.attachment_host}/upload1/{'1' * 32}/202611/{'2' * 32}.pdf"
    query = "?AccessKeyId=fixture&Expires=2000000000&Signature=fixture"
    candidates = [
        file_path.replace(handler.attachment_host, "elsewhere.example") + query,
        file_path.replace("/upload1/", "/global/") + query,
        file_path + query + "&Signature=duplicate",
    ]
    files = [{"id": str(index), "name": "附件.pdf", "url": url} for index, url in enumerate(candidates, 1)]
    payload = JSON_CASES[3][4]["data"] | {"files": files}
    original = _json({"data": payload}, part.request.url)
    parts = handler.attachment_fetches("notice", ref, original)
    assert [file.request for file in parts] == [None, None, None]
    with pytest.raises(ValueError, match="another notice"):
        handler.attachment_fetches("notice", replace(ref, source_key="m2-18"), original)
    with pytest.raises(ValueError, match="no body or accepted attachment"):
        handler.check_detail(_json({"data": payload | {"context": ""}}, part.request.url))


def test_ymzec_synthetic_response_and_inert_network_contract():
    handler = api.YmzecTradeInfo()
    channel, = handler.channels
    with pytest.raises(SourceBlocked, match="authorization_required"):
        handler.list_request(channel, WINDOW, 1)
    url = handler.origin + handler.list_path
    row = {"tradeId": "ymz-17", "tradeName": TITLE, "publishTime": "2026-11-19"}
    listing = handler.parse_list(channel, _json({"data": {"records": [row]}}, url))
    assert not listing.has_next and listing.coverage_gap == "pagination_unverified"
    (ref,) = listing.refs
    assert ref.source_key == "ymz-17" and urlsplit(ref.source_url).fragment == "/trade-info-detail?tradeId=ymz-17"
    with pytest.raises(SourceBlocked, match="authorization_required"):
        handler.detail_fetches(channel, ref)
    raw = _json({"data": row | {"tradeContent": CONTENT}}, handler.origin + handler.detail_path + "?tradeId=ymz-17")
    handler.check_detail(raw)
    assert "<td>合成正文</td>" in api.ymzec_html(raw.body)


def test_staged_registration_uses_native_entry_points(tmp_path):
    pack = tmp_path / "information_domains" / "procurement"
    pack.mkdir(parents=True)
    (pack / "src").symlink_to(PACK / "src", target_is_directory=True)
    (pack / "pack.yaml").write_text("pack: {provides: {sources: sources.yaml, documents: documents.yaml}}")
    entries = [(case[0], api, case[1], "application/json") for case in JSON_CASES]
    entries += [
        (web.ChongqingTrade014, web, "chongqing_trade_html", "text/html"),
        (web.BeifangTender, web, "beifang_tender_html", "text/html"),
        (web.JiangsuTradeMirror, web, "ewb_construction_html", "text/html"),
        (web.XuzhouTrade, web, "ewb_construction_html", "text/html"),
        (web.SuzhouTrade, web, "ewb_construction_html", "text/html"),
        (api.LuzhouNotices, api, "luzhou_html", "text/html"),
        (api.YmzecTradeInfo, api, "ymzec_html", "application/json"),
    ]
    declarations, documents, inventory = [], [], []
    for cls, module, adapter, media_type in entries:
        declarations.append({
            "source_id": cls.source_id, "admission": "staged", "handler": f"{module.__name__}:{cls.__name__}",
            "access": {"min_interval_seconds": 10, "throttle_backoff_seconds": 300, "max_attempts": 1},
        })
        documents.append({
            "source_id": cls.source_id, "media_type": media_type, "adapter": f"{module.__name__}:{adapter}",
        })
        inventory.append({"source_id": cls.source_id, "readiness": "candidate",
                          "reference_access_mode": "authenticated" if cls is api.YmzecTradeInfo else "public"})
    (pack / "source-inventory.yaml").write_text(yaml.safe_dump({"sources": inventory}))
    (pack / "sources.yaml").write_text(yaml.safe_dump({"sources": declarations}))
    (pack / "documents.yaml").write_text(yaml.safe_dump({"documents": documents}))
    loaded = sources.catalog(tmp_path)
    assert len(loaded) == 11
    for cls, _, _, _ in entries:
        record = loaded[cls.source_id]
        assert isinstance(record.declaration.handler, cls)
        expected = "authorization_required" if cls is api.YmzecTradeInfo else "staged"
        assert record.admission == expected and record.declaration.schedule is None
    assert sources.load(tmp_path) == {}
    assert {key: item.media_type for key, item in load_documents(tmp_path).items()} == {
        item["source_id"]: item["media_type"] for item in documents
    }
