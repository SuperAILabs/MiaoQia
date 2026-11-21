"""Synthetic public HTTP responses with isolated real PostgreSQL acquisition and evidence databases."""
from __future__ import annotations

import json
import os
import re
import subprocess
import sys
from dataclasses import replace
from datetime import timedelta
from shutil import copyfile
from urllib.parse import urlencode

import psycopg
import pytest
import yaml
from psycopg.types.json import Jsonb

from miaoqia.acquisition.api.frozen_batches import FrozenBatch, FrozenBatchDenied, ObservationRef
from miaoqia.acquisition.api.handlers import DetailRef, FetchResponse, Window
from miaoqia.acquisition.application.fetch import COMPLETED, Fetcher, SourceStopped
from miaoqia.acquisition.application.frozen_batches import FrozenBatches
from miaoqia.acquisition.application.sources import for_validation, load
from miaoqia.acquisition.infrastructure.archive_spool import ArchiveSpool
from miaoqia.evidence import MIGRATIONS as EVIDENCE_MIGRATIONS, live_ingest
from miaoqia.kernel import db, ids, payloads, reqctx
from miaoqia_pack_procurement.sources.anhui_ggzy import BASE, AnhuiGgzyConstruction

from test_fetch import DAY, REPO, Script, env, events, fetcher, rows  # noqa: F401

GUID = "00000000-0000-4000-8000-000000000003"
OTHER = "00000000-0000-4000-8000-000000000004"
NOTICE = f"{GUID}:3"
LIST, BODY = f"{BASE}/jsgc/list", f"{BASE}/jsgc/newDetailSub"
FILE = f"{BASE}/mock-files/{GUID}/3/results.pdf"
PDF = b"%PDF-1.7\nexplicit synthetic bytes\n%%EOF"


@pytest.fixture
def source():
    class SyntheticAnhui(AnhuiGgzyConstruction):
        channels = ("3:1",)
        attachment_path = re.compile(
            r"/mock-files/(?P<guid>[0-9a-f-]+)/(?P<nature>[123])/(?P<part_key>[a-z-]+)\.pdf"
        )

    declared = load(REPO / "packs")[AnhuiGgzyConstruction.source_id]
    return replace(declared, handler=SyntheticAnhui(), access=replace(declared.access, max_attempts=1))


def page(*, found=True):
    item = f"""<li class="list-item">
    <a href="/jsgc/newDetail?guid={GUID}&amp;tenderProjectType=1&amp;bulletinNature=3">
    <span class="title" title="【MOCK】设备安装工程中标结果公告"></span>
    <span class="date">{DAY}</span></a></li>""" if found else ""
    return FetchResponse(200, "text/html", f'<ul class="list clear">{item}</ul>'.encode(), LIST)


def body(*, project="001", extra=""):
    return FetchResponse(200, "text/html", f"""<article>
    <h1 class="article-title">【MOCK】设备安装工程中标结果公告</h1>
    <div class="article-text-box">
    <p>项目编号：MOCK-AH-2026-{project}</p>
    <p>招标人：虚构测试招标单位</p>
    <p>第1包：</p>
    <p>供应商名称：虚构测试建设有限公司</p>
    <p>中标（成交）金额：123456元</p>
    <a download="补充材料.pdf" href="{FILE}">附件</a>
    {extra}
    </div>
    </article>""".encode(), BODY)


def file_response(status, *, contents=PDF, target=None):
    return FetchResponse(status, "application/pdf", contents if status == 200 else b"", FILE, redirect_to=target)


@pytest.fixture
def crccep_original():
    _, source = for_validation(REPO / "packs", "crccep-bidding-mat")
    handler = source.handler
    window = Window(DAY, DAY)
    list_url = handler.list_request("MAT", window, 1).url
    native = {"id": "crc-fixture-17", "noticeTitle": "虚构材料采购项目公开招标公告", "noticeType": "2", "subtypeId": "MAT",
              "sourcingModeID": "ZBCG", "publishTime": DAY.isoformat()}
    page = FetchResponse(200, "application/json", json.dumps({
        "list": [native], "pageNum": 1, "pageSize": 15, "currentPageSize": 1, "pages": 1, "total": 1,
    }).encode(), list_url)
    ref, = handler.parse_list("MAT", page).refs
    part, = handler.detail_fetches("MAT", ref)
    attachment_url = handler.attachment_origin + handler.attachment_path + "?" + urlencode({
        "fileId": "public/fixture.pdf", "fileName": "fixture.pdf",
    })
    portal_resource = attachment_url.replace("fixture.pdf", "portal-help.pdf")
    document = FetchResponse(200, "application/json", json.dumps(native | {
        "noticeContent": f'<p>项目编号：MOCK-17</p><p>招标人：虚构测试单位</p><a href="{attachment_url}">附件</a>',
        "siteFooter": f'<a href="{portal_resource}">站点帮助</a>',
    }).encode(), part.request.url)
    return source, window, page, document, attachment_url


@pytest.mark.parametrize(("allowed", "redirect", "issue"), [
    (True, False, None),
    (False, False, "target_outside_channel"),
    (True, True, "part_redirect_disallowed"),
])
def test_crccep_attachment_origin_is_explicit_and_does_not_receive_publisher_headers(env, crccep_original,
                                                                                  allowed, redirect, issue):
    source, window, page, document, attachment_url = crccep_original
    approval = source.access.attachment_origins if allowed else ()
    source = replace(source, access=replace(source.access, attachment_origins=approval,
                                            headers=(("X-Fixture-Publisher", "body-only"),)))
    receipt = FetchResponse(302, "text/html", b"", attachment_url, redirect_to="https://unapproved.example/file") \
        if redirect else FetchResponse(200, "application/pdf", PDF, attachment_url)
    collector, transport = fetcher(env, {page.url: [page], document.url: [document], attachment_url: [receipt]})

    result = collector.run(source, window, max_pages=1)
    assert result.archived == (1 if issue else 2)
    assert [gap["reason"] for gap in result.gaps] == ([issue] if issue else [])
    assert [request.url for request in transport.sent] == [page.url, document.url] + ([attachment_url] if allowed else [])
    assert all(dict(request.headers)["X-Fixture-Publisher"] == "body-only" for request in transport.sent[:2])
    if allowed:
        assert transport.sent[-1].headers == ()
    latest = events(env[0], COMPLETED)[-1]["data"]["attachments"][0]
    assert latest["status"] == ("pending" if issue else "available")
    if issue is None:
        assert env[1].get(latest["artifact_ref"]) == PDF


@pytest.mark.parametrize("inline_body", [False, True])
def test_m2_downloads_only_the_current_notice_files_and_redacts_its_signed_url(env, inline_body):
    _, source = for_validation(REPO / "packs", "m2-bid-list")
    handler = source.handler
    window = Window(DAY, DAY)
    list_url = handler.list_request("notice", window, 1).url
    native = {"tenderInfoId": "25931", "noticeName": "虚构材料采购公告", "publishedAt": DAY.isoformat()}
    listing = FetchResponse(200, "application/json", json.dumps({
        "data": {"infoList": [native], "total": 1},
    }).encode(), list_url)
    ref, = handler.parse_list("notice", listing).refs
    (part,) = handler.detail_fetches("notice", ref)
    file_path = f"https://{handler.attachment_host}:443/upload1/{'1' * 32}/202611/{'2' * 32}.pdf"
    signature = "?AccessKeyId=fixture&Expires=2000000000&Signature=fixture-signed-value"
    attachment_url = file_path + signature
    outside_url = file_path.replace("/" + "2" * 32 + ".pdf", "/" + "3" * 32 + ".pdf") + signature
    notice = native | {
        "context": f'<p>正文</p><a href="{outside_url}">其他公开资源</a>' if inline_body else "",
        "files": [{"id": "9007199254740993123", "name": "原件.pdf", "url": attachment_url}],
    }
    original = FetchResponse(200, "application/json", json.dumps({
        "data": notice, "files": [{"id": "9007199254740993124", "name": "站点文件.pdf", "url": outside_url}],
    }).encode(), part.request.url)
    collector, transport = fetcher(env, {
        listing.url: [listing], original.url: [original],
        attachment_url: [FetchResponse(200, "application/pdf", PDF, attachment_url)],
    })

    result = collector.run(source, window, max_pages=1)
    assert result.archived == 2 and not result.gaps
    assert [request.url for request in transport.sent] == [listing.url, original.url, attachment_url]
    latest = events(env[0], COMPLETED)[-1]["data"]
    attachment, = latest["attachments"]
    assert attachment["part_key"] == "9007199254740993123" and attachment["status"] == "available"
    assert env[1].get(attachment["artifact_ref"]) == PDF
    assert rows(env[0], "SELECT source_url FROM acq_raw_artifacts WHERE media_type = 'application/pdf'") == [(file_path,)]
    assert rows(env[0], "SELECT url FROM acq_fetch_attempts WHERE attachment_part_key IS NOT NULL") == [(file_path,)]


def test_crccep_freezes_original_before_field_approval_then_replays_same_observations(pg_factory, env, tmp_path,
                                                                                    crccep_original):
    database, store, _ = env
    source, window, page, document, attachment_url = crccep_original
    collector, _ = fetcher(env, {page.url: [page], document.url: [document], attachment_url: [
        FetchResponse(200, "application/pdf", PDF, attachment_url),
    ]})
    assert collector.run(source, window, max_pages=1).archived == 2
    acquired = events(database, COMPLETED)
    latest = acquired[-1]["data"]
    ref = ObservationRef(source.source_id, latest["notice_key"], latest["observation_version"])
    part_ref = replace(ref, part_key=latest["attachments"][0]["part_key"])
    service = FrozenBatches(database, store, authorize=lambda operation, original: original.ref in (ref, part_ref))
    with reqctx.use(reqctx.service("frozen-crccep-test")):
        frozen = service.freeze((ref, part_ref))
        assert service.read(frozen, 0) == document.body
        assert service.read(frozen, 1) == PDF

    dsns = pg_factory()
    db.migrate(dsns["owner"], EVIDENCE_MIGRATIONS)
    evidence = db.Database(dsns["app"])
    pack_root, object_root = REPO / "packs", tmp_path / "objects"
    ingest = live_ingest(str(object_root), str(pack_root))
    try:
        for event in acquired:
            with reqctx.use(reqctx.service("evidence")), evidence.tx() as conn:
                ingest.on_completed(conn, event)
        assert rows(evidence, "SELECT count(*) FROM evd_snapshots") == [(0,)]
        assert events(evidence, "evidence.extraction.revised.v1") == []

        command = [sys.executable, "-m", "miaoqia.evidence.replay", "--pending-source", source.source_id,
                   "--limit", "1", "--packs"]
        settings = os.environ | {"MIAOQIA_EVIDENCE_DB_DSN": dsns["app"],
                                 "MIAOQIA_EVIDENCE_OBJECT_STORE_DIR": str(object_root)}
        held = subprocess.run([*command, str(pack_root)], env=settings, capture_output=True, text=True, timeout=30)
        assert held.returncode == 2
        assert rows(evidence, "SELECT count(*) FROM evd_pending_live_events") == [(2,)]

        approved_packs = tmp_path / "approved-packs"
        pack_path = "information_domains/procurement"
        original, approved = pack_root / pack_path, approved_packs / pack_path
        approved.mkdir(parents=True)
        copyfile(original / "pack.yaml", approved / "pack.yaml")
        (approved / "src").symlink_to(original / "src", target_is_directory=True)
        documents = yaml.safe_load((original / "documents.yaml").read_text())
        entry, = [item for item in documents["documents"] if item["source_id"] == source.source_id]
        entry["live_parse"] = True
        (approved / "documents.yaml").write_text(yaml.safe_dump(documents))
        for expected in (1, 1, 0):
            replayed = subprocess.run([*command, str(approved_packs)], env=settings, capture_output=True, text=True,
                                      timeout=30)
            assert replayed.returncode == 0, replayed.stderr
            assert json.loads(replayed.stdout) == {"source_id": source.source_id, "processed": expected}
        assert rows(evidence, "SELECT version FROM evd_live_observations") == [(2,)]
        assert events(evidence, "evidence.extraction.revised.v1")
    finally:
        evidence.close()


def test_body_and_attachment_versions_feed_existing_evidence(pg_factory, env, tmp_path, source):
    database, store, _ = env
    dsns = pg_factory()
    db.migrate(dsns["owner"], EVIDENCE_MIGRATIONS)
    evidence = db.Database(dsns["app"])
    ingest = live_ingest(str(tmp_path / "objects"), str(REPO / "packs"))
    publisher_body, correction = body(), body(project="002")
    collector, transport = fetcher(env, {
        LIST: [page(), page(found=False), page(), page(), page()],
        BODY: [publisher_body, publisher_body, publisher_body, correction, correction],
        FILE: [file_response(404), file_response(200), file_response(200, contents=PDF + b" revised"),
               file_response(404), file_response(404)],
    })

    def deliver(event):
        payloads.validator(COMPLETED, {}).validate(event["data"])
        with reqctx.use(reqctx.service("evidence")), evidence.tx() as conn:
            ingest.on_completed(conn, event)

    window = Window(DAY, DAY)
    try:
        report = collector.run(source, window, max_pages=1)
        assert report.archived == 1 and report.gaps[0]["reason"] == "http_404"
        (initial,) = events(database, COMPLETED)
        assert initial["data"]["attachments"] == [
            {"part_key": "results", "name": "补充材料.pdf", "required": True, "status": "pending"}]
        assert store.get(initial["subject"]) == publisher_body.body
        deliver(initial)
        assert rows(evidence, """
            SELECT quality_status, warnings @> '[{"code":"REQUIRED_ATTACHMENT_PENDING"}]'::jsonb,
                   facts::text LIKE '%123456%'
            FROM evd_extraction_revisions
        """) == [("warning", True, True)]

        tomorrow = DAY + timedelta(days=1)
        # The notice is absent from the current list: the durable pending observation is claimed.
        recovered = collector.run(source, Window(tomorrow, tomorrow), max_pages=1, mode="incremental")
        assert recovered.archived == 1 and recovered.listed == 0
        _, supplement = events(database, COMPLETED)
        assert supplement["subject"] == initial["subject"]
        available = supplement["data"]["attachments"][0]
        assert available["status"] == "available" and store.get(available["artifact_ref"]) == PDF

        modified = collector.run(source, window, max_pages=1, refetch=True)
        assert modified.archived == 1
        corrected = collector.run(source, window, max_pages=1, refetch=True)
        assert corrected.archived == 1 and corrected.gaps[0]["reason"] == "http_404"
        assert collector.run(source, window, max_pages=1).archived == 0

        captured = events(database, COMPLETED)
        assert [e["data"]["observation_version"] for e in captured] == [1, 2, 3, 4]
        assert [e["data"]["notice_key"] for e in captured] == [NOTICE] * 4
        assert [p["status"] for e in captured for p in e["data"]["attachments"]] == [
            "pending", "available", "available", "pending"]
        assert captured[2]["data"]["attachments"][0]["content_hash"] != available["content_hash"]
        assert captured[3]["subject"] != initial["subject"]
        assert captured[3]["data"]["attachments"] == initial["data"]["attachments"]
        for event in [*captured[1:], captured[-1], supplement, initial]:
            deliver(event)
        assert rows(evidence, "SELECT version FROM evd_live_observations") == [(4,)]
        assert rows(evidence, """
            SELECT seq, receipts->0->>'status' FROM evd_snapshot_attachments p
            JOIN evd_snapshots s ON s.id = p.snapshot_id ORDER BY seq
        """) == [(1, "pending"), (2, "available"), (3, "available"), (4, "pending")]
        assert rows(database, """
            SELECT part_key, status, artifact_id IS NOT NULL FROM acq_observation_attachments ORDER BY version
        """) == [("results", "pending", False), ("results", "available", True),
                  ("results", "available", True), ("results", "pending", False)]
        assert rows(database, """
            SELECT attachment_version, status FROM acq_fetch_attempts
            WHERE attachment_part_key = 'results' ORDER BY id
        """) == [(1, 404), (1, 200), (2, 200), (4, 404), (4, 404)]
        assert len([request for request in transport.sent if request.url == FILE]) == 5
    finally:
        evidence.close()


def test_attachment_403_keeps_body_and_stops_source_until_review(env, source):
    database, _, _ = env
    collector, publisher = fetcher(env, {LIST: [page(), page()], BODY: [body(), body()],
                                         FILE: [file_response(403), file_response(200)]})
    window = Window(DAY, DAY)
    stopped = collector.run(source, window, max_pages=1)
    assert stopped.archived == 1 and stopped.blocked == "http_403"
    assert stopped.gaps[0]["part_key"] == "results"
    assert events(database, COMPLETED)[0]["data"]["attachments"][0]["status"] == "pending"
    with pytest.raises(SourceStopped):
        collector.run(source, window, max_pages=1)
    assert len(publisher.sent) == 3
    assert collector.run(source, window, max_pages=1, unblock=True).archived == 1
    assert [e["data"]["attachments"][0]["status"] for e in events(database, COMPLETED)] == ["pending", "available"]


def set_rights(database, source, action, epoch, **extra):
    command = {"action": action, "expected_epoch": epoch, "source_id": source.source_id, **extra}
    with psycopg.connect(os.environ["MIAOQIA_TEST_PG"], dbname=database.engine.url.database) as owner:
        return owner.execute("SELECT acq_rights.public_source_command(%s, %s, %s, %s)",
                             (Jsonb(command), "fixture", "synthetic authorization transition",
                              ids.new("evt"))).fetchone()[0]


def test_revocation_prevents_pending_download_even_with_unblock(env, source):
    database, _, _ = env
    collector, publisher = fetcher(env, {LIST: [page(), page()], BODY: [body(), body()],
                                         FILE: [file_response(404), file_response(200)]})
    window = Window(DAY, DAY)
    collector.run(source, window, max_pages=1)
    epoch = set_rights(database, source, "revoke_public", 0)
    report = collector.run(source, window, max_pages=1, unblock=True)
    assert report.blocked == "source_rights_revoked" and len(publisher.sent) == 3
    assert len(events(database, COMPLETED)) == 1
    set_rights(database, source, "restore_public", epoch, evidence_hash="sha256:" + "0" * 64)
    assert collector.run(source, window, max_pages=1).archived == 1
    assert len(events(database, COMPLETED)) == 2


def test_revocation_during_download_cannot_promote_pending_attachment(env, source, monkeypatch):
    from hashlib import sha256

    database, store, _ = env
    collector, publisher = fetcher(env, {LIST: [page()], BODY: [body()], FILE: [file_response(200)]})
    send = publisher.send

    def revoke_after_dispatch(request, timeout):
        reply = send(request, timeout)
        if request.url == FILE:
            set_rights(database, source, "revoke_public", 0)
        return reply

    monkeypatch.setattr(publisher, "send", revoke_after_dispatch)
    report = collector.run(source, Window(DAY, DAY), max_pages=1)
    assert report.archived == 1 and report.blocked == "source_rights_revoked"
    (initial,) = events(database, COMPLETED)
    assert initial["data"]["attachments"][0]["status"] == "pending"
    with pytest.raises(FileNotFoundError):
        store.get("raw/" + sha256(PDF).hexdigest())


def test_attachment_deadline_retains_verified_body_and_pending_part(env, source):
    database, store, clock = env
    publisher_body = body()
    collector, publisher = fetcher(env, {LIST: [page()], BODY: [publisher_body]})
    report = collector.run(source, Window(DAY, DAY), max_pages=1, deadline=clock.t + 3)
    # The declared site's two-second interval allows list and body but no third dispatch.
    assert report.truncated and report.archived == 1
    assert report.gaps[0]["reason"] == "attachment_deadline"
    assert [request.url for request in publisher.sent] == [LIST, BODY]
    (initial,) = events(database, COMPLETED)
    assert store.get(initial["subject"]) == publisher_body.body
    assert initial["data"]["attachments"][0]["status"] == "pending"


def test_archive_spool_restarts_pending_attachment_for_the_same_body(env, source, tmp_path, monkeypatch):
    database, store, clock = env
    spool_root = tmp_path / "spool"
    initial = Script({LIST: [page()], BODY: [body()], FILE: [file_response(200)]})
    put = store.put

    def outage(key, content):
        if content == PDF:
            raise OSError("fixture attachment archive unavailable")
        put(key, content)

    with monkeypatch.context() as patch:
        patch.setattr(store, "put", outage)
        report = Fetcher(database, store, initial, clock=clock, spool=ArchiveSpool(spool_root)).run(
            source, Window(DAY, DAY), max_pages=1,
        )
    assert report.deferred == "archive_object_pending" and len(initial.sent) == 3
    (body_event,) = events(database, COMPLETED)
    assert body_event["data"]["attachments"][0]["status"] == "pending"
    assert rows(database, "SELECT kind, state FROM acq_request_tasks ORDER BY kind") == [
        ("attachment", "staged"), ("body", "settled"), ("list", "settled"),
    ]

    offline = Script({})
    recovered = Fetcher(database, store, offline, clock=clock, spool=ArchiveSpool(spool_root)).run(
        source, Window(DAY, DAY), max_pages=1,
    )
    assert recovered.run_id == report.run_id and recovered.archived == 1 and offline.sent == []
    _, original = events(database, COMPLETED)
    assert original["data"]["observation_version"] == 2 and original["subject"] == body_event["subject"]
    attachment = original["data"]["attachments"][0]
    assert attachment["status"] == "available" and store.get(attachment["artifact_ref"]) == PDF
    assert rows(database, "SELECT kind, state FROM acq_request_tasks ORDER BY kind") == [
        ("attachment", "settled"), ("body", "settled"), ("list", "settled"),
    ]
    assert list(spool_root.glob("[0-9a-f]*")) == []


def test_anhui_rejects_unbound_paths_and_pins_response_targets(env, source):
    database, _, _ = env
    candidate = body(extra=f"""
       <a href="{FILE}">ordinary reference</a>
       <a download href="https://other.test/mock-files/{GUID}/3/results.pdf">foreign</a>
       <a download href="/mock-files/{OTHER}/3/unrelated.pdf">another notice</a>
       <a download href="/mock-files/{GUID}/2/unrelated.pdf">another stage</a>
       <a download href="/mock-files/{GUID}/3/../3/unrelated.pdf">traversal</a>
    """)
    ref = DetailRef(NOTICE, f"{BASE}/jsgc/newDetail?guid={GUID}", DAY)
    assert AnhuiGgzyConstruction().attachment_fetches("3:1", ref, candidate) == ()
    assert [p.artifact_key for p in source.handler.attachment_fetches("3:1", ref, candidate)] == ["results"]
    collector, publisher = fetcher(env, {
        LIST: [page(), page()], BODY: [candidate, FetchResponse(307, "text/html", b"", BODY, redirect_to="/other-body")],
        FILE: [file_response(302, target=f"/mock-files/{OTHER}/3/results.pdf")],
    })
    window = Window(DAY, DAY)
    assert collector.run(source, window, max_pages=1).gaps[0]["reason"] == "part_redirect_disallowed"
    assert collector.run(source, window, max_pages=1).gaps[0]["reason"] == "part_redirect_disallowed"
    assert [request.url for request in publisher.sent] == [LIST, BODY, FILE, LIST, BODY]
    assert len(events(database, COMPLETED)) == 1


def test_available_attachment_failure_preserves_history_and_queues_recovery(env, source):
    database, store, _ = env
    collector, _ = fetcher(env, {LIST: [page(), page(), page()], BODY: [body(), body(), body()],
                                 FILE: [file_response(200), file_response(404), file_response(200)]})
    window = Window(DAY, DAY)
    assert collector.run(source, window, max_pages=1).archived == 2
    report = collector.run(source, window, max_pages=1, refetch=True)
    assert report.archived == 1 and report.gaps[0]["reason"] == "http_404"
    assert collector.run(source, window, max_pages=1).archived == 1
    captured = [e["data"]["attachments"][0] for e in events(database, COMPLETED)]
    assert [part["status"] for part in captured] == ["pending", "available", "pending", "available"]
    assert captured[1] == captured[3]
    assert store.get(captured[1]["artifact_ref"]) == PDF


def test_freeze_binds_available_attachment_to_its_observation(env, source):
    database, store, _ = env
    collector, _ = fetcher(env, {LIST: [page(), page()], BODY: [body(), body()],
                                 FILE: [file_response(404), file_response(200)]})
    window = Window(DAY, DAY)
    for _ in range(2):
        collector.run(source, window, max_pages=1, refetch=True)
    pending = ObservationRef(source.source_id, NOTICE, 1, "results")
    available = replace(pending, observation_version=2)
    service = FrozenBatches(database, store, authorize=lambda operation, original: original.ref == available)
    with reqctx.use(reqctx.service("frozen-attachment-test")):
        with pytest.raises(FrozenBatchDenied):
            service.freeze((pending,))
        frozen = FrozenBatch.parse(service.freeze((available,)).dto())
        assert frozen.items[0].ref.part_key == "results"
        assert service.read(frozen, 0) == PDF
