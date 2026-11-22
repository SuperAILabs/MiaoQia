"""Exercise source validation through the CLI with offline publisher responses."""

import json
from dataclasses import replace
from datetime import date, datetime, timezone
from hashlib import sha256
from pathlib import Path
from unittest.mock import Mock

import pytest

from miaoqia.acquisition.api.handlers import (AccessContract, DetailRef, FetchRequest, FetchResponse, GroupedNotice,
                                            GroupSelector, ListPage, NoticeGroup, OffsetPage)
from miaoqia.acquisition.application import sources
from miaoqia.acquisition.application.fetch import RunReport
from miaoqia.acquisition.infrastructure.transport import TransportError
from miaoqia.runtime_acquisition import fetch
from miaoqia_pack_procurement.sources.ccgp_html import HubeiGovernmentProcurement
from test_ccgp_sources import CENTRAL_LIST, HUBEI_HOME, HUBEI_PATH, page
from test_sources import response

PACKS = Path(__file__).resolve().parents[3]
HOMEPAGE = "hubei-government-procurement-homepage"
HOMEPAGE_CHANNEL = "homepage-tenders"
HOMEPAGE_HANDLER = "miaoqia_pack_procurement.sources.ccgp_html:HubeiGovernmentProcurement"
HOMEPAGE_KEY = "a" * 32
NATIONAL = "national-public-resources"
NATIONAL_CHANNEL = "2:01"
NATIONAL_LIST = "https://www.ggzy.gov.cn/information/pubTradingInfo/getTradList"
OBSERVED_AT = datetime(2026, 11, 19, 8, tzinfo=timezone.utc)
CCGP_DAY = "2026-11-19"


class FakeClock:
    def __init__(self):
        self.elapsed = 0.0

    def monotonic(self):
        return self.elapsed

    def sleep(self, seconds):
        self.elapsed += seconds

    def now(self):
        return OBSERVED_AT


@pytest.fixture(autouse=True)
def no_persistence(monkeypatch):
    monkeypatch.delenv("MIAOQIA_ACQUISITION_DB_DSN", raising=False)
    monkeypatch.delenv("MIAOQIA_ACQUISITION_OBJECT_STORE_DIR", raising=False)
    database = Mock(side_effect=AssertionError("unexpected acquisition database initialization"))
    object_store = Mock(side_effect=AssertionError("unexpected object store initialization"))
    spool = Mock(side_effect=AssertionError("unexpected archive spool initialization"))
    monkeypatch.setattr(fetch.db, "Database", database)
    monkeypatch.setattr(fetch, "open_object_store", object_store)
    monkeypatch.setattr(fetch.ArchiveSpool, "configured", spool)
    yield
    database.assert_not_called()
    object_store.assert_not_called()
    spool.assert_not_called()


@pytest.fixture(autouse=True)
def clock(monkeypatch):
    fake_clock = FakeClock()
    monkeypatch.setattr(fetch, "SystemClock", lambda: fake_clock)
    return fake_clock


@pytest.fixture
def transport_factory(monkeypatch):
    transport = Mock(spec=fetch.UrllibTransport)
    transport.send.side_effect = AssertionError("unexpected HTTP request")
    factory = Mock(return_value=transport)
    monkeypatch.setattr(fetch, "UrllibTransport", factory)
    return factory


@pytest.fixture
def homepage_responses():
    another_path = HUBEI_PATH.replace(HOMEPAGE_KEY, "b" * 32)
    listing = page(
        f'<div id="area-1-2"><ul><li><a href="{HUBEI_PATH}">合成采购公告</a><span>{CCGP_DAY}</span></li>'
        f'<li><a href="{another_path}">另一条合成采购公告</a><span>{CCGP_DAY}</span></li></ul></div>',
        HUBEI_HOME,
    )
    detail = page('<div class="con_info"><h2>合成采购公告</h2>'
                  '<div class="art_con"><p>项目编号：HB-TEST</p></div></div>',
                  HUBEI_HOME.rstrip("/") + HUBEI_PATH)
    return listing, detail


def run_validation(capsys, source_id, channel, *options, exit_code, outcome, admission, handler, pagination):
    assert fetch.main(["--validate-source", source_id, "--channel", channel, "--packs", str(PACKS), *options]) == exit_code
    captured = capsys.readouterr()
    report = json.loads(captured.out)
    assert set(report) == {
        "source_id", "admission", "handler", "channel", "since", "until", "observed_at", "outcome",
        "pagination", "pages", "details", "issues", "requests",
    }
    assert (report["source_id"], report["channel"], report["admission"], report["handler"]) == (
        source_id, channel, admission, handler,
    )
    assert (report["outcome"], report["pagination"]) == (outcome, pagination)
    assert datetime.fromisoformat(report["observed_at"]) == OBSERVED_AT
    return report


def assert_page(item, request, result, *, number, refs, has_next, coverage_gap=None):
    assert item == {
        "page": number,
        "request_fingerprint": request.fingerprint(),
        "response_sha256": "sha256:" + sha256(result.body).hexdigest(),
        "refs": refs,
        "keyed_refs": refs,
        "undated_refs": 0,
        "has_next": has_next,
        "coverage_gap": coverage_gap,
    }


class GroupProbe:
    source_id = "fixture-group-probe"
    channels = ("public",)
    windowed = True

    def list_request(self, channel, window, page):
        return FetchRequest("GET", f"https://source.example/groups?page={page}")

    def parse_list(self, channel, response):
        page = int(response.url[-1])
        group = NoticeGroup(f"section-{page}", FetchRequest("GET", f"https://source.example/group/{page}"))
        return ListPage((), False, reported_page=page, offset=OffsetPage(2, 1, 1), groups=(group,))

    def expand_group(self, channel, group, response):
        def notice(key, day):
            return GroupedNotice(DetailRef(key, group.request.url, date(2026, 11, day), "Fixture notice"),
                                 GroupSelector("fixture", 1, group.group_key, key), "sha256:" + "a" * 64)

        first = notice("notice-1", 19)
        return (first, first) if response.body == b"duplicate" else (first, notice("notice-2", 18))


@pytest.mark.parametrize("scenario", ["valid", "duplicate", "foreign", "blocked"])
def test_group_validation_checks_members_without_extra_fetches(monkeypatch, capsys, transport_factory, scenario):
    handler = GroupProbe()
    handler_ref = "fixture:GroupProbe"
    declared = sources.DeclaredSource(handler.source_id, handler_ref, handler, AccessContract(2, 30))
    entry = sources.CatalogSource(handler.source_id, "candidate", "public", declared, "staged")
    monkeypatch.setattr(sources, "for_validation", lambda *args: (entry, declared))
    expected_url = "https://source.example/group/1"
    body = b"duplicate" if scenario == "duplicate" else b"group original"
    group_response = FetchResponse(
        403 if scenario == "blocked" else 200, "application/json", body,
        "https://foreign.example/group/1" if scenario == "foreign" else expected_url,
    )

    def send(request, timeout):
        if request.url == expected_url:
            return group_response
        assert request.url in {"https://source.example/groups?page=1", "https://source.example/groups?page=2"}
        return FetchResponse(200, "application/json", b"list", request.url)

    transport_factory.return_value.send.side_effect = send
    status, outcome = {"valid": (0, "validated"), "duplicate": (1, "failed"), "foreign": (1, "failed"),
                       "blocked": (2, "blocked")}[scenario]
    report = run_validation(capsys, handler.source_id, "public", "--since", CCGP_DAY, "--until", CCGP_DAY,
                            "--max-pages", "2", "--max-details", "1", exit_code=status, outcome=outcome,
                            admission="staged", handler=handler_ref, pagination="advanced")
    assert [item["groups"] for item in report["pages"]] == [1, 1]
    assert len(report["requests"]) == 3 and report["requests"][-1]["phase"] == "group"
    if scenario == "valid":
        detail, = report["details"]
        assert detail["notice_count"] == 2 and detail["group_key"] == "section-1"
        assert detail["response_sha256"] == "sha256:" + sha256(body).hexdigest()
        member, = detail["notices"]
        assert member["source_key"] == "notice-1" and member["published"] == CCGP_DAY
        assert member["selector"]["group_key"] == "section-1"
    else:
        assert report["details"] == []
        issue, = report["issues"]
        assert issue["code"] == {"duplicate": "invalid_group_content", "foreign": "group_response_target_mismatch",
                                  "blocked": "http_403"}[scenario]


def assert_detail(item, request, result, *, source_key, published):
    assert item == {
        "page": 1,
        "source_key": source_key,
        "artifact_key": source_key,
        "published": published,
        "request_fingerprint": request.fingerprint(),
        "response_sha256": "sha256:" + sha256(result.body).hexdigest(),
        "media_type": result.media_type,
    }


@pytest.mark.parametrize("available_pages, exit_code, outcome, issues", [
    pytest.param(2, 0, "validated", [], id="terminal-page"),
    pytest.param(3, 3, "limited", [{"stage": "list", "code": "page_limit", "page": 3}], id="page-budget"),
])
def test_validation_defaults_advance_two_pages_and_sample_one_detail(
    capsys, clock, transport_factory, available_pages, exit_code, outcome, issues,
):
    day = "2026-11-11"
    recorded = response("national_list_central.json", "application/json", NATIONAL_LIST)
    rows = json.loads(recorded.body)["data"]["records"]
    first_row = rows[0]
    detail_url = "https://www.ggzy.gov.cn" + first_row["url"].replace("/html/a/", "/html/b/", 1)
    detail = response("national_detail_body.html", "text/plain", detail_url)
    total = 3 if available_pages == 2 else 5
    listings = {
        str(number): FetchResponse(
            200, recorded.media_type,
            json.dumps({"code": 200, "data": {"current": number, "pages": available_pages, "size": 2,
                                               "total": total, "records": records}}).encode(),
            NATIONAL_LIST,
        )
        for number, records in enumerate((rows[:2], rows[2:min(total, 4)]), 1)
    }
    sent_at = []

    def send(request, timeout):
        sent_at.append(clock.monotonic())
        if (request.method, request.url) == ("GET", detail_url):
            return detail
        assert (request.method, request.url) == ("POST", NATIONAL_LIST)
        form = dict(request.form)
        assert (form["SOURCE_TYPE"], form["DEAL_CLASSIFY"], form["TIMEBEGIN"], form["TIMEEND"]) == ("2", "01", day, day)
        return listings[form["PAGENUMBER"]]

    transport = transport_factory.return_value
    transport.send.side_effect = send
    report = run_validation(
        capsys, NATIONAL, NATIONAL_CHANNEL, "--until", day,
        exit_code=exit_code, outcome=outcome, admission="active",
        handler="miaoqia_pack_procurement.sources.national_ggzy:NationalPublicResources", pagination="advanced",
    )

    assert (report["since"], report["until"]) == (day, day)
    transport_factory.assert_called_once()
    requests = [call.args[0] for call in transport.send.call_args_list]
    first, second = [request for request in requests if request.method == "POST"]
    body, = [request for request in requests if request.method == "GET"]
    assert (dict(first.form)["PAGENUMBER"], dict(second.form)["PAGENUMBER"]) == ("1", "2")
    page_one, page_two = report["pages"]
    assert_page(page_one, first, listings["1"], number=1, refs=2, has_next=True)
    assert_page(page_two, second, listings["2"], number=2, refs=min(total, 4) - 2, has_next=available_pages > 2)
    sample, = report["details"]
    assert_detail(sample, body, detail, source_key=first_row["id"], published=day)
    interval = sources.catalog(PACKS)[NATIONAL].declaration.access.min_interval_seconds
    assert sent_at == [0, interval, interval * 2]
    assert report["issues"] == issues
    assert [(attempt["stage"], attempt["page"], attempt["status"]) for attempt in report["requests"]] == [
        ("list", 1, 200), ("list", 2, 200), ("detail", 1, 200),
    ]


def test_active_homepage_is_limited_even_with_a_valid_body(capsys, transport_factory, homepage_responses):
    listing, detail = homepage_responses
    transport = transport_factory.return_value
    transport.send.side_effect = [listing, detail]
    report = run_validation(
        capsys, HOMEPAGE, HOMEPAGE_CHANNEL, "--since", "2026-11-18", "--until", CCGP_DAY,
        "--max-pages", "2", "--max-details", "1", exit_code=3, outcome="limited", admission="active",
        handler=HOMEPAGE_HANDLER, pagination="first_page_only",
    )

    assert (report["since"], report["until"]) == ("2026-11-18", CCGP_DAY)
    transport_factory.assert_called_once()
    list_request, detail_request = [call.args[0] for call in transport.send.call_args_list]
    assert (list_request.method, list_request.url) == ("GET", listing.url)
    assert (detail_request.method, detail_request.url) == ("GET", detail.url)
    observed, = report["pages"]
    assert_page(observed, list_request, listing, number=1, refs=2, has_next=False, coverage_gap="homepage_current_window")
    sample, = report["details"]
    assert_detail(sample, detail_request, detail, source_key=HOMEPAGE_KEY, published=CCGP_DAY)
    assert report["issues"] == [{"stage": "list", "code": "homepage_current_window", "page": 1}]
    for attempt, request, evidence in zip(
        report["requests"], (list_request, detail_request), (observed, sample), strict=True,
    ):
        assert attempt["page"] == evidence["page"] and attempt["status"] == 200
        assert attempt["method"] == request.method
        assert attempt["request_fingerprint"] == request.fingerprint()
        assert attempt["response_sha256"] == evidence["response_sha256"]


def test_central_ccgp_advances_then_reports_unverified_terminal_page(capsys, transport_factory):
    def listing(key, url, next_link=""):
        return page(
            f'<ul class="c_list_bid"><li><a href="./202611/t20261119_{key}.htm">合成招标公告</a>'
            f'发布时间：<em>{CCGP_DAY}</em></li></ul>{next_link}',
            url,
        )

    first = listing("99990001", CENTRAL_LIST, '<a href="index_1.htm">下一页</a>')
    second = listing("99990002", CENTRAL_LIST + "index_1.htm")
    detail = page(
        '<div class="vF_detail_main"><div class="vF_detail_header"><h2>合成招标公告</h2></div>'
        '<div class="vF_detail_content"><p>项目编号：SYNTHETIC-01</p></div></div>',
        CENTRAL_LIST + "202611/t20261119_99990001.htm",
    )
    transport = transport_factory.return_value
    transport.send.side_effect = [first, second, detail]
    report = run_validation(
        capsys, "ccgp-central-public-tenders", "central-open-tenders", "--until", CCGP_DAY,
        exit_code=3, outcome="limited", admission="active", pagination="advanced",
        handler="miaoqia_pack_procurement.sources.ccgp_html:CentralPublicTenders",
    )

    requests = [call.args[0] for call in transport.send.call_args_list]
    assert [(request.method, request.url) for request in requests] == [
        ("GET", result.url) for result in (first, second, detail)
    ]
    page_one, page_two = report["pages"]
    assert_page(page_one, requests[0], first, number=1, refs=1, has_next=True)
    assert_page(page_two, requests[1], second, number=2, refs=1, has_next=False, coverage_gap="pagination_unverified")
    sample, = report["details"]
    assert_detail(sample, requests[2], detail, source_key="99990001", published=CCGP_DAY)
    assert report["issues"] == [{"stage": "list", "code": "pagination_unverified", "page": 2}]


@pytest.mark.parametrize("status, markup, exit_code, outcome, code", [
    pytest.param(200, "<title>访问验证</title>", 2, "blocked", "captcha_challenge", id="challenge"),
    pytest.param(200, "<div id='app'></div>", 1, "failed", "invalid_list", id="malformed-list"),
    pytest.param(403, "", 2, "blocked", "http_403", id="forbidden"),
    pytest.param(429, "", 2, "blocked", "throttled", id="rate-limited"),
    pytest.param(503, "", 1, "failed", "http_503", id="upstream-error"),
])
def test_list_failure_reports_issue_and_stops(capsys, transport_factory, status, markup, exit_code, outcome, code):
    transport = transport_factory.return_value
    transport.send.side_effect = [page(markup, HUBEI_HOME, status)]
    report = run_validation(
        capsys, HOMEPAGE, HOMEPAGE_CHANNEL, "--until", CCGP_DAY, exit_code=exit_code, outcome=outcome,
        admission="active", handler=HOMEPAGE_HANDLER, pagination="not_observed",
    )

    transport_factory.assert_called_once()
    transport.send.assert_called_once()
    assert report["pages"] == report["details"] == []
    assert report["issues"] == [{"stage": "list", "code": code, "page": 1}]
    assert [attempt["status"] for attempt in report["requests"]] == [status]


def test_validation_records_invalid_detail_without_claiming_a_sample(capsys, transport_factory, homepage_responses):
    listing, detail = homepage_responses
    invalid = page("<div id='app'></div>", detail.url)
    transport_factory.return_value.send.side_effect = [listing, invalid]
    report = run_validation(
        capsys, HOMEPAGE, HOMEPAGE_CHANNEL, "--until", CCGP_DAY, exit_code=1, outcome="failed",
        admission="active", handler=HOMEPAGE_HANDLER, pagination="first_page_only",
    )
    assert len(report["pages"]) == 1 and report["details"] == []
    assert report["issues"][-1] == {"stage": "detail", "code": "invalid_content", "page": 1}
    assert report["requests"][-1]["response_sha256"] == "sha256:" + sha256(invalid.body).hexdigest()


@pytest.mark.parametrize("second_status, outcome, exit_code", [(200, "failed", 1), (403, "blocked", 2)])
def test_validation_samples_another_record_after_invalid_body(capsys, transport_factory, homepage_responses,
                                                               second_status, outcome, exit_code):
    listing, detail = homepage_responses
    another_key = "b" * 32
    transport = transport_factory.return_value
    transport.send.side_effect = [
        listing, page("<div id='app'></div>", detail.url),
        replace(detail, url=detail.url.replace(HOMEPAGE_KEY, another_key), status=second_status),
    ]
    report = run_validation(
        capsys, HOMEPAGE, HOMEPAGE_CHANNEL, "--until", CCGP_DAY, "--max-details", "2",
        exit_code=exit_code, outcome=outcome, admission="active", handler=HOMEPAGE_HANDLER,
        pagination="first_page_only",
    )
    assert transport.send.call_count == 3
    codes = [issue["code"] for issue in report["issues"]]
    assert "invalid_content" in codes
    if second_status == 403:
        assert codes[-1] == "http_403" and report["details"] == []
    else:
        assert [item["source_key"] for item in report["details"]] == [another_key]


@pytest.mark.parametrize("first_status, code, outcome, exit_code", [
    (None, "network_error", "failed", 1),
    (503, "http_503", "failed", 1),
    (403, "http_403", "blocked", 2),
])
def test_validation_continues_after_transient_detail_errors(capsys, transport_factory, homepage_responses,
                                                            first_status, code, outcome, exit_code):
    listing, detail = homepage_responses
    another_key = "b" * 32
    first = TransportError("timeout") if first_status is None else replace(detail, status=first_status)
    transport = transport_factory.return_value
    transport.send.side_effect = [listing, first, replace(detail, url=detail.url.replace(HOMEPAGE_KEY, another_key))]
    report = run_validation(
        capsys, HOMEPAGE, HOMEPAGE_CHANNEL, "--until", CCGP_DAY, "--max-details", "2",
        exit_code=exit_code, outcome=outcome, admission="active", handler=HOMEPAGE_HANDLER,
        pagination="first_page_only",
    )
    assert {"stage": "detail", "code": code, "page": 1} in report["issues"]
    assert [item["source_key"] for item in report["details"]] == ([] if first_status == 403 else [another_key])


def test_validation_rejects_signed_detail_before_sending(capsys, monkeypatch, transport_factory, homepage_responses):
    listing, _ = homepage_responses
    detail_fetches = HubeiGovernmentProcurement.detail_fetches

    def signed_detail(handler, channel, ref):
        part, = detail_fetches(handler, channel, ref)
        request = replace(part.request, headers=(("Sign", "synthetic-signature"),))
        return (replace(part, request=request),)

    monkeypatch.setattr(HubeiGovernmentProcurement, "detail_fetches", signed_detail)
    transport = transport_factory.return_value
    transport.send.side_effect = [listing]
    report = run_validation(
        capsys, HOMEPAGE, HOMEPAGE_CHANNEL, "--until", CCGP_DAY, exit_code=3, outcome="limited",
        admission="active", handler=HOMEPAGE_HANDLER, pagination="first_page_only",
    )

    transport.send.assert_called_once()
    assert transport.send.call_args.args[0].url == listing.url
    assert report["details"] == []
    assert report["issues"][-1] == {"stage": "detail", "code": "non_public_request_header", "page": 1}
    assert "synthetic-signature" not in json.dumps(report)


@pytest.mark.parametrize("stage, location, code", [
    pytest.param("list", "/?token=synthetic", "credential_parameter_disallowed", id="credential-parameter"),
    pytest.param("list", "https://outside.invalid/", "redirect_disallowed", id="cross-origin-list"),
    pytest.param("detail", "/redirected-notice.html", "part_redirect_disallowed", id="pinned-detail"),
])
def test_validation_stops_before_restricted_redirect_hop(capsys, transport_factory, homepage_responses, stage, location, code):
    listing, detail = homepage_responses
    transport = transport_factory.return_value
    redirect_url = detail.url if stage == "detail" else listing.url
    redirect = FetchResponse(302, "text/html", b"", redirect_url, redirect_to=location)
    initial = [listing] if stage == "detail" else []
    transport.send.side_effect = [*initial, redirect]
    report = run_validation(
        capsys, HOMEPAGE, HOMEPAGE_CHANNEL, "--until", CCGP_DAY, exit_code=3, outcome="limited",
        admission="active", handler=HOMEPAGE_HANDLER,
        pagination="first_page_only" if initial else "not_observed",
    )

    assert report["issues"][-1] == {"stage": stage, "code": code, "page": 1}
    assert report["details"] == []
    assert [call.args[0].url for call in transport.send.call_args_list] == [
        result.url for result in (*initial, redirect)
    ]
    assert [attempt["status"] for attempt in report["requests"]] == [result.status for result in (*initial, redirect)]
    assert location not in json.dumps(report)


@pytest.mark.parametrize("body, exit_code, issue", [
    pytest.param('{"token":"synthetic"}', 3, "credential_parameter_disallowed", id="credential"),
    pytest.param('{"filter":{"Access_Token":"synthetic"}}', 3, "credential_parameter_disallowed", id="nested"),
    pytest.param('{"rules":[[{"client_secret":"synthetic"}]]}', 3, "credential_parameter_disallowed", id="array"),
    pytest.param('{"filter":{"token":null}}', 3, "credential_parameter_disallowed", id="unrecognized-token-type"),
    pytest.param('{"filter":{"token":"synthetic","token":""}}', 1, "invalid_json_request", id="duplicate-key"),
    pytest.param('{"filter":', 1, "invalid_json_request", id="malformed"),
    pytest.param('[{"token":"synthetic"}]', 1, "invalid_json_request", id="not-an-object"),
    pytest.param('{"filter":{"number":NaN}}', 1, "invalid_json_request", id="non-json-number"),
    pytest.param('{"filter":' * 13 + '1' + '}' * 13, 1, "invalid_json_request", id="depth-limit"),
    pytest.param('{"filter":{"rules":[{"token":"","city":"武汉"}]}}', 3, None, id="public-json"),
])
def test_validation_checks_json_body_before_sending(
    capsys, monkeypatch, transport_factory, homepage_responses, body, exit_code, issue,
):
    request = FetchRequest("POST", HUBEI_HOME, json_body=body)
    monkeypatch.setattr(HubeiGovernmentProcurement, "list_request", lambda *_: request)
    transport = transport_factory.return_value
    if issue is None:
        transport.send.side_effect = homepage_responses
    report = run_validation(
        capsys, HOMEPAGE, HOMEPAGE_CHANNEL, "--until", CCGP_DAY, exit_code=exit_code,
        outcome="failed" if exit_code == 1 else "limited", admission="active", handler=HOMEPAGE_HANDLER,
        pagination="first_page_only" if issue is None else "not_observed",
    )
    if issue is None:
        assert transport.send.call_args_list[0].args[0].json_body == body
        assert len(report["details"]) == 1
    else:
        transport.send.assert_not_called()
        assert report["requests"] == []
        assert report["issues"] == [{"stage": "list", "code": issue, "page": 1}]
        assert "synthetic" not in json.dumps(report)


@pytest.mark.parametrize("source_id, reason", [
    ("ccgp-sichuan-ggxx", "handler_pending"),
    ("chongqing-government-procurement-homepage", "handler_pending"),
    ("chongqing-government-procurement-spa", "handler_pending"),
    ("sichuan-government-procurement-homepage", "handler_pending"),
    ("ymzec-trade-info", "authorization_required"),
])
def test_validation_rejects_unavailable_sources_before_transport(capsys, transport_factory, source_id, reason):
    with pytest.raises(SystemExit) as exc:
        fetch.main(["--validate-source", source_id, "--channel", "published", "--packs", str(PACKS)])
    assert exc.value.code == 2
    captured = capsys.readouterr()
    assert not captured.out and source_id in captured.err and reason in captured.err
    transport_factory.assert_not_called()


@pytest.mark.parametrize("options", [
    pytest.param([], id="missing-channel"),
    pytest.param(["--channel", HOMEPAGE_CHANNEL, "--refetch"], id="refetch"),
    pytest.param(["--channel", HOMEPAGE_CHANNEL, "--unblock"], id="unblock"),
])
def test_invalid_validation_options_exit_before_transport(capsys, transport_factory, options):
    with pytest.raises(SystemExit) as exc:
        fetch.main(["--validate-source", HOMEPAGE, "--packs", str(PACKS), *options])
    assert exc.value.code == 2
    captured = capsys.readouterr()
    assert not captured.out and "error:" in captured.err
    transport_factory.assert_not_called()


def test_unknown_channel_exits_before_transport(capsys, transport_factory):
    with pytest.raises(SystemExit) as exc:
        fetch.main(["--validate-source", HOMEPAGE, "--channel", "unknown", "--packs", str(PACKS)])
    assert exc.value.code == 2
    captured = capsys.readouterr()
    assert not captured.out and "unknown channel" in captured.err
    transport_factory.assert_not_called()


def test_list_sources_reports_channels_without_transport(capsys, transport_factory):
    assert fetch.main(["--list-sources", "--packs", str(PACKS)]) == 0
    catalog = {entry["source_id"]: entry for entry in json.loads(capsys.readouterr().out)}
    homepage = catalog[HOMEPAGE]
    assert homepage["admission"] == "active"
    assert homepage["channels"] == [HOMEPAGE_CHANNEL]
    assert homepage["handler"] == HOMEPAGE_HANDLER
    assert homepage["runtime"] is None
    transport_factory.assert_not_called()


def test_staged_source_remains_ineligible_for_collection(capsys, monkeypatch, tmp_path, transport_factory):
    monkeypatch.setenv("MIAOQIA_ACQUISITION_DB_DSN", "postgresql://dummy.invalid/acq")
    monkeypatch.setenv("MIAOQIA_ACQUISITION_OBJECT_STORE_DIR", str(tmp_path / "objects"))

    staged_source = "crecgec-home"
    with pytest.raises(SystemExit) as exc:
        fetch.main(["--source", staged_source, "--packs", str(PACKS)])
    assert exc.value.code == 2
    captured = capsys.readouterr()
    assert not captured.out and staged_source in captured.err
    transport_factory.assert_not_called()


@pytest.mark.parametrize("blocked, deferred, exit_code", [
    (None, None, 0),
    ("http_403", "source_busy", 2),
    (None, "archive_spool_full", 3),
])
def test_collection_exit_reflects_deferred_report(
    capsys, monkeypatch, tmp_path, transport_factory, blocked, deferred, exit_code,
):
    monkeypatch.setenv("MIAOQIA_ACQUISITION_DB_DSN", "postgresql://dummy.invalid/acq")
    monkeypatch.setenv("MIAOQIA_ACQUISITION_OBJECT_STORE_DIR", str(tmp_path / "objects"))
    monkeypatch.setenv("MIAOQIA_ACQUISITION_ARCHIVE_SPOOL_DIR", str(tmp_path / "spool"))
    database, objects, spool = Mock(), object(), object()
    monkeypatch.setattr(fetch.db, "Database", Mock(return_value=database))
    monkeypatch.setattr(fetch, "open_object_store", Mock(return_value=objects))
    spool_factory = Mock(return_value=spool)
    monkeypatch.setattr(fetch.ArchiveSpool, "configured", spool_factory)
    collector = Mock()
    collector.run.return_value = RunReport(NATIONAL, blocked=blocked, deferred=deferred)
    collector_factory = Mock(return_value=collector)
    monkeypatch.setattr(fetch, "Fetcher", collector_factory)

    assert fetch.main(["--source", NATIONAL, "--packs", str(PACKS)]) == exit_code

    report = json.loads(capsys.readouterr().out)
    assert (report["blocked"], report["deferred"]) == (blocked, deferred)
    assert spool_factory.call_args.args[0]("ARCHIVE_SPOOL_DIR") == str(tmp_path / "spool")
    collector_factory.assert_called_once_with(database, objects, transport_factory.return_value, spool=spool)
    assert collector.run.call_args.kwargs["max_pages"] == 1
    transport_factory.return_value.send.assert_not_called()
    database.close.assert_called_once_with()
