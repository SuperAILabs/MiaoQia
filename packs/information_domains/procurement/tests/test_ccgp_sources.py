"""Offline SourceHandler contracts using synthetic responses shaped like the public reference."""
from __future__ import annotations

import json
from dataclasses import replace
from datetime import date, datetime, timedelta, timezone
from hashlib import sha256

import pytest

from miaoqia.acquisition.api.handlers import FetchResponse, SourceBlocked, Window
from miaoqia.sdk import Document, ExtractionContext
from miaoqia.sdk.testing import html_to_document
from miaoqia_pack_procurement.documents import anhui_ccgp_html
from miaoqia_pack_procurement.extractors.tender_notice import TenderNoticeExtractor
from miaoqia_pack_procurement.sources.ccgp_html import (
    CentralPublicTenders,
    HubeiGovernmentProcurement,
    central_tender_html,
)
from miaoqia_pack_procurement.sources.hunan_ccgp import HunanGovernmentProcurement, HunanProcurementCategory

DAY = date(2026, 11, 19)
WINDOW = Window(DAY, DAY)
CENTRAL_LIST = "https://www.ccgp.gov.cn/cggg/zygg/gkzb/"
HUBEI_HOME = "https://www.ccgp-hubei.gov.cn/"
HUBEI_PATH = f"/notice/202611/notice_{'a' * 32}.html"
HUNAN_LIST = "http://www.ccgp-hunan.gov.cn/portal/searchHome"


def page(body: str, url: str, status: int = 200) -> FetchResponse:
    return FetchResponse(status, "text/html", body.encode(), url)


def portal(data: dict, url: str) -> FetchResponse:
    return FetchResponse(200, "application/json", json.dumps({
        "success": True, "result": {"data": data},
    }, ensure_ascii=False).encode(), url)


def extract_tender(html: str, source_url: str) -> dict:
    doc = Document(html_to_document(html, "snp_" + "0" * 26, source_url))
    extractor = TenderNoticeExtractor()
    assert extractor.applies(doc, {})
    (fact,) = extractor.extract(doc, ExtractionContext(doc, f"{extractor.id}@{extractor.version}")).facts
    return fact


def test_central_static_contract_and_pagination_gap():
    handler = CentralPublicTenders()
    (channel,) = handler.channels
    list_body = """
        <ul class="c_list_bid">
          <li><a href="./202611/t20261119_99990001.htm">合成中央单位公开招标公告</a>
              发布时间：<em>2026-11-19 18:01</em> 地域：<em>江西</em></li>
          <li><a href="/cggg/dfgg/gkzb/202611/t20261119_99990002.htm">地方公告</a></li>
          <li><a href="//example.invalid/cggg/zygg/gkzb/202611/t20261119_99990003.htm">外站</a></li>
          <li><a href="./202611/t20261119_99990001.htm">重复链接</a></li>
        </ul>
    """
    first_request = handler.list_request(channel, WINDOW, 1)
    assert (first_request.method, first_request.url) == ("GET", CENTRAL_LIST)
    listing = handler.parse_list(channel, page(list_body + '<a href="index_1.htm">下一页</a>', first_request.url))
    assert listing.has_next and listing.coverage_gap is None
    (ref,) = listing.refs
    assert ref.source_key == "99990001" and ref.published == DAY
    assert ref.source_url == CENTRAL_LIST + "202611/t20261119_99990001.htm"
    assert ref.title == "合成中央单位公开招标公告"

    second_request = handler.list_request(channel, WINDOW, 2)
    assert second_request.url == CENTRAL_LIST + "index_1.htm"
    uncertain = page(list_body + '<a href="//example.invalid/cggg/zygg/gkzb/index_2.htm">外站下一页</a>',
                     second_request.url)
    second = handler.parse_list(channel, uncertain)
    assert not second.has_next and second.coverage_gap == "pagination_unverified"

    (detail,) = handler.detail_fetches(channel, ref)
    assert (detail.artifact_key, detail.request.method, detail.request.url) == (ref.source_key, "GET", ref.source_url)
    response = page("""
        <h1>网站导航</h1>
        <div class="vF_detail_main">
          <div class="vF_detail_header"><h2>合成中央单位公开招标公告</h2></div>
          <table><tr><td>采购单位</td><td>合成采购单位</td></tr>
            <tr><td>代理机构名称</td><td>合成代理机构</td></tr>
            <tr><td>公告时间</td><td>2026年11月19日 18:01</td></tr></table>
          <div class="vF_detail_content"><p>项目编号：SYNTHETIC-01</p></div>
        </div>
    """, detail.request.url)
    handler.check_detail(response)
    normalized = central_tender_html(response.body)
    assert "网站导航" not in normalized and "<table>" in normalized
    fact = extract_tender(normalized, ref.source_url)
    assert fact["title"]["value"] == ref.title and fact["project_code"]["value"] == "SYNTHETIC-01"
    assert fact["buyer"]["name_raw"] == "合成采购单位" and fact["agency"]["name_raw"] == "合成代理机构"
    assert fact["published_at"]["value"] == "2026-11-19T10:01:00Z"
    assert all(fact[field]["prov"]["evidence_ref"].startswith("ev_") for field in ("buyer", "agency", "published_at"))
    with pytest.raises(ValueError, match="announcement title or body"):
        handler.check_detail(replace(response, body=response.body.replace(
            "项目编号：SYNTHETIC-01".encode(), b" ")))


def test_central_html_pager_advances_and_marks_recent_archive_boundary():
    handler = CentralPublicTenders()
    (channel,) = handler.channels
    listing = ('<ul class="c_list_bid"><li><a href="./202611/t20261119_99990001.htm">合成招标公告</a>'
               '发布时间：2026-11-19</li></ul>')

    def with_pager(current):
        return listing + f"<p class='pager'><script>Pager({{size:25, current:{current}, prefix:'index',suffix:'htm'}});</script></p>"

    first = handler.parse_list(channel, page(with_pager(0), CENTRAL_LIST))
    second_url = handler.list_request(channel, WINDOW, 2).url
    second = handler.parse_list(channel, page(with_pager(1), second_url))
    last_url = handler.list_request(channel, WINDOW, 25).url
    last = handler.parse_list(channel, page(with_pager(24), last_url))
    assert first.has_next and second.has_next and not last.has_next
    assert first.coverage_gap is None and second.coverage_gap is None
    assert last.coverage_gap == "rolling_archive_boundary" and not last.ordered_by_publication
    assert first.refs[0].source_key == last.refs[0].source_key == "99990001"
    with pytest.raises(ValueError, match="does not match the published pager"):
        handler.parse_list(channel, page(with_pager(0), second_url))


def test_hubei_homepage_scope_and_stable_notice_key():
    handler = HubeiGovernmentProcurement()
    (channel,) = handler.channels
    assert handler.list_request(channel, WINDOW, 1).url == HUBEI_HOME
    with pytest.raises(ValueError, match="limited"):
        handler.list_request(channel, WINDOW, 2)
    body = f"""
        <div id="area-1-1"><a href="{HUBEI_PATH}">其他栏目</a></div>
        <div id="area-1-2"><ul>
          <li><a href="{HUBEI_PATH}">2026-01-01 合成采购项目竞争性磋商公告</a><span>2026-11-19</span></li>
          <li><a href="https://example.invalid{HUBEI_PATH}">外站</a></li>
          <li><a href="https://hostile@www.ccgp-hubei.gov.cn{HUBEI_PATH}">带用户信息</a></li>
          <li><a href="https://www.ccgp-hubei.gov.cn:invalid{HUBEI_PATH}">错误端口</a></li>
          <li><a href="https://www.ccgp-hubei.gov.cn:444{HUBEI_PATH}">其他端口</a></li>
          <li><a href="{HUBEI_PATH}?track=1">非约定详情路径</a></li>
        </ul></div>
    """
    listing = handler.parse_list(channel, page(body, HUBEI_HOME))
    assert not listing.has_next and listing.coverage_gap == "homepage_current_window"
    (ref,) = listing.refs
    assert ref.source_key == "a" * 32 and ref.published == DAY
    assert ref.title.endswith("竞争性磋商公告")
    (fetch,) = handler.detail_fetches(channel, ref)
    assert fetch.request.url == HUBEI_HOME.rstrip("/") + HUBEI_PATH
    assert fetch.artifact_key == ref.source_key
    detail_markup = ('<div class="con_info"><h2>合成采购公告</h2>'
                     '<div class="art_con"><p>项目编号：HB-TEST</p></div></div>')
    handler.check_detail(page(detail_markup, fetch.request.url))
    for url in (HUBEI_HOME, "https://example.invalid" + HUBEI_PATH, "http://www.ccgp-hubei.gov.cn" + HUBEI_PATH):
        with pytest.raises(ValueError):
            handler.check_detail(page(detail_markup, url))
    with pytest.raises(ValueError, match="announcement container"):
        handler.check_detail(page('<footer><h2>公共导航</h2><p>项目编号：OTHER</p></footer>', fetch.request.url))


def test_hunan_home_query_keeps_original_bytes_and_reusable_portal_document_adapter():
    handler = HunanGovernmentProcurement()
    (channel,) = handler.channels
    request = handler.list_request(channel, WINDOW, 1)
    assert (request.method, request.url) == ("POST", HUNAN_LIST)
    assert json.loads(request.json_body) == {
        "code": "160-396330", "subCodes": ["160-945988"], "districtCode": "430000", "pageSize": 10,
        "needTotal": True, "needValidCount": False, "needNewCnt": True,
    }
    with pytest.raises(ValueError, match="limited"):
        handler.list_request(channel, WINDOW, 2)

    article = "synthetic/a+b=="
    rows = [
        {"articleId": article, "title": "合成湖南采购项目竞争性磋商公告", "publishDateString": "2026年11月19日"},
        {"articleId": "synthetic-2", "title": "合成湖南采购项目询价公告",
         "publishDate": int(datetime(2026, 11, 18, 16, tzinfo=timezone.utc).timestamp() * 1000)},
    ]
    listing = handler.parse_list(channel, portal({"firstId": 622707, "children": rows}, request.url))
    assert not listing.has_next and listing.coverage_gap == "homepage_current_window"
    assert [ref.published for ref in listing.refs] == [DAY, DAY]
    ref = listing.refs[0]
    assert ref.source_key == article
    (detail,) = handler.detail_fetches(channel, ref)
    assert detail.artifact_key == article
    assert detail.request.url.endswith("/portal/detail?articleId=synthetic%2Fa%2Bb%3D%3D&parentId=622707")
    new_list = handler.parse_list(channel, portal({"firstId": "622708", "children": rows}, request.url))
    (updated_locator,) = handler.detail_fetches(channel, new_list.refs[0])
    assert updated_locator.artifact_key == detail.artifact_key
    assert updated_locator.request.fingerprint() != detail.request.fingerprint()

    originals = []
    for project_code in ("HN-TEST-1", "HN-TEST-2"):
        original = portal({"articleId": article, "title": ref.title,
                           "content": f"<p>项目编号：{project_code}</p><p>采购人：合成采购单位</p>"}, ref.source_url)
        originals.append(original.body)
        handler.check_detail(original)
        fact = extract_tender(anhui_ccgp_html(original.body), ref.source_url)
        assert fact["method"] == "consultation" and fact["project_code"]["value"] == project_code
    assert sha256(originals[0]).digest() != sha256(originals[1]).digest()
    assert handler.detail_fetches(channel, ref) == (detail,)


@pytest.mark.parametrize("handler, url", [
    (CentralPublicTenders(), CENTRAL_LIST), (HubeiGovernmentProcurement(), HUBEI_HOME),
    (HunanGovernmentProcurement(), HUNAN_LIST),
])
@pytest.mark.parametrize("status, body, code", [
    (403, "", "http_403"),
    (200, "<title>访问验证</title><p>为保证您的正常访问，请进行验证</p>", "captcha_challenge"),
    (200, "<h1>用户登录</h1><form><input type='password'></form>", "login_required"),
])
def test_source_blocks_are_exposed_through_spi(handler, url, status, body, code):
    with pytest.raises(SourceBlocked) as exc:
        handler.parse_list(handler.channels[0], page(body, url, status))
    assert exc.value.code == code
    with pytest.raises(SourceBlocked) as exc:
        handler.check_detail(page(body, url, status))
    assert exc.value.code == code


@pytest.mark.parametrize("handler, url", [
    (CentralPublicTenders(), CENTRAL_LIST), (HubeiGovernmentProcurement(), HUBEI_HOME),
    (HunanGovernmentProcurement(), HUNAN_LIST),
])
def test_unknown_page_is_not_reported_as_empty_coverage(handler, url):
    with pytest.raises(ValueError):
        handler.parse_list(handler.channels[0], page("<div id='app'></div>", url))
    with pytest.raises(ValueError):
        handler.check_detail(page("<div id='app'></div>", url))


def test_hunan_rejects_malformed_envelope_and_wrong_detail_identity():
    handler = HunanGovernmentProcurement()
    invalid = FetchResponse(200, "application/json", b'{"success":true,"result":[]}', HUNAN_LIST)
    with pytest.raises(ValueError, match="portal response"):
        handler.parse_list(handler.channels[0], invalid)
    url = "http://www.ccgp-hunan.gov.cn/portal/detail?articleId=synthetic-1&parentId=622707"
    with pytest.raises(ValueError, match="articleId"):
        handler.check_detail(portal({"content": "<p>正文</p>"}, url))
    with pytest.raises(ValueError, match="articleId"):
        handler.check_detail(portal({"articleId": "another", "content": "<p>正文</p>"}, url))
    with pytest.raises(ValueError, match="no announcement body"):
        handler.check_detail(portal({"articleId": "synthetic-1", "content": "<div><script>test()</script></div>"}, url))


def test_hunan_does_not_parse_a_mislabeled_access_gate_as_json():
    handler = HunanGovernmentProcurement()
    response = FetchResponse(200, "application/json", "<h1>访问验证</h1>".encode(), HUNAN_LIST)
    with pytest.raises(SourceBlocked, match="captcha_challenge"):
        handler.parse_list(handler.channels[0], response)


def test_hunan_category_preflight_shares_query_and_uses_stable_window_identity():
    handler = HunanProcurementCategory()
    channel, = handler.channels
    now = datetime(2026, 11, 19, 2, tzinfo=timezone.utc)
    first = handler.list_plan(channel, WINDOW, 2, now)
    later = handler.list_plan(channel, WINDOW, 2, now + timedelta(seconds=5))
    assert first.identity == later.identity and first.request.fingerprint() != later.request.fingerprint()
    assert first.preflight.url.endswith("/portal/sensitiveWords/check") and first.request.url.endswith("/portal/category")
    assert first.preflight.json_body == first.request.json_body
    assert json.loads(first.request.json_body) == {
        "categoryCode": "160-945988", "pageNo": 2, "pageSize": 100,
        "publishDateBegin": DAY.isoformat(), "publishDateEnd": DAY.isoformat(),
        "_t": int(now.timestamp() * 1000),
    }


@pytest.mark.parametrize(("result", "error"), [
    ({"block": None}, None), ({"block": {"reason": "fixture"}}, SourceBlocked), ({}, ValueError),
])
def test_hunan_category_accepts_only_explicit_preflight_clearance(result, error):
    handler = HunanProcurementCategory()
    response = FetchResponse(200, "application/json", json.dumps({"success": True, "result": result}).encode(),
                             "http://www.ccgp-hunan.gov.cn/portal/sensitiveWords/check")
    if error is None:
        handler.check_list_preflight(response)
    else:
        with pytest.raises(error):
            handler.check_list_preflight(response)


def test_hunan_category_checks_requested_date_and_preserves_detail_identity():
    handler = HunanProcurementCategory()
    channel, = handler.channels
    request = handler.list_request(channel, WINDOW, 2)
    row = {"articleId": "fixture/a+b=", "title": "合成采购公告", "publishDateString": DAY.isoformat()}
    response = portal({"total": 101, "empty": False, "data": [row]}, request.url)
    terminal = handler.parse_list(channel, response).for_request(2, window=WINDOW)
    ref, = terminal.refs
    assert ref.source_key == row["articleId"] and ref.published == DAY and not terminal.has_next
    detail, = handler.detail_fetches(channel, ref)
    assert detail.artifact_key == row["articleId"] and detail.request.url.endswith("articleId=fixture%2Fa%2Bb%3D&parentId=622707")
    other_day = DAY + timedelta(days=1)
    with pytest.raises(ValueError, match="outside the request window"):
        handler.parse_list(channel, response).for_request(2, window=Window(other_day, other_day))

    empty = portal({"total": 0, "empty": True, "data": []}, request.url)
    verified = handler.parse_list(channel, empty).for_request(1, window=WINDOW)
    assert verified.refs == () and not verified.has_next and verified.coverage_gap is None
