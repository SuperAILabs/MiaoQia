"""Live ingest of acquisition.fetch.completed.v1 on a real PostgreSQL, using responses recorded from the pilot sources."""
from __future__ import annotations

import hashlib
import json
import os
from dataclasses import replace
from pathlib import Path
from unittest.mock import Mock
from uuid import uuid4

import pytest
from sqlalchemy import text
from sqlalchemy.exc import OperationalError

from miaoqia.evidence import MIGRATIONS, live_ingest
from miaoqia.evidence.application.live import COMPLETED, LEGACY_COMPLETED
from miaoqia.evidence.extensions.packs import DocumentAdapter, load_documents
from miaoqia.evidence.replay import main as replay_main
from miaoqia.kernel import db, ids, reqctx
from miaoqia.kernel.consumer import Consumer
from miaoqia.kernel.objectstore import FilesystemObjectStore, open_object_store

REPO = Path(__file__).resolve().parents[4]
FIXTURES = REPO / "packs/information_domains/procurement/tests/fixtures/sources"


@pytest.fixture
def live(pg_factory, tmp_path):
    dsns = pg_factory()
    db.migrate(dsns["owner"], MIGRATIONS)
    database = db.Database(dsns["app"])
    objects = tmp_path / "objects"
    ingest = live_ingest(str(objects), str(REPO / "packs"))
    yield database, ingest, FilesystemObjectStore(objects)
    database.close()


def archived(store, source_id: str, fixture: str, url: str) -> dict:
    """Archive like acquisition does and return the fetch.completed envelope."""
    raw = (FIXTURES / fixture).read_bytes()
    digest = hashlib.sha256(raw).hexdigest()
    store.put(f"raw/{digest}", raw)
    data = {"source_id": source_id, "artifact_ref": f"raw/{digest}", "rights_tags": [f"public:{source_id}"],
            "content_hash": f"sha256:{digest}", "fetched_at": "2026-11-11T08:00:00Z", "source_url": url}
    return {"source": "miaoqia/acquisition", "subject": data["artifact_ref"], "type": LEGACY_COMPLETED,
            "data": data, "id": ids.new("evt"), "specversion": "1.0", "time": data["fetched_at"],
            "datacontenttype": "application/json", "miaoqiazone": "public"}


def deliver(database, ingest, env):
    with reqctx.use(reqctx.service("evidence")), database.tx() as conn:
        ingest.on_completed(conn, env)


def rows(database, sql):
    with reqctx.use(reqctx.service("test")), database.tx() as conn:
        return [tuple(r) for r in conn.execute(text(sql))]


def deny_original_writes(ingest, monkeypatch):
    put = ingest.pipeline.store.put

    def put_derived(ref, raw):
        if ref.startswith("raw/"):
            raise PermissionError("evidence has read-only access to originals")
        put(ref, raw)

    monkeypatch.setattr(ingest.pipeline.store, "put", put_derived)


CASES = [
    ("anhui-ggzy-construction", "anhui_ggzy_detail_tender.html",
     "https://ggzy.ah.gov.cn/jsgc/newDetail?guid=15a09741-1789-4d86-96e2-b67972e7f5a8&tenderProjectType=1&bulletinNature=1",
     "proc.tender-notice@0.5.3"),
    ("anhui-government-procurement-spa", "anhui_ccgp_detail.json",
     "https://www.ccgp-anhui.gov.cn/portal/detail?articleId=ZW2UGlPzLzKA0ISW4BGnSA%3D%3D&parentId=541080",
     "proc.tender-notice@0.5.3"),
    ("national-public-resources", "national_detail_body.html",
     "https://www.ggzy.gov.cn/information/deal/html/a/420000/0201/20261111/f391aca0239344f9172d9c50c88e35d4.html",
     "proc.result-notice@0.2.1"),
]


@pytest.mark.parametrize("source_id, fixture, url, extractor", CASES)
def test_fetch_completed_becomes_snapshot_and_revision(live, source_id, fixture, url, extractor, monkeypatch):
    database, ingest, store = live
    env = archived(store, source_id, fixture, url)
    deny_original_writes(ingest, monkeypatch)
    deliver(database, ingest, env)
    (snap,) = rows(database, "SELECT source_id, source_key, artifact_ref, rights_tags, media_type FROM evd_snapshots")
    media = "application/json" if fixture.endswith(".json") else "text/html"
    assert snap == (source_id, url, env["data"]["artifact_ref"], [f"public:{source_id}"], media)
    assert rows(database, "SELECT extractor FROM evd_extraction_revisions") == [(extractor,)]
    # The original is archived unchanged under the same content-addressed key as acquisition's.
    assert store.get(env["data"]["artifact_ref"]) == (FIXTURES / fixture).read_bytes()
    # Redelivery is idempotent.
    deliver(database, ingest, env)
    assert rows(database, "SELECT count(*) FROM evd_snapshots") == [(1,)]
    assert rows(database, "SELECT count(*) FROM evd_extraction_revisions") == [(1,)]


def test_rejects_hash_mismatch_and_foreign_source(live):
    database, ingest, store = live
    env = archived(store, *CASES[0][:3])
    forged = {**env, "data": {**env["data"], "content_hash": "sha256:" + "0" * 64}}
    with pytest.raises(ValueError, match="content hash"):
        deliver(database, ingest, forged)
    with pytest.raises(ValueError, match="lineage"):
        deliver(database, ingest, {**env, "source": "miaoqia/evidence"})
    assert rows(database, "SELECT count(*) FROM evd_snapshots") == [(0,)]


@pytest.mark.parametrize("proven", [True, False])
def test_legacy_link_requires_matching_historical_bytes(live, proven):
    database, ingest, store = live
    source, fixture, url, _ = CASES[0]
    legacy = archived(store, source, fixture, url)
    deliver(database, ingest, legacy)
    previous = rows(database, "SELECT id, source_key FROM evd_snapshots")[0]
    raw = store.get(legacy["subject"]).replace(b"<", b"<!-- synthetic change --><", 1)
    digest = hashlib.sha256(raw).hexdigest()
    key = f"raw/{digest}"
    store.put(key, raw)
    data = dict(legacy["data"], notice_key="guid:1", observation_version=1, artifact_ref=key,
                source_url="https://ggzy.ah.gov.cn/jsgc/replacement", content_hash=f"sha256:{digest}",
                legacy_artifact={"source_url": url, "content_hash": legacy["data"]["content_hash"] if proven
                                 else f"sha256:{'0' * 64}"})
    current = dict(legacy, type=COMPLETED, subject=key, data=data)
    deliver(database, ingest, current)
    deliver(database, ingest, legacy)
    (stream,) = rows(database, "SELECT stream_key FROM evd_live_observations")
    assert stream == (url if proven else "notice:guid:1",)
    snapshots = rows(database, "SELECT id, source_key FROM evd_snapshots")
    assert len(snapshots) == 2 and previous in snapshots
    assert rows(database, "SELECT count(*) FROM evd_evidence_refs WHERE snapshot_id = "
                          "(SELECT id FROM evd_snapshots ORDER BY observed_at, id LIMIT 1)")[0][0] > 0


def test_live_ingest_disabled_without_object_store():
    assert live_ingest("", str(REPO / "packs")) is None


def test_required_attachment_recovery_appends_snapshot_while_body_remains_available(live):
    database, ingest, store = live
    env = archived(store, *CASES[0][:3])
    part = {"part_key": "results", "name": "补充材料.pdf", "required": True}
    data = dict(env["data"], notice_key="fixture:1", observation_version=1,
                attachments=[dict(part, status="pending")])
    pending = dict(env, type=COMPLETED, data=data)
    deliver(database, ingest, pending)
    assert rows(database, """
        SELECT quality_status, warnings @> '[{"code":"REQUIRED_ATTACHMENT_PENDING"}]'::jsonb
        FROM evd_extraction_revisions
    """) == [("warning", True)]
    first_snapshot = rows(database, "SELECT id FROM evd_snapshots")[0][0]

    original = b"%PDF-synthetic attachment"
    digest = hashlib.sha256(original).hexdigest()
    ref = f"raw/{digest}"
    available = dict(part, status="available", artifact_ref=ref, content_hash=f"sha256:{digest}",
                     media_type="application/pdf", size_bytes=len(original))
    supplement = dict(pending, data=dict(data, observation_version=2, attachments=[available]))
    with pytest.raises(FileNotFoundError):
        deliver(database, ingest, supplement)
    assert rows(database, "SELECT version FROM evd_live_observations") == [(1,)]

    store.put(ref, original)
    for event in (supplement, supplement, pending):
        deliver(database, ingest, event)
    snapshots = rows(database, """
        SELECT snapshot_id, receipts->0->>'status'
        FROM evd_snapshot_attachments JOIN evd_snapshots ON id = snapshot_id
        ORDER BY seq
    """)
    assert snapshots[0] == (first_snapshot, "pending")
    assert len(snapshots) == 2 and snapshots[1][0] != first_snapshot and snapshots[1][1] == "available"


def set_live_parse(ingest, source, enabled):
    adapter = ingest.adapters.get(source) or DocumentAdapter(source, "text/html", bytes.decode)
    ingest.adapters[source] = replace(adapter, live_parse=enabled)


def observation(env, version, *, ref=None, **details):
    ref = ref or env["subject"]
    data = dict(env["data"], notice_key="fixture:1", observation_version=version,
                artifact_ref=ref, content_hash=f"sha256:{ref.removeprefix('raw/')}", **details)
    return dict(env, id=ids.new("evt"), type=COMPLETED, subject=ref, data=data)


def drain(database, ingest, source):
    with reqctx.use(reqctx.service("evidence")), database.tx() as conn:
        return ingest.drain_one(conn, source)


def test_assessment_reports_admitted_inputs_without_materializing(live, monkeypatch, capsys):
    database, ingest, store = live
    source = "m2-bid-list"
    url = "https://gzc.m2.com.cn/detail/notice?id=ASSESSMENT-URL&platformKey=M2"
    clock = "2026-11-11T08:00:00Z"

    def stage(version, details):
        raw = json.dumps({"data": details}, ensure_ascii=False).encode()
        digest = hashlib.sha256(raw).hexdigest()
        ref = f"raw/{digest}"
        store.put(ref, raw)
        data = {"source_id": source, "artifact_ref": ref, "rights_tags": [f"public:{source}"],
                "content_hash": f"sha256:{digest}", "fetched_at": clock, "source_url": url}
        event = observation({"data": data, "subject": ref, "source": "miaoqia/acquisition", "specversion": "1.0",
                             "time": clock, "datacontenttype": "application/json", "miaoqiazone": "public"}, version)
        deliver(database, ingest, event)
        return event

    monkeypatch.setenv("MIAOQIA_EVIDENCE_DB_DSN", database.engine.url.render_as_string(hide_password=False))
    monkeypatch.setenv("MIAOQIA_EVIDENCE_OBJECT_STORE_DIR", str(store.root))
    command = ["--packs", str(REPO / "packs"), "--assess-pending-source"]
    assert replay_main([*command, source]) == 1
    assert json.loads(capsys.readouterr().out) == {"source_id": source, "observations": []}

    title = "合成设备采购公开招标公告"
    accepted_event = stage(1, {"noticeName": title, "context": "<p>项目编号：ASSESSMENT-CODE</p><p>采购人：合成验收单位</p>"})
    assert replay_main([*command, source]) == 0
    (positive,) = json.loads(capsys.readouterr().out)["observations"]
    unmatched = stage(2, {"noticeName": "合成材料说明", "context": "<p>两种材料的规格说明。</p>"})
    rejected_event = stage(3, {"noticeName": title, "files": []})
    keys_before = sorted(p.relative_to(store.root) for p in store.root.rglob("*") if p.is_file())
    assert replay_main([*command, source, "--limit", "2"]) == 1
    output = capsys.readouterr()
    assert not output.err and all(value not in output.out for value in (title, "ASSESSMENT-URL", "ASSESSMENT-CODE"))
    assessed, nonmatching = json.loads(output.out)["observations"]
    assert assessed == positive
    assert (assessed["event_id"], assessed["status"], assessed["content_hash"]) == (
        accepted_event["id"], "assessed", accepted_event["data"]["content_hash"])
    assert nonmatching["event_id"] == unmatched["id"] and nonmatching["status"] == "assessed"
    assert nonmatching["field_evidence"] == "no_matching_extractor" and nonmatching["revisions"] == []
    assert assessed["field_evidence"] == "available"
    revision, = assessed["revisions"]
    assert revision["extractor"] == "proc.tender-notice@0.5.3"
    assert revision["fact_count"] == 1 and 2 <= revision["fact_evidence_count"] <= revision["evidence_count"]
    assert revision["validation_error_count"] == 0
    assert replay_main([*command, source]) == 1
    *_, rejected = json.loads(capsys.readouterr().out)["observations"]
    assert (rejected["event_id"], rejected["status"]) == (rejected_event["id"], "input_rejected")
    assert not drain(database, ingest, source)
    assert rows(database, "SELECT count(*) FROM evd_pending_live_events") == [(3,)]
    for table in ("evd_live_observations", "evd_snapshots", "evd_extraction_revisions", "evd_evidence_refs", "kernel_outbox"):
        assert rows(database, f"SELECT count(*) FROM {table}") == [(0,)]
    assert sorted(p.relative_to(store.root) for p in store.root.rglob("*") if p.is_file()) == keys_before

    assert replay_main([*command, "anhui-ggzy-construction"]) == 2
    assert json.loads(capsys.readouterr().err) == {"error": "source_not_paused"}


@pytest.mark.parametrize("entrypoint", ["adapter", "selection_adapter", "browser_projection"])
@pytest.mark.parametrize("configured, expected", [("", None), ("true", True), ("false", False),
                                                   ('"true"', ValueError), ("null", ValueError), ("1", ValueError)])
def test_document_adapter_live_parse_admission(tmp_path, entrypoint, configured, expected):
    pack = tmp_path / "information_domains" / "fixture"
    source_dir = pack / "src"
    source_dir.mkdir(parents=True)
    module = f"live_fixture_{uuid4().hex}"
    (source_dir / f"{module}.py").write_text("def to_html(raw): return raw.decode()\n")
    (pack / "pack.yaml").write_text("pack: {provides: {documents: docs.yaml}}\n")
    binding = "true" if entrypoint == "browser_projection" else f"{module}:to_html"
    fields = f"documents:\n- source_id: fixture\n  media_type: text/html\n  {entrypoint}: {binding}\n"
    (pack / "docs.yaml").write_text(fields + (f"  live_parse: {configured}\n" if configured else ""))
    if expected is ValueError:
        with pytest.raises(ValueError, match="live_parse must be a boolean"):
            load_documents(tmp_path)
    elif entrypoint != "adapter" and not configured:
        with pytest.raises(ValueError, match=f"{entrypoint} requires explicit live_parse"):
            load_documents(tmp_path)
    else:
        adapter = load_documents(tmp_path)["fixture"]
        assert adapter.live_parse is expected
        assert (adapter.to_html is not None) == (entrypoint == "adapter")
        assert (adapter.to_selected_html is not None) == (entrypoint == "selection_adapter")
        assert adapter.browser_projection is (entrypoint == "browser_projection")


def test_defer_inbox_is_atomic_and_preserves_envelope(live, monkeypatch):
    database, ingest, store = live
    source = "xinjiang-government-procurement-home"
    url = "http://www.ccgp-xinjiang.gov.cn/site/detail?parentId=1&articleId=fixture"
    event = observation(archived(store, source, "synthetic_xinjiang_shell.html", url), 1)
    read = Mock(side_effect=AssertionError("held observation must not open object storage"))
    monkeypatch.setattr(ingest.store, "get", read)
    message = Mock()
    message.value.return_value = json.dumps(event).encode()
    message.topic.return_value = "public.acquisition"
    dead_letter = Mock()

    def fail_after_enqueue(conn, env):
        ingest.on_completed(conn, env)
        raise RuntimeError("fail after enqueue")

    consumer = Consumer("evidence.live", "evidence", "unused", ["public.acquisition"],
                        {COMPLETED: fail_after_enqueue}, database, max_attempts=1)
    monkeypatch.setattr(consumer, "_dead_letter", dead_letter)
    consumer._handle(message, Mock(), "dlq.evidence")
    assert str(dead_letter.call_args.args[-1]) == "fail after enqueue"
    assert rows(database, "SELECT event_id FROM kernel_inbox") == []
    assert rows(database, "SELECT event_id FROM evd_pending_live_events") == []

    consumer.handlers[COMPLETED] = ingest.on_completed
    dead_letter.reset_mock()
    consumer._handle(message, Mock(), "dlq.evidence")
    consumer._handle(message, Mock(), "dlq.evidence")
    deliver(database, ingest, event)
    dead_letter.assert_not_called()
    read.assert_not_called()
    assert not drain(database, ingest, source)
    assert rows(database, "SELECT event_id FROM kernel_inbox") == [(event["id"],)]
    assert rows(database, "SELECT event_id, source_id, envelope FROM evd_pending_live_events") == [
        (event["id"], source, event)]
    for table in ("evd_live_observations", "evd_snapshots", "evd_extraction_revisions", "kernel_outbox"):
        assert rows(database, f"SELECT count(*) FROM {table}") == [(0,)]


@pytest.fixture(params=["filesystem", "s3"])
def replay_store(request, live, monkeypatch):
    database, ingest, store = live
    if request.param == "filesystem":
        yield database, ingest, store, lambda ref, raw: (store.root / ref).write_bytes(raw)
        return
    endpoint = os.environ.get("MIAOQIA_TEST_FROZEN_S3_ENDPOINT")
    if not endpoint:
        pytest.skip("set MIAOQIA_TEST_FROZEN_S3_ENDPOINT for a disposable S3-compatible service")
    import boto3

    monkeypatch.setenv("AWS_ENDPOINT_URL_S3", endpoint)
    bucket = f"mq-evidence-replay-{uuid4().hex}"
    client = boto3.client("s3", endpoint_url=endpoint, region_name="us-east-1")
    client.create_bucket(Bucket=bucket)
    try:
        ingest = live_ingest(f"s3://{bucket}", str(REPO / "packs"))
        yield database, ingest, ingest.store, lambda ref, raw: client.put_object(Bucket=bucket, Key=ref, Body=raw)
    finally:
        for page in client.get_paginator("list_objects_v2").paginate(Bucket=bucket):
            for item in page.get("Contents", []):
                client.delete_object(Bucket=bucket, Key=item["Key"])
        client.delete_bucket(Bucket=bucket)
        client.close()


def test_drain_order_legacy_reversion_and_retry(replay_store, monkeypatch):
    database, ingest, store, overwrite = replay_store
    source, fixture, url, _ = CASES[0]
    legacy = archived(store, source, fixture, url)
    raw = store.get(legacy["subject"])
    changed = raw.replace("EPC公开招标公告".encode(), "EPC公开招标公告（测试变更）".encode(), 1)
    changed_ref = f"raw/{hashlib.sha256(changed).hexdigest()}"
    first = observation(legacy, 1, ref=changed_ref,
                        legacy_artifact={"source_url": url, "content_hash": legacy["data"]["content_hash"]})
    reverted = observation(legacy, 2)
    duplicate = observation(legacy, 2)
    deny_original_writes(ingest, monkeypatch)

    set_live_parse(ingest, source, False)
    for event in (legacy, first):
        deliver(database, ingest, event)
    set_live_parse(ingest, source, True)
    for event in (reverted, duplicate):
        deliver(database, ingest, event)
    assert rows(database, "SELECT id FROM evd_snapshots") == []
    assert drain(database, ingest, source)
    pending_sql = "SELECT event_id FROM evd_pending_live_events ORDER BY receipt_seq"
    assert rows(database, pending_sql) == [(event["id"],) for event in (first, reverted, duplicate)]
    for error in (FileNotFoundError, ValueError):
        with pytest.raises(error):
            drain(database, ingest, source)
        assert rows(database, pending_sql) == [(event["id"],) for event in (first, reverted, duplicate)]
        assert rows(database, "SELECT version FROM evd_live_observations") == []
        if error is FileNotFoundError:
            overwrite(changed_ref, b"tampered")
    overwrite(changed_ref, changed)
    assert drain(database, ingest, source)
    assert rows(database, "SELECT version FROM evd_live_observations") == [(1,)]
    assert drain(database, ingest, source)
    assert drain(database, ingest, source)
    assert not drain(database, ingest, source)
    assert rows(database, "SELECT version FROM evd_live_observations") == [(2,)]
    assert rows(database, "SELECT source_key, artifact_ref FROM evd_snapshots ORDER BY seq") == [
        (url, legacy["subject"]), (url, changed_ref), (url, legacy["subject"])]


@pytest.mark.parametrize("contender", ["drain", "live"])
def test_live_and_drain_share_transaction_source_fence(live, contender):
    database, ingest, store = live
    source = CASES[0][0]
    legacy = archived(store, *CASES[0][:3])
    set_live_parse(ingest, source, False)
    deliver(database, ingest, legacy)
    set_live_parse(ingest, source, True)
    event = observation(legacy, 1)
    with reqctx.use(reqctx.service("evidence")), database.tx() as leader:
        assert ingest.drain_one(leader, source)
        with pytest.raises(OperationalError, match="lock timeout"), database.tx() as follower:
            follower.execute(text("SET LOCAL lock_timeout = '100ms'"))
            if contender == "drain":
                ingest.drain_one(follower, source)
            else:
                ingest.on_completed(follower, event)
        unmanaged = archived(store, *CASES[1][:3])
        unmanaged.pop("id")
        deliver(database, ingest, unmanaged)
    assert not drain(database, ingest, source)
    deliver(database, ingest, event)
    assert rows(database, "SELECT version FROM evd_live_observations") == [(1,)]


