"""Browser listing discovery through Fetcher, PostgreSQL and the local archive."""
import json
import time
from dataclasses import replace
from datetime import timedelta
from hashlib import sha256

import pytest
from sqlalchemy import text

from miaoqia.acquisition.api.browser_pairs import BROWSER_COMPLETED, read_browser_pair
from miaoqia.acquisition.api.handlers import BrowserListPlan, BrowserSelection, FetchRequest, FetchResponse, OffsetPage
from miaoqia.acquisition.application import discovery
from miaoqia.acquisition.application.fetch import Fetcher, SourceStopped
from miaoqia.acquisition.infrastructure.archive_spool import ArchiveSpool
from miaoqia.acquisition.infrastructure.browser_capture import BrowserCapture
from miaoqia.kernel import reqctx

from test_browser_jobs import ACTIVE, KEY, ORIGINAL, WINDOW, BrowserSource, Render
from test_fetch import DAY, LIST_URL, Handler, Script, env, events, rows

LIST_DATA_URL = "https://src.example/api/list"
LIST_BYTES = f'{{ "refs": ["{KEY}"], "next": false, "page": 1 }}\r\n'.encode()
OFFSET_BYTES = LIST_BYTES.replace(b'"page": 1', b'"offset": {"total_records": 1, "page_size": 10, "row_count": 1}')
LIST_HTML = f'<section id="results" data-page="1"><a data-id="{KEY}">公告</a></section>'
NOTICE_URL = f"https://src.example/d/{KEY}"
NOTICE_DATA_URL = f"https://src.example/body/{KEY}"
REFS = [{"notice_key": KEY, "source_url": NOTICE_URL, "published_on": DAY.isoformat()}]


class ListingSource:
    source_id = BrowserSource.source_id
    channels = BrowserSource.channels
    windowed = BrowserSource.windowed
    ordered_by_publication = BrowserSource.ordered_by_publication
    contract_version = BrowserSource.contract_version
    browser_detail = BrowserSource.browser_detail
    validate_browser_notice = BrowserSource.validate_browser_notice

    def __init__(self, *, ref_changes=None):
        self.ref_changes = ref_changes
        self.parsed = []

    def browser_list(self, channel, window, page):
        return BrowserListPlan(f"{LIST_URL}?page={page}", FetchRequest("GET", f"{LIST_DATA_URL}?page={page}"),
                               "#results")

    def parse_browser_list(self, channel, original, selection):
        self.parsed.append((original.body, selection.html))
        listing = Handler().parse_list(channel, original)
        payload = json.loads(original.body)
        page = payload.get("page")
        if page is not None and f'data-page="{page}"' not in selection.html:
            raise ValueError("fixture response page does not match the visible listing")
        if any(f'data-id="{key}"' not in selection.html for key in payload["refs"]):
            raise ValueError("fixture original does not match the visible listing")
        if self.ref_changes:
            listing = replace(listing, refs=(replace(listing.refs[0], **self.ref_changes),))
        if "offset" in payload:
            return replace(listing, offset=OffsetPage(**payload["offset"]))
        return replace(listing, reported_page=page)


class ListingRender(Render):
    def __init__(self, *, fault=None, status=200, body=LIST_BYTES, html=LIST_HTML):
        super().__init__()
        self.fault = fault
        self.status = status
        self.body = body
        self.html = html
        self.list_calls = []

    def render(self, plan, permit, *, timeout):
        if not isinstance(plan, BrowserListPlan):
            return super().render(plan, permit, timeout=timeout)
        self.list_calls.append(plan)
        with permit(FetchRequest("GET", plan.navigation_url)) as finish:
            finish(200, None)
        request = plan.body_request
        wrong_url = request.url.replace("page=1", "page=9")
        if self.fault == "permit_request":
            request = replace(request, url=wrong_url)
        with permit(request) as finish:
            finish(self.status, "90" if self.status == 429 else None)
        response_url = wrong_url if self.fault == "capture_request" else request.url
        response = FetchResponse(self.status, "application/json", self.body, response_url)
        capture = BrowserCapture(response, "GET", "list-request", "list-frame", "list-loader", "192.0.2.1", 443)
        selection = BrowserSelection(self.html, "list-navigation", "list-frame",
                                     "stale-loader" if self.fault == "loader" else "list-loader")
        return capture, selection


@pytest.fixture
def browser_fetcher(env, tmp_path):
    database, store, clock = env
    spool_root = tmp_path / "spool"
    http = Script({})

    def make(renderer):
        return Fetcher(database, store, http, clock=clock, spool=ArchiveSpool(spool_root), browser=renderer)

    yield make
    assert http.sent == []


def source(handler):
    return replace(ACTIVE, handler=handler, access=replace(ACTIVE.access, list_retention_days=2))


def attempts(database):
    return rows(database, """
        SELECT a.id, a.method, a.url, a.status, a.outcome, a.fingerprint, d.task_key
        FROM acq_fetch_attempts a JOIN acq_download_starts d ON d.id = a.download_id ORDER BY a.id
    """)


def assert_no_discovery(database, renderer, report):
    assert (report.pages, report.listed, report.archived) == (0, 0, 0)
    assert renderer.calls == 0
    assert rows(database, "SELECT count(*) FROM acq_discovery_pages") == [(0,)]
    assert not events(database, BROWSER_COMPLETED)


@pytest.mark.parametrize("body", [LIST_BYTES, OFFSET_BYTES], ids=["reported", "offset"])
def test_browser_discovery_binds_list_attempt_and_expires_only_original(env, browser_fetcher, body):
    database, store, clock = env
    handler = ListingSource()
    renderer = ListingRender(body=body)
    report = browser_fetcher(renderer).run(source(handler), WINDOW, max_pages=1)
    assert (report.pages, report.listed, report.archived, report.failed, report.gaps) == (1, 1, 1, 0, [])
    assert renderer.calls == 1 and handler.parsed == [(body, LIST_HTML)]
    (plan,) = renderer.list_calls
    assert (plan.navigation_url, plan.body_request.url, plan.body_selector) == (
        f"{LIST_URL}?page=1", f"{LIST_DATA_URL}?page=1", "#results",
    )
    ledger = attempts(database)
    assert [attempt[1:5] for attempt in ledger] == [
        ("GET", url, 200, "ok") for url in (LIST_URL, LIST_DATA_URL, NOTICE_URL, NOTICE_DATA_URL)
    ]
    assert [attempt[5] for attempt in ledger] == [
        request.fingerprint() for request in (
            FetchRequest("GET", plan.navigation_url), plan.body_request,
            FetchRequest("GET", NOTICE_URL), FetchRequest("GET", NOTICE_DATA_URL),
        )
    ]
    assert clock.sleeps == [2, 2, 2]
    list_nav, list_attempt, body_nav, body_attempt = ledger
    assert list_nav[6] == list_attempt[6] and body_nav[6] == body_attempt[6]
    assert list_attempt[6] != body_attempt[6]
    (receipt,) = rows(database, """
        SELECT attempt_id, run_id, channel, window_start, window_end, page, refs, has_next,
               response_hash, object_ref, expires_at, browser_binding
        FROM acq_discovery_pages
    """)
    attempt, run_id, channel, start, end, page, refs, has_next, digest, key, expires_at, binding = receipt
    assert (attempt, run_id, channel, start, end, page, refs, has_next) == (
        list_attempt[0], report.run_id, "all", DAY, DAY, 1, REFS, False,
    )
    assert key.startswith("discovery/") and store.get(key) == body
    assert digest == f"sha256:{sha256(body).hexdigest()}"
    assert expires_at == clock.now() + timedelta(days=2)
    assert binding == {
        "version": 1, "task_key": list_attempt[6], "navigation_attempt_id": list_nav[0],
        "request_fingerprint": list_attempt[5], "request_id": "list-request", "navigation_id": "list-navigation",
        "frame_id": "list-frame", "loader_id": "list-loader", "media_type": "application/json",
        "size_bytes": len(body), "selection_hash": f"sha256:{sha256(LIST_HTML.encode()).hexdigest()}",
    }
    (event,) = events(database, BROWSER_COMPLETED)
    assert event["data"]["binding"]["list_attempt_id"] == attempt
    assert event["data"]["binding"]["attempt_id"] == body_attempt[0]

    with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
        conn.execute(text("UPDATE acq_discovery_pages SET expires_at = now() - interval '1 second'"))
    assert discovery.expire(database, store, deadline=time.monotonic() + 5) == 1
    with pytest.raises(FileNotFoundError):
        store.get(key)
    assert rows(database, """
        SELECT attempt_id, response_hash, refs, browser_binding, expired_at IS NOT NULL FROM acq_discovery_pages
    """) == [(attempt, digest, refs, binding, True)]
    assert read_browser_pair(store, event["data"])[0] == ORIGINAL


@pytest.mark.parametrize("fault, expected_urls", [
    ("loader", [LIST_URL, LIST_DATA_URL]),
    ("capture_request", [LIST_URL, LIST_DATA_URL]),
    ("permit_request", [LIST_URL]),
])
def test_browser_discovery_rejects_inconsistent_capture(env, browser_fetcher, fault, expected_urls):
    database, store, _ = env
    handler = ListingSource()
    renderer = ListingRender(fault=fault)
    report = browser_fetcher(renderer).run(source(handler), WINDOW, max_pages=1)
    assert_no_discovery(database, renderer, report)
    assert report.failed == 1 and report.gaps[0]["reason"] == "invalid_browser_list" and report.blocked is None
    assert len(renderer.list_calls) == 1 and handler.parsed == []
    assert [attempt[2] for attempt in attempts(database)] == expected_urls
    assert not (store.root / "discovery").exists()


@pytest.mark.parametrize("ref_changes", [
    {"source_key": None},
    {"source_key": f"/d/{KEY}"},
    {"published": None},
    {"source_url": NOTICE_URL.replace("https://", "http://")},
    {"source_url": f"https://other.example/d/{KEY}"},
], ids=["missing-key", "path-as-key", "missing-date", "http", "foreign-origin"])
def test_browser_discovery_rejects_invalid_projected_refs(env, browser_fetcher, ref_changes):
    database, _, _ = env
    handler = ListingSource(ref_changes=ref_changes)
    renderer = ListingRender()
    report = browser_fetcher(renderer).run(source(handler), WINDOW, max_pages=1)
    assert_no_discovery(database, renderer, report)
    assert report.failed == 1 and report.gaps[0]["reason"] == "invalid_browser_list"
    assert handler.parsed == [(LIST_BYTES, LIST_HTML)]


def test_browser_discovery_rejects_duplicate_publisher_key(env, browser_fetcher):
    database, _, _ = env
    renderer = ListingRender(body=LIST_BYTES.replace(f'"{KEY}"'.encode(), f'"{KEY}", "{KEY}"'.encode()))
    report = browser_fetcher(renderer).run(source(ListingSource()), WINDOW, max_pages=1)
    assert_no_discovery(database, renderer, report)
    assert report.gaps[0]["reason"] == "invalid_browser_list"


def test_browser_notice_cannot_use_a_different_list_task_binding(env, browser_fetcher):
    database, _, _ = env

    class MismatchedReceipt(ListingRender):
        def render(self, plan, permit, *, timeout):
            if not isinstance(plan, BrowserListPlan):
                with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
                    conn.execute(text("""
                        UPDATE acq_discovery_pages
                        SET browser_binding = jsonb_set(browser_binding, '{task_key}', to_jsonb(repeat('0', 64)))
                    """))
            return super().render(plan, permit, timeout=timeout)

    report = browser_fetcher(MismatchedReceipt()).run(source(ListingSource()), WINDOW, max_pages=1)
    assert (report.pages, report.listed, report.archived, report.failed) == (1, 1, 0, 1)
    assert report.gaps[0]["reason"] == "browser_observation_conflict"
    assert not events(database, BROWSER_COMPLETED)


@pytest.mark.parametrize("body, html", [
    (LIST_BYTES.replace(b', "page": 1', b""), LIST_HTML),
    (LIST_BYTES.replace(b'"page": 1', b'"page": 2'), LIST_HTML.replace('data-page="1"', 'data-page="2"')),
    (OFFSET_BYTES.replace(b'"row_count": 1', b'"row_count": 0'), LIST_HTML),
], ids=["missing", "wrong-page", "wrong-offset-count"])
def test_browser_discovery_requires_publisher_page_attestation(env, browser_fetcher, body, html):
    database, _, _ = env
    handler = ListingSource()
    renderer = ListingRender(body=body, html=html)
    report = browser_fetcher(renderer).run(source(handler), WINDOW, max_pages=1)
    assert_no_discovery(database, renderer, report)
    assert report.failed == 1 and report.gaps[0]["reason"] == "invalid_browser_list"
    assert handler.parsed == [(body, html)]


@pytest.mark.parametrize("status, body, outcome, reason", [
    (429, LIST_BYTES, "throttled", "throttled"),
    (403, LIST_BYTES, "blocked", "http_403"),
    (200, b'{"captcha": true}', "ok", "captcha_challenge"),
], ids=["throttled", "forbidden", "publisher-challenge"])
def test_browser_discovery_stops_on_throttle_or_block(env, browser_fetcher, status, body, outcome, reason):
    database, _, _ = env
    handler = ListingSource()
    active = source(handler)
    renderer = ListingRender(status=status, body=body)
    fetcher = browser_fetcher(renderer)
    report = fetcher.run(active, WINDOW, max_pages=1)
    assert_no_discovery(database, renderer, report)
    assert handler.parsed == ([(body, LIST_HTML)] if status == 200 else [])
    assert [attempt[1:5] for attempt in attempts(database)] == [
        ("GET", LIST_URL, 200, "ok"), ("GET", LIST_DATA_URL, status, outcome),
    ]
    assert reason in {gap["reason"] for gap in report.gaps}
    if status == 429:
        assert report.failures[-1]["retryable"]
        assert rows(database, """
            SELECT EXTRACT(EPOCH FROM ready_at - sent_at) FROM acq_download_pacing WHERE scope = 'site/src.example'
        """) == [(90,)]
    else:
        assert report.blocked == reason
        assert rows(database, "SELECT state, state_reason FROM acq_sources") == [("blocked", reason)]
        with pytest.raises(SourceStopped):
            fetcher.run(active, WINDOW, max_pages=1)
        assert len(renderer.list_calls) == 1


@pytest.mark.parametrize("operation", ["put", "get"])
def test_browser_discovery_resumes_exact_staged_list_after_object_outage(env, browser_fetcher, monkeypatch, operation):
    database, store, _ = env
    active = source(ListingSource())
    first = ListingRender()
    later = ListingRender(body=LIST_BYTES.replace(KEY.encode(), b"new"), html=LIST_HTML.replace(KEY, "new"))
    access_object = getattr(store, operation)

    def unavailable(key, *args, **kwargs):
        if key.startswith("discovery/"):
            raise OSError("fixture discovery archive unavailable")
        return access_object(key, *args, **kwargs)

    with monkeypatch.context() as outage:
        outage.setattr(store, operation, unavailable)
        interrupted = browser_fetcher(first).run(active, WINDOW, max_pages=1)
        assert interrupted.deferred == "list_archive_pending"
        assert_no_discovery(database, first, interrupted)
        initial_attempts = attempts(database)
        assert [attempt[2] for attempt in initial_attempts] == [LIST_URL, LIST_DATA_URL]
        assert rows(database, "SELECT kind, state, last_attempt FROM acq_request_tasks") == [
            ("list", "staged", initial_attempts[1][0]),
        ]
        retried = browser_fetcher(later).run(active, WINDOW, max_pages=1, resume_id=interrupted.run_id)
        assert retried.run_id == interrupted.run_id and retried.deferred == "list_archive_pending"
        assert_no_discovery(database, later, retried)
        assert later.list_calls == [] and attempts(database) == initial_attempts

    recovered = browser_fetcher(later).run(active, WINDOW, max_pages=1, resume_id=interrupted.run_id)
    assert recovered.run_id == interrupted.run_id and recovered.deferred is None
    assert (recovered.pages, recovered.listed, recovered.archived, recovered.failed) == (1, 1, 1, 0)
    assert [gap["reason"] for gap in recovered.gaps] == ["recovered_list_requires_rescan"]
    assert len(first.list_calls) == 1 and later.list_calls == [] and later.calls == 1
    ledger = attempts(database)
    assert ledger[:2] == initial_attempts and [attempt[2] for attempt in ledger[2:]] == [NOTICE_URL, NOTICE_DATA_URL]
    ((attempt_id, key, refs, binding),) = rows(database, """
        SELECT attempt_id, object_ref, refs, browser_binding FROM acq_discovery_pages
    """)
    assert attempt_id == initial_attempts[1][0] and refs == REFS
    assert binding["navigation_id"] == "list-navigation"
    assert rows(database, "SELECT state, last_attempt FROM acq_request_tasks WHERE kind = 'list'") == [
        ("settled", attempt_id),
    ]
    assert store.get(key) == LIST_BYTES
    (event,) = events(database, BROWSER_COMPLETED)
    assert event["data"]["binding"]["list_attempt_id"] == attempt_id
