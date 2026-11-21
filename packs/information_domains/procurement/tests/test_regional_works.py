"""Synthetic HTTP responses exercising the regional works protocols offline."""

import json
from dataclasses import replace
from datetime import date
from urllib.parse import parse_qs, urlsplit

import pytest

from miaoqia.acquisition.api.handlers import FetchResponse, SourceBlocked, Window, checked_detail_ref
from miaoqia_pack_procurement.sources.regional_works import (
    HefeiWorks,
    HenanHomepageWorks,
    HubeiWorks,
    HunanWorks,
    JiangxiAnnouncements,
    ShaanxiGovernmentProcurement,
    ShaanxiWorks,
    ShanxiWorks,
    SichuanWorks,
    TibetWorks,
    XinjiangWorks,
    hefei_works_html,
    henan_works_html,
    hubei_works_html,
    jiangxi_works_html,
    shaanxi_procurement_html,
    shaanxi_works_html,
    shanxi_works_html,
    sichuan_works_html,
    xinjiang_works_html,
)

WINDOW = Window(date(2026, 11, 1), date(2026, 11, 19))
PUBLISHED = date(2026, 11, 18)
UUID = "11111111-2222-4333-8444-555555555555"
HEFEI_SECTION = "/jyxx/002001/002001001"
HANDLERS = (
    HefeiWorks, HenanHomepageWorks, HubeiWorks, HunanWorks, JiangxiAnnouncements,
    ShaanxiWorks, ShaanxiGovernmentProcurement, ShanxiWorks, SichuanWorks, TibetWorks, XinjiangWorks,
)


def _response(url, body, media_type="text/html", status=200):
    return FetchResponse(status, media_type, body.encode("utf-8"), url)


def _listing(handler, body, media_type="text/html"):
    channel = handler.channels[0]
    request = handler.list_request(channel, WINDOW, 1)
    return handler.parse_list(channel, _response(request.url, body, media_type))


@pytest.mark.parametrize("handler_type", HANDLERS)
def test_listing_rejects_invalid_pages_and_off_origin_responses(handler_type):
    handler = handler_type()
    channel = handler.channels[0]
    request = handler.list_request(channel, WINDOW, 1)
    invalid_page = 2 if handler_type is TibetWorks else 0
    with pytest.raises(ValueError):
        handler.list_request(channel, WINDOW, invalid_page)
    with pytest.raises(ValueError):
        handler.list_request("unknown", WINDOW, 1)
    for bad_origin in ("https://outside.invalid", handler.origin + ":9999",
                       handler.origin.replace("://", "://user@")):
        with pytest.raises(ValueError):
            handler.parse_list(channel, _response(request.url.replace(handler.origin, bad_origin), ""))


@pytest.mark.parametrize(("handler_type", "media_type"), [
    (HefeiWorks, "text/html"),
    (HubeiWorks, "text/plain"),
    (ShaanxiWorks, "text/html"),
    (ShaanxiGovernmentProcurement, "text/html"),
    (HunanWorks, "text/html"),
    (TibetWorks, "text/html"),
])
def test_access_challenge_and_login_responses_are_blocked(handler_type, media_type):
    handler = handler_type()
    channel = handler.channels[0]
    url = handler.list_request(channel, WINDOW, 1).url
    captcha = _response(url, "<html><title>访问验证</title></html>", media_type)
    login = _response(url, "<html><h1>登录</h1><form><input type='password'></form></html>", media_type)
    for response, cause in ((captcha, "captcha_challenge"), (login, "login_required"),
                            (_response(url, "", status=403), "http_403")):
        with pytest.raises(SourceBlocked, match=cause):
            handler.parse_list(channel, response)


@pytest.mark.parametrize(("handler_type", "path", "key", "layout"), [
    (HefeiWorks, f"/jyxx/002001/002001001/20261117/{UUID}.html", UUID, "pagination_unverified"),
])
def test_html_routes_keep_keys_separate_and_reject_invalid_links(handler_type, path, key, layout):
    handler = handler_type()
    markup = f"""<html><ul>
        <li><a href="{path}">合成工程公告</a><time>2026-11-18</time></li>
        <li hidden><a href="{path}">隐藏</a></li>
        <li><a href="{handler.origin}{path}">重复</a></li>
        <li><a href="{path}?return=other">未确认查询变体</a></li>
        <li><a href="https://outside.invalid{path}">站外</a></li>
        <li><a href="{handler.origin}:9443{path}">异端口</a></li>
    </ul></html>"""
    listing = _listing(handler, markup)
    assert not listing.has_next and listing.coverage_gap == layout
    (ref,) = listing.refs
    assert (ref.source_key, ref.source_url, ref.published, ref.title) == (
        key, handler.origin + path, PUBLISHED, "合成工程公告",
    )
    (part,) = handler.detail_fetches(handler.channels[0], ref)
    assert part.artifact_key == key
    assert part.request.method == "GET" and part.request.url == ref.source_url

    identity_marker = f'<input id="infoguid" value="{key}">' if handler_type is HefeiWorks else ""
    original = _response(ref.source_url, f"""
        <html><head><title>合成工程公告</title></head><body><nav>交易菜单</nav>
        {identity_marker}
        <h1>合成工程公告</h1><div class="ewb-project-notice"><p>招标人：示例单位</p>
        <table><tr><td>项目编号：X-12</td></tr></table></div></body></html>
    """)
    handler.check_detail(original)
    with pytest.raises(ValueError):
        handler.check_detail(_response(ref.source_url, "<h1>合成工程公告</h1><nav>招标人</nav>"))
    with pytest.raises(ValueError):
        handler.check_detail(_response(handler.origin + "/", original.body.decode("utf-8")))

    if handler_type is HefeiWorks:
        adapted = hefei_works_html(original.body)
        assert "示例单位" in adapted and "<table>" in adapted and "交易菜单" not in adapted


def test_hefei_unverified_slug_is_archivable_as_pending_identity():
    handler = HefeiWorks()
    listing = _listing(handler, '<li><a href="20261117/unstable-slug.html">合成公告</a></li>')
    assert listing.coverage_gap == "pagination_unverified" and not listing.has_next
    (ref,) = listing.refs
    assert ref.source_key is None and ref.published is None
    (part,) = handler.detail_fetches(handler.channels[0], ref)
    assert part.artifact_key == "body" and part.request.url == ref.source_url


def _hefei_page(key, current, total, next_link=""):
    return f"""<ul class="ewb-right-item"><li class="ewb-right-list clearfix">
      <a href="{HEFEI_SECTION}/20261118/{key}.html">合成工程公告</a><span>2026-11-18</span>
      </li></ul><div class="ewb-page"><ul>
      <li><span id="index">{current}/{total}</span></li><li>{next_link}</li></ul></div>"""


def test_hefei_static_pagination_reports_page_and_distinct_native_keys():
    handler = HefeiWorks()
    channel = handler.channels[0]
    next_key = "aaaaaaaa-2222-4333-8444-555555555555"
    first = _listing(handler, _hefei_page(UUID, 1, 2, f'<a href="{HEFEI_SECTION}/2.html">下页&gt;</a>'))
    second_request = handler.list_request(channel, WINDOW, 2)
    assert (second_request.method, second_request.url) == ("GET", f"{handler.origin}{HEFEI_SECTION}/2.html")
    second = handler.parse_list(channel, _response(second_request.url, _hefei_page(next_key, 2, 2, "下页&gt;")))
    first.verify_request(1, window=None)
    second.verify_request(2, window=None)
    with pytest.raises(ValueError, match="does not match request"):
        first.verify_request(2, window=None)
    assert first.has_next and not second.has_next
    assert first.ordered_by_publication and second.ordered_by_publication
    assert first.coverage_gap is second.coverage_gap is None
    assert [(ref.source_key, ref.published) for ref in first.refs + second.refs] == [
        (UUID, PUBLISHED), (next_key, PUBLISHED),
    ]


@pytest.mark.parametrize(("page", "counter", "next_link"), [
    (2, (1, 3), f'<a href="{HEFEI_SECTION}/2.html">下页&gt;</a>'),
    (1, (2, 3), f'<a href="{HEFEI_SECTION}/3.html">下页&gt;</a>'),
    (1, (1, 3), f'<a href="https://outside.invalid{HEFEI_SECTION}/2.html">下页&gt;</a>'),
    (1, (1, 3), f'<a href="{HEFEI_SECTION}/3.html">下页&gt;</a>'),
    (1, (1, 3), f'<a hidden href="{HEFEI_SECTION}/2.html">下页&gt;</a>'),
    (1, (1, 3), ""),
    (1, (1, 1), f'<a href="{HEFEI_SECTION}/2.html">下页&gt;</a>'),
])
def test_hefei_rejects_inconsistent_pager(page, counter, next_link):
    handler = HefeiWorks()
    channel = handler.channels[0]
    request = handler.list_request(channel, WINDOW, page)
    with pytest.raises(ValueError, match="Hefei"):
        handler.parse_list(channel, _response(request.url, _hefei_page(UUID, *counter, next_link)))


@pytest.mark.parametrize("identity", ["", '<input id="infoguid" value="wrong">',
                                     f'<input id="infoguid" value="{UUID}"><input id="infoguid">'])
def test_hefei_requires_unique_matching_publisher_identifier(identity):
    handler = HefeiWorks()
    url = f"{handler.origin}{HEFEI_SECTION}/20261118/{UUID}.html"
    body = f'<h1>合成公告</h1>{identity}<div class="ewb-project-notice"><p>招标人：示例单位</p></div>'
    with pytest.raises(ValueError, match="identifier does not match"):
        handler.check_detail(_response(url, body))


def test_hefei_emits_only_the_visible_announcement_from_a_multi_tab_original():
    handler = HefeiWorks()
    url = f"{handler.origin}{HEFEI_SECTION}/20261118/{UUID}.html"
    response = _response(url, f"""
        <title>合成公告</title><input id="infoguid" value="{UUID}">
        <div class="ewb-info-bd"><div class="ewb-project-notice">招标人：示例招标人</div></div>
        <div class="ewb-info-bd hidden"><div class="ewb-project-notice">中标结果：隐藏中标人</div></div>
        <div aria-hidden="true"><div class="ewb-project-notice">更正：隐藏栏目的变更</div></div>
    """)
    handler.check_detail(response)
    adapted = hefei_works_html(response.body)
    assert "示例招标人" in adapted and "隐藏" not in adapted


@pytest.mark.parametrize(("handler_type", "adapter", "body"), [
    (HenanHomepageWorks, henan_works_html,
     '<div class="main"><div class="title"><div class="title-text">招标</div></div>'
     '<div class="text detail-list">公告正文</div></div>'),
    (JiangxiAnnouncements, jiangxi_works_html,
     '<div class="content"><p class="title">招标</p><div class="text">公告正文</div></div>'),
])
def test_portal_construction_details_are_bound_to_the_native_key_and_body(handler_type, adapter, body):
    handler = handler_type()
    locator = f"/jyxx/002001/002001001/20261118/{UUID}.html"
    row = {"title": "合成公告", "infoid": UUID, "categorynum": "002001001"}
    if handler_type is HenanHomepageWorks:
        payload = {"custom": {"count": 1, "infodata": [row | {"infourl": locator, "infodate": "2026-11-18"}]}}
        with pytest.raises(ValueError, match="public pagination"):
            handler.list_request(handler.channels[0], WINDOW, 7)
    else:
        payload = {"result": {"totalcount": 1, "records": [row | {"linkurl": locator, "webdate": "2026-11-18"}]}}
    media_type = "text/html" if handler_type is HenanHomepageWorks else "application/json"
    listing = _listing(handler, json.dumps(payload), media_type).for_request(1, window=WINDOW)
    (ref,) = listing.refs
    assert (ref.source_key, ref.source_url, ref.published) == (UUID, handler.origin + locator, PUBLISHED)
    assert not listing.has_next
    source = (f'<a id="viewGuid" value="cms_002001001_{UUID}"></a><div class="container">{body}</div>'
              '<footer><a href="/files/help.pdf" download>站点资源</a></footer>')
    handler.check_detail(_response(ref.source_url, source))
    normalized = adapter(source.encode())
    assert "公告正文" in normalized and "help.pdf" not in normalized
    with pytest.raises(ValueError, match="identity"):
        handler.check_detail(_response(ref.source_url, source.replace(UUID, "unrelated")))


def _shanxi_row(section, key):
    return f"""<div class="cs_two_content">
        <a href="http://prec.sxzwfw.gov.cn:80/jyxx{section}/{key}.jhtml">
          <p class="cs_bz_cont">合成工程公告</p>
          <p><span>交易场所：示例市</span><span class="cs_bz_cont_1_time">2026/11/18</span></p>
        </a>
      </div>"""


def _shanxi_page(rows, *, current, total):
    return f"""{''.join(rows)}<a href="/jyxxgczb/91.jhtml">页脚链接</a>
    <script>laypage.render({{
      elem: 'page'
      ,count: {total}
      ,limit: 10
      ,curr: {current}
      ,jump: function (page) {{}}
    }});</script>"""


@pytest.mark.parametrize("section", ["gczb", "gchxr", "gcgs", "gcgz"])
def test_shanxi_public_list_and_scoped_detail(section):
    handler = ShanxiWorks()
    request = handler.list_request(handler.channels[0], WINDOW, 1)
    assert request.method == "GET" and urlsplit(request.url).path == "/jyxxgc/index.jhtml"
    listing = _listing(handler, _shanxi_page([_shanxi_row(section, 90)], current=1, total=1))
    (ref,) = listing.refs
    assert (ref.source_key, ref.title, ref.published) == ("90", "合成工程公告", PUBLISHED)
    assert ref.source_url == f"{handler.origin}/jyxx{section}/90.jhtml"
    assert listing.coverage_gap is None and not listing.has_next
    (part,) = handler.detail_fetches(handler.channels[0], ref)
    raw = ('<script>Cms.viewCount("", "90");</script><section>'
           '<p class="cs_title_P1">合成工程项目招标公告</p><div class="cs_xq_content">'
           '<div class="div-title2">公告发布时间：2026-11-18 08:21:00</div>'
           '<div class="div-article2"><p>项目编号：SN-001</p></div></div></section>'
           '<footer><a href="/file/site.pdf" download>网站资源</a></footer>')
    handler.check_detail(_response(part.request.url, raw))
    adapted = shanxi_works_html(raw.encode())
    assert "SN-001" in adapted and "site.pdf" not in adapted and not hasattr(handler, "attachment_fetches")
    assert "<h1>合成工程项目招标公告</h1>" in adapted and "公告发布时间：2026-11-18" in adapted
    with pytest.raises(ValueError, match="identity"):
        handler.check_detail(_response(part.request.url, raw.replace('"90"', '"91"')))


def test_shanxi_tender_plan_matches_the_listing_and_replays_only_its_table():
    handler = ShanxiWorks()
    listing = _listing(handler, _shanxi_page([_shanxi_row("zbjh", 90)], current=1, total=1))
    (ref,) = listing.refs
    assert ref.source_key == "90" and listing.coverage_gap is None
    assert ref.source_url.endswith("/jyxxzbjh/90.jhtml")
    raw = ('<script>Cms.viewCount("", "90");</script><section>'
           '<p class="cs_title_P1">合成工程公告</p><p class="cs_title_P3">发布日期：2026-11-18 08:21</p>'
           '<div class="cs_xq_content"><table class="bid_msgTable"><tr><td>投资项目统一代码：SN-001</td>'
           '</tr></table></div></section><footer><a href="/file/site.pdf">网站资源</a></footer>')
    response = _response(ref.source_url, raw)
    assert checked_detail_ref(handler, ref, response) == ref
    normalized = shanxi_works_html(raw.encode())
    assert 'data-notice-kind="tender-plan"' in normalized and "SN-001" in normalized and "site.pdf" not in normalized
    for bad_ref in (replace(ref, source_key="91"), replace(ref, title="其他项目"),
                    replace(ref, published=date(2026, 11, 19))):
        with pytest.raises(ValueError):
            checked_detail_ref(handler, bad_ref, response)
    with pytest.raises(ValueError, match="tender plan body"):
        handler.check_detail(_response(ref.source_url, raw.replace('class="bid_msgTable"', 'class="unrelated"')))
    with pytest.raises(ValueError, match="construction announcement body"):
        handler.check_detail(replace(response, url=ref.source_url.replace("zbjh", "gczb")))


def test_shanxi_rolling_pages_use_raw_row_count_and_keep_unknown_route_gap():
    handler = ShanxiWorks()
    channel, = handler.channels
    first = handler.list_request(channel, WINDOW, 1)
    second = handler.list_request(channel, WINDOW, 2)
    assert urlsplit(second.url).path == "/jyxxgc/index_2.jhtml"
    first_markup = _shanxi_page([_shanxi_row("gczb", key) for key in range(10, 20)], current=1, total=12)
    last_rows = [_shanxi_row("gczb", 20), _shanxi_row("unknown", 21)]
    last_markup = _shanxi_page(last_rows, current=2, total=12)
    one = handler.parse_list(channel, _response(first.url, first_markup)).for_request(1, window=None)
    two = handler.parse_list(channel, _response(second.url, last_markup)).for_request(2, window=None)
    assert one.has_next and one.offset.row_count == 10
    assert not two.has_next and two.offset.row_count == 2
    assert [ref.source_key for ref in two.refs] == ["20"]
    assert two.coverage_gap == "unrecognized_list_rows" and two.ordered_by_publication
    with pytest.raises(ValueError, match="pager does not match"):
        handler.parse_list(channel, _response(second.url, first_markup))
    with pytest.raises(ValueError, match="row count does not match"):
        handler.parse_list(channel, _response(second.url, _shanxi_page(last_rows[:1], current=2, total=12)))


@pytest.mark.parametrize(("handler_type", "branch", "category", "adapter"), [
    (ShaanxiWorks, "001001001", "001001001001", shaanxi_works_html),
    (ShaanxiGovernmentProcurement, "001001004", "001001004001", shaanxi_procurement_html),
])
def test_shaanxi_requests_and_detail_use_publisher_identifier(handler_type, branch, category, adapter):
    handler = handler_type()
    channel = handler.channels[0]
    request = handler.list_request(channel, WINDOW, 2)
    assert request.method == "POST"
    assert urlsplit(request.url).path == "/inteligentsearch_new/rest/esinteligentsearch/getFullTextDataNew"
    query = json.loads(request.json_body)
    assert not query["token"] and (query["pn"], query["rn"]) == (10, 10)
    assert query["condition"][0]["equal"] == category and not query["condition"][0]["isLike"]
    assert query["time"] == [{"fieldName": "webdate", "startTime": "2026-11-01 00:00:00",
                             "endTime": "2026-11-19 23:59:59"}]

    key = "1234567890abcDEF1234567890abcDEF"
    locator = f"/jydt/001001/{branch}/{category}/20261117/{key}.html"
    row = {"infoid": key, "id": f"{category}{key}_001", "title": "<b>合成公告</b>",
           "webdate": "2026-11-18 08:00:00", "categorynum": category, "linkurl": locator}

    def listing(records, code=200):
        payload = "\ufeff" + json.dumps({"code": code, "content": json.dumps({
            "result": {"totalcount": 11, "records": records},
        })})
        return handler.parse_list(channel, _response(request.url, payload, "text/plain"))

    final_page = listing([row]).for_request(2, window=WINDOW)
    (notice,) = final_page.refs
    assert not final_page.has_next and final_page.coverage_gap is None
    first_rows = [row | {"infoid": f"{i:032x}", "linkurl": locator.replace(key, f"{i:032x}")} for i in range(10)]
    assert listing(first_rows).for_request(1, window=WINDOW).has_next
    with pytest.raises(ValueError, match="row count"):
        listing([row]).for_request(1, window=WINDOW)
    with pytest.raises(ValueError, match="outside the request window"):
        listing([row | {"webdate": "2026-11-20 00:00:00"}]).for_request(2, window=WINDOW)
    assert (notice.source_key, notice.source_url, notice.published) == (key, handler.origin + locator, PUBLISHED)
    other_branch, other_category = ("001001004", "001001004001") if handler_type is ShaanxiWorks else (
        "001001001", "001001001001",
    )
    other_route = locator.replace(f"/{branch}/{category}/", f"/{other_branch}/{other_category}/")
    for invalid_row in (
        row | {"infoid": None}, row | {"infoid": row["id"]}, row | {"categorynum": other_category},
        row | {"linkurl": other_route, "categorynum": other_category},
        row | {"linkurl": "https://other.invalid" + locator}, row | {"webdate": None, "infodate": "2026-11-18"},
    ):
        with pytest.raises(ValueError, match="announcement row"):
            listing([invalid_row])
    with pytest.raises(ValueError, match="search did not return success"):
        listing([row], code=500)

    if handler_type is ShaanxiWorks:
        bindings = f' data-infoid="{key}" data-value="{category}"'
        article = '<div id="mainContent"><p>公告正文</p></div>'
        extra_marker = ""
    else:
        bindings = ""
        article = '<div class="epoint-article-content jynr news_content"><div id="noticeArea"><p>公告正文</p></div></div>'
        extra_marker = f'<input type="hidden" id="souceinfoid" value="{key}">'
    content = (f'<span id="viewGuid" value="cms_{key}"{bindings}></span>'
               '<div id="c"><h3 class="article-title">合成公告</h3>'
               '<div class="info-source">【信息来源：示例市】【信息时间：2026-11-18】</div>'
               f'{article}</div>{extra_marker}'
               '<footer><a href="/site-guide.pdf" download>站点资源</a></footer>')
    assert handler.check_detail(_response(notice.source_url, content)) == PUBLISHED
    assert handler.detail_fetches(channel, notice)[0].request.url == notice.source_url
    normalized = adapter(content.encode())
    assert "公告正文" in normalized and "site-guide.pdf" not in normalized
    assert not hasattr(handler, "attachment_fetches")
    with pytest.raises(ValueError, match="identity"):
        handler.check_detail(_response(notice.source_url, content.replace(f"cms_{key}", "cms_unrelated")))
    with pytest.raises(ValueError, match="identity"):
        handler.check_detail(_response(notice.source_url, content.replace(f'="{key}"', '="unrelated"')))
    with pytest.raises(ValueError, match="identity"):
        handler.check_detail(_response(handler.origin + other_route, content))
    with pytest.raises(ValueError, match="heading or body missing"):
        handler.check_detail(_response(notice.source_url, content.replace("公告正文", "")))
    for label in ("", "【信息时间：2026-11-31】", "【信息时间：2026-11-18】【信息时间：2026-11-19】"):
        with pytest.raises(ValueError, match="publication date"):
            handler.check_detail(_response(notice.source_url, content.replace("【信息时间：2026-11-18】", label)))


@pytest.mark.parametrize("native_key", [UUID, "798C5AF546FD400A8D8BD22F4741AD30"])
def test_sichuan_dated_construction_query_and_scoped_detail(native_key):
    handler = SichuanWorks()
    channel, = handler.channels
    request = handler.list_request(channel, WINDOW, 2)
    query = json.loads(request.json_body)
    assert request.method == "POST" and urlsplit(request.url).path == handler.list_path
    assert (query["pn"], query["rn"], query["condition"][0]["equal"]) == (12, 12, "002001001")
    assert query["time"] == [{"fieldName": "webdate", "startTime": "2026-11-01 00:00:00",
                              "endTime": "2026-11-19 23:59:59"}]
    path = f"/jyxx/002001/002001001/20261118/{native_key}.html"
    row = {"categorynum": "002001001", "linkurl": path, "title": "合成工程公告",
           "webdate": "2026-11-18 08:00:00"}
    response = _response(request.url, json.dumps({"result": {"totalcount": 13, "records": [row]}}), "text/plain")
    listing = handler.parse_list(channel, response).for_request(2, window=WINDOW)
    assert not listing.has_next and listing.coverage_gap is None
    (ref,) = listing.refs
    assert (ref.source_key, ref.source_url, ref.published) == (native_key.lower(), handler.origin + path, PUBLISHED)
    assert handler.detail_fetches(channel, ref)[0].request.url == ref.source_url
    raw = (f'<a id="viewGuid" value="cms_002001001_{native_key}"></a>'
           '<div id="infoContainer"><h2 id="title">合成工程公告</h2>'
           '<div id="newsText"><p>项目编号：SC-01</p></div>'
           '<div id="attach_content"><a href="/files/portal-guide.pdf" download>站点指南</a></div>'
           '</div><footer>导航</footer>')
    handler.check_detail(_response(ref.source_url, raw))
    normalized = sichuan_works_html(raw.encode())
    assert "SC-01" in normalized and "导航" not in normalized and "portal-guide.pdf" not in normalized
    assert not hasattr(handler, "attachment_fetches")
    with pytest.raises(ValueError, match="identity"):
        handler.check_detail(_response(ref.source_url, raw.replace("cms_002001001_", "cms_other_")))


@pytest.mark.parametrize("nested_list", [False, True])
def test_hubei_uses_independent_announcement_body_and_matches_its_identity(nested_list):
    handler = HubeiWorks()
    channel = handler.channels[0]
    request = handler.list_request(channel, WINDOW, 1)
    assert request.method == "POST" and dict(request.form) == {"currentPage": "1", "pageSize": "10", "area": ""}
    rows = [{"tenderBulletinGuid": UUID, "bulletinName": "合成招标公告",
             "bulletinContent": "<p>招标人：示例单位</p>", "bulletinIssueTime": "20261118082100"}]
    payload = {"jsgcZbggList": json.dumps(rows) if nested_list else rows, "currentPage": 2, "pageSize": 10}
    listing = _listing(handler, json.dumps(payload), "text/html")
    (ref,) = listing.refs
    assert ref.source_key == UUID and ref.published == PUBLISHED
    assert parse_qs(urlsplit(ref.source_url).query) == {"guid": [UUID]}
    assert listing.reported_page == 2 and not listing.has_next
    assert listing.coverage_gap == "unverified_archive_boundary"
    with pytest.raises(ValueError, match="page does not match"):
        listing.verify_request(1, window=None)
    (part,) = handler.detail_fetches(channel, ref)
    assert part.request.method == "GET" and urlsplit(part.request.url).path == handler.detail_path
    body = _response(part.request.url, json.dumps({"tender": rows[0], "otherDownloads": ["site.pdf"]}))
    handler.check_detail(body)
    normalized = hubei_works_html(body.body)
    assert "<h1>合成招标公告</h1>" in normalized and "招标人：示例单位" in normalized
    assert "site.pdf" not in normalized and not hasattr(handler, "attachment_fetches")
    with pytest.raises(ValueError, match="identity does not match"):
        handler.check_detail(_response(part.request.url.replace(UUID, "aaaaaaaa-2222-4333-8444-555555555555"),
                                       body.body.decode("utf-8")))
    with pytest.raises(ValueError, match="detail route"):
        handler.check_detail(_response(request.url, json.dumps(payload)))


def test_xinjiang_uses_current_public_category_and_scoped_detail():
    handler = XinjiangWorks()
    channel = handler.channels[0]
    query = json.loads(handler.list_request(channel, WINDOW, 2).json_body)
    assert (query['pn'], query['rn'], query['cnum'], query['condition'][0]['equal']) == (10, 10, '002', '001001001')
    route = f"/jyxx/001001/001001001/20261117/{UUID}.html"
    row = {"categorynum": "001001001", "id": "search-index-key", "linkurl": route,
           "title": "合成工程公告", "webdate": "2026-11-18 00:00:00"}
    listing = _listing(handler, json.dumps({"result": {"totalcount": 11, "records": [row]}}), "text/plain")
    assert not listing.for_request(2, window=None).has_next
    (ref,) = listing.refs
    assert (ref.source_key, ref.published, ref.source_url) == (UUID, PUBLISHED, handler.origin + route)
    (part,) = handler.detail_fetches(channel, ref)
    raw = (f'<span id="viewGuid" value="cms_001001001_{UUID}"></span>'
           '<div class="public-article"><h3 class="public-article-title">合成工程公告</h3>'
           '<div class="public-article-container"><p>项目编号：XJ-01</p></div></div>'
           '<footer><a href="/files/site.pdf" download>网站资源</a></footer>')
    handler.check_detail(_response(part.request.url, raw))
    normalized = xinjiang_works_html(raw.encode())
    assert "XJ-01" in normalized and "site.pdf" not in normalized and not hasattr(handler, "attachment_fetches")
    with pytest.raises(ValueError, match="identity"):
        handler.check_detail(_response(part.request.url, raw.replace('cms_001001001_', 'cms_other_')))


def _hunan_page(handler, current, total, records):
    request = handler.list_request(handler.channels[0], WINDOW, current)
    return _response(request.url, json.dumps({"code": 200, "success": True, "data": {
        "current": current, "size": 10, "total": total, "pages": (total + 9) // 10, "records": records,
    }}), "application/json")


def _hunan_notice(prefix, kind="ZHAOBIAO_NOTICE", when="2026-11-12 17:31:04"):
    return {"id": prefix + UUID, "constructionSectionId": UUID, "constructionTenderId": "same-tender",
            "bulletinType": kind, "noticeSendTime": when, "noticeName": f"合成公告 {kind}",
            "noticeContent": f"<p>正文 {kind}</p>"}


@pytest.fixture
def hunan_section():
    handler = HunanWorks()
    response = _hunan_page(handler, 2, 11, [{"bidSectionId": UUID, "noticeSendTime": "2026-11-18 15:02:40"}])
    (group,) = handler.parse_list(handler.channels[0], response).groups
    return handler, group


def _hunan_expansion(group, notices, **envelope):
    payload = {"code": 200, "success": True, "data": {"noticeList": notices}} | envelope
    return _response(group.request.url, json.dumps(payload, ensure_ascii=False), "application/json")


def test_hunan_section_pagination_uses_publisher_counts(hunan_section):
    handler, group = hunan_section
    channel = handler.channels[0]
    request = handler.list_request(channel, WINDOW, 2)
    assert request.method == "GET" and urlsplit(request.url).path == handler.list_path
    assert parse_qs(urlsplit(request.url).query, keep_blank_values=True) == {
        "current": ["2"], "size": ["10"], "tenderProjectType": ["CONSTRUCTION"],
        "notice": [""], "tenderMode": ["OPEN"], "status": [""], "regionCode": [""],
    }
    first = _hunan_page(handler, 1, 11, [{"bidSectionId": f"section-{i}"} for i in range(10)])
    page = handler.parse_list(channel, first).for_request(1, window=None)
    assert page.has_next and len(page.groups) == 10 and not page.refs and not page.ordered_by_publication
    last = _hunan_page(handler, 2, 11, [{"bidSectionId": UUID, "noticeSendTime": "2026-12-31"}])
    listing = handler.parse_list(channel, last).for_request(2, window=WINDOW)
    assert listing.groups == (group,) and not listing.refs and not listing.has_next
    assert listing.coverage_gap == "section_notice_coverage_unverified"
    with pytest.raises(ValueError, match="page does not match"):
        listing.for_request(1, window=None)
    assert group.group_key == UUID and group.request.method == "GET"
    assert group.request.url == handler.origin + handler.detail_path + f"?sectionId={UUID}"
    recovered = HunanWorks().group_request(channel, group.group_key)
    assert recovered == group.request and recovered.fingerprint() == group.request.fingerprint()
    for invalid_channel, invalid_key in (("unknown", UUID), (channel, "../other"), (channel, 123)):
        with pytest.raises(ValueError):
            handler.group_request(invalid_channel, invalid_key)
    empty = handler.parse_list(channel, _hunan_page(handler, 1, 0, []))
    assert not empty.has_next and not empty.groups and empty.coverage_gap
    for page in (True, 1.0, handler._max_page + 1):
        with pytest.raises(ValueError, match="invalid Hunan construction section list request"):
            handler.list_request(channel, WINDOW, page)


@pytest.mark.parametrize("overrides", [
    {"current": 1}, {"current": True}, {"size": 10.0}, {"total": -1}, {"total": True},
    {"pages": 3}, {"pages": 2.0}, {"records": []}, {"records": "[]"},
    {"records": [None]}, {"records": [{"bidSectionId": 123}]},
])
def test_hunan_rejects_inconsistent_pagination(overrides):
    handler = HunanWorks()
    response = _hunan_page(handler, 2, 11, [{"bidSectionId": UUID}])
    payload = json.loads(response.body)
    payload["data"].update(overrides)
    bad = _response(response.url, json.dumps(payload), response.media_type)
    with pytest.raises(ValueError):
        handler.parse_list(handler.channels[0], bad)


def test_hunan_rejects_duplicate_sections_and_changed_listing_routes(hunan_section):
    handler, _ = hunan_section
    channel = handler.channels[0]
    response = _hunan_page(handler, 1, 2, [{"bidSectionId": UUID}] * 2)
    with pytest.raises(ValueError, match="duplicate Hunan construction section"):
        handler.parse_list(channel, response)
    valid = _hunan_page(handler, 1, 1, [{"bidSectionId": UUID}])
    for url in (valid.url + "&current=1", valid.url.replace("size=10", "size=20"),
                valid.url.replace("tenderMode=OPEN", "tenderMode=OTHER"), valid.url + "#fragment"):
        with pytest.raises(ValueError):
            handler.parse_list(channel, FetchResponse(200, valid.media_type, valid.body, url))


def test_hunan_expansion_and_selection_share_member_binding(hunan_section):
    from dataclasses import replace
    from hashlib import sha256

    from miaoqia_pack_procurement.sources.regional_works import hunan_works_selected_html

    handler, group = hunan_section

    def expand(response):
        return handler.expand_group(handler.channels[0], group, response)

    tender = _hunan_notice("aaaaaaaa-2222-4333-8444-555555555555")
    clarification = _hunan_notice("bbbbbbbb-2222-4333-8444-555555555555", "CHENGQING_NOTICE", "2026-11-18 15:02:40")
    raw = _hunan_expansion(group, [clarification, tender], time=1)
    original = sha256(raw.body).hexdigest()
    later, target = expand(raw)
    assert (later.ref.source_key, later.ref.published) == (clarification["id"], PUBLISHED)
    assert (target.ref.source_key, target.ref.published) == (tender["id"], date(2026, 11, 12))
    assert target.selector.notice_key == tender["id"] and target.selector.group_key == UUID
    assert target.ref.source_url == later.ref.source_url == group.request.url
    assert target.record_digest != later.record_digest

    new_member = _hunan_notice("cccccccc-2222-4333-8444-555555555555", "GENGZHENG_NOTICE")
    changed_neighbour = clarification | {"noticeContent": "<p>邻居改动后</p>" + " " * 124_000}
    moved = _hunan_expansion(group, [tender | {"transportHint": "new"}, new_member, changed_neighbour], time=2)
    assert sha256(moved.body).hexdigest() != original and expand(moved)[0] == target
    assert expand(moved)[2].record_digest != later.record_digest

    for change in ({"id": new_member["id"]}, {"noticeName": "新标题"}, {"bulletinType": "GENGZHENG_NOTICE"},
                   {"noticeSendTime": "2026-11-12 17:31:05"}, {"noticeSendTime": "2026-11-13 17:31:04"},
                   {"noticeContent": "<p>目标改动后</p>"}):
        (revision,) = expand(_hunan_expansion(group, [tender | change]))
        assert revision.record_digest != target.record_digest

    binding = {"published_on": target.ref.published, "record_digest": target.record_digest}
    html = hunan_works_selected_html(raw.body, target.selector, **binding)
    assert html == handler.to_selected_html(moved.body, target.selector, **binding)
    assert "合成公告 ZHAOBIAO_NOTICE" in html and "2026-11-12 17:31:04" in html and "正文 ZHAOBIAO_NOTICE" in html
    assert "CHENGQING_NOTICE" not in html and "GENGZHENG_NOTICE" not in html
    assert sha256(raw.body).hexdigest() == original
    for selector in (replace(target.selector, kind="other-source"), replace(target.selector, version=2),
                     replace(target.selector, group_key="other-section"), replace(target.selector, notice_key="missing"),
                     later.selector):
        with pytest.raises(ValueError):
            handler.to_selected_html(raw.body, selector, **binding)
    for change in ({"published_on": PUBLISHED}, {"record_digest": later.record_digest}):
        with pytest.raises(ValueError, match="binding does not match"):
            handler.to_selected_html(raw.body, target.selector, **(binding | change))


@pytest.mark.parametrize("overrides", [
    {"id": UUID}, {"id": 123}, {"id": "0:1"}, {"constructionSectionId": "unrelated"},
    {"bulletinType": "招标公告"}, {"bulletinType": False}, {"noticeSendTime": "2026-11-12"},
    {"noticeSendTime": "2026-02-30 17:31:04"}, {"noticeSendTime": "2026-11-12 24:00:00"},
    {"noticeSendTime": "2026-11-12 17:31:04+08:00"}, {"noticeName": None}, {"noticeName": " "},
    {"noticeContent": "", "siteFooter": "<p>站点内容</p>"},
    {"noticeContent": "<script>body</script><p hidden>隐藏内容</p>"},
])
def test_hunan_rejects_invalid_member_fields(hunan_section, overrides):
    handler, group = hunan_section
    row = _hunan_notice("aaaaaaaa-2222-4333-8444-555555555555") | overrides
    with pytest.raises(ValueError):
        handler.expand_group(handler.channels[0], group, _hunan_expansion(group, [row]))


def test_hunan_rejects_duplicate_keys_and_invalid_group_envelopes(hunan_section):
    handler, group = hunan_section
    channel = handler.channels[0]
    row = _hunan_notice("aaaaaaaa-2222-4333-8444-555555555555")
    valid = _hunan_expansion(group, [row])
    (member,) = handler.expand_group(channel, group, valid)
    for response in (_hunan_expansion(group, [row, row]),
                     _hunan_expansion(group, [row, row | {"noticeName": "另一标题"}])):
        with pytest.raises(ValueError, match="duplicate Hunan construction notice key"):
            handler.expand_group(channel, group, response)
        with pytest.raises(ValueError, match="duplicate Hunan construction notice key"):
            handler.to_selected_html(response.body, member.selector, published_on=member.ref.published,
                                     record_digest=member.record_digest)
    for response in (_hunan_expansion(group, []), _hunan_expansion(group, [row] * 257),
                     _hunan_expansion(group, json.dumps([row])), _hunan_expansion(group, [row], success=False),
                     _hunan_expansion(group, [row], code="200"),
                     _hunan_expansion(group, [row], time=float("nan")),
                     FetchResponse(200, valid.media_type, valid.body[:-1], valid.url)):
        with pytest.raises(ValueError):
            handler.expand_group(channel, group, response)
    duplicate_field = valid.body.replace(b'"id":', b'"id":"replacement","id":')
    with pytest.raises(ValueError, match="duplicate JSON key"):
        handler.expand_group(channel, group, FetchResponse(200, valid.media_type, duplicate_field, valid.url))


def test_hunan_group_expansion_rejects_wrong_targets_and_access_gates(hunan_section):
    from dataclasses import replace

    handler, group = hunan_section
    channel = handler.channels[0]
    row = _hunan_notice("aaaaaaaa-2222-4333-8444-555555555555")
    valid = _hunan_expansion(group, [row])
    for url in (valid.url.replace(handler.origin, "https://outside.invalid"),
                valid.url.replace(UUID, "wrong-section"), valid.url + f"&sectionId={UUID}", valid.url + "#fragment"):
        with pytest.raises(ValueError):
            handler.expand_group(channel, group, replace(valid, url=url))
        with pytest.raises(ValueError):
            handler.expand_group(channel, replace(group, request=replace(group.request, url=url)), valid)
    with pytest.raises(ValueError, match="official group request"):
        handler.expand_group(channel, replace(group, request=replace(group.request, method="POST")), valid)
    with pytest.raises(ValueError, match="invalid Hunan construction section channel"):
        handler.expand_group("other-channel", group, valid)
    with pytest.raises(SourceBlocked, match="captcha_challenge"):
        handler.expand_group(channel, group, _response(valid.url, "<html><title>访问验证</title></html>", valid.media_type))
    with pytest.raises(SourceBlocked, match="api_403"):
        handler.expand_group(channel, group, _hunan_expansion(group, [row], code=403))


def test_hunan_validation_cli_samples_a_group_and_filters_by_member_date(monkeypatch, capsys, hunan_section):
    from hashlib import sha256
    from unittest.mock import Mock

    from miaoqia.runtime_acquisition import fetch
    from test_source_validation import FakeClock, run_validation

    handler, group = hunan_section
    channel = handler.channels[0]
    candidates = [{"bidSectionId": UUID, "noticeSendTime": "2026-11-18 15:02:40"}]
    candidates.extend({"bidSectionId": f"section-{i}"} for i in range(9))
    first = _hunan_page(handler, 1, 11, candidates)
    second = _hunan_page(handler, 2, 11, [{"bidSectionId": "last-section"}])
    tender = _hunan_notice("aaaaaaaa-2222-4333-8444-555555555555")
    clarification = _hunan_notice("bbbbbbbb-2222-4333-8444-555555555555", "CHENGQING_NOTICE", "2026-11-18 15:02:40")
    detail = _hunan_expansion(group, [clarification, tender])
    transport = Mock(spec=fetch.UrllibTransport)
    transport.send.side_effect = [first, second, detail]
    monkeypatch.setattr(fetch, "SystemClock", FakeClock)
    monkeypatch.setattr(fetch, "UrllibTransport", lambda: transport)
    report = run_validation(capsys, handler.source_id, channel, "--since", "2026-11-12", "--until", "2026-11-12",
                            "--max-pages", "2", "--max-details", "1", exit_code=3, outcome="limited",
                            admission="active", handler=f"{HunanWorks.__module__}:HunanWorks", pagination="advanced")
    assert [(page["refs"], page["groups"], page["has_next"]) for page in report["pages"]] == [
        (0, 10, True), (0, 1, False),
    ]
    (observed,) = report["details"]
    assert (observed["group_key"], observed["notice_count"], observed["size_bytes"]) == (UUID, 2, len(detail.body))
    assert observed["response_sha256"] == "sha256:" + sha256(detail.body).hexdigest()
    (selected,) = observed["notices"]
    assert (selected["source_key"], selected["published"], selected["selector"]["notice_key"]) == (
        tender["id"], "2026-11-12", tender["id"],
    )
    assert selected["record_digest"] != observed["response_sha256"]
    assert [call.args[0].url for call in transport.send.call_args_list] == [first.url, second.url, detail.url]


def test_tibet_records_literal_static_links_without_executing_script():
    handler = TibetWorks()
    request = handler.list_request(handler.channels[0], WINDOW, 1)
    assert request.method == "GET" and request.url == "http://ggzy.xizang.gov.cn/p22/jyxxgc.html"
    listing = _listing(handler, """
        <ul><li><p onclick="window.open('/jyxxgcgg/7121.jhtml','_blank');">合成工程公告</p>
        <p>2026-11-18</p></li>
        <li><p onclick="window.open('http://outside.invalid/jyxxgcgg/7122.jhtml')">站外</p></li>
        <li><p onclick="window.open('/jyxxgcgg/' + runtimeValue)">动态</p></li></ul>
    """)
    (ref,) = listing.refs
    assert ref.source_key == "7121" and ref.published == PUBLISHED
    assert ref.source_url == "http://ggzy.xizang.gov.cn/jyxxgcgg/7121.jhtml"
    assert not listing.has_next and listing.coverage_gap == "project_code_followup_unavailable"
    assert not handler.detail_fetches(handler.channels[0], ref)


@pytest.mark.parametrize("handler_type", [HubeiWorks, HunanWorks, ShaanxiWorks, ShaanxiGovernmentProcurement])
def test_json_error_and_missing_records_cannot_count_as_empty(handler_type):
    handler = handler_type()
    with pytest.raises(SourceBlocked, match="api_403"):
        _listing(handler, '{"code":403}', "application/json")
    with pytest.raises(ValueError):
        _listing(handler, '{"data":[]}', "application/json")
