"""Synthetic protocol fixtures for the native bulletin board rules."""
from dataclasses import replace
from datetime import date
from html import escape
from pathlib import Path
from urllib.parse import urlsplit

import pytest

from miaoqia.acquisition.api.handlers import DetailRef, FetchResponse, SourceBlocked, Window, checked_detail_ref
from miaoqia.acquisition.application import sources
from miaoqia.evidence.extensions.packs import load_documents
from miaoqia.sdk import Document, ExtractionContext
from miaoqia.sdk.testing import html_to_document
from miaoqia.sdk.validation import validate_extraction
from miaoqia_pack_procurement.extractors.tender_notice import TenderNoticeExtractor
from miaoqia_pack_procurement.validation import validate

PACKS = Path(__file__).resolve().parents[3]
DAY = date(2026, 11, 19)


def test_new_sources_with_handlers_have_an_explicit_parsing_decision():
    legacy_live = {"anhui-ggzy-construction", "anhui-government-procurement-spa", "national-public-resources"}
    documents = load_documents(PACKS)
    for source_id, entry in sources.catalog(PACKS).items():
        if entry.declaration is None or source_id in legacy_live:
            continue
        assert source_id in documents and documents[source_id].live_parse is not None, source_id
    for source_id in ("jiangxi-ggzy-announcements", "chongqing-ggzy-trade-014", "jsjyztb-web-list",
                      "tibet-ggzy-construction", "xinjiang-government-procurement-home", "ymzec-trade-info"):
        assert documents[source_id].live_parse is False


@pytest.fixture
def cnce():
    entry = sources.catalog(PACKS)["cncecyc-ywgg1gc"]
    assert entry.admission == "active" and entry.declaration.schedule.initial_backfill_days == 30
    return entry.declaration.handler


def cnce_listing(current, total, next_control, notice_id="314"):
    row = (f'<li><a href="/cms/channel/ywgg1gc/{notice_id}.htm?tracking=fixture#top">【MOCK】工程招标公告</a>'
           f'<a href="https://other.example/cms/channel/ywgg1gc/999.htm">foreign link</a>'
           '<span class="bidDate">2026-11-19</span></li>') if notice_id else ""
    return (f'<ul id="list1">{row}</ul><div class="pagination">'
            f'<div class="pag-txt">当前<em>{current}</em>页 / 共计<em>{total}</em>页</div>'
            f'<div class="pag-link">{next_control}</div></div>').encode()


@pytest.mark.parametrize("page,total,query,has_next", [(1, 3, "", True), (2, 3, "pageNo=2", True),
                                                       (3, 3, "pageNo=3", False)])
def test_cnce_pagination_and_identity(cnce, page, total, query, has_next):
    request = cnce.list_request("published", Window(DAY, DAY), page)
    assert urlsplit(request.url).query == query
    control = (f'<a class="pageItem" page="{page + 1}" aria-label="Next">下页</a>' if has_next
               else '<a class="disabled">下页</a>')
    notice_id = str(313 + page)
    raw = cnce_listing(page, total, control, notice_id=notice_id)
    result = cnce.parse_list("published", FetchResponse(200, "text/html", raw, request.url))
    result.verify_request(page, window=None)
    assert result.has_next is has_next and result.coverage_gap is None
    assert result.ordered_by_publication
    ref, = result.refs
    assert (ref.source_key, ref.source_url, ref.published) == (
        notice_id, f"{cnce.origin}/cms/channel/ywgg1gc/{notice_id}.htm", DAY,
    )
    detail, = cnce.detail_fetches("published", ref)
    assert detail.artifact_key == notice_id and detail.request.url == ref.source_url


def test_cnce_reports_unrecognized_rows_and_missing_dates_as_coverage_gaps(cnce):
    request = cnce.list_request("published", Window(DAY, DAY), 1)
    markup = cnce_listing(1, 1, '<a class="disabled">下页</a>')
    for raw in (markup.replace(b"</ul>", b"<li>Unrecognized row</li></ul>"),
                markup.replace(b"2026-11-19", b"unknown")):
        result = cnce.parse_list("published", FetchResponse(200, "text/html", raw, request.url))
        assert result.coverage_gap == "unrecognized_list_rows"
        assert not result.ordered_by_publication


def test_cnce_uses_the_publisher_date_field_instead_of_dates_in_link_text(cnce):
    request = cnce.list_request("published", Window(DAY, DAY), 1)
    markup = cnce_listing(1, 1, '<a class="disabled">下页</a>')
    raw = markup.replace("工程招标公告".encode(), "2024-01-02 工程招标公告".encode())
    result = cnce.parse_list("published", FetchResponse(200, "text/html", raw, request.url))
    assert result.ordered_by_publication and result.refs[0].published == DAY
    raw = raw.replace(b'2026-11-19</span>', b'unknown</span>')
    result = cnce.parse_list("published", FetchResponse(200, "text/html", raw, request.url))
    assert result.refs[0].published is None and not result.ordered_by_publication


def test_cnce_ambiguous_rows_cannot_prove_order_even_when_total_counts_match(cnce):
    request = cnce.list_request("published", Window(DAY, DAY), 1)
    markup = cnce_listing(1, 2, '<a class="pageItem" page="2">下页</a>')
    raw = markup.replace(b'</li>', b'<a href="/cms/channel/ywgg1gc/315.htm">Another notice</a></li><li>Unknown</li>')
    result = cnce.parse_list("published", FetchResponse(200, "text/html", raw, request.url))
    assert [ref.source_key for ref in result.refs] == ["314", "315"]
    assert all(ref.published is None for ref in result.refs)
    assert result.coverage_gap == "unrecognized_list_rows"
    assert not result.ordered_by_publication


@pytest.mark.parametrize("requested,current,total,control,notice_id", [
    (3, 2, 2, '<a class="disabled">下页</a>', "314"),
    (2, 2, 2, '<a class="disabled">下页</a>', None),
    (1, 1, 0, '<a class="disabled">下页</a>', None),
    (1, 1, 3, '<a class="pageItem" page="3">下页</a>', "314"),
    (2, 2, 2, '<a class="pageItem" page="3">下页</a>', "314"),
    (1, 1, 1, '', "314"),
])
def test_cnce_rejects_clamping_empty_or_inconsistent_listing(cnce, requested, current, total, control, notice_id):
    request = cnce.list_request("published", Window(DAY, DAY), requested)
    raw = cnce_listing(current, total, control, notice_id=notice_id)
    with pytest.raises(ValueError):
        cnce.parse_list("published", FetchResponse(200, "text/html", raw, request.url))


def test_cnce_requires_official_detail_route_and_native_body(cnce):
    locator = f"{cnce.origin}/cms/channel/ywgg1gc/314.htm"
    raw = "<h1>【MOCK】招标公告</h1><article class='ninfo-con'><p>工程内容</p></article>".encode()
    cnce.check_detail(FetchResponse(200, "text/html", raw, locator))
    assert "工程内容" in load_documents(PACKS)[cnce.source_id].to_html(raw)
    for address in (locator.replace(cnce.origin, "https://other.example"), cnce.board_url, f"{locator}?other=1"):
        with pytest.raises(ValueError, match="official origin|CNCE notice route"):
            cnce.check_detail(FetchResponse(200, "text/html", raw, address))
    with pytest.raises(ValueError, match="missing or ambiguous"):
        cnce.check_detail(FetchResponse(200, "text/html", b"<p>Not an announcement</p>", locator))
    with pytest.raises(SourceBlocked, match="http_403"):
        cnce.check_detail(FetchResponse(403, "text/html", raw, locator))


def test_cnce_full_title_and_bracketed_notice_number_keep_original_evidence(cnce):
    title = "【MOCK】合成工程 公开招标公告"
    raw = (
        "<h1>供应商门户</h1>"
        f"<div class='ninfo-title'><h2>{title}</h2></div>"
        "<article class='ninfo-con'><h1>招标公告</h1><p>工程简称</p>"
        "<p>（招标编号：SYN-2026/17）</p><p>招标人：合成工程有限公司</p></article>"
    ).encode()
    adapter = load_documents(PACKS)[cnce.source_id]
    doc = Document(html_to_document(adapter.to_html(raw), "snp_" + "0" * 26))
    extractor = TenderNoticeExtractor()
    assert extractor.applies(doc, {"source_id": cnce.source_id})
    ctx = ExtractionContext(doc, f"{extractor.id}@{extractor.version}")
    result = extractor.extract(doc, ctx)
    fact, = result.facts
    assert fact["title"]["value"] == title
    assert fact["notice_code"]["value"] == "SYN-2026/17"
    assert "project_code" not in fact
    assert not validate_extraction(doc, result.facts, ctx.evidence, semantic_validator=validate)


def test_railway_materials_discovers_notice_links_but_rejects_unverified_bodies():
    source_id = "crecgec-home"
    path = "/notice/315.html"
    entry = sources.catalog(PACKS)[source_id]
    assert entry.admission == "staged"
    handler = entry.declaration.handler
    channel, = handler.channels
    request = handler.list_request(channel, Window(DAY, DAY), 1)
    row = (f'<a href="{path}?tracking=fixture#top">【MOCK】工程招标公告</a> 2026-11-19'
           f'<a href="https://other.example{path}">foreign link</a>')
    response = FetchResponse(200, "text/html", row.encode(), request.url)
    parsed = handler.parse_list(channel, response)
    (ref,) = parsed.refs
    assert ref.source_key == "315" and urlsplit(ref.source_url).path == path
    assert not urlsplit(ref.source_url).query
    assert not parsed.has_next and parsed.coverage_gap == "verified_entry_page_only"
    (detail,) = handler.detail_fetches(channel, ref)
    raw = ('<body><h1>【MOCK】工程招标公告</h1><p>平台说明</p>'
           '<iframe src="/assets/tmp/article.html"></iframe>'
           '<footer><a href="/files/public.pdf">平台文件</a></footer></body>').encode()
    with pytest.raises(ValueError, match="procurement notice body is not verified"):
        handler.check_detail(FetchResponse(200, "text/html", raw, detail.request.url))
    document = load_documents(PACKS)[source_id]
    assert not document.live_parse
    with pytest.raises(ValueError, match="procurement notice body is not verified"):
        document.to_html(raw)


@pytest.fixture
def scbid():
    return sources.load(PACKS)["scbid-zbxx"].handler


def scbid_listing(page):
    links = "".join(
        f'<li class="pagination-page pagination-item {"current" if index == page else ""}">'
        f'<a href="/zbxx/p{index}.html">{index}</a></li>' for index in (1, 2, 3)
    )
    return (
        '<div class="w bidding-sections-lists"><div class="rows-container"><div class="row">'
        f'<div class="title title608"><a href="/bx/detail/974001_{314 + page}.html">'
        '【MOCK】2024-01-01 工程招标公告</a></div>'
        '<a class="region" href="/bx/region_511000">测试地区</a><div class="stime">11-19</div>'
        f'</div></div><ul class="pagination-ul">{links}</ul></div>'
        '<aside><a href="/bx/detail/123456_654321.html">侧栏公告</a></aside>'
    ).encode()


@pytest.mark.parametrize("number,suffix,has_next,gap", [
    (1, "", True, "list_publication_date_unavailable"),
    (2, "/p2.html", False, "pagination_unverified"),
])
def test_scbid_verified_public_pages(scbid, number, suffix, has_next, gap):
    request = scbid.list_request("published", Window(DAY, DAY), number)
    assert request.method == "GET" and request.url == scbid.board_url + suffix
    response = FetchResponse(200, "text/html", scbid_listing(number), request.url)
    listing = scbid.parse_list("published", response).for_request(number, window=None)
    assert listing.has_next is has_next and listing.coverage_gap == gap
    assert not listing.ordered_by_publication
    (ref,) = listing.refs
    assert ref.source_key == str(314 + number) and ref.published is None
    assert ref.source_url == f"{scbid.origin}/bx/detail/974001_{314 + number}.html"
    assert "工程招标公告" in ref.title
    with pytest.raises(ValueError, match="public listing request"):
        scbid.list_request("published", Window(DAY, DAY), 3)


@pytest.mark.parametrize("before,after,error", [
    (b"/zbxx/p2.html", b"https://example.org/zbxx/p2.html", "next page"),
    (b"/zbxx/p1.html", b"/zbxx/p2.html", "requested page"),
    (b"/bx/detail/974001_315.html", b"https://example.org/bx/detail/974001_315.html", "announcement row"),
    (b'class="stime"', b'class="unknown"', "notice fields"),
])
def test_scbid_rejects_mismatched_pager_or_listing_rows(scbid, before, after, error):
    raw = scbid_listing(1).replace(before, after, 1)
    with pytest.raises(ValueError, match=error):
        scbid.parse_list("published", FetchResponse(200, "text/html", raw, scbid.board_url))


def test_scbid_checks_the_official_detail_and_uses_its_publication_field(scbid):
    listing = scbid.parse_list("published", FetchResponse(200, "text/html", scbid_listing(1), scbid.board_url))
    ref, = listing.refs
    part, = scbid.detail_fetches("published", ref)
    assert part.artifact_key == ref.source_key == "315" and part.request.url == ref.source_url
    raw = ('<script>var metadata = {\n    data: {nid: "315"},\n};</script>'
           '<div class="title scbid-printer-part-title">【MOCK】工程招标公告 &amp; 说明</div>'
           '<div class="subtitle"><span class="scbid-printer-part-time">'
           '<span>发布日期：2026年11月19日 14:30</span><span>地区：测试地区</span>'
           '</span></div><div class="content"><p>正文日期 2024-01-01</p>'
           '<script>var unrelated = {\n    data: {nid: "316"},\n};</script></div>').encode()
    assert scbid.check_detail(FetchResponse(200, "text/html", raw, ref.source_url)) == date(2026, 11, 19)
    normalized = load_documents(PACKS)[scbid.source_id].to_html(raw)
    assert "<h1>【MOCK】工程招标公告 &amp; 说明</h1>" in normalized
    assert "<p>公告时间：2026-11-19</p>" in normalized and "正文日期 2024-01-01" in normalized
    with pytest.raises(ValueError, match="detail reference"):
        scbid.detail_fetches("published", type(ref)("316", ref.source_url, None))
    with pytest.raises(ValueError, match="detail identity does not match"):
        scbid.check_detail(FetchResponse(200, "text/html", raw, ref.source_url.replace("_315.html", "_316.html")))
    with pytest.raises(ValueError, match="conflicting SCBid detail identity"):
        scbid.check_detail(FetchResponse(200, "text/html", raw.split(b"</script>", 1)[1], ref.source_url))
    conflict = raw + b'<script>var metadata = {\n    data: {nid: "317"},\n};</script>'
    with pytest.raises(ValueError, match="conflicting SCBid detail identity"):
        scbid.check_detail(FetchResponse(200, "text/html", conflict, ref.source_url))
    with pytest.raises(ValueError, match="detail publication date"):
        scbid.check_detail(FetchResponse(200, "text/html", raw.replace("11月19日".encode(), "02月30日".encode()), ref.source_url))
    with pytest.raises(ValueError, match="official origin"):
        scbid.check_detail(FetchResponse(200, "text/html", raw, ref.source_url.replace(scbid.origin, "https://other.example")))
    with pytest.raises(SourceBlocked, match="http_403"):
        scbid.check_detail(FetchResponse(403, "text/html", raw, ref.source_url))
    with pytest.raises(SourceBlocked, match="captcha_challenge"):
        scbid.check_detail(FetchResponse(200, "text/html", "<title>访问验证</title>".encode(), ref.source_url))


@pytest.fixture
def shangrao():
    entry = sources.catalog(PACKS)["ygcgfwpt-transaction"]
    assert entry.admission == "active" and entry.declaration.schedule.initial_backfill_days == 30
    return entry.declaration.handler


def shangrao_listing(page=1, total=61):
    rows = []
    for i in range(min(10, total - (page - 1) * 10)):
        key = f"12345678-abcd-4567-8abc-{page * 10 + i:012x}"
        rows.append(
            f'<li class="list-item"><a href="/jyxx/002001/002001003/20261119/{key}.html">'
            '<p class="title text-overflow"><font>[市本级]</font>【MOCK】2020-01-01 工程招标公告</p>'
            '<span class="date">2026-11-19</span></a></li>'
        )
    return (
        f'<ul id="infolist">{"".join(rows)}</ul><div id="pager"></div>'
        f'<script>\nvar pageIndex = {page - 1}+1;\nvar pageSize = 10;\n'
        f'var pagetotalcount = 6;\nvar total = {total};\n</script>'
    ).encode()


@pytest.mark.parametrize("page,total,rows,has_next,gap", [
    (1, 61, 10, True, None),
    (2, 61, 10, True, None),
    (2, 15, 5, False, None),
    (6, 61, 10, False, "pagination_requires_captcha"),
])
def test_shangrao_public_listing_boundary(shangrao, page, total, rows, has_next, gap):
    request = shangrao.list_request("published", Window(DAY, DAY), page)
    expected_suffix = "transaction_inquiry.html" if page == 1 else f"{page}.html"
    assert request.method == "GET"
    assert request.url == f"https://www.ygcgfwpt.com/jyxx/002001/002001003/{expected_suffix}"
    response = FetchResponse(200, "text/html", shangrao_listing(page, total), request.url)
    result = shangrao.parse_list("published", response).for_request(page, window=None)
    assert not shangrao.windowed
    assert result.reported_page == page and result.ordered_by_publication
    assert result.has_next is has_next and result.coverage_gap == gap
    assert len(result.refs) == rows
    for i, ref in enumerate(result.refs):
        key = f"12345678-abcd-4567-8abc-{page * 10 + i:012x}"
        assert ref.source_key == key and ref.source_url.endswith(f"/20261119/{key}.html")
        assert ref.published == DAY and "工程招标公告" in ref.title
        part, = shangrao.detail_fetches("published", ref)
        assert part.artifact_key == key and part.request.url == ref.source_url
    with pytest.raises(ValueError, match="public listing request"):
        shangrao.list_request("published", Window(DAY, DAY), 7)


@pytest.mark.parametrize("before,after,error", [
    (b"pageIndex = 1+1", b"pageIndex = 0+1", "listing route"),
    (b"pageSize = 10", b"pageSize = 20", "page metadata"),
    (b"var total = 61;", b"var total = 61;\nvar total = 61;", "page metadata"),
    (b"total = 61", b"total = 15", "row count"),
    (b'span class="date"', b'span class="unrelated"', "announcement row"),
    (b'/002001003/20261119/', b'/002001004/20261119/', "announcement row"),
])
def test_shangrao_rejects_unproven_listing_metadata_and_rows(shangrao, before, after, error):
    request = shangrao.list_request("published", Window(DAY, DAY), 2)
    raw = shangrao_listing(2).replace(before, after, 1)
    with pytest.raises(ValueError, match=error):
        shangrao.parse_list("published", FetchResponse(200, "text/html", raw, request.url))


def test_shangrao_checks_native_detail_identity_and_normalizes_its_heading(shangrao):
    request = shangrao.list_request("published", Window(DAY, DAY), 1)
    page = shangrao.parse_list("published", FetchResponse(200, "text/html", shangrao_listing(), request.url))
    ref = page.refs[0]
    raw = (f'<title>上饶市阳光采购平台</title><h3 class="title">【MOCK】招标公告 &amp; 说明</h3>'
           f'<span id="infoid">{ref.source_key}</span><span id="cnumnow">002001003</span>'
           '<div id="content-main"><p>工程内容</p></div>').encode()
    shangrao.check_detail(FetchResponse(200, "text/html", raw, ref.source_url))
    normalized = load_documents(PACKS)[shangrao.source_id].to_html(raw)
    assert "<h1>【MOCK】招标公告 &amp; 说明</h1>" in normalized and "工程内容" in normalized
    assert "上饶市阳光采购平台" not in normalized
    with pytest.raises(ValueError, match="detail reference"):
        shangrao.detail_fetches("published", type(ref)(page.refs[1].source_key, ref.source_url, DAY))
    with pytest.raises(ValueError, match="detail identity does not match"):
        shangrao.check_detail(FetchResponse(200, "text/html", raw, page.refs[1].source_url))
    with pytest.raises(ValueError, match="official origin"):
        shangrao.check_detail(FetchResponse(200, "text/html", raw, ref.source_url.replace(shangrao.origin, "https://other.example")))
    with pytest.raises(ValueError, match="detail identity"):
        shangrao.check_detail(FetchResponse(200, "text/html", raw.replace(b'infoid', b'other'), ref.source_url))
    with pytest.raises(ValueError, match="title or body"):
        shangrao.check_detail(FetchResponse(200, "text/html", raw.replace(b'content-main', b'other'), ref.source_url))
    with pytest.raises(SourceBlocked, match="http_403"):
        shangrao.check_detail(FetchResponse(403, "text/html", raw, ref.source_url))
    with pytest.raises(SourceBlocked, match="captcha_challenge"):
        shangrao.parse_list("published", FetchResponse(200, "text/html", "<title>访问验证</title>".encode(), request.url))


def test_xinjiang_procurement_homepage_keeps_publisher_query_fields():
    source_id = "xinjiang-government-procurement-home"
    locator = "/site/detail?parentId=1&articleId=hello%2F%2B%3D"
    source = sources.catalog(PACKS)[source_id]
    handler = source.declaration.handler
    request = handler.list_request("published", Window(DAY, DAY), 1)
    raw = f'<main><a href="{escape(locator)}">【MOCK】招标公告</a></main>'.encode()
    page = handler.parse_list("published", FetchResponse(200, "text/html", raw, request.url))
    (ref,) = page.refs
    assert ref.source_url.endswith(locator) and ref.source_key == "hello/+="
    assert page.coverage_gap
    assert handler.detail_fetches("published", ref) == ()
    shell = (Path(__file__).parent / "fixtures/sources/synthetic_xinjiang_shell.html").read_bytes()
    with pytest.raises(ValueError, match="no independently fetchable announcement body"):
        handler.check_detail(FetchResponse(200, "text/html", shell, ref.source_url))
    with pytest.raises(ValueError, match="no independently fetchable announcement body"):
        load_documents(PACKS)[source_id].to_html(shell)


def test_tibet_procurement_uses_publisher_identity_and_excludes_page_resources():
    source = sources.catalog(PACKS)["tibet-government-procurement-home"]
    handler = source.declaration.handler
    request = handler.list_request("published", Window(DAY, DAY), 1)
    route = "/freecms/site/xizang/xzcggg/info/2026/123456.html"
    raw = (f'<div class="projectAnnouncement"><div class="notice_list"><ul>'
           f'<li><a href="{route}"><span></span>合成采购公告<span>2026-11-19</span></a></li>'
           f'</ul></div></div><footer><a href="{route.replace("123456", "987654")}">网站资源</a></footer>').encode()
    page = handler.parse_list("published", FetchResponse(200, "text/html", raw, request.url))
    (ref,) = page.refs
    assert page.coverage_gap == "homepage_current_window" and not page.has_next
    assert (ref.source_key, ref.published, ref.title) == ("2026:123456", DAY, "合成采购公告")
    (detail,) = handler.detail_fetches("published", ref)
    assert detail.request.url.endswith(route)
    fragment = ('<div id="print-content"><div class="notice-hear"><h2>合成采购公告 &amp; 说明</h2>'
                '<span>发布时间：2026-11-19 11:00:00</span></div>'
                '<div class="notice-con"><p>正文内容</p></div></div>'
                '<div id="printArea"><a href="/download/site.pdf" download>网站资源</a></div>').encode()
    handler.check_detail(FetchResponse(200, "text/html", fragment, detail.request.url))
    normalized = load_documents(PACKS)[handler.source_id].to_html(fragment)
    assert "<h1>合成采购公告 &amp; 说明</h1>" in normalized and "正文内容" in normalized
    assert "<p>公告时间：2026-11-19 11:00:00</p>" in normalized
    assert "site.pdf" not in normalized and not hasattr(handler, "attachment_fetches")
    with pytest.raises(ValueError, match="announcement body"):
        handler.check_detail(FetchResponse(200, "text/html", fragment.replace("正文内容".encode(), b""),
                                           detail.request.url))
    with pytest.raises(ValueError, match="publication date"):
        handler.check_detail(FetchResponse(200, "text/html", fragment.replace(b"2026-11-19", b"2025-11-19"),
                                           detail.request.url))


def test_tibet_blank_detail_date_requires_matching_list_identity_and_title():
    handler = sources.catalog(PACKS)["tibet-government-procurement-home"].declaration.handler
    url = handler.list_url + "freecms/site/xizang/xzcggg/info/2026/123456.html"
    ref = DetailRef("2026:123456", url, DAY, "合成采购公告")
    raw = ('<div id="print-content"><div class="notice-hear"><h2>合成采购公告</h2>'
           '<span>发布时间：</span></div><div class="notice-con">正文内容</div></div>'
           '<a href="/file/site.pdf">平台资料</a>').encode()
    response = FetchResponse(200, "text/html", raw, url)

    assert checked_detail_ref(handler, ref, response) == ref
    assert load_documents(PACKS)[handler.source_id].to_html(raw) == (
        '<h1>合成采购公告</h1><div class="notice-con">正文内容</div>'
    )
    for conflicting in (replace(ref, source_key="2026:123457"), replace(ref, title="其他采购公告"),
                        replace(ref, published=None)):
        with pytest.raises(ValueError):
            checked_detail_ref(handler, conflicting, response)
    for value in ("待定", "2026-11-20"):
        with pytest.raises(ValueError, match="publication date"):
            checked_detail_ref(handler, ref, replace(response, body=raw.replace("发布时间：".encode(),
                                                                              f"发布时间：{value}".encode())))
    duplicate = raw.replace(b"</span>", "</span><span>发布时间：2026-11-19</span>".encode())
    with pytest.raises(ValueError, match="ambiguous"):
        handler.to_html(duplicate)


def test_henan_open_procurement_fetches_the_official_body_fragment():
    source = sources.catalog(PACKS)["henan-government-procurement-home"]
    handler = source.declaration.handler
    href = "/henan/content?infoId=753&amp;channelCode=H600101&amp;bz=1"
    raw = (f'<ul><li><span class="Right Gray">2026-11-19</span><a href="{href}">采购公告</a></li>'
           f'<li><a href="{href.replace("H600101", "H600107")}">采购意向</a></li></ul>').encode()
    request = handler.list_request("published", Window(DAY, DAY), 1)
    page = handler.parse_list("published", FetchResponse(200, "text/html", raw, request.url))
    assert page.coverage_gap == "homepage_current_window"
    (ref,) = page.refs
    assert ref.published == DAY and ref.source_key == "753"
    (detail,) = handler.detail_fetches("published", ref)
    assert detail.request.url.endswith("/henan/cgxx/cggg/webinfo/2026/11/19/753.htm")
    fragment = ('<table class="Content"><tr><td>项目编号：MOCK-1</td></tr></table>'
                '<footer><a href="/download/site.pdf">站点公共文件</a></footer>').encode()
    handler.check_detail(FetchResponse(200, "text/html", fragment, detail.request.url))
    normalized = load_documents(PACKS)[handler.source_id].to_html(fragment)
    assert "MOCK-1" in normalized and "站点公共文件" not in normalized
    with pytest.raises(ValueError, match="announcement body"):
        handler.check_detail(FetchResponse(200, "text/html", b'<h2>Header only</h2>', detail.request.url))
