"""Verify public detail locators with scripted HTTP and PostgreSQL collection state."""
from __future__ import annotations

import hashlib
import json
from dataclasses import replace

import pytest

from miaoqia.acquisition.api.handlers import DetailFetch, FetchRequest, FetchResponse, SourceBlocked, Window
from miaoqia.acquisition.application import source_validation
from miaoqia.acquisition.application.fetch import NOTICE_ARCHIVED, Fetcher, SourceStopped
from miaoqia.acquisition.application.sources import CatalogSource
from miaoqia.acquisition.infrastructure.archive_spool import ArchiveSpool

from test_fetch import (DAY, LIST_URL, SOURCE, FakeClock, Handler, Script, StreamingScript, env, events, expire_lease,
                        listing, rows)


LOCATOR = FetchRequest("GET", "https://src.example/lookup?infoid=a&categorynum=003001001")
BODY = FetchRequest("GET", "https://src.example/public/20261111/a.html")
REF_URL = "https://src.example/d/a"
WINDOW = Window(DAY, DAY)
LOCATED = FetchResponse(200, "application/octet-stream", json.dumps({"url": BODY.url}).encode(), LOCATOR.url)
ORIGINAL = FetchResponse(200, "text/html", b"<article>Publisher original</article>", BODY.url)


class LocatorHandler(Handler):
    def __init__(self, locator=LOCATOR):
        self.locator = locator

    def detail_fetches(self, channel, ref):
        return (DetailFetch(f"{ref.source_key}/body", locator=self.locator),)

    def resolve_detail(self, channel, ref, response):
        assert (channel, ref.source_key, ref.source_url) == ("all", "a", REF_URL)
        if response.body == b"captcha":
            raise SourceBlocked("captcha_challenge")
        return FetchRequest("GET", json.loads(response.body)["url"])

    def check_detail(self, response):
        if response.body == b"captcha":
            raise SourceBlocked("captcha_challenge")
        if not response.body.startswith(b"<article>"):
            raise ValueError("missing article")


LOCATOR_SOURCE = replace(SOURCE, handler=LocatorHandler(), handler_ref="tests:LocatorHandler")


def _digest(body):
    return "sha256:" + hashlib.sha256(body).hexdigest()


def _script(locator=LOCATED, original=ORIGINAL, *, transport=Script):
    return transport({LIST_URL: [listing("a")], LOCATOR.url: [locator], BODY.url: [original]})


def _runtime(env, transport, *, source=LOCATOR_SOURCE, spool=None, **options):
    database, store, clock = env
    fetcher = Fetcher(database, store, transport, clock=clock, spool=spool)
    return fetcher.run(source, WINDOW, max_pages=1, **options)


def _validator(transport, *, source=LOCATOR_SOURCE, clock=None):
    entry = CatalogSource(source.source_id, "verified", "http", source, "staged")
    return source_validation.validate(entry, source, "all", WINDOW, max_pages=1, max_details=1,
                                      transport_factory=lambda: transport, clock=clock or FakeClock())


def test_body_requires_exactly_one_request_or_locator():
    assert DetailFetch("a/body", BODY).locator is None
    assert DetailFetch("a/body", locator=LOCATOR).request is None
    assert DetailFetch("a/attachment", None, "attachment").request is None
    for kwargs in ({}, {"request": BODY, "locator": LOCATOR}, {"kind": "attachment", "locator": LOCATOR}):
        with pytest.raises(ValueError):
            DetailFetch("a/body", **kwargs)


def test_locator_and_body_share_runtime_budget_and_lease(env, tmp_path):
    database, store, clock = env
    transport = _script(transport=StreamingScript)
    spool = ArchiveSpool(tmp_path / "spool")
    report = _runtime(env, transport, spool=spool)
    assert [request.url for request in transport.sent] == [LIST_URL, LOCATOR.url, BODY.url]
    assert clock.sleeps == [2, 2]
    assert (report.archived, report.failed, report.gaps) == (1, 0, [])

    digest = _digest(ORIGINAL.body)
    (event,) = events(database, NOTICE_ARCHIVED)
    assert (event["data"]["source_url"], event["data"]["content_hash"]) == (REF_URL, digest)
    assert store.get(event["subject"]) == ORIGINAL.body
    assert rows(database, "SELECT source_key, content_hash, request_fingerprint FROM acq_raw_artifacts") == [
        ("a/body", digest, BODY.fingerprint()),
    ]
    assert rows(database, "SELECT notice_key FROM acq_source_observations") == [("a",)]
    attempts = rows(database, """
        SELECT t.scope->>2, d.run_id, d.generation, d.site, a.fingerprint, t.last_attempt = a.id
        FROM acq_download_starts d JOIN acq_fetch_attempts a ON a.download_id = d.id
        JOIN acq_request_tasks t ON t.task_key = d.task_key WHERE t.kind = 'body' ORDER BY d.id
    """)
    assert attempts == [("locator", report.run_id, 1, "src.example", LOCATOR.fingerprint(), True),
                        ("located", report.run_id, 1, "src.example", BODY.fingerprint(), True)]
    assert not list(spool.root.glob("[0-9a-f]*"))


def test_validator_reports_locator_attempt_and_only_original_detail():
    transport, clock = _script(), FakeClock()
    report = _validator(transport, clock=clock)
    assert report.outcome == "validated" and not report.issues
    assert [request.url for request in transport.sent] == [LIST_URL, LOCATOR.url, BODY.url]
    assert clock.sleeps == [2, 2]
    assert report.requests[1] == {
        "stage": "detail", "phase": "locator", "page": 1, "method": "GET", "status": 200,
        "request_fingerprint": LOCATOR.fingerprint(), "response_sha256": _digest(LOCATED.body),
    }
    assert report.details == [{
        "page": 1, "source_key": "a", "artifact_key": "a/body", "published": DAY.isoformat(),
        "request_fingerprint": BODY.fingerprint(), "response_sha256": _digest(ORIGINAL.body),
        "media_type": "text/html",
    }]


def test_validator_keeps_public_request_checks_on_the_resolved_body():
    located = replace(LOCATED, body=json.dumps({"url": BODY.url + "?signature=fixture"}).encode())
    transport = _script(located)
    report = _validator(transport)
    assert [request.url for request in transport.sent] == [LIST_URL, LOCATOR.url]
    assert not report.details and report.outcome == "limited"
    assert report.issues[0]["code"] == "credential_parameter_disallowed"


@pytest.mark.parametrize(("phase", "changes", "reason", "blocked"), [
    ("locator", {"status": 401}, "http_401", True),
    ("locator", {"status": 403}, "http_403", True),
    ("locator", {"body": b"captcha"}, "captcha_challenge", True),
    ("locator", {"status": 429, "retry_after": "90"}, "throttled", False),
    ("locator", {"retry_after": "90"}, "throttled", False),
    ("locator", {"status": 302, "redirect_to": BODY.url}, "part_redirect_disallowed", False),
    ("locator", {"url": LOCATOR.url + "&other=1"}, "detail_response_target_mismatch", False),
    ("locator", {"body": b"{}"}, "invalid_detail_locator", False),
    ("body", {"status": 302, "redirect_to": LOCATOR.url}, "part_redirect_disallowed", False),
    ("body", {"url": "https://other.example/a.html"}, "detail_response_target_mismatch", False),
    ("body", {"body": b"captcha"}, "captcha_challenge", True),
    ("body", {"status": 429}, "throttled", False),
    ("body", {"status": 404}, "http_404", False),
    ("body", {"body": LOCATED.body}, "invalid_content", False),
])
def test_rejected_response_never_becomes_an_original(env, phase, changes, reason, blocked):
    locator = replace(LOCATED, **changes) if phase == "locator" else LOCATED
    original = replace(ORIGINAL, **changes) if phase == "body" else ORIGINAL
    database, _, _ = env
    expected = [LIST_URL, LOCATOR.url] + ([BODY.url] if phase == "body" else [])

    runtime_transport = _script(locator, original)
    report = _runtime(env, runtime_transport)
    assert [request.url for request in runtime_transport.sent] == expected
    assert report.blocked == (reason if blocked else None)
    assert reason in [gap["reason"] for gap in report.gaps]
    assert not events(database, NOTICE_ARCHIVED)
    assert rows(database, "SELECT count(*) FROM acq_raw_artifacts") == [(0,)]
    if blocked:
        with pytest.raises(SourceStopped):
            _runtime(env, Script({}))

    validation_transport = _script(locator, original)
    validation = _validator(validation_transport)
    assert [request.url for request in validation_transport.sent] == expected
    assert not validation.details
    assert validation.issues == [{"stage": "detail", "code": reason, "page": 1}]
    assert (validation.outcome == "blocked") is (blocked or reason == "throttled")


@pytest.mark.parametrize("phase", ["locator", "body"])
@pytest.mark.parametrize("foreign", ["http://src.example", "https://src.example:8443", "https://other.example"])
def test_both_requests_are_confined_to_the_list_origin(env, phase, foreign):
    source, locator = LOCATOR_SOURCE, LOCATED
    if phase == "locator":
        source = replace(source, handler=LocatorHandler(FetchRequest("GET", foreign + "/lookup")))
    else:
        locator = replace(locator, body=json.dumps({"url": foreign + "/body"}).encode())
    expected = [LIST_URL] + ([LOCATOR.url] if phase == "body" else [])

    runtime_transport = _script(locator)
    report = _runtime(env, runtime_transport, source=source)
    assert [request.url for request in runtime_transport.sent] == expected
    assert report.gaps[0]["reason"] == "target_outside_channel"
    assert not events(env[0], NOTICE_ARCHIVED)

    validation_transport = _script(locator)
    report = _validator(validation_transport, source=source)
    assert [request.url for request in validation_transport.sent] == expected
    assert not report.details and report.outcome == "limited"
    assert report.issues[0]["code"] == "target_outside_channel"


def test_runtime_and_validation_apply_remaining_deadline_to_resolved_body(env, monkeypatch):
    transport = _script()
    report = _runtime(env, transport, deadline=env[2].monotonic() + 3)
    assert [request.url for request in transport.sent] == [LIST_URL, LOCATOR.url]
    assert report.truncated and report.gaps[0]["reason"] == "deadline"
    assert not events(env[0], NOTICE_ARCHIVED)

    monkeypatch.setattr(source_validation, "_RUN_SECONDS", 3)
    transport = _script()
    validation = _validator(transport)
    assert [request.url for request in transport.sent] == [LIST_URL, LOCATOR.url]
    assert not validation.details and validation.issues[0]["code"] == "time_limit"


def test_lease_loss_after_locator_prevents_original_request(env):
    class ExpiredLeaseHandler(LocatorHandler):
        def resolve_detail(self, channel, ref, response):
            expire_lease(env[0])
            return super().resolve_detail(channel, ref, response)

    transport = _script()
    report = _runtime(env, transport, source=replace(LOCATOR_SOURCE, handler=ExpiredLeaseHandler()))
    assert [request.url for request in transport.sent] == [LIST_URL, LOCATOR.url]
    assert report.truncated and report.gaps[0]["reason"] == "lease_lost"
    assert not events(env[0], NOTICE_ARCHIVED)


@pytest.mark.parametrize("tamper", [False, True], ids=["intact", "changed-query"])
def test_resume_uses_only_a_bound_original_capture(env, tmp_path, monkeypatch, tamper):
    database, store, _ = env
    spool = ArchiveSpool(tmp_path / "spool")
    transport = _script(transport=StreamingScript)
    put = store.put

    def fail_original_write(key, content):
        if key.startswith("raw/"):
            raise OSError("fixture archive unavailable")
        put(key, content)

    with monkeypatch.context() as patch:
        patch.setattr(store, "put", fail_original_write)
        failed = _runtime(env, transport, spool=spool)
    assert failed.deferred == "archive_object_pending"
    assert [request.url for request in transport.sent] == [LIST_URL, LOCATOR.url, BODY.url]
    assert not events(database, NOTICE_ARCHIVED)
    (locator_key,), (body_key,) = rows(database, """
        SELECT task_key FROM acq_request_tasks WHERE kind = 'body' ORDER BY scope ->> 2 DESC
    """)
    assert spool.get(locator_key) is None
    capture = spool.get(body_key)
    assert capture.response == ORIGINAL

    expected_original = ORIGINAL
    expected_requests = []
    expected_attempts = [(LOCATOR.fingerprint(), 1), (BODY.fingerprint(), 1)]
    routes = {}
    if tamper:
        spool.discard(body_key, capture.attempt_id)
        spool.put(body_key, replace(capture, response=replace(ORIGINAL, url=BODY.url + "?different=1")))
        new_body = replace(BODY, url=BODY.url + "?version=2")
        expected_original = replace(ORIGINAL, url=new_body.url, body=b"<article>New original</article>")
        expected_requests = [LOCATOR.url, new_body.url]
        expected_attempts += [(LOCATOR.fingerprint(), 2), (new_body.fingerprint(), 2)]
        routes = {LOCATOR.url: [replace(LOCATED, body=json.dumps({"url": new_body.url}).encode())],
                  new_body.url: [expected_original]}
    transport = StreamingScript(routes)
    recovered = _runtime(env, transport, spool=spool)
    assert (recovered.run_id, recovered.archived, recovered.deferred) == (failed.run_id, 1, None)
    assert [request.url for request in transport.sent] == expected_requests
    (event,) = events(database, NOTICE_ARCHIVED)
    assert store.get(event["subject"]) == expected_original.body
    assert rows(database, "SELECT notice_key FROM acq_source_observations") == [("a",)]
    assert rows(database, "SELECT request_fingerprint FROM acq_raw_artifacts") == [(expected_attempts[-1][0],)]
    assert rows(database, """
        SELECT a.fingerprint, d.generation FROM acq_download_starts d
        JOIN acq_fetch_attempts a ON a.download_id = d.id
        JOIN acq_request_tasks t ON t.task_key = d.task_key WHERE t.kind = 'body' ORDER BY d.id
    """) == expected_attempts
    assert not list(spool.root.glob("[0-9a-f]*"))
