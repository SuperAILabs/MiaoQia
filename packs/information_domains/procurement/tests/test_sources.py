"""Source handlers against responses recorded during the D1b probe (no network)."""
import json
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
    form = dict(req.form)
    assert form["SOURCE_TYPE"] == "2" and form["TIMEBEGIN"] == form["TIMEEND"] == "2026-11-11"

    page = h.parse_list("2:01", response("national_list_central.json", "application/json"))
    assert len(page.refs) == 20 and page.has_next
    ref = page.refs[0]
    assert ref.source_key == "006748b3c4518bdd487085c97e24650ba218"
    assert ref.source_url.startswith("https://www.ggzy.gov.cn/information/deal/html/a/0067/")

    (fetch,) = h.detail_fetches("2:01", ref)
    assert "/information/deal/html/b/0067/" in fetch.request.url
    h.check_detail(response("national_detail_body.html", "text/plain"))


def test_national_captcha_blocks_source():
    body = json.dumps({"code": 829, "captchaToken": "t", "captchaImage": "AAAA"}).encode()
    with pytest.raises(SourceBlocked) as exc:
        NationalPublicResources().parse_list("2:01", FetchResponse(200, "application/json", body, "u"))
    assert exc.value.code == "captcha_challenge"
