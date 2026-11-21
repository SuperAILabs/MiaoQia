"""Source handlers against responses recorded during the D1b probe (no network)."""
import json
import re
from dataclasses import replace
from datetime import date
from pathlib import Path

import pytest

from miaoqia.acquisition.api.handlers import FetchResponse, SourceBlocked, Window
from miaoqia_pack_procurement.sources.anhui_ccgp import AnhuiGovernmentProcurement
from miaoqia_pack_procurement.sources.anhui_ggzy import AnhuiGgzyConstruction
from miaoqia_pack_procurement.sources.national_ggzy import NationalPublicResources

FIXTURES = Path(__file__).parent / "fixtures/sources"
TODAY = Window(date(2026, 11, 11), date(2026, 11, 11))


def response(name: str, media_type: str, url: str = "https://example.invalid/") -> FetchResponse:
    return FetchResponse(200, media_type, (FIXTURES / name).read_bytes(), url)


def test_anhui_ggzy_list_and_detail():
    h = AnhuiGgzyConstruction()
    assert len(h.channels) == 15 and not h.windowed
    req = h.list_request("1:1", TODAY, 3)
    assert req.method == "POST" and ("currentPage", "3") in req.form and ("bulletinNature", "1") in req.form

    page = h.parse_list("1:1", response("anhui_ggzy_list_first.html", "text/html"))
    assert len(page.refs) == 10 and page.has_next
    first = page.refs[0]
    assert first.source_key == "15a09741-1789-4d86-96e2-b67972e7f5a8:1"
    assert first.published == date(2026, 11, 11) and first.title.endswith("EPC公开招标公告")
    assert first.source_url.startswith("https://ggzy.ah.gov.cn/jsgc/newDetail?guid=15a09741")

    (fetch,) = h.detail_fetches("1:1", first)
    assert fetch.artifact_key == "15a09741-1789-4d86-96e2-b67972e7f5a8/tender"
    assert dict(fetch.request.form) == {"type": "tender", "bulletinNature": "1",
                                        "guid": "15a09741-1789-4d86-96e2-b67972e7f5a8", "statusGuid": ""}
    h.check_detail(response("anhui_ggzy_detail_tender.html", "text/html"))
    with pytest.raises(ValueError):
        h.check_detail(response("anhui_ggzy_detail_empty.html", "text/html"))


def test_anhui_ggzy_last_page_has_no_next():
    page = AnhuiGgzyConstruction().parse_list("1:1", response("anhui_ggzy_list_last.html", "text/html"))
    assert page.refs and not page.has_next and page.refs[-1].published == date(2024, 1, 2)


def test_anhui_ggzy_only_discovers_attachments_inside_the_notice_container():
    handler = AnhuiGgzyConstruction()
    first = handler.parse_list("1:1", response("anhui_ggzy_list_first.html", "text/html")).refs[0]
    guid, nature = first.source_key.split(":")
    prefix = f"/fixture/{guid}/{nature}/"
    uri = "https://ggzy.ah.gov.cn/jsgc/newDetailSub"
    content = f"""
        <p class="article-title">Fixture notice</p>
        <div class="article-text-box-other"><a download href="{prefix}sidebar.pdf">Sidebar</a></div>
        <div class="article-text-box outer"><p><a download="spec.pdf" href="{prefix}spec.pdf">Spec</a></p></div>
        <footer><a download href="{prefix}footer.pdf">Footer</a></footer>
    """
    body = FetchResponse(200, "text/html", content.encode(), uri)
    assert handler.attachment_fetches("1:1", first, body) == ()

    handler.attachment_path = re.compile(r"/fixture/(?P<guid>[a-f0-9-]+?)/(?P<nature>\d)/(?P<part_key>[^/]+)")
    attachment, = handler.attachment_fetches("1:1", first, body)
    assert attachment.artifact_key == "spec.pdf" and attachment.request.url.endswith(prefix + "spec.pdf")
    assert attachment.name == "spec.pdf"

    for invalid in (content.replace('class="article-text-box outer"', 'class="unknown"'),
                    content + '<div class="article-text-box"></div>'):
        with pytest.raises(ValueError, match="missing or ambiguous"):
            handler.attachment_fetches("1:1", first, replace(body, body=invalid.encode()))


def test_anhui_ggzy_rejects_unknown_page():
    with pytest.raises(ValueError):
        AnhuiGgzyConstruction().parse_list("1:1", FetchResponse(200, "text/html", b"<html>maintenance</html>", "u"))


def test_anhui_ccgp_list_and_detail():
    h = AnhuiGovernmentProcurement()
    req = h.list_request("ZcyAnnouncement2", TODAY, 2)
    assert json.loads(req.json_body) == {"categoryCode": "ZcyAnnouncement2", "pageNo": 2, "pageSize": 15}

    page = h.parse_list("ZcyAnnouncement2", response("anhui_ccgp_list.json", "application/json"))
    assert len(page.refs) == 15 and page.has_next
    ref = page.refs[0]
    assert ref.source_key == "ZW2UGlPzLzKA0ISW4BGnSA=="
    assert ref.published == date(2026, 11, 11)       # 1794383809000 ms is 2026-11-11 in China time
    assert "articleId=ZW2UGlPzLzKA0ISW4BGnSA%3D%3D" in ref.source_url

    (fetch,) = h.detail_fetches("ZcyAnnouncement2", ref)
    assert fetch.request.method == "GET" and fetch.artifact_key == ref.source_key
    h.check_detail(response("anhui_ccgp_detail.json", "application/json"))
    with pytest.raises(ValueError):
        h.check_detail(FetchResponse(200, "application/json", b'{"success":false,"result":null}', "u"))


def test_national_list_and_detail():
    h = NationalPublicResources()
    assert h.windowed and h.channels == ("2:01",)
    req = h.list_request("2:01", TODAY, 1)
    assert req.method == "POST" and req.url == "https://www.ggzy.gov.cn/information/pubTradingInfo/getTradList"
    form = dict(req.form)
    assert form["SOURCE_TYPE"] == "2" and form["TIMEBEGIN"] == form["TIMEEND"] == "2026-11-11"

    page = h.parse_list("2:01", response("national_list_central.json", "application/json"))
    assert len(page.refs) == 20 and page.has_next
    assert page.reported_page == 1 and page.coverage_gap == "publisher_result_cap"
    assert page.for_request(1, window=TODAY).has_next
    with pytest.raises(ValueError, match="does not match request"):
        page.for_request(2, window=TODAY)
    other_day = Window(date(2026, 11, 12), date(2026, 11, 12))
    with pytest.raises(ValueError, match="outside the request window"):
        page.for_request(1, window=other_day)
    ref = page.refs[0]
    assert ref.source_key == "006748b3c4518bdd487085c97e24650ba218"
    assert ref.source_url.startswith("https://www.ggzy.gov.cn/information/deal/html/a/0067/")

    (fetch,) = h.detail_fetches("2:01", ref)
    assert "/information/deal/html/b/0067/" in fetch.request.url
    h.check_detail(response("national_detail_body.html", "text/plain"))


@pytest.mark.parametrize(("body", "reason"), [
    ("<h4>示例公告</h4><div id='mycontent'><table><tr><th>示例字段</th><td>暂无</td></tr></table></div>",
     "national_detail_metadata_only"),
    ("<span>发布时间：2026-11-11</span><div id='mycontent'><table><tr><th>项目</th><td>示例</td></tr></table></div>",
     "national_detail_metadata_only"),
    ("<span>发布时间</span><div id='mycontent'><div class='detail_content'><script>window.shell=1</script></div></div>",
     "detail body missing"),
    ("<h4>示例公告</h4><p>暂无</p>", "detail body missing"),
])
def test_national_rejects_missing_body_with_specific_diagnostic(body, reason, caplog):
    handler = NationalPublicResources()
    with pytest.raises(ValueError, match=f"^{reason}$"):
        handler.check_detail(FetchResponse(200, "text/plain", body.encode(), "https://example.invalid/"))
    if reason == "national_detail_metadata_only":
        assert caplog.messages == ["source=national-public-resources reason=national_detail_metadata_only"]
    else:
        assert not caplog.messages


def test_national_captcha_blocks_source():
    body = json.dumps({"code": 829, "captchaToken": "t", "captchaImage": "AAAA"}).encode()
    with pytest.raises(SourceBlocked) as exc:
        NationalPublicResources().parse_list("2:01", FetchResponse(200, "application/json", body, "u"))
    assert exc.value.code == "captcha_challenge"


def _doc(html: str):
    from miaoqia.sdk import Document
    from miaoqia.sdk.testing import html_to_document
    return Document(html_to_document(html, "snp_" + "0" * 26, "https://example.invalid/"))


def test_anhui_ccgp_document_adapter_and_tender_fields():
    from miaoqia.sdk import ExtractionContext
    from miaoqia_pack_procurement.documents import anhui_ccgp_html
    from miaoqia_pack_procurement.extractors.tender_notice import TenderNoticeExtractor
    doc = _doc(anhui_ccgp_html((FIXTURES / "anhui_ccgp_detail.json").read_bytes()))
    ex = TenderNoticeExtractor()
    assert ex.applies(doc, {})
    (fact,) = ex.extract(doc, ExtractionContext(doc, f"{ex.id}@{ex.version}")).facts
    # The method label lists every option; the title decides it.
    assert fact["method"] == "consultation" and fact["title"]["value"].endswith("竞争性磋商公告[2026]")
    for bad in (b'{"success":false,"result":null}', b"\xff\xfe", b'{"success":true', b"[1]"):
        with pytest.raises(ValueError):
            anhui_ccgp_html(bad)


@pytest.mark.parametrize(("clause", "expected"), [
    ("本项目是否接受联合体：否", False), ("本项目是否接受联合体：是", True),
    ("5.本项目不接受联合体投标。", False), ("本次招标接受联合体投标。", True),
    ("本项目是否接受联合体：详见附件", None), ("是否接受联合体？", None),
    ("本项目接受联合体投标。</p><p>本项目不接受联合体投标。", None),
])
def test_consortium_clause_requires_an_unambiguous_answer(clause, expected):
    from miaoqia.sdk import ExtractionContext
    from miaoqia_pack_procurement.extractors.tender_notice import TenderNoticeExtractor
    doc = _doc(f"<h1>合成采购招标公告</h1><p>{clause}</p>")
    extractor = TenderNoticeExtractor()
    fact, = extractor.extract(doc, ExtractionContext(doc, f"{extractor.id}@{extractor.version}")).facts
    assert fact["lots"][0].get("consortium_allowed") is expected


def test_live_pages_select_the_right_extractor():
    from miaoqia_pack_procurement.extractors.result_notice import ResultNoticeExtractor
    from miaoqia_pack_procurement.extractors.tender_notice import TenderNoticeExtractor
    tender = _doc((FIXTURES / "anhui_ggzy_detail_tender.html").read_text(encoding="utf-8"))
    award = _doc((FIXTURES / "national_detail_body.html").read_text(encoding="utf-8"))
    assert TenderNoticeExtractor().applies(tender, {}) and not ResultNoticeExtractor().applies(tender, {})
    assert ResultNoticeExtractor().applies(award, {}) and not TenderNoticeExtractor().applies(award, {})


@pytest.mark.parametrize("title, tender", [
    ("某某工程EPC公开招标公告", True),
    ("某某项目竞争性磋商公告[2026]", True),
    ("某某项目招标公告（第2次）", True),
    ("某某项目中标结果公告", False),
    ("某某项目招标公告更正公告", False),
    ("某某项目采购意向公告", False),
])
def test_tender_full_title_matching(title, tender):
    from miaoqia_pack_procurement.extractors.tender_notice import TenderNoticeExtractor
    assert TenderNoticeExtractor().applies(_doc(f"<html><body><h1>{title}</h1><p>项目编号：A1</p></body></html>"), {}) is tender
