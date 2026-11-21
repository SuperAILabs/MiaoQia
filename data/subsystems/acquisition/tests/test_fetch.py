"""Fetch runtime against a real PostgreSQL with a scripted transport and a fake clock (no network)."""
from __future__ import annotations

import dataclasses
import hashlib
import json
import os
import select
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from contextlib import ExitStack
from threading import Event, Thread
from datetime import date, datetime, timedelta, timezone
from pathlib import Path

import pytest
from sqlalchemy import text

from miaoqia.acquisition import MIGRATIONS
from miaoqia.acquisition.api.handlers import (AccessContract, DetailFetch, DetailRef, FetchRequest, FetchResponse,
                                              ListPage, ListRequestPlan, OffsetPage, SourceBlocked, Window)
from miaoqia.acquisition.application import discovery, dispatch, plans, source_validation, sources
from miaoqia.acquisition.application.fetch import COMPLETED, Fetcher, SourceStopped
from miaoqia.acquisition.infrastructure.archive_spool import ArchiveSpool
from miaoqia.acquisition.infrastructure.download_slots import DownloadSlot, DownloadSlots
from miaoqia.acquisition.infrastructure.transport import TransportError
from miaoqia.kernel import db, reqctx
from miaoqia.kernel.objectstore import FilesystemObjectStore

REPO = Path(__file__).resolve().parents[4]
DAY = date(2026, 11, 11)
LIST_URL = "https://src.example/list"


class FakeClock:
    def __init__(self):
        self.t = 1000.0
        self.sleeps: list[float] = []

    def monotonic(self):
        return self.t

    def sleep(self, seconds):
        self.sleeps.append(seconds)
        self.t += seconds

    def now(self):
        return datetime(2026, 11, 11, 8, 0, tzinfo=timezone.utc)


class Script:
    """Answers by URL from queued responses; an exception instance is raised instead of returned."""

    def __init__(self, routes):
        self.routes = {k: list(v) for k, v in routes.items()}
        self.sent: list[FetchRequest] = []

    def send(self, request, timeout):
        self.sent.append(request)
        item = self.routes[request.url].pop(0)
        if isinstance(item, Exception):
            raise item
        return item


class StreamingScript(Script):
    def send_capture(self, request, timeout, write):
        response = super().send(request, timeout)
        if response.status == 200:
            for start in range(0, len(response.body), 4):
                write(response.body[start:start + 4])
        return dataclasses.replace(response, body=b"")


def ok(body: bytes, media="text/html") -> FetchResponse:
    return FetchResponse(200, media, body, "u")


def listing(*keys: str, has_next=False) -> FetchResponse:
    return ok(json.dumps({"refs": list(keys), "next": has_next}).encode(), "application/json")


class Handler:
    """Minimal list → detail source: list JSON names detail keys, one artifact per key."""

    source_id = "test-source"
    channels = ("all",)
    windowed = False
    ordered_by_publication = True

    def list_request(self, channel, window, page):
        return FetchRequest("GET", f"{LIST_URL}?page={page}" if page > 1 else LIST_URL)

    def parse_list(self, channel, response):
        body = json.loads(response.body)
        if body.get("captcha"):
            raise SourceBlocked("captcha_challenge")
        refs = []
        for key in body["refs"]:
            name, _, day = key.partition("@")
            refs.append(DetailRef(name, f"https://src.example/d/{name}", date.fromisoformat(day) if day else DAY))
        return ListPage(tuple(refs), body["next"], ordered_by_publication=self.ordered_by_publication)

    def detail_fetches(self, channel, ref):
        return (DetailFetch(ref.source_key, FetchRequest("GET", ref.source_url)),)

    def check_detail(self, response):
        if b"empty" in response.body:
            raise ValueError("empty")


SOURCE = sources.DeclaredSource("test-source", "tests:Handler", Handler(),
                                AccessContract(min_interval_seconds=2, throttle_backoff_seconds=60, max_attempts=3))


@pytest.fixture
def env(pg_factory, tmp_path):
    dsns = pg_factory()
    db.migrate(dsns["owner"], MIGRATIONS)
    database = db.Database(dsns["app"])
    clock = FakeClock()
    yield database, FilesystemObjectStore(tmp_path / "objects"), clock
    database.close()


def fetcher(env, routes):
    database, store, clock = env
    transport = Script(routes)
    return Fetcher(database, store, transport, clock=clock), transport


def rows(database, sql):
    from miaoqia.kernel import reqctx
    with reqctx.use(reqctx.service("acquisition")), database.tx() as c:
        return [tuple(r) for r in c.execute(text(sql))]


def events(database, kind):
    return [e for (e,) in rows(database, "SELECT envelope FROM kernel_outbox ORDER BY seq") if e["type"] == kind]


def test_archives_details_and_emits_completed(env):
    f, transport = fetcher(env, {LIST_URL: [listing("a", "b")],
                                 "https://src.example/d/a": [ok(b"<html>A</html>")],
                                 "https://src.example/d/b": [ok(b"<html>B</html>")]})
    report = f.run(SOURCE, Window(DAY, DAY), max_pages=5)
    assert (report.listed, report.archived, report.failed) == (2, 2, 0)
    database, store, clock = env
    done = events(database, COMPLETED)
    assert [e["data"]["source_url"] for e in done] == ["https://src.example/d/a", "https://src.example/d/b"]
    data = done[0]["data"]
    assert data["rights_tags"] == ["public:test-source"] and data["artifact_ref"] == data["content_hash"].replace("sha256:", "raw/")
    assert store.get(data["artifact_ref"]) == b"<html>A</html>"
    assert rows(database, "SELECT count(*) FROM acq_fetch_attempts WHERE outcome = 'ok'") == [(3,)]
    # Requests to the same source are paced by the access contract.
    assert clock.sleeps == [2, 2]


def test_verified_detail_date_controls_archive_membership(env):
    class DatedDetails(Handler):
        def parse_list(self, channel, response):
            page = super().parse_list(channel, response)
            return dataclasses.replace(page, refs=tuple(
                dataclasses.replace(ref, published=None) if ref.source_key != "conflict" else ref for ref in page.refs
            ))

        def check_detail(self, response):
            return date.fromisoformat(response.body.decode())

    previous = DAY - timedelta(days=1)
    day, outside = ok(DAY.isoformat().encode()), ok(previous.isoformat().encode())
    collector, _ = fetcher(env, {LIST_URL: [listing("current", "outside", "conflict")],
                                 "https://src.example/d/current": [day],
                                 "https://src.example/d/outside": [outside],
                                 "https://src.example/d/conflict": [outside]})
    source = dataclasses.replace(SOURCE, handler=DatedDetails())
    report = collector.run(source, Window(DAY, DAY), max_pages=1)
    assert (report.archived, report.skipped, report.failed) == (1, 1, 1)
    assert rows(env[0], "SELECT notice_key, published_on FROM acq_source_observations") == [("current", DAY)]
    assert rows(env[0], "SELECT count(*) FROM acq_raw_artifacts") == [(1,)]
    assert len(events(env[0], COMPLETED)) == 1


def test_incremental_run_skips_archived_and_refetch_dedups_identical_content(env):
    routes = {LIST_URL: [listing("a"), listing("a"), listing("a")],
              "https://src.example/d/a": [ok(b"same"), ok(b"same")]}
    f, transport = fetcher(env, routes)
    f.run(SOURCE, Window(DAY, DAY), max_pages=1)
    second = f.run(SOURCE, Window(DAY, DAY), max_pages=1)
    assert (second.skipped, second.archived) == (1, 0)
    third = f.run(SOURCE, Window(DAY, DAY), max_pages=1, refetch=True)
    assert (third.unchanged, third.archived) == (1, 0)
    assert len(events(env[0], COMPLETED)) == 1
    assert rows(env[0], "SELECT count(*) FROM acq_raw_artifacts") == [(1,)]


def test_pagination_stops_at_window_start(env):
    f, transport = fetcher(env, {LIST_URL: [listing("new@2026-11-11", "old@2026-11-09", has_next=True)],
                                 "https://src.example/d/new": [ok(b"n")]})
    report = f.run(SOURCE, Window(date(2026, 11, 10), DAY), max_pages=5)
    assert (report.pages, report.listed, report.archived) == (1, 1, 1)


def test_changes_and_reversions_have_distinct_observations_and_reuse_bytes(env):
    database, store, _ = env
    f, _ = fetcher(env, {
        LIST_URL: [listing("a", "a")] * 4,
        "https://src.example/d/a": [ok(body) for body in (b"A", b"B", b"A", b"A")],
    })
    reports = [f.run(SOURCE, Window(DAY, DAY), max_pages=1, refetch=True) for _ in range(4)]
    assert [(r.archived, r.unchanged, r.skipped) for r in reports] == [(1, 0, 1)] * 3 + [(0, 1, 1)]
    data = [e["data"] for e in events(database, COMPLETED)]
    assert [(v["notice_key"], v["observation_version"], store.get(v["artifact_ref"])) for v in data] == [
        ("a", 1, b"A"), ("a", 2, b"B"), ("a", 3, b"A")]
    assert data[0]["artifact_ref"] == data[2]["artifact_ref"]
    assert rows(database, "SELECT count(*) FROM acq_raw_artifacts") == [(2,)]
    assert rows(database, "SELECT artifact_id FROM acq_source_observations ORDER BY version") == [(1,), (2,), (1,)]


def test_legacy_artifact_witness_uses_the_stable_part_key_even_when_the_url_changes(env):
    database, _, _ = env
    f, _ = fetcher(env, {LIST_URL: [listing("a")], "https://src.example/d/a": [ok(b"new")]})
    digest = hashlib.sha256(b"legacy").hexdigest()
    with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
        f.register(conn, SOURCE)
        conn.execute(text("""
            INSERT INTO acq_raw_artifacts (source_id, source_key, content_hash, artifact_ref, media_type, size_bytes,
                                          source_url, request_fingerprint, fetched_at)
            VALUES ('test-source', 'a', :hash, :ref, 'text/html', 6, 'https://src.example/old/a', 'old', now())
        """), {"hash": f"sha256:{digest}", "ref": f"raw/{digest}"})
    f.run(SOURCE, Window(DAY, DAY), max_pages=1)
    (event,) = events(database, COMPLETED)
    assert event["data"]["legacy_artifact"] == {"source_url": "https://src.example/old/a", "content_hash": f"sha256:{digest}"}


def test_missing_notice_key_archives_without_handing_off_a_fact(env):
    class UnknownIdentity(Handler):
        def parse_list(self, channel, response):
            page = super().parse_list(channel, response)
            return ListPage(tuple(dataclasses.replace(ref, source_key=None) for ref in page.refs), page.has_next)

        def detail_fetches(self, channel, ref):
            return (DetailFetch("body", FetchRequest("GET", ref.source_url)),)

    source = dataclasses.replace(SOURCE, handler=UnknownIdentity())
    collector, _ = fetcher(env, {LIST_URL: [listing("a")], "https://src.example/d/a": [ok(b"pending")]})
    report = collector.run(source, Window(DAY, DAY), max_pages=1)
    assert report.pending_identity == 1 and report.gaps[0]["reason"] == "identity_pending"
    assert not events(env[0], COMPLETED)
    (key,) = rows(env[0], "SELECT artifact_ref FROM acq_pending_identity JOIN acq_raw_artifacts ON id = artifact_id")[0]
    assert env[1].get(key) == b"pending"


def test_older_generation_cannot_rewind_an_already_committed_notice(env, monkeypatch):
    database, store, _ = env
    window = Window(DAY, DAY)
    url = "https://src.example/d/a"
    original_put = store.put

    def delayed_put(key, data):
        if data == b"older":
            newer, transport = fetcher(env, {LIST_URL: [listing("a")], url: [ok(b"newer")]})
            busy = newer.run(SOURCE, window, max_pages=1, refetch=True)
            assert busy.deferred == "source_busy" and transport.sent == []
            expire_lease(database)
            assert newer.run(SOURCE, window, max_pages=1, refetch=True).archived == 1
        original_put(key, data)

    monkeypatch.setattr(store, "put", delayed_put)
    collector, _ = fetcher(env, {LIST_URL: [listing("a")], url: [ok(b"older")]})
    report = collector.run(SOURCE, window, max_pages=1, refetch=True)
    assert report.truncated and report.gaps[-1]["reason"] == "lease_lost" and report.archived == 0
    (event,) = events(database, COMPLETED)
    assert store.get(event["subject"]) == b"newer"
    assert rows(database, "SELECT id, generation, report->>'archived' FROM acq_collection_runs") == [
        (report.run_id, 2, "1")]


def expire_lease(database):
    with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
        conn.execute(text("UPDATE acq_sources SET lease_until = clock_timestamp() WHERE active_run_id IS NOT NULL"))


def test_capture_expiry_preserves_page_receipt_and_detail(env):
    database, store, _ = env
    page = listing("a")
    collector, _ = fetcher(env, {LIST_URL: [page], "https://src.example/d/a": [ok(b"A")]})
    source = dataclasses.replace(SOURCE, access=dataclasses.replace(SOURCE.access, list_retention_days=2))
    report = collector.run(source, Window(DAY, DAY), max_pages=1)
    (capture,) = rows(database, """
        SELECT run_id, refs, response_hash, object_ref, expires_at - interval '2 days', expired_at
        FROM acq_discovery_pages
    """)
    run, refs, content_hash, key, created, expired = capture
    assert run == report.run_id and refs[0]["notice_key"] == "a"
    assert content_hash == f"sha256:{hashlib.sha256(page.body).hexdigest()}" and store.get(key) == page.body
    assert created == env[2].now() and expired is None
    with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
        conn.execute(text("UPDATE acq_discovery_pages SET expires_at = now() - interval '1 second'"))
    assert discovery.expire(database, store, deadline=time.monotonic() + 5) == 1
    with pytest.raises(FileNotFoundError):
        store.get(key)
    assert store.get(events(database, COMPLETED)[0]["subject"]) == b"A"
    assert rows(database, "SELECT response_hash, refs, expired_at IS NOT NULL FROM acq_discovery_pages") == [
        (content_hash, refs, True)]


def test_detail_write_failure_leaves_a_gap_then_archives_on_retry(env, monkeypatch):
    database, store, _ = env
    collector, _ = fetcher(env, {LIST_URL: [listing("a")] * 2, "https://src.example/d/a": [ok(b"A")] * 2})
    put = store.put

    def unavailable_for_raw(key, data):
        if key.startswith("raw/"):
            raise OSError("mock store offline")
        put(key, data)

    with monkeypatch.context() as broken:
        broken.setattr(store, "put", unavailable_for_raw)
        report = collector.run(SOURCE, Window(DAY, DAY), max_pages=1)
        assert report.failed == 1 and report.gaps[0]["reason"] == "archive_write"
        assert events(database, COMPLETED) == []
    assert collector.run(SOURCE, Window(DAY, DAY), max_pages=1).archived == 1
    assert events(database, COMPLETED)[0]["data"]["observation_version"] == 1


def test_throttle_backs_off_then_succeeds(env):
    f, transport = fetcher(env, {LIST_URL: [FetchResponse(429, "text/html", b"", "u"), listing("a")],
                                 "https://src.example/d/a": [ok(b"A")]})
    report = f.run(SOURCE, Window(DAY, DAY), max_pages=1)
    assert report.archived == 1
    assert 60 in env[2].sleeps
    assert rows(env[0], "SELECT outcome FROM acq_fetch_attempts ORDER BY id") == [("throttled",), ("ok",), ("ok",)]


def test_exhausted_retries_emit_retryable_failure(env):
    f, transport = fetcher(env, {LIST_URL: [listing("a")],
                                 "https://src.example/d/a": [TransportError("timeout")] * 3})
    report = f.run(SOURCE, Window(DAY, DAY), max_pages=1)
    assert (report.archived, report.failed) == (0, 1)
    (failed,) = events(env[0], "acquisition.fetch.failed.v1")
    assert failed["data"] == {"source_id": "test-source", "source_url": "https://src.example/d/a",
                              "failed_at": "2026-11-11T08:00:00Z", "error_code": "network_error", "retryable": True}


def test_invalid_detail_is_not_archived(env):
    f, transport = fetcher(env, {LIST_URL: [listing("a")], "https://src.example/d/a": [ok(b"empty")]})
    report = f.run(SOURCE, Window(DAY, DAY), max_pages=1)
    assert report.failures == [{"url": "https://src.example/d/a", "error_code": "invalid_content", "retryable": False}]
    assert rows(env[0], "SELECT count(*) FROM acq_raw_artifacts") == [(0,)]


@pytest.mark.parametrize("answer, code", [
    (ok(json.dumps({"captcha": True}).encode(), "application/json"), "captcha_challenge"),
    (FetchResponse(403, "text/html", b"", "u"), "http_403"),
])
def test_block_stops_source_until_unblocked(env, answer, code):
    f, transport = fetcher(env, {LIST_URL: [answer, listing()]})
    report = f.run(SOURCE, Window(DAY, DAY), max_pages=1)
    assert report.blocked == code
    assert rows(env[0], "SELECT state, state_reason FROM acq_sources") == [("blocked", code)]
    (failed,) = events(env[0], "acquisition.fetch.failed.v1")
    assert failed["data"]["error_code"] == code and failed["data"]["retryable"] is False
    with pytest.raises(SourceStopped):
        f.run(SOURCE, Window(DAY, DAY), max_pages=1)
    assert len(transport.sent) == 1
    resumed = f.run(SOURCE, Window(DAY, DAY), max_pages=1, unblock=True)
    assert resumed.blocked is None
    assert rows(env[0], "SELECT state FROM acq_sources") == [("active",)]


def test_attempts_and_artifacts_are_append_only(env):
    f, transport = fetcher(env, {LIST_URL: [listing("a")], "https://src.example/d/a": [ok(b"A")]})
    f.run(SOURCE, Window(DAY, DAY), max_pages=1)
    from miaoqia.kernel import reqctx
    for table in ("acq_raw_artifacts", "acq_fetch_attempts"):
        with pytest.raises(Exception, match="append-only"):
            with reqctx.use(reqctx.service("acquisition")), env[0].tx() as c:
                c.execute(text(f"DELETE FROM {table}"))


def test_repository_declarations_load():
    declared = sources.load(REPO / "packs")
    assert set(declared) == {
        "anhui-ggzy-construction", "anhui-government-procurement-spa", "national-public-resources",
        "bfjsjt-tender", "cncecyc-ywgg1gc", "crccep-bidding-mat", "chongqing-ggzy-trade-014",
        "hefei-ggzy-engineer", "luzhou-ggzy-tzgg",
        "cotenders-news", "hunan-government-procurement", "m2-bid-list",
        "ccgp-central-public-tenders", "hubei-government-procurement-homepage", "xuzhou-ggzy", "ygcgfwpt-transaction",
        "suzhou-ggzy-trade", "js-218-206-153-6-trade", "henan-government-procurement-home",
        "tibet-government-procurement-home", "hubei-ggzy-construction", "shanxi-prec", "xinjiang-ggzy-construction",
        "sichuan-ggzy-construction", "henan-ggzy-homepage-direct", "hunan-ggzy-construction",
        "jiangxi-ggzy-announcements", "shaanxi-ggzy-construction", "shaanxi-government-procurement",
        "scbid-zbxx", "jsjyztb-web-list",
    }
    assert declared["anhui-government-procurement-spa"].access.min_interval_seconds >= 10


def test_declaration_requires_verified_inventory_entry(tmp_path):
    pack = tmp_path / "information_domains/demo"
    (pack / "src/demo_pack").mkdir(parents=True)
    (pack / "src/demo_pack/__init__.py").write_text("")
    (pack / "src/demo_pack/h.py").write_text("class H:\n    source_id = 'demo'\n    channels = ('x',)\n    windowed = False\n")
    (pack / "pack.yaml").write_text("pack:\n  provides:\n    sources: sources.yaml\n")
    (pack / "source-inventory.yaml").write_text("sources:\n- source_id: demo\n  readiness: candidate\n")
    (pack / "sources.yaml").write_text("sources:\n- source_id: demo\n  handler: demo_pack.h:H\n"
                                       "  access: {min_interval_seconds: 2, throttle_backoff_seconds: 60}\n")
    with pytest.raises(ValueError, match="not capability_verified"):
        sources.load(tmp_path)


def health(database):
    return rows(database, "SELECT state, reason_code, last_success_at IS NOT NULL FROM acq_source_health")


def test_health_transitions_publish_changes_once(env):
    database = env[0]
    f, transport = fetcher(env, {
        LIST_URL: [listing("a"), listing("b"), listing("c"), FetchResponse(403, "text/html", b"", "u")],
        "https://src.example/d/a": [ok(b"A")],
        "https://src.example/d/b": [TransportError("timeout")] * 3,
        "https://src.example/d/c": [ok(b"C")],
    })
    assert f.run(SOURCE, Window(DAY, DAY), max_pages=1).health == "healthy"
    assert health(database) == [("healthy", "ok", True)]
    assert f.run(SOURCE, Window(DAY, DAY), max_pages=1).health == "degraded"   # list ok, detail failed
    assert f.run(SOURCE, Window(DAY, DAY), max_pages=1).health == "healthy"
    assert f.run(SOURCE, Window(DAY, DAY), max_pages=1).health == "unavailable"
    assert health(database) == [("unavailable", "http_403", True)]
    changes = [(e["data"]["before"], e["data"]["after"], e["data"]["reason_code"])
               for e in events(database, "acquisition.source.health_changed.v1")]
    assert changes == [("unknown", "healthy", "ok"), ("healthy", "degraded", "partial_failures"),
                       ("degraded", "healthy", "ok"), ("healthy", "unavailable", "http_403")]


def test_all_failed_run_is_unavailable_without_success(env):
    f, transport = fetcher(env, {LIST_URL: [FetchResponse(500, "text/html", b"", "u")] * 3})
    assert f.run(SOURCE, Window(DAY, DAY), max_pages=1).health == "unavailable"
    assert health(env[0]) == [("unavailable", "all_failed", False)]


def test_deadline_stops_between_requests(env):
    database, store, clock = env
    f, transport = fetcher(env, {LIST_URL: [listing("a", "b")]})
    report = f.run(SOURCE, Window(DAY, DAY), max_pages=5, deadline=clock.t + 1.5)
    assert report.truncated and report.archived == 0 and report.health == "degraded"
    assert [request.url for request in transport.sent] == [LIST_URL]
    assert rows(database, "SELECT run_id FROM acq_fetch_attempts") == [(report.run_id,)]
    assert rows(database, "SELECT id, report->'gaps' FROM acq_collection_runs WHERE finished_at IS NOT NULL") == [
        (report.run_id, report.gaps)
    ]


def test_interleaved_source_runs_have_independent_deadlines(env):
    database, store, clock = env
    other_url = "https://other.example/list"

    class OtherHandler(Handler):
        def list_request(self, channel, window, page):
            return FetchRequest("GET", other_url)

    other_source = dataclasses.replace(SOURCE, source_id="other-source", handler=OtherHandler())

    class Interleaved(Script):
        def send(self, request, timeout):
            if request.url == LIST_URL:
                self.sent.append(request)
                other = collector.run(other_source, Window(DAY, DAY), max_pages=1)
                assert other.health == "healthy"
                clock.sleep(3)
                return listing("a")
            return super().send(request, timeout)

    transport = Interleaved({other_url: [listing()]})
    collector = Fetcher(database, store, transport, clock=clock)
    report = collector.run(SOURCE, Window(DAY, DAY), max_pages=1, deadline=clock.t + 2)
    assert report.truncated and report.archived == report.pages == 0
    assert rows(database, "SELECT source_id, report->>'truncated' FROM acq_collection_runs ORDER BY id") == [
        ("test-source", "true"), ("other-source", "false")
    ]
    assert rows(database, "SELECT source_id FROM acq_fetch_attempts ORDER BY id") == [
        ("other-source",), ("test-source",)
    ]
    assert rows(database, "SELECT run_id FROM acq_discovery_pages") == [(2,)]


def test_page_limit_is_persisted_as_coverage_gap(env):
    collector, _ = fetcher(env, {LIST_URL: [listing("a", has_next=True)],
                                "https://src.example/d/a": [ok(b"A")]})
    report = collector.run(SOURCE, Window(DAY, DAY), max_pages=1)
    assert report.archived == 1 and report.health == "degraded"
    assert report.gaps == [{"channel": "all", "since": DAY.isoformat(), "until": DAY.isoformat(),
                             "page": 2, "reason": "page_limit"}]
    assert rows(env[0], "SELECT report->'gaps' FROM acq_collection_runs") == [(report.gaps,)]


def test_an_older_result_only_ends_a_verified_date_ordered_listing(env):
    class Unordered(Handler):
        ordered_by_publication = False

    old = "old@" + (DAY - timedelta(days=1)).isoformat()
    for handler, want_pages in ((Unordered(), 2), (Handler(), 1)):
        source = dataclasses.replace(SOURCE, handler=handler)
        collector, _ = fetcher(env, {LIST_URL: [listing(old, has_next=True)],
                                    f"{LIST_URL}?page=2": [listing("a")],
                                    "https://src.example/d/a": [ok(b"A")]})
        report = collector.run(source, Window(DAY, DAY), max_pages=2)
        assert report.pages == want_pages and not report.gaps
    assert rows(env[0], "SELECT url FROM acq_fetch_attempts WHERE url LIKE '%?%'") == []


def test_restricted_list_end_keeps_gap_only_when_date_window_extends_beyond_it(env):
    class RestrictedList(Handler):
        def parse_list(self, channel, response):
            listing = super().parse_list(channel, response)
            return dataclasses.replace(listing, coverage_gap="pagination_requires_captcha")

    boundary = DAY - timedelta(days=1)
    source = dataclasses.replace(SOURCE, handler=RestrictedList())
    collector, _ = fetcher(env, {LIST_URL: [listing(f"old@{boundary}"), listing(f"old@{boundary}")],
                                 "https://src.example/d/old": [ok(b"older announcement")]})
    assert collector.run(source, Window(DAY, DAY), max_pages=1).gaps == []
    incomplete = collector.run(source, Window(boundary, boundary), max_pages=1)
    assert incomplete.archived == 1
    assert [gap["reason"] for gap in incomplete.gaps] == ["pagination_requires_captcha"]


def test_order_and_overlap_gaps_keep_scanning_and_archive_new_notices(env):
    yesterday = DAY - timedelta(days=1)
    older = DAY - timedelta(days=2)
    collector, transport = fetcher(env, {
        LIST_URL: [listing("a", f"b@{yesterday}", has_next=True)],
        f"{LIST_URL}?page=2": [listing("c", f"b@{yesterday}", f"old@{older}", has_next=True)],
        f"{LIST_URL}?page=3": [listing("d")],
        **{f"https://src.example/d/{key}": [ok(key.encode())] for key in "abcd"},
    })
    result = collector.run(SOURCE, Window(yesterday, DAY), max_pages=3)
    assert (result.pages, result.archived, result.failed) == (3, 4, 0)
    assert [(gap["page"], gap["reason"]) for gap in result.gaps] == [
        (2, "overlapping_page"), (2, "inconsistent_publication_order"),
        (3, "inconsistent_publication_order"),
    ]
    assert [request.url for request in transport.sent].count("https://src.example/d/b") == 1
    assert rows(env[0], "SELECT count(*) FROM acq_discovery_pages") == [(3,)]


def test_offset_total_change_records_gap_and_continues_to_new_terminal_page(env):
    class OffsetHandler(Handler):
        def parse_list(self, channel, response):
            listing = super().parse_list(channel, response)
            total = 4 if response.url == LIST_URL else 5
            return dataclasses.replace(listing, offset=OffsetPage(total, 2, len(listing.refs)))

    old = (DAY - timedelta(days=1)).isoformat()
    pages = {LIST_URL: listing("a", "b"), f"{LIST_URL}?page=2": listing(f"c@{old}", f"d@{old}"),
             f"{LIST_URL}?page=3": listing(f"e@{old}")}
    collector, _ = fetcher(env, {
        **{url: [dataclasses.replace(response, url=url)] for url, response in pages.items()},
        **{f"https://src.example/d/{key}": [ok(key.encode())] for key in "ab"},
    })
    result = collector.run(dataclasses.replace(SOURCE, handler=OffsetHandler()), Window(DAY, DAY), max_pages=3)
    assert (result.pages, result.archived, result.failed) == (3, 2, 0)
    assert [(gap["page"], gap["reason"]) for gap in result.gaps] == [(2, "offset_total_changed")]


def plan_sources(database, declared, now):
    with reqctx.use(reqctx.service("acquisition")):
        created = 0
        for source in declared.values():
            with database.tx() as conn:
                created += plans.materialize(conn, source, now)
        return created


def run_planned(collector, declared, now, mode="incremental"):
    return dispatch.dispatch_once(collector, declared, plans.fingerprints_json(declared), mode=mode, now=now)


def test_scheduled_jobs_collect_declared_sources(env):
    database, _, clock = env
    scheduled = dataclasses.replace(SOURCE, schedule=sources.Schedule(
        every_minutes=60, lookback_days=1, max_pages=2, initial_backfill_days=0,
    ))
    collector, transport = fetcher(env, {LIST_URL: [listing("a"), listing("a")],
                                         "https://src.example/d/a": [ok(b"A"), ok(b"A")]})
    declared = {scheduled.source_id: scheduled, "unscheduled": SOURCE}
    now = clock.now()
    assert plan_sources(database, declared, now) == 1
    assert plan_sources(database, declared, now) == 0
    assert run_planned(collector, declared, now).archived == 1
    later = now + timedelta(hours=1)
    assert plan_sources(database, declared, later) == 1
    assert run_planned(collector, declared, later).unchanged == 1
    with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
        conn.execute(text("UPDATE acq_sources SET state = 'blocked', state_reason = 'captcha_challenge'"))
    assert plan_sources(database, declared, later + timedelta(hours=1)) == 0
    assert run_planned(collector, declared, later + timedelta(hours=1)) is None
    assert len(transport.sent) == 4


def test_scheduled_current_window_does_not_wait_for_older_recovery(env):
    database, _, clock = env
    source = dataclasses.replace(SOURCE, schedule=sources.Schedule(
        every_minutes=60, lookback_days=0, max_pages=1, initial_backfill_days=0,
    ))
    declared = {source.source_id: source}
    collector, transport = fetcher(env, {LIST_URL: [listing("today"), listing()],
                                         "https://src.example/d/today": [ok(b"fresh")]})
    # Materialize yesterday's intent, then abandon its already-committed claim.
    now = clock.now()
    yesterday = now - timedelta(days=1)
    assert plan_sources(database, declared, yesterday) == 1
    with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
        abandoned, _ = plans.claim(conn, plans.fingerprints_json(declared), yesterday, mode="incremental")
    expire_lease(database)
    assert plan_sources(database, declared, now) == 1
    assert run_planned(collector, declared, now).archived == 1
    # A fresh UTC+8 date gets its own window. The older plan keeps its run ID.
    assert run_planned(collector, declared, now).run_id == abandoned.run_id
    assert [request.url for request in transport.sent] == [LIST_URL, "https://src.example/d/today", LIST_URL]
    assert rows(database, "SELECT window_end, generation FROM acq_collection_runs ORDER BY id") == [
        (DAY - timedelta(days=1), 2), (DAY, 1),
    ]
    assert rows(database, "SELECT count(*) FROM acq_collection_plans WHERE settled_at IS NOT NULL") == [(2,)]


@pytest.mark.parametrize("mode", ["incremental", "backfill"])
@pytest.mark.parametrize("claimed", [False, True])
def test_changed_source_contract_replaces_same_occurrence(env, mode, claimed):
    from miaoqia.acquisition.application.collection_leases import LeaseLost

    database, _, clock = env
    source = dataclasses.replace(SOURCE, schedule=sources.Schedule(
        every_minutes=60, lookback_days=0, max_pages=1, initial_backfill_days=2,
    ))
    revised = dataclasses.replace(source, access=dataclasses.replace(source.access, timeout_seconds=21),
                                  schedule=dataclasses.replace(source.schedule, max_pages=3, backfill_max_pages=4))
    declared = {source.source_id: source}
    changed = {source.source_id: revised}
    now = clock.now()
    collector, transport = fetcher(env, {LIST_URL: [listing()] * 3})
    assert plan_sources(database, declared, now) == 2
    if mode == "backfill":
        finished = run_planned(collector, declared, now)
    with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
        abandoned = None
        if claimed:
            abandoned, _ = plans.claim(conn, plans.fingerprints_json(declared), now, mode=mode)
            conn.execute(text("UPDATE acq_collection_runs SET pending_report = '{\"pages\": 2}' WHERE id = :run"),
                         {"run": abandoned.run_id})
        conn.execute(text("UPDATE acq_collection_plans SET not_before = :retry WHERE mode = :mode"),
                     {"mode": mode, "retry": now + timedelta(minutes=2)})
    if abandoned:
        assert plan_sources(database, changed, now) == 0
        expire_lease(database)

    snapshot = rows(database, "SELECT next_due_at FROM acq_collection_schedule")
    assert plan_sources(database, changed, now) == (2 if mode == "incremental" else 1)
    assert plan_sources(database, changed, now) == 0
    assert rows(database, "SELECT next_due_at FROM acq_collection_schedule") == snapshot
    assert run_planned(collector, changed, now, mode) is None
    later = now + timedelta(minutes=2)
    assert run_planned(collector, declared, later, mode) is None
    report = run_planned(collector, changed, later, mode)
    assert report is not None
    history = rows(database, "SELECT occurrence_key, due_at, not_before, window_start, window_end, max_pages, "
                             "config_fingerprint, run_id, settlement_reason FROM acq_collection_plans "
                             f"WHERE mode = '{mode}' ORDER BY id")
    old, replacement = history
    assert old[:5] == replacement[:5]
    assert old[6:] == (sources.config_fingerprint(source), abandoned.run_id if abandoned else None,
                       "source_contract_changed")
    assert replacement[5:] == (3 if mode == "incremental" else 4, sources.config_fingerprint(revised),
                               report.run_id, None)
    if abandoned:
        assert report.run_id != abandoned.run_id
        assert rows(database, "SELECT report, pending_report FROM acq_collection_runs "
                              f"WHERE id = {abandoned.run_id}") == [
            ({"pages": 2, "termination_reason": "source_contract_changed"}, None),
        ]
        with pytest.raises(LeaseLost), reqctx.use(reqctx.service("acquisition")), abandoned.writable(database):
            pytest.fail("superseded worker retained write authority")
    if mode == "backfill":
        assert rows(database, "SELECT run_id, settlement_reason FROM acq_collection_plans "
                              "WHERE mode = 'incremental'") == [(finished.run_id, None)]
        assert rows(database, "SELECT next_day FROM acq_initial_backfill") == [(None,)]
    assert len(transport.sent) == (1 if mode == "incremental" else 2)


def test_source_contract_switch_serializes_planners_and_claims(env):
    from miaoqia.acquisition.application.collection_leases import LeaseLost

    database, _, clock = env
    source = dataclasses.replace(SOURCE, schedule=sources.Schedule(
        every_minutes=60, lookback_days=0, max_pages=1, initial_backfill_days=0,
    ))
    revised = dataclasses.replace(source, access=dataclasses.replace(source.access, timeout_seconds=21))
    declared = {source.source_id: source}
    changed = {source.source_id: revised}
    old_fingerprints, new_fingerprints = map(plans.fingerprints_json, (declared, changed))
    now = clock.now()
    assert plan_sources(database, declared, now) == 1
    peer = db.Database(database.engine.url.render_as_string(hide_password=False))

    def claim(connection, fingerprints):
        with reqctx.use(reqctx.service("acquisition")), connection.tx() as conn:
            return plans.claim(conn, fingerprints, now, mode="incremental")

    try:
        original, _ = claim(database, old_fingerprints)
        before = rows(database, "SELECT collection_fingerprint, access, active_run_id, lease_generation, lease_until "
                                "FROM acq_sources")
        assert plan_sources(peer, changed, now) == 0
        assert rows(database, "SELECT collection_fingerprint, access, active_run_id, lease_generation, lease_until "
                              "FROM acq_sources") == before
        assert claim(peer, new_fingerprints) is None
        expire_lease(database)

        with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
            conn.execute(text("UPDATE acq_sources SET state = 'blocked', state_reason = 'captcha_challenge'"))
        assert plan_sources(peer, changed, now) == 0
        assert claim(peer, old_fingerprints) is None
        assert rows(database, "SELECT collection_fingerprint, access FROM acq_sources") == [before[0][:2]]
        with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
            conn.execute(text("UPDATE acq_sources SET state = 'active', state_reason = NULL"))

        with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
            assert plans.materialize(conn, revised, now) == 1
            assert claim(peer, old_fingerprints) is None
            assert claim(peer, new_fingerprints) is None
        with ThreadPoolExecutor(max_workers=2) as pool:
            assert list(pool.map(lambda connection: plan_sources(connection, changed, now), (database, peer))) == [0, 0]
            results = list(pool.map(lambda connection: claim(connection, new_fingerprints), (database, peer)))
        (replacement, _), = [result for result in results if result is not None]
        assert claim(peer, old_fingerprints) is None
        with pytest.raises(LeaseLost), reqctx.use(reqctx.service("acquisition")), peer.tx() as conn:
            original.release(conn)
        with reqctx.use(reqctx.service("acquisition")), replacement.writable(database):
            assert replacement.run_id != original.run_id
    finally:
        peer.close()


@pytest.mark.parametrize("change_contract", [False, True])
def test_dispatch_upgrade_preserves_preexisting_plan_and_cursor(pg_factory, change_contract):
    from alembic import command
    from alembic.config import Config

    dsns = pg_factory()
    db.migrate(dsns["owner"])
    config = Config()
    config.set_main_option("script_location", MIGRATIONS)
    config.set_main_option("sqlalchemy.url", db._url(dsns["owner"]))
    command.upgrade(config, "0013")

    source = dataclasses.replace(SOURCE, schedule=sources.Schedule(
        every_minutes=60, lookback_days=0, max_pages=1, initial_backfill_days=0,
    ))
    now = FakeClock().now()
    due = now - timedelta(days=2)
    historical_day = due.astimezone(plans.CHINA).date()
    next_due = now + timedelta(hours=1)
    database = db.Database(dsns["app"])
    try:
        with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
            sources.register(conn, source)
            run_id = conn.execute(text("""
                INSERT INTO acq_collection_runs
                    (source_id, mode, window_start, window_end, max_pages, refetch, started_at, generation)
                VALUES (:source, 'incremental', :day, :day, 1, true, :due, 1) RETURNING id
            """), {"source": source.source_id, "day": historical_day, "due": due}).scalar_one()
            conn.execute(text("INSERT INTO acq_collection_schedule VALUES (:source, :next_due)"),
                         {"source": source.source_id, "next_due": next_due})
            conn.execute(text("""
                INSERT INTO acq_collection_plans (source_id, mode, occurrence_key, due_at, not_before,
                    window_start, window_end, max_pages, refetch, handler, config_fingerprint, budget_seconds, run_id)
                VALUES (:source, 'incremental', :key, :due, :due, :day, :day, 1, true, :handler, :fingerprint, 3600, :run)
            """), {"source": source.source_id, "key": due.isoformat(), "due": due, "day": historical_day,
                   "handler": source.handler_ref, "fingerprint": sources.config_fingerprint(source), "run": run_id})

        command.upgrade(config, "head")
        if change_contract:
            source = dataclasses.replace(source, access=dataclasses.replace(source.access, timeout_seconds=21))
        declared = {source.source_id: source}
        assert plan_sources(database, declared, now) == int(change_contract)
        assert rows(database, "SELECT next_due_at FROM acq_collection_schedule") == [(next_due,)]
        with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
            lease, _ = plans.claim(conn, plans.fingerprints_json(declared), now, mode="incremental")
        assert lease.window == Window(historical_day, historical_day)
        assert (lease.run_id != run_id, lease.generation) == (change_contract, 1 if change_contract else 2)
    finally:
        database.close()


def test_concurrent_planners_and_fetchers_drain_forty_sources_without_duplicate_runs(env):
    database, store, clock = env
    schedule = sources.Schedule(every_minutes=60, lookback_days=0, max_pages=1, initial_backfill_days=0)
    declared = {f"source-{index}": dataclasses.replace(SOURCE, source_id=f"source-{index}", schedule=schedule)
                for index in range(40)}
    peer = db.Database(database.engine.url.render_as_string(hide_password=False))
    now = clock.now()
    transport = Script({LIST_URL: [listing()] * len(declared)})
    fetchers = [Fetcher(database, store, transport, clock=clock), Fetcher(peer, store, transport, clock=clock)]

    def drain(collector):
        processed = 0
        while run_planned(collector, declared, now) is not None:
            processed += 1
        return processed

    try:
        with ThreadPoolExecutor(max_workers=2) as pool:
            assert sum(pool.map(lambda connection: plan_sources(connection, declared, now), (database, peer))) == 40
            assert sum(pool.map(drain, fetchers)) == 40
    finally:
        peer.close()

    assert len(transport.sent) == 40
    assert rows(database, "SELECT count(*), count(DISTINCT run_id) FROM acq_collection_plans "
                          "WHERE settled_at IS NOT NULL") == [(40, 40)]


def test_initial_backfill_scans_older_days_before_retrying_gaps_in_order(env):
    database, _, clock = env
    source = dataclasses.replace(SOURCE, schedule=sources.Schedule(every_minutes=60, lookback_days=0, max_pages=1))
    declared = {source.source_id: source}
    empty, incomplete = listing(), listing(has_next=True)
    dates = [DAY - timedelta(days=offset) for offset in range(1, 30)]
    scans = [incomplete, incomplete, *([empty] * 27), incomplete, empty, empty]
    collector, _ = fetcher(env, {LIST_URL: [empty, *scans]})
    now = clock.now()
    assert plan_sources(database, declared, now) == 2
    assert run_planned(collector, declared, now, "backfill") is None
    assert run_planned(collector, declared, now) is not None

    assert run_planned(collector, declared, now, "backfill") is not None
    yesterday, previous = dates[:2]
    assert rows(database, "SELECT oldest_day, next_day FROM acq_initial_backfill") == [(dates[-1], previous)]
    assert rows(database, "SELECT day FROM acq_initial_backfill_gaps") == [(yesterday,)]
    for _ in scans[1:]:
        assert plan_sources(database, declared, now) == 1
        assert run_planned(collector, declared, now, "backfill") is not None

    assert rows(database, "SELECT window_start FROM acq_collection_runs WHERE mode = 'backfill' ORDER BY id") == [
        (day,) for day in [*dates, yesterday, previous, yesterday]]
    assert rows(database, "SELECT next_day FROM acq_initial_backfill") == [(None,)]
    assert rows(database, "SELECT day FROM acq_initial_backfill_gaps") == []


@pytest.mark.parametrize("recheck", ["contract_change", "manual"])
def test_initial_backfill_keeps_known_source_boundary_until_verified_recheck(env, recheck):
    class BoundaryHandler(Handler):
        def parse_list(self, channel, response):
            page = super().parse_list(channel, response)
            return dataclasses.replace(page, coverage_gap=json.loads(response.body).get("boundary"))

    database, _, clock = env
    source = dataclasses.replace(SOURCE, handler=BoundaryHandler(), schedule=sources.Schedule(
        every_minutes=60, lookback_days=0, max_pages=1, initial_backfill_days=2,
    ))
    sources_by_id = {source.source_id: source}
    boundary = ok(b'{"refs":[],"next":false,"boundary":"pagination_requires_captcha"}', "application/json")
    collector, transport = fetcher(env, {LIST_URL: [listing(), boundary, listing()]})
    now = clock.now()
    assert plan_sources(database, sources_by_id, now) == 2
    assert run_planned(collector, sources_by_id, now) is not None
    report = run_planned(collector, sources_by_id, now, "backfill")
    yesterday = DAY - timedelta(days=1)
    assert rows(database, "SELECT day, last_run_id FROM acq_initial_backfill_gaps") == [(yesterday, report.run_id)]
    assert rows(database, "SELECT report->'wait_for_source_change' FROM acq_collection_runs "
                          "WHERE mode = 'backfill'") == [(True,)]
    assert plan_sources(database, sources_by_id, now) == 0
    assert len(transport.sent) == 2

    if recheck == "contract_change":
        revised = dataclasses.replace(source, access=dataclasses.replace(source.access, timeout_seconds=21))
        revised_sources = {source.source_id: revised}
        assert plan_sources(database, revised_sources, now) == 1
        assert run_planned(collector, revised_sources, now, "backfill") is not None
    else:
        assert collector.run(source, Window(yesterday, yesterday), max_pages=1).gaps == []
    assert rows(database, "SELECT day FROM acq_initial_backfill_gaps") == []
    assert len(transport.sent) == 3


def test_backfill_report_and_cursor_commit_with_lease_release(env, monkeypatch):
    from sqlalchemy.exc import DBAPIError
    from miaoqia.acquisition.application.collection_leases import CollectionLease

    database, _, clock = env
    source = dataclasses.replace(SOURCE, schedule=sources.Schedule(
        every_minutes=60, lookback_days=0, max_pages=1, initial_backfill_days=3,
    ))
    declared = {source.source_id: source}
    collector, _ = fetcher(env, {LIST_URL: [listing(), listing(has_next=True), listing()]})
    now = clock.now()
    assert plan_sources(database, declared, now) == 2
    assert run_planned(collector, declared, now) is not None
    release = CollectionLease.release

    def rollback_backfill(lease, conn):
        if lease.mode == "backfill":
            conn.execute(text("SELECT 1 / 0"))
        release(lease, conn)

    with monkeypatch.context() as patch:
        patch.setattr(CollectionLease, "release", rollback_backfill)
        with pytest.raises(DBAPIError, match="division by zero"):
            run_planned(collector, declared, now, "backfill")

    yesterday = DAY - timedelta(days=1)
    assert rows(database, "SELECT next_day FROM acq_initial_backfill") == [(yesterday,)]
    assert rows(database, "SELECT day FROM acq_initial_backfill_gaps") == []
    ((run_id, generation, report),) = rows(database, "SELECT id, generation, report FROM acq_collection_runs "
                                           "WHERE mode = 'backfill'")
    assert generation == 1 and report is None
    assert rows(database, "SELECT run_id, settled_at FROM acq_collection_plans WHERE mode = 'backfill'") == [(run_id, None)]
    expire_lease(database)

    assert run_planned(collector, declared, now, "backfill").run_id == run_id
    assert rows(database, "SELECT next_day FROM acq_initial_backfill") == [(yesterday - timedelta(days=1),)]
    assert rows(database, "SELECT id, generation, finished_at IS NOT NULL FROM acq_collection_runs "
                "WHERE mode = 'backfill'") == [(run_id, 2, True)]


def test_repository_schedules_fit_their_period():
    declared = sources.load(REPO / "packs")
    for src in declared.values():
        s = src.schedule
        assert s is not None, src.source_id
        details_per_page = {"anhui-ggzy-construction": 10, "anhui-government-procurement-spa": 15,
                            "national-public-resources": 20, "hefei-ggzy-engineer": 20,
                            "cncecyc-ywgg1gc": 14, "crccep-bidding-mat": 15, "luzhou-ggzy-tzgg": 10,
                            "bfjsjt-tender": 500, "cotenders-news": 10, "hunan-government-procurement": 100,
                            "m2-bid-list": 100, "ccgp-central-public-tenders": 20,
                            "hubei-government-procurement-homepage": 20, "xuzhou-ggzy": 12,
                            "ygcgfwpt-transaction": 10, "suzhou-ggzy-trade": 15,
                            "js-218-206-153-6-trade": 15, "henan-government-procurement-home": 15,
                            "tibet-government-procurement-home": 12, "hubei-ggzy-construction": 10,
                            "shanxi-prec": 10, "xinjiang-ggzy-construction": 10,
                            "sichuan-ggzy-construction": 12, "henan-ggzy-homepage-direct": 8,
                            "hunan-ggzy-construction": 10, "chongqing-ggzy-trade-014": 12
                            }.get(src.source_id, getattr(src.handler, "page_size", None))
        assert type(details_per_page) is int and details_per_page > 0, src.source_id
        windows = s.lookback_days + 1 if src.handler.windowed else 1
        list_requests = 2 if hasattr(src.handler, "list_plan") else 1
        requests_per_detail = 2 if src.transport == "browser" or src.source_id == "suzhou-ggzy-trade" else 1
        worst = len(src.handler.channels) * windows * s.max_pages * (list_requests + details_per_page * requests_per_detail)
        assert worst * src.access.min_interval_seconds <= s.every_minutes * 60, src.source_id


def test_module_jobs_disabled_without_object_store(monkeypatch):
    from miaoqia.acquisition import module
    from miaoqia.kernel.runtime import Env
    monkeypatch.delenv("MIAOQIA_ACQUISITION_OBJECT_STORE_DIR", raising=False)
    assert module().jobs(Env(role="acq-scheduler", brokers="", db=None, subsystem="acquisition")) == []


def test_module_splits_planner_probe_and_fixed_fetch_workers(monkeypatch, tmp_path):
    from miaoqia.acquisition import module
    from miaoqia.kernel.runtime import Env, prepare_role

    monkeypatch.setenv("MIAOQIA_ACQUISITION_OBJECT_STORE_DIR", str(tmp_path))
    monkeypatch.setenv("MIAOQIA_PACKS_DIR", str(REPO / "packs"))
    monkeypatch.setenv("MIAOQIA_DEPLOY_DIR", str(REPO / "deploy"))
    acquisition = module()
    scheduler_env = Env(role="acq-scheduler", brokers="", db=None, subsystem="acquisition")
    fetcher_env = dataclasses.replace(scheduler_env, role="acq-fetcher")
    assert [job.name for job in acquisition.jobs(scheduler_env)] == ["acquisition.plan-windows"]
    assert [consumer.name for consumer in acquisition.consumers(scheduler_env)] == ["acquisition.probe"]
    scheduler_spec, _ = prepare_role("acquisition", "acq-scheduler", [acquisition])
    fetcher_spec, _ = prepare_role("acquisition", "acq-fetcher", [acquisition])
    assert scheduler_spec["jobs"] and not scheduler_spec.get("workers")
    assert fetcher_spec["workers"] and fetcher_spec["consumers"] is False and not fetcher_spec.get("jobs")
    assert [worker.name for worker in acquisition.workers(fetcher_env)] == [
        "acquisition.fetch-online-1", "acquisition.fetch-online-2",
        "acquisition.fetch-recovery", "acquisition.fetch-history", "acquisition.maintenance",
    ]


def test_windowed_details_match_the_current_channel_day_and_deadline(env):
    class WindowedHandler(Handler):
        windowed = True
        channels = ("construction", "government")

        def list_request(self, channel, window, page):
            return FetchRequest("GET", f"{LIST_URL}?channel={channel}&day={window.since}")

    yesterday = DAY - timedelta(days=1)
    source = dataclasses.replace(SOURCE, handler=WindowedHandler())
    first = f"{LIST_URL}?channel=construction&day={DAY}"
    second = f"{LIST_URL}?channel=construction&day={yesterday}"
    collector, transport = fetcher(env, {
        first: [listing("today", f"old@{yesterday}")],
        "https://src.example/d/today": [ok(b"current day")],
    })
    report = collector.run(source, Window(yesterday, DAY), max_pages=1, deadline=env[2].t + 3)
    assert [request.url for request in transport.sent] == [first, "https://src.example/d/today"]
    assert second not in [request.url for request in transport.sent]
    assert report.archived == 1 and report.truncated
    assert report.gaps == [{"channel": "construction", "since": yesterday.isoformat(),
                            "until": yesterday.isoformat(), "page": 1, "reason": "deadline"}]


def test_coverage_gap_keeps_detail_and_enqueues_backfill_day(env):

    class FirstPageOnly(Handler):
        def parse_list(self, channel, response):
            page = super().parse_list(channel, response)
            return dataclasses.replace(page, coverage_gap="single_page_only")

    database, _, clock = env
    source = dataclasses.replace(SOURCE, handler=FirstPageOnly(),
                                 schedule=sources.Schedule(every_minutes=60, lookback_days=0, max_pages=1))
    yesterday = DAY - timedelta(days=1)
    collector, _ = fetcher(env, {LIST_URL: [listing("today"), listing(f"old@{yesterday}")],
                                 "https://src.example/d/today": [ok(b"today")],
                                 "https://src.example/d/old": [ok(b"old")]})
    declared = {source.source_id: source}
    now = clock.now()
    assert plan_sources(database, declared, now) == 2
    assert run_planned(collector, declared, now).archived == 1
    assert run_planned(collector, declared, now, "backfill").archived == 1
    assert rows(database, "SELECT next_day FROM acq_initial_backfill") == [(yesterday - timedelta(days=1),)]
    assert rows(database, "SELECT day FROM acq_initial_backfill_gaps") == [(yesterday,)]
    assert rows(database, "SELECT report->>'archived', report->'gaps'->0->>'reason'"
                          " FROM acq_collection_runs ORDER BY id") == [
        ("1", "single_page_only"), ("1", "single_page_only")]
    assert len(events(database, COMPLETED)) == 2


def test_slots_share_site_and_online_limit_and_reserve_backfill(env):
    database, _, _ = env
    peer = db.Database(database.engine.url.render_as_string(hide_password=False))
    try:
        local, remote = DownloadSlots(database), DownloadSlots(peer)
        with ExitStack() as slots:
            for number in range(3):
                admitted = slots.enter_context(local.try_acquire(source=f"source-{number}", site=f"site-{number}",
                                                                  mode="incremental"))
                assert isinstance(admitted, DownloadSlot)
            with remote.try_acquire(source="other", site="site-0", mode="backfill") as site_busy:
                assert isinstance(site_busy, float)
            with remote.try_acquire(source="source-0", site="other", mode="backfill") as source_busy:
                assert isinstance(source_busy, float)
            with remote.try_acquire(source="other", site="other", mode="incremental") as process_busy:
                assert isinstance(process_busy, float)
            with remote.try_acquire(source="other", site="other", mode="backfill") as backfill_available:
                assert isinstance(backfill_available, DownloadSlot)
                with local.try_acquire(source="extra", site="extra", mode="backfill") as backfill_busy:
                    assert isinstance(backfill_busy, float)
        with remote.try_acquire(source="other", site="site-0", mode="incremental") as released:
            assert isinstance(released, DownloadSlot)
    finally:
        peer.close()


def test_concurrent_same_host_waits_while_other_site_continues(env):
    database, store, _ = env
    sending, finish = Event(), Event()
    results = {}
    other_url = "https://other.example/list"

    class Blocking(Script):
        def send(self, request, timeout):
            sending.set()
            assert finish.wait(5)
            return listing()

    class OtherHandler(Handler):
        def list_request(self, channel, window, page):
            return FetchRequest("GET", other_url)

    first = Fetcher(database, store, Blocking({}))
    window = Window(DAY, DAY)

    def run_first():
        results["first"] = first.run(SOURCE, window, max_pages=1)

    worker = Thread(target=run_first)
    worker.start()
    try:
        assert sending.wait(5)
        same = dataclasses.replace(SOURCE, source_id="same-site")
        other = dataclasses.replace(SOURCE, source_id="other-site", handler=OtherHandler())
        scripted = Script({other_url: [listing()]})
        collector = Fetcher(database, store, scripted)
        timed_out = collector.run(same, window, max_pages=1, deadline=time.monotonic() + 0.25)
        assert timed_out.truncated and scripted.sent == []
        assert collector.run(other, window, max_pages=1).health == "healthy"
    finally:
        finish.set()
        worker.join(5)
    assert not worker.is_alive() and results["first"].health == "healthy"


def test_retry_after_is_shared_by_site_across_fetchers(env):
    database, _, clock = env
    window = Window(DAY, DAY)
    throttled, _ = fetcher(env, {LIST_URL: [FetchResponse(429, "text/html", b"", "u", retry_after="95")]})
    assert throttled.run(SOURCE, window, max_pages=1, deadline=clock.t + 10).truncated
    sibling = dataclasses.replace(SOURCE, source_id="same-site")
    collector, transport = fetcher(env, {LIST_URL: [listing()]})
    assert collector.run(sibling, window, max_pages=1, deadline=clock.t + 94).truncated
    assert transport.sent == []
    assert collector.run(sibling, window, max_pages=1, deadline=clock.t + 100).health == "healthy"
    assert clock.t == 1095
    assert rows(database, "SELECT outcome FROM acq_fetch_attempts ORDER BY id") == [("throttled",), ("ok",)]


def test_killed_worker_leaves_durable_run_and_restart_recovers_original_window(env, tmp_path):
    database, store, clock = env
    command = """
import os
import runpy
import sys
import time
test = runpy.run_path(sys.argv[1])
class Interrupted:
    def send(self, request, timeout):
        print("dispatch", flush=True)
        time.sleep(30)
collector = test["Fetcher"](test["db"].Database(os.environ["MQ_ACQ_FIXTURE_DSN"]),
                            test["FilesystemObjectStore"](sys.argv[2]), Interrupted(), clock=test["FakeClock"]())
collector.run(test["SOURCE"], test["Window"](test["DAY"], test["DAY"]), max_pages=1,
              refetch=True, mode="incremental")
"""
    with subprocess.Popen([sys.executable, "-c", command, __file__, str(tmp_path / "worker-objects")],
                          env={**os.environ, "MQ_ACQ_FIXTURE_DSN": database.engine.url.render_as_string(
                              hide_password=False)}, stdout=subprocess.PIPE, text=True) as worker:
        try:
            assert select.select([worker.stdout], [], [], 10)[0]
            assert worker.stdout.readline().strip() == "dispatch"
            assert rows(database, "SELECT generation, finished_at FROM acq_collection_runs") == [(1, None)]
            assert rows(database, "SELECT count(*) FROM acq_fetch_attempts") == [(0,)]
        finally:
            worker.kill()
            worker.wait(5)
    expire_lease(database)  # Age the lease deterministically after the actual worker exit.
    next_day = DAY + timedelta(days=1)
    restarted, _ = fetcher(env, {LIST_URL: [listing("a")], "https://src.example/d/a": [ok(b"recovered")]})
    report = restarted.run(SOURCE, Window(next_day, next_day), max_pages=1, refetch=True,
                           deadline=clock.t + 20, mode="incremental", recover=True)
    assert report.archived == 1
    assert rows(database, "SELECT id, generation, window_start FROM acq_collection_runs") == [(report.run_id, 2, DAY)]
    assert rows(database, "SELECT generation FROM acq_download_starts ORDER BY id") == [(1,), (2,), (2,)]
    assert rows(database, "SELECT count(*) FROM acq_fetch_attempts WHERE download_id IS NOT NULL") == [(2,)]

    settled = restarted.run(SOURCE, Window(next_day, next_day), max_pages=1, refetch=True,
                            mode="incremental", resume_id=report.run_id)
    assert settled.run_id is None and settled.deferred == "recovery_not_claimed"
    assert rows(database, "SELECT count(*) FROM acq_collection_runs") == [(1,)]


def test_redirect_hops_reacquire_budget_and_record_actual_attempts(env):
    database, _, clock = env
    collector, transport = fetcher(env, {
        LIST_URL: [FetchResponse(302, "text/html", b"", LIST_URL, redirect_to="/moved")],
        "https://src.example/moved": [listing()],
    })
    assert collector.run(SOURCE, Window(DAY, DAY), max_pages=1).health == "healthy"
    assert clock.t == 1002
    assert len(transport.sent) == 2
    assert rows(database, "SELECT status FROM acq_fetch_attempts ORDER BY id") == [(302,), (200,)]
    assert rows(database, "SELECT DISTINCT site FROM acq_download_starts") == [("src.example",)]


def test_postgres_commit_failure_replays_window_without_duplicate_observation(env, monkeypatch):
    from sqlalchemy.exc import DBAPIError
    from miaoqia.acquisition.application import fetch as runtime

    database, store, _ = env
    detail = "https://src.example/d/a"
    collector, _ = fetcher(env, {LIST_URL: [listing("a"), listing("a")], detail: [ok(b"raw"), ok(b"raw")]})
    window = Window(DAY, DAY)
    writer_type = type(runtime.writer)
    append = writer_type.append

    def reject_archive(writer, conn, event):
        if event.type == COMPLETED:
            conn.execute(text("SELECT 1 / 0"))
        return append(writer, conn, event)

    with monkeypatch.context() as patch:
        patch.setattr(writer_type, "append", reject_archive)
        with pytest.raises(DBAPIError, match="division by zero"):
            collector.run(SOURCE, window, max_pages=1)
    assert store.get("raw/" + hashlib.sha256(b"raw").hexdigest()) == b"raw"
    assert rows(database, "SELECT count(*) FROM acq_source_observations") == [(0,)]
    assert rows(database, "SELECT count(*) FROM acq_raw_artifacts") == [(0,)]
    assert events(database, COMPLETED) == []
    expire_lease(database)
    report = collector.run(SOURCE, window, max_pages=1)
    assert report.archived == 1
    assert rows(database, "SELECT id, generation FROM acq_collection_runs") == [(report.run_id, 2)]
    assert rows(database, "SELECT version FROM acq_source_observations") == [(1,)]
    assert len(events(database, COMPLETED)) == 1


@pytest.mark.parametrize(("key_prefix", "reason", "attempt_count"), [
    ("discovery/", "list_archive_pending", 1), ("raw/", "archive_object_pending", 2),
])
@pytest.mark.parametrize("transport_type", [Script, StreamingScript])
def test_archive_spool_restarts_with_original_bytes(env, monkeypatch, tmp_path, key_prefix, reason, attempt_count,
                                                    transport_type):
    database, store, clock = env
    spool_root = tmp_path / "spool"
    window = Window(DAY, DAY)
    detail = "https://src.example/d/a"
    first = transport_type({LIST_URL: [listing("a")], detail: [ok(b"first original")]})
    put = store.put

    def outage(key, content):
        if key.startswith(key_prefix):
            raise OSError("fixture archive unavailable")
        put(key, content)

    with monkeypatch.context() as patch:
        patch.setattr(store, "put", outage)
        report = Fetcher(database, store, first, clock=clock, spool=ArchiveSpool(spool_root)).run(
            SOURCE, window, max_pages=1,
        )
    assert report.deferred == reason
    assert len(first.sent) == attempt_count
    assert not events(database, COMPLETED)
    assert rows(database, "SELECT finished_at FROM acq_collection_runs") == [(None,)]

    resumed = transport_type({detail: [ok(b"first original")]} if attempt_count == 1 else {})
    recovered = Fetcher(database, store, resumed, clock=clock, spool=ArchiveSpool(spool_root)).run(
        SOURCE, window, max_pages=1,
    )
    assert recovered.run_id == report.run_id and recovered.archived == 1 and recovered.deferred is None
    assert [gap["reason"] for gap in recovered.gaps] == ["recovered_list_requires_rescan"]
    assert len(resumed.sent) == 2 - attempt_count
    assert rows(database, "SELECT count(*) FROM acq_discovery_pages") == [(1,)]
    assert rows(database, "SELECT count(*) FROM acq_fetch_attempts") == [(2,)]
    (original,) = events(database, COMPLETED)
    assert store.get(original["subject"]) == b"first original"
    assert list(spool_root.glob("[0-9a-f]*")) == []


PLANNED_LIST_URL = LIST_URL + "?channel=all"
PREFLIGHT_URL = "https://src.example/check"
PREFLIGHT_OK = FetchResponse(200, "application/json", b'{"block":null}', PREFLIGHT_URL)


class PreflightHandler(Handler):
    windowed = True

    def list_request(self, channel, window, page):
        return FetchRequest.post_json(PLANNED_LIST_URL, {
            "categoryCode": "test-category", "pageNo": page, "pageSize": 100,
            "publishDateBegin": window.since.isoformat(), "publishDateEnd": window.until.isoformat(),
        })

    def list_plan(self, channel, window, page, now):
        identity = self.list_request(channel, window, page)
        body = {**json.loads(identity.json_body), "_t": int(now.timestamp() * 1000)}
        return ListRequestPlan(identity, FetchRequest.post_json(identity.url, body),
                               FetchRequest.post_json(PREFLIGHT_URL, body))

    def check_list_preflight(self, response):
        payload = json.loads(response.body)
        if payload.get("captcha"):
            raise SourceBlocked("captcha_challenge")
        if payload["block"] is not None:
            raise SourceBlocked("publisher_query_blocked")


PREFLIGHT_SOURCE = dataclasses.replace(SOURCE, handler=PreflightHandler(), handler_ref="tests:PreflightHandler")


def planned_listing(*keys, has_next=False):
    return dataclasses.replace(listing(*keys, has_next=has_next), url=PLANNED_LIST_URL)


def validate_preflight(source, clock, transport, max_pages=2):
    entry = sources.CatalogSource(source.source_id, "verified", "http", source, "staged")
    return source_validation.validate(entry, source, "all", Window(DAY, DAY), max_pages=max_pages,
                                      max_details=1, transport_factory=lambda: transport, clock=clock)


def test_validator_preflights_each_page_with_paired_wire_parameters():
    clock = FakeClock()
    detail = "https://src.example/d/a"
    transport = Script({PREFLIGHT_URL: [PREFLIGHT_OK, PREFLIGHT_OK],
                        PLANNED_LIST_URL: [planned_listing("a", has_next=True), planned_listing("b")],
                        detail: [ok(b"detail")]})
    report = validate_preflight(PREFLIGHT_SOURCE, clock, transport)
    assert (report.outcome, report.pagination) == ("validated", "advanced")
    assert [request.url for request in transport.sent] == [PREFLIGHT_URL, PLANNED_LIST_URL,
                                                          PREFLIGHT_URL, PLANNED_LIST_URL, detail]
    assert clock.sleeps == [2] * 4
    for page, request_offset in enumerate((0, 2), start=1):
        preflight, request = transport.sent[request_offset:request_offset + 2]
        assert preflight.json_body == request.json_body
        assert json.loads(request.json_body) == {
            "categoryCode": "test-category", "pageNo": page, "pageSize": 100,
            "publishDateBegin": DAY.isoformat(), "publishDateEnd": DAY.isoformat(),
            "_t": int(clock.now().timestamp() * 1000),
        }
        assert report.requests[request_offset]["phase"] == "preflight"
        assert report.requests[request_offset]["stage"] == "list"
        assert report.requests[request_offset]["request_fingerprint"] == preflight.fingerprint()
        assert report.pages[page - 1]["request_fingerprint"] == request.fingerprint()


@pytest.mark.parametrize(("reply", "reason", "blocked"), [
    (dataclasses.replace(PREFLIGHT_OK, status=403), "http_403", True),
    (dataclasses.replace(PREFLIGHT_OK, body=b'{"captcha":true}'), "captcha_challenge", True),
    (dataclasses.replace(PREFLIGHT_OK, body=b'{"block":true}'), "publisher_query_blocked", True),
    (dataclasses.replace(PREFLIGHT_OK, body=b'{}'), "invalid_list_preflight", False),
    (dataclasses.replace(PREFLIGHT_OK, url=LIST_URL), "list_response_target_mismatch", False),
    (dataclasses.replace(PREFLIGHT_OK, status=307, redirect_to=LIST_URL), "part_redirect_disallowed", False),
    (dataclasses.replace(PREFLIGHT_OK, retry_after="90"), "throttled", False),
])
def test_preflight_rejection_never_downloads_or_archives_list(env, tmp_path, reply, reason, blocked):
    database, store, clock = env
    spool = ArchiveSpool(tmp_path / "spool")
    transport = StreamingScript({PREFLIGHT_URL: [reply]})
    report = Fetcher(database, store, transport, clock=clock, spool=spool).run(
        PREFLIGHT_SOURCE, Window(DAY, DAY), max_pages=2,
    )
    assert [request.url for request in transport.sent] == [PREFLIGHT_URL]
    assert report.pages == 0 and report.gaps[0]["reason"] == reason
    assert report.blocked == (reason if blocked else None)
    assert rows(database, "SELECT kind, state FROM acq_request_tasks") == [("list", "failed")]
    assert rows(database, "SELECT count(*) FROM acq_discovery_pages") == [(0,)]
    assert {path.name for path in spool.root.iterdir()} == {".lock"}

    validation_transport = Script({PREFLIGHT_URL: [reply]})
    validation = validate_preflight(PREFLIGHT_SOURCE, FakeClock(), validation_transport)
    assert [request.url for request in validation_transport.sent] == [PREFLIGHT_URL]
    assert validation.pages == [] and validation.issues == [{"stage": "list", "code": reason, "page": 1}]


@pytest.mark.parametrize(("field", "change", "reason"), [
    ("preflight", {"url": "https://elsewhere.example/check"}, "target_outside_channel"),
    ("request", {"url": LIST_URL + "?channel=other"}, "invalid_list_plan"),
    ("request", {"method": "GET"}, "invalid_list_plan"),
])
def test_invalid_list_plan_reaches_neither_transport(env, field, change, reason):
    class InvalidPlan(PreflightHandler):
        def list_plan(self, channel, window, page, now):
            plan = super().list_plan(channel, window, page, now)
            return dataclasses.replace(plan, **{field: dataclasses.replace(getattr(plan, field), **change)})

    source = dataclasses.replace(PREFLIGHT_SOURCE, handler=InvalidPlan())
    database, store, clock = env
    transport = Script({})
    report = Fetcher(database, store, transport, clock=clock).run(source, Window(DAY, DAY), max_pages=2)
    assert report.gaps[0]["reason"] == reason
    validation = validate_preflight(source, clock, transport)
    assert validation.issues == [{"stage": "list", "code": reason, "page": 1}]
    assert transport.sent == []


def test_list_retry_requires_a_new_preflight(env):
    database, store, clock = env
    unavailable = dataclasses.replace(planned_listing(), status=500)
    transport = Script({PREFLIGHT_URL: [PREFLIGHT_OK, PREFLIGHT_OK],
                        PLANNED_LIST_URL: [unavailable, planned_listing()]})
    report = Fetcher(database, store, transport, clock=clock).run(PREFLIGHT_SOURCE, Window(DAY, DAY), max_pages=1)
    assert report.pages == 1
    assert [request.url for request in transport.sent] == [PREFLIGHT_URL, PLANNED_LIST_URL] * 2
    assert clock.sleeps == [2, 4, 2]


def test_preflight_and_list_share_one_deadline(env, monkeypatch):
    database, store, clock = env
    transport = Script({PREFLIGHT_URL: [PREFLIGHT_OK]})
    report = Fetcher(database, store, transport, clock=clock).run(
        PREFLIGHT_SOURCE, Window(DAY, DAY), max_pages=1, deadline=clock.monotonic() + 1.5,
    )
    assert report.truncated and report.pages == 0 and report.gaps[0]["reason"] == "deadline"
    assert [request.url for request in transport.sent] == [PREFLIGHT_URL]
    assert rows(database, "SELECT kind, state FROM acq_request_tasks") == [("list", "queued")]

    monkeypatch.setattr(source_validation, "_RUN_SECONDS", 1.5)
    validation_transport = Script({PREFLIGHT_URL: [PREFLIGHT_OK]})
    validation = validate_preflight(PREFLIGHT_SOURCE, FakeClock(), validation_transport)
    assert [request.url for request in validation_transport.sent] == [PREFLIGHT_URL]
    assert validation.issues == [{"stage": "list", "code": "time_limit", "page": 1}]


@pytest.mark.parametrize("wrong_query", [False, True])
def test_preflight_spool_recovery_uses_stable_identity_and_pinned_target(env, monkeypatch, tmp_path, wrong_query):
    database, store, clock = env
    spool = ArchiveSpool(tmp_path / "spool")
    window = Window(DAY, DAY)
    original = planned_listing()
    first = StreamingScript({PREFLIGHT_URL: [PREFLIGHT_OK], PLANNED_LIST_URL: [original]})

    def outage(key, body):
        raise OSError("fixture archive unavailable")

    with monkeypatch.context() as patch:
        patch.setattr(store, "put", outage)
        report = Fetcher(database, store, first, clock=clock, spool=spool).run(
            PREFLIGHT_SOURCE, window, max_pages=1,
        )
    assert report.deferred == "list_archive_pending"
    original_plan = PREFLIGHT_SOURCE.handler.list_plan("all", window, 1, clock.now())
    assert first.sent == [original_plan.preflight, original_plan.request]
    assert clock.sleeps == [2]
    task = rows(database, "SELECT task_key, kind, scope, fingerprint, state, last_attempt FROM acq_request_tasks")
    (task_key, kind, scope, identity, state, last_attempt), = task
    assert (kind, scope, identity, state, last_attempt) == (
        "list", ["all", DAY.isoformat(), DAY.isoformat(), 1], original_plan.identity.fingerprint(), "staged", 2,
    )
    capture = spool.get(task_key)
    assert capture.response == original and capture.attempt_id == last_attempt
    if wrong_query:
        spool.discard(task_key, capture.attempt_id)
        wrong_response = dataclasses.replace(capture.response, url=LIST_URL + "?channel=other")
        spool.put(task_key, dataclasses.replace(capture, response=wrong_response))

    later = clock.now() + timedelta(seconds=30)
    monkeypatch.setattr(clock, "now", lambda: later)
    new_plan = PREFLIGHT_SOURCE.handler.list_plan("all", window, 1, clock.now())
    assert new_plan.identity == original_plan.identity and new_plan.request != original_plan.request
    resumed = StreamingScript({PREFLIGHT_URL: [PREFLIGHT_OK], PLANNED_LIST_URL: [original]} if wrong_query else {})
    recovered = Fetcher(database, store, resumed, clock=clock, spool=spool).run(
        PREFLIGHT_SOURCE, window, max_pages=1,
    )
    expected = first.sent + resumed.sent
    assert recovered.run_id == report.run_id and recovered.pages == 1 and recovered.deferred is None
    assert resumed.sent == ([new_plan.preflight, new_plan.request] if wrong_query else [])
    assert [gap["reason"] for gap in recovered.gaps] == ([] if wrong_query else ["recovered_list_requires_rescan"])
    assert rows(database, """SELECT d.task_key, d.fingerprint, a.fingerprint FROM acq_download_starts d
                              JOIN acq_fetch_attempts a ON a.download_id = d.id ORDER BY d.id""") == [
        (task_key, request.fingerprint(), request.fingerprint()) for request in expected
    ]
    assert rows(database, "SELECT state, last_attempt FROM acq_request_tasks") == [("settled", len(expected))]
    (attempt, object_ref), = rows(database, "SELECT attempt_id, object_ref FROM acq_discovery_pages")
    assert attempt == len(expected) and store.get(object_ref) == original.body
    assert {path.name for path in spool.root.iterdir()} == {".lock"}


def test_streamed_challenge_never_becomes_a_discovery_artifact(env, tmp_path):
    database, store, clock = env
    spool = ArchiveSpool(tmp_path / "spool")
    transport = StreamingScript({LIST_URL: [ok(b'{"captcha": true}', "application/json")]})
    report = Fetcher(database, store, transport, clock=clock, spool=spool).run(SOURCE, Window(DAY, DAY), max_pages=1)
    assert report.blocked == "captcha_challenge"
    assert rows(database, "SELECT count(*) FROM acq_discovery_pages") == [(0,)]
    assert rows(database, "SELECT state FROM acq_request_tasks") == [("failed",)]
    assert {path.name for path in spool.root.iterdir()} == {".lock"}


def test_restart_revalidates_a_complete_stream_after_interrupted_parser(env, tmp_path, monkeypatch):
    database, store, clock = env
    spool = ArchiveSpool(tmp_path / "spool")
    window = Window(DAY, DAY)
    first = StreamingScript({LIST_URL: [listing("a")]})

    def interrupted(*_):
        raise RuntimeError("fixture stopped before parsing")

    with monkeypatch.context() as patch:
        patch.setattr(Handler, "parse_list", interrupted)
        collector = Fetcher(database, store, first, clock=clock, spool=spool)
        with pytest.raises(RuntimeError, match="stopped before parsing"):
            collector.run(SOURCE, window, max_pages=1)

    assert rows(database, "SELECT count(*) FROM acq_discovery_pages") == [(0,)]
    expire_lease(database)
    detail = "https://src.example/d/a"
    resumed = StreamingScript({detail: [ok(b"original")]})
    report = Fetcher(database, store, resumed, clock=clock, spool=spool).run(SOURCE, window, max_pages=1)
    assert report.archived == 1
    assert [request.url for request in resumed.sent] == [detail]
    assert rows(database, "SELECT count(*) FROM acq_fetch_attempts") == [(2,)]
    assert {path.name for path in spool.root.iterdir()} == {".lock"}


def test_archive_spool_reuses_response_after_database_rollback(env, tmp_path, monkeypatch):
    from sqlalchemy.exc import DBAPIError
    from miaoqia.acquisition.application import fetch as runtime

    database, store, clock = env
    spool_root = tmp_path / "spool"
    window = Window(DAY, DAY)
    initial = Script({LIST_URL: [listing("a")], "https://src.example/d/a": [ok(b"original")]})
    writer_type = type(runtime.writer)
    append = writer_type.append

    def reject(writer, conn, event):
        if event.type == COMPLETED:
            conn.execute(text("SELECT 1 / 0"))
        return append(writer, conn, event)

    with monkeypatch.context() as patch:
        patch.setattr(writer_type, "append", reject)
        with pytest.raises(DBAPIError, match="division by zero"):
            Fetcher(database, store, initial, clock=clock, spool=ArchiveSpool(spool_root)).run(
                SOURCE, window, max_pages=1,
            )
    assert events(database, COMPLETED) == []
    expire_lease(database)
    offline = Script({})
    report = Fetcher(database, store, offline, clock=clock, spool=ArchiveSpool(spool_root)).run(
        SOURCE, window, max_pages=1,
    )
    assert report.archived == 1 and offline.sent == []
    assert rows(database, "SELECT generation FROM acq_collection_runs") == [(2,)]
    assert rows(database, "SELECT count(*) FROM acq_fetch_attempts") == [(2,)]
    (original,) = events(database, COMPLETED)
    assert store.get(original["subject"]) == b"original"


def test_archive_spool_backpressure_leaves_request_queued_without_http(env, tmp_path):
    database, store, clock = env
    transport = Script({})
    spool = ArchiveSpool(tmp_path / "spool", capacity=64 * 1024)
    report = Fetcher(database, store, transport, clock=clock, spool=spool).run(SOURCE, Window(DAY, DAY), max_pages=1)
    assert report.deferred == "archive_spool_full" and transport.sent == []
    assert rows(database, "SELECT kind, state, last_attempt FROM acq_request_tasks") == [("list", "queued", None)]
    assert rows(database, "SELECT count(*) FROM acq_fetch_attempts") == [(0,)]
    assert rows(database, "SELECT finished_at, pending_report->>'deferred' FROM acq_collection_runs") == [
        (None, "archive_spool_full"),
    ]


def test_archive_spool_lost_with_host_retries_without_claiming_old_bytes(env, tmp_path, monkeypatch):
    database, store, clock = env
    first = Script({LIST_URL: [listing("a")], "https://src.example/d/a": [ok(b"lost original")]})
    window = Window(DAY, DAY)
    put = store.put

    def fail_raw(key, content):
        if key.startswith("raw/"):
            raise OSError("fixture archive unavailable")
        put(key, content)

    with monkeypatch.context() as patch:
        patch.setattr(store, "put", fail_raw)
        previous = Fetcher(database, store, first, clock=clock, spool=ArchiveSpool(tmp_path / "old-host")).run(
            SOURCE, window, max_pages=1,
        )
    assert previous.deferred == "archive_object_pending" and events(database, COMPLETED) == []
    second = Script({LIST_URL: [listing("a")], "https://src.example/d/a": [ok(b"new download")]})
    recovered = Fetcher(database, store, second, clock=clock, spool=ArchiveSpool(tmp_path / "replacement-host")).run(
        SOURCE, window, max_pages=1,
    )
    assert recovered.run_id == previous.run_id and recovered.archived == 1
    (original,) = events(database, COMPLETED)
    assert store.get(original["subject"]) == b"new download"
    assert rows(database, """
        SELECT t.kind, t.state, count(a.id)
        FROM acq_request_tasks t JOIN acq_download_starts d USING (task_key)
        JOIN acq_fetch_attempts a ON a.download_id = d.id
        GROUP BY t.kind, t.state ORDER BY t.kind
    """) == [("body", "settled", 2), ("list", "settled", 2)]
