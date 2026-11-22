"""Run browser bodies through the same window, discovery and download ledger as HTTP."""
from dataclasses import replace

import pytest

from miaoqia.acquisition import module
from miaoqia.acquisition.api.browser_pairs import BROWSER_COMPLETED, read_browser_pair
from miaoqia.acquisition.api.handlers import BrowserDetailPlan, BrowserSelection, FetchRequest, FetchResponse, Window
from miaoqia.acquisition.application import dispatch, plans
from miaoqia.acquisition.application.fetch import Fetcher
from miaoqia.acquisition.application.sources import Schedule
from miaoqia.acquisition.infrastructure.archive_spool import ArchiveSpool
from miaoqia.acquisition.infrastructure.browser_capture import BrowserCapture
from miaoqia.kernel import reqctx
from miaoqia.kernel.runtime import Env

from test_fetch import DAY, LIST_URL, SOURCE, Handler, Script, env, events, listing, rows

KEY = "90071992547409931"
ORIGINAL = b"opaque publisher entity"
WINDOW = Window(DAY, DAY)


class BrowserSource:
    source_id = SOURCE.source_id
    channels = ("all",)
    windowed = False
    ordered_by_publication = True
    contract_version = 1
    list_request = Handler.list_request
    parse_list = Handler.parse_list

    def browser_detail(self, channel, ref):
        return BrowserDetailPlan(ref.source_url, FetchRequest("GET", f"https://src.example/body/{ref.source_key}"))

    def validate_browser_notice(self, ref, original, selected):
        if original.body != ORIGINAL or f'id="{ref.source_key}"' not in selected.html:
            raise ValueError("fixture body does not belong to the listing")
        return DAY


ACTIVE = replace(SOURCE, handler=BrowserSource(), transport="browser")


class Render:
    def __init__(self, *, outside_plan=False, body_status=200):
        self.outside_plan = outside_plan
        self.body_status = body_status
        self.calls = 0

    def render(self, plan, permit, *, timeout):
        self.calls += 1
        with permit(FetchRequest("GET", plan.navigation_url)) as finish:
            finish(200, None)
        body = FetchRequest("GET", "https://src.example/other") if self.outside_plan else plan.body_request
        with permit(body) as finish:
            finish(self.body_status, None)
        response = FetchResponse(200, "application/octet-stream", ORIGINAL, body.url)
        capture = BrowserCapture(response, "GET", "r1", "frame", "loader", "192.0.2.1", 443)
        return capture, BrowserSelection(f'<article id="{KEY}"><p>Selected notice</p></article>',
                                         "navigation", "frame", "loader")


def test_browser_worker_uses_shared_requests_and_archives_pair(env, tmp_path):
    database, store, clock = env
    renderer = Render()
    http = Script({LIST_URL: [listing(KEY), listing(KEY)]})
    fetcher = Fetcher(database, store, http, clock=clock, spool=ArchiveSpool(tmp_path / "spool"), browser=renderer)
    schedule = Schedule(every_minutes=60, lookback_days=0, max_pages=1, initial_backfill_days=0)
    active = replace(ACTIVE, schedule=schedule)
    declared = {active.source_id: active}
    with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
        assert plans.materialize(conn, active, clock.now()) == 1
    legacy_http = {SOURCE.source_id: replace(SOURCE, schedule=schedule)}
    assert dispatch.dispatch_once(fetcher, legacy_http, plans.fingerprints_json(legacy_http),
                                  mode="incremental", now=clock.now()) is None
    report = dispatch.dispatch_once(fetcher, declared, plans.fingerprints_json(declared),
                                    mode="incremental", now=clock.now())
    assert (report.pages, report.archived, report.failed, report.gaps) == (1, 1, 0, [])
    (event,) = events(database, BROWSER_COMPLETED)
    assert read_browser_pair(store, event["data"])[0] == ORIGINAL
    assert rows(database, "SELECT url FROM acq_fetch_attempts ORDER BY id") == [
        (LIST_URL,), (f"https://src.example/d/{KEY}",), (f"https://src.example/body/{KEY}",),
    ]
    assert len(clock.sleeps) == 2
    assert fetcher.run(ACTIVE, WINDOW, max_pages=1).skipped == 1
    assert renderer.calls == 1
    assert [call.url for call in http.sent] == [LIST_URL, LIST_URL]


def test_browser_worker_refuses_route_outside_plan_and_does_not_publish(env, tmp_path):
    database, store, clock = env
    http = Script({LIST_URL: [listing(KEY)]})
    fetcher = Fetcher(database, store, http, clock=clock, spool=ArchiveSpool(tmp_path / "spool"),
                      browser=Render(outside_plan=True))
    report = fetcher.run(ACTIVE, WINDOW, max_pages=1)
    assert report.archived == 0 and report.failed == 1
    assert report.gaps[0]["reason"] == "invalid_browser_capture"
    assert rows(database, "SELECT url FROM acq_fetch_attempts ORDER BY id") == [
        (LIST_URL,), (f"https://src.example/d/{KEY}",),
    ]
    assert not events(database, BROWSER_COMPLETED)


def test_unconfigured_browser_cannot_fetch_the_public_list(env):
    database, store, clock = env
    http = Script({})
    report = Fetcher(database, store, http, clock=clock).run(ACTIVE, WINDOW, max_pages=1)
    assert report.deferred == "browser_runtime_unavailable"
    assert not http.sent and not events(database, BROWSER_COMPLETED)


def test_browser_rate_limit_is_reported_and_applies_shared_site_cooldown(env, tmp_path):
    database, store, clock = env
    http = Script({LIST_URL: [listing(KEY)]})
    fetcher = Fetcher(database, store, http, clock=clock, spool=ArchiveSpool(tmp_path / "spool"),
                      browser=Render(body_status=429))
    report = fetcher.run(ACTIVE, WINDOW, max_pages=1)
    assert report.archived == 0 and report.gaps[0]["reason"] == "throttled"
    assert report.failures[0]["retryable"]
    assert rows(database, """
        SELECT EXTRACT(EPOCH FROM ready_at - sent_at) FROM acq_download_pacing WHERE scope = 'site/src.example'
    """) == [(60,)]
    assert not events(database, BROWSER_COMPLETED)


def test_browser_permit_cannot_wait_past_the_render_deadline(env, tmp_path):
    database, store, clock = env
    http = Script({LIST_URL: [listing(KEY)]})
    fetcher = Fetcher(database, store, http, clock=clock, spool=ArchiveSpool(tmp_path / "spool"), browser=Render())
    short = replace(ACTIVE, access=replace(ACTIVE.access, timeout_seconds=1))
    report = fetcher.run(short, WINDOW, max_pages=1)
    assert report.archived == 0 and report.gaps[0]["reason"] == "browser_deadline"
    assert rows(database, "SELECT url FROM acq_fetch_attempts ORDER BY id") == [(LIST_URL,)]
    assert clock.sleeps == []


def test_resumed_browser_window_commits_staged_pair_without_new_navigation(env, tmp_path, monkeypatch):
    database, store, clock = env
    http = Script({LIST_URL: [listing(KEY), listing(KEY)]})
    renderer = Render()
    fetcher = Fetcher(database, store, http, clock=clock, spool=ArchiveSpool(tmp_path / "spool"), browser=renderer)
    put = store.put

    def unavailable(key, value):
        if key.startswith("projection/"):
            raise OSError("unavailable")
        put(key, value)

    with monkeypatch.context() as fault:
        fault.setattr(store, "put", unavailable)
        interrupted = fetcher.run(ACTIVE, WINDOW, max_pages=1)
    assert interrupted.deferred == "browser_archive_pending"
    assert not events(database, BROWSER_COMPLETED)
    resumed = fetcher.run(ACTIVE, WINDOW, max_pages=1, resume_id=interrupted.run_id)
    assert resumed.failed == 0 and resumed.archived == 1 and renderer.calls == 1
    assert len(events(database, BROWSER_COMPLETED)) == 1


def test_browser_role_requires_bound_renderer_and_selects_only_browser_sources(env, tmp_path, monkeypatch):
    from miaoqia.acquisition.application import sources

    database, _, _ = env
    load_calls = []

    def load(packs, *, transport=None):
        load_calls.append(transport)
        return {SOURCE.source_id: ACTIVE if transport == "browser" else SOURCE}

    monkeypatch.setattr(sources, "load", load)
    monkeypatch.setenv("MIAOQIA_ACQUISITION_OBJECT_STORE_DIR", str(tmp_path / "objects"))
    monkeypatch.setenv("MIAOQIA_ACQUISITION_ARCHIVE_SPOOL_DIR", str(tmp_path / "spool"))
    browser_env = Env("acq-browser", "", database, "acquisition")
    http_env = Env("acq-fetcher", "", database, "acquisition")
    with pytest.raises(ValueError, match="requires a confined renderer"):
        module().workers(browser_env)

    http = module().workers(http_env)
    assert [worker.name for worker in http][:1] == ["acquisition.fetch-online-1"]
    browser = module(browser_factory=Render).workers(browser_env)
    assert [worker.name for worker in browser] == ["acquisition.browser-collect", "acquisition.maintenance"]
    assert load_calls == ["browser", "http", "browser"]
