"""Fetch runtime against a real PostgreSQL with a scripted transport and a fake clock (no network)."""
from __future__ import annotations

import json
from datetime import date, datetime, timezone
from pathlib import Path

import pytest
from sqlalchemy import text

from miaoqia.acquisition import MIGRATIONS
from miaoqia.acquisition.api.handlers import (AccessContract, DetailFetch, DetailRef, FetchRequest, FetchResponse,
                                              ListPage, SourceBlocked, Window)
from miaoqia.acquisition.application import sources
from miaoqia.acquisition.application.fetch import Fetcher, SourceStopped
from miaoqia.acquisition.infrastructure.transport import TransportError
from miaoqia.kernel import db
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


def ok(body: bytes, media="text/html") -> FetchResponse:
    return FetchResponse(200, media, body, "u")


def listing(*keys: str, has_next=False) -> FetchResponse:
    return ok(json.dumps({"refs": list(keys), "next": has_next}).encode(), "application/json")


class Handler:
    """Minimal list → detail source: list JSON names detail keys, one artifact per key."""

    source_id = "test-source"
    channels = ("all",)
    windowed = False

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
        return ListPage(tuple(refs), body["next"])

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
    done = events(database, "acquisition.fetch.completed.v1")
    assert [e["data"]["source_url"] for e in done] == ["https://src.example/d/a", "https://src.example/d/b"]
    data = done[0]["data"]
    assert data["rights_tags"] == ["public:test-source"] and data["artifact_ref"] == data["content_hash"].replace("sha256:", "raw/")
    assert store.get(data["artifact_ref"]) == b"<html>A</html>"
    assert rows(database, "SELECT count(*) FROM acq_fetch_attempts WHERE outcome = 'ok'") == [(3,)]
    # Requests to the same source are paced by the access contract.
    assert clock.sleeps == [2, 2]


def test_incremental_run_skips_archived_and_refetch_dedups_identical_content(env):
    routes = {LIST_URL: [listing("a"), listing("a"), listing("a")],
              "https://src.example/d/a": [ok(b"same"), ok(b"same")]}
    f, transport = fetcher(env, routes)
    f.run(SOURCE, Window(DAY, DAY), max_pages=1)
    second = f.run(SOURCE, Window(DAY, DAY), max_pages=1)
    assert (second.skipped, second.archived) == (1, 0)
    third = f.run(SOURCE, Window(DAY, DAY), max_pages=1, refetch=True)
    assert (third.unchanged, third.archived) == (1, 0)
    assert len(events(env[0], "acquisition.fetch.completed.v1")) == 1
    assert rows(env[0], "SELECT count(*) FROM acq_raw_artifacts") == [(1,)]


def test_pagination_stops_at_window_start(env):
    f, transport = fetcher(env, {LIST_URL: [listing("new@2026-11-11", "old@2026-11-09", has_next=True)],
                                 "https://src.example/d/new": [ok(b"n")]})
    report = f.run(SOURCE, Window(date(2026, 11, 10), DAY), max_pages=5)
    assert (report.pages, report.listed, report.archived) == (1, 1, 1)


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
    assert set(declared) == {"anhui-ggzy-construction", "anhui-government-procurement-spa", "national-public-resources"}
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
