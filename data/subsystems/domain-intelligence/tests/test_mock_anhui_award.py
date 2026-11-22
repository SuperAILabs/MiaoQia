"""Synthetic Anhui award. All publisher responses, names and award amounts are test material."""
from __future__ import annotations

from collections import deque
from dataclasses import replace
from datetime import date, datetime, timedelta, timezone
import os
import subprocess

import pytest
from sqlalchemy import text

from miaoqia.acquisition import MIGRATIONS
from miaoqia.acquisition.api.handlers import FetchResponse, Window
from miaoqia.acquisition.application.fetch import COMPLETED, Fetcher
from miaoqia.acquisition.application.sources import load
from miaoqia.runtime_data import read_routes
from miaoqia.kernel import db, payloads, reqctx
from miaoqia.kernel.internal_http import Client, Problem, Server

from test_facts import REPO, _events, _facts, fact_version, plane  # noqa: F401

SOURCE = "anhui-ggzy-construction"
GUID = "00000000-0000-4000-8000-000000000003"
DAY = date(2026, 11, 11)
WINNER = "虚构测试建设有限公司"
OTHER_WINNER = "虚构测试机电有限公司"


def listing(guid, day, has_next):
    more = '<a onclick="pagination(2);return false;">下一页</a>' if has_next else ""
    return f"""<ul class="list clear"><li class="list-item">
      <a href="/jsgc/newDetail?guid={guid}&amp;tenderProjectType=1&amp;bulletinNature=3">
      <span class="title" title="【MOCK】设备安装工程中标结果公告"></span>
      <span class="date">{day}</span></a></li></ul>{more}""".encode()


def award(winner):
    return f"""<article>
      <h1 class="article-title">【MOCK】设备安装工程中标结果公告</h1>
      <p>项目编号：MOCK-AH-2026-003</p>
      <p>招标人：虚构测试招标单位</p>
      <p>第1包：</p>
      <p>供应商名称：{winner}</p>
      <p>中标（成交）金额：123456元</p>
      </article>""".encode()


class MockPublisher:
    def __init__(self):
        self.details = deque([(503, b""), (200, award(WINNER)), (200, award(OTHER_WINNER)), (200, award(WINNER))])

    def send(self, request, timeout):
        form = dict(request.form)
        assert request.method == "POST" and form["bulletinNature"] == "3" and timeout > 0
        if request.url.endswith("/list"):
            page = int(form["currentPage"])
            assert form["tenderProjectType"] == "1" and page in (1, 2)
            body = (listing("00000000-0000-4000-8000-000000000004", "2026-11-12", True)
                    if page == 1 else listing(GUID, DAY, False))
            return FetchResponse(200, "text/html", body, request.url)
        assert request.url.endswith("/newDetailSub") and form["guid"] == GUID and form["type"] == "zbjg"
        status, body = self.details.popleft()
        return FetchResponse(status, "text/html", body, request.url)


class Clock:
    t = 1000.0

    def monotonic(self):
        return self.t

    def sleep(self, seconds):
        self.t += seconds

    def now(self):
        return datetime.combine(DAY, datetime.min.time(), timezone.utc) + timedelta(seconds=self.t)


@pytest.fixture
def collected(pg_factory, plane):
    dsns = pg_factory()
    db.migrate(dsns["owner"], MIGRATIONS)
    database = db.Database(dsns["app"])
    source = load(REPO / "packs")[SOURCE]
    source.handler.channels = ("3:1",)
    source = replace(source, access=replace(source.access, min_interval_seconds=1))
    store = plane[2].store
    collector = Fetcher(database, store, MockPublisher(), clock=Clock())
    window = Window(DAY, DAY)
    try:
        short = collector.run(source, window, max_pages=1, refetch=True)
        assert short.archived == 0 and short.gaps[0]["page"] == 2 and short.gaps[0]["reason"] == "page_limit"
        reports = [collector.run(source, window, max_pages=2, refetch=True) for _ in range(3)]
        assert all(r.pages == 2 and r.archived == 1 and not r.gaps for r in reports)
        yield database, store, _events(database, COMPLETED)
    finally:
        database.close()


def test_mock_acquisition_recovers_page_and_http_failure(collected):
    database, store, events = collected
    assert [event["data"]["notice_key"] for event in events] == [f"{GUID}:3"] * 3
    assert [event["data"]["observation_version"] for event in events] == [1, 2, 3]
    assert [store.get(event["subject"]) for event in events] == [award(WINNER), award(OTHER_WINNER), award(WINNER)]
    assert events[0]["subject"] == events[2]["subject"] != events[1]["subject"]
    for event in events:
        payloads.validator(COMPLETED, {}).validate(event["data"])
    with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
        assert conn.execute(text("SELECT count(*) FROM acq_raw_artifacts")).scalar_one() == 2
        assert conn.execute(text("SELECT outcome FROM acq_fetch_attempts WHERE status = 503")).scalar_one() == "http_error"


def test_mock_extraction_and_authorized_read(collected, plane, tls_factory, running_server):
    from miaoqia.evidence.application.live import LiveIngest
    from miaoqia.evidence.extensions.packs import load_documents

    _, store, events = collected
    evd, dom, pipeline, materializer, _ = plane
    ingest = LiveIngest(store, pipeline, load_documents(REPO / "packs"))
    with reqctx.use(reqctx.service("evidence")):
        for event in [*events, events[1], events[2]]:
            with evd.tx() as conn:
                ingest.on_completed(conn, event)
    revisions = _events(evd, "evidence.extraction.revised.v1")
    assert len(revisions) == 3
    def deliver(*items):
        with reqctx.use(reqctx.service("domain-intelligence")):
            for revision in items:
                with dom.tx() as conn:
                    materializer.on_extraction_revised(conn, revision)

    deliver(*revisions[:2], revisions[0])
    (initial,) = _facts(dom)
    assert initial["version"] == 2
    _assert_remote(evd, dom, store, initial, tls_factory, running_server,
                   advance=lambda: deliver(revisions[2], revisions[1]))
    (fact,) = _facts(dom)
    assert fact["version"] == 3 and fact["notice_type"] == "award"
    assert fact["project_code"]["value"] == "MOCK-AH-2026-003"
    (result,) = fact["lots"][0]["bid_results"]
    assert result["bidder"]["name_raw"] == WINNER and result["price"]["value"] == "123456"


def _assert_remote(evd, dom, store, initial, tls_factory, running_server, *, advance):
    class MockIdentity:
        def resolve(self, *, deadline):
            assert reqctx.current().principal.id == "mock-research-user"

    class MockRights:
        allowed = True
        display = "full_text"
        resolve_hook = None
        confirm_hook = None
        decisions = []

        def status(self, *, deadline):
            pass

        def resolve(self, request, *, deadline):
            assert request["inputs"] == [{"source_id": SOURCE, "tags": [f"public:{SOURCE}"]}]
            self.decisions.append(request)
            if self.resolve_hook:
                self.resolve_hook()
            return {"allowed": self.allowed, "display": self.display, "receipt": "mock-receipt"}

        def confirm(self, request, *, deadline):
            assert request == {**self.decisions[-1], "receipt": "mock-receipt"}
            if self.confirm_hook:
                self.confirm_hook()
            return {"allowed": self.allowed, "display": self.display if self.allowed else "none"}

    identity, rights = MockIdentity(), MockRights()
    handlers = read_routes(dom, evd, store, rights, identity)
    client_peer, server_peer = "spiffe://miaoqia/service/e2e-harness", "spiffe://miaoqia/service/data-api"
    access = {(client_peer, op, "interactive", None) for op, _ in handlers.values()}
    server = Server(("127.0.0.1", 0), tls_factory("server", server_peer), access, identity, handlers)
    url = running_server(server)
    peer_tls = tls_factory("client", client_peer)
    client = Client(url, peer_tls, server_peer)
    ctx = reqctx.Context(reqctx.Principal("mock-research-user", "user"), "mock-tenant", "mock-cell", "interactive")
    current = {"view": "current", "recorded_cutoff": None}

    def read(route, body):
        return client.request("POST", "/internal/v1/data/" + route, body)

    with reqctx.use(ctx):
        body = {"fact_id": initial["id"], "basis": current, "version": 2}
        assert read("get-fact", body)["fact"] == initial

        # The current head is newer than the cutoff. A prior version still needs admission.
        with dom.tx() as conn:
            old = fact_version(conn, initial["id"], 1)
        cutoff = {"view": "known_at", "recorded_cutoff": old["recorded_at"]}
        request = {**body, "version": None, "basis": cutoff}
        before = len(rights.decisions)
        assert read("get-fact", request)["fact"] == old
        assert len(rights.decisions) == before + 1
        rights.allowed = False
        with pytest.raises(Problem) as denied:
            read("get-fact", request)
        assert denied.value.status == 403
        rights.allowed = True

        # Publish a newer head from another transaction after the selected DTO is frozen.
        rights.resolve_hook = advance
        assert read("get-fact", {**body, "version": None})["fact"]["version"] == 2
        rights.resolve_hook = None
        fact = read("get-fact", {**body, "version": None})["fact"]
        assert fact["version"] == 3
        body["version"] = 3
        evidence_id = fact["lots"][0]["bid_results"][0]["bidder"]["prov"]["evidence_ref"]
        proof = read("resolve-evidence", {"evidence_ref": evidence_id})["evidence"]
        assert proof["snippet"] == WINNER and proof["locator"]
        chunk_request = {"fact_id": fact["id"], "version": 3, "ordinal": 0}
        chunk = read("get-notice-chunk", chunk_request)
        assert chunk["snapshot_ref"] == fact["snapshot_ref"] and chunk["text"]
        assert chunk["locator"]["char_end"] - chunk["locator"]["char_start"] == len(chunk["text"])
        assert read("get-notice-chunk", {**chunk_request, "version": 2}) is None

        rights.display = "excerpt"
        assert read("get-notice-chunk", chunk_request) is None
        assert read("get-fact", body)["fact"] == fact
        excerpt = read("resolve-evidence", {"evidence_ref": evidence_id})["evidence"]
        assert excerpt["display_level"] == "excerpt" and excerpt["context"] is None

        rights.confirm_hook = lambda: setattr(rights, "allowed", False)
        with pytest.raises(Problem) as denied:
            read("get-fact", body)
        assert denied.value.status == 403
        rights.allowed = True
        rights.confirm_hook = None
        rights.display = "full_text"
        rights.confirm_hook = lambda: setattr(rights, "allowed", False)
        with pytest.raises(Problem) as denied:
            read("get-notice-chunk", chunk_request)
        assert denied.value.status == 403
        rights.allowed = True
        rights.confirm_hook = None

        before = len(rights.decisions)
        assert read("get-fact", {**body, "fact_id": "ntc_absent"}) is None
        assert read("resolve-evidence", {"evidence_ref": "ev_absent"}) is None
        assert len(rights.decisions) == before

    if os.environ.get("MIAOQIA_TEST_DATAREAD_GO") == "1":
        command = ["go", "test", "./kernel/dataread", "./cmd/platform",
                   "-run", "^Test(AgainstPythonListener|A02AgainstPythonListener)$", "-count=1", "-v"]
        run = subprocess.run(command, cwd=REPO / "services", timeout=30, capture_output=True, text=True,
                             env=dict(os.environ, MIAOQIA_TEST_DATAREAD_URL=url,
                                      MIAOQIA_TEST_DATAREAD_FACT=fact["id"], MIAOQIA_TEST_DATAREAD_EVIDENCE=evidence_id,
                                      MIAOQIA_INTERNAL_TLS_CERT=peer_tls.cert, MIAOQIA_INTERNAL_TLS_KEY=peer_tls.key,
                                      MIAOQIA_INTERNAL_TLS_CA=peer_tls.ca))
        assert run.returncode == 0, run.stdout + run.stderr
        assert "--- PASS: TestAgainstPythonListener" in run.stdout
        assert "--- PASS: TestA02AgainstPythonListener" in run.stdout
