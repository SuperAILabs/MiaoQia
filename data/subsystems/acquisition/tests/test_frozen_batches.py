"""Frozen original authorization and recovery using real acquisition rows and synthetic HTTP bodies."""
from __future__ import annotations

import json
import os
import subprocess
import sys
from contextlib import ExitStack, closing
from dataclasses import replace
from datetime import timedelta
from urllib.parse import urlsplit
from uuid import uuid4

import psycopg
import pytest

from miaoqia.acquisition.api.frozen_batches import (
    FrozenBatch, FrozenBatchDenied, FrozenObjectError, FrozenOriginal, FrozenRecords, ObservationRef,
)
from miaoqia.acquisition.api.handlers import FetchResponse, Window
from miaoqia.acquisition.application import sources
from miaoqia.acquisition.application.fetch import NOTICE_ARCHIVED
from miaoqia.acquisition.application.frozen_batches import FrozenBatches
from miaoqia.acquisition.infrastructure.privileges import provision_frozen_reader
from miaoqia.kernel import db, reqctx
from miaoqia.kernel.objectstore import FilesystemObjectStore, ObjectConflict, S3ObjectStore, s3_client

from test_fetch import DAY, LIST_URL, REPO, SOURCE, FakeClock, env, events, fetcher, listing, ok  # noqa: F401
from test_public_source_rights import revoke, write_command


A, B = b"<html>test A</html>", b"<html>test B</html>"
REFS = tuple(ObservationRef(SOURCE.source_id, "notice", version) for version in (1, 2, 3))
PACKS = REPO / "packs"
HUNAN = "hunan-ggzy-construction"
SECTION = "11111111-2222-4333-8444-555555555555"
TARGET = ObservationRef(HUNAN, "aaaaaaaa-2222-4333-8444-555555555555" + SECTION, 1)
NEIGHBOR = ObservationRef(HUNAN, "bbbbbbbb-2222-4333-8444-555555555555" + SECTION, 1)
REVISED = replace(TARGET, observation_version=2)
CALLER = reqctx.Context(reqctx.Principal("frozen-test-fixture", "service_account"),
                        tenant="test-tenant", cell="fixture", purpose="simulation")


class RecordingStore(FilesystemObjectStore):
    def __init__(self, root, trace):
        super().__init__(root)
        self.trace = trace

    def get(self, key, **kwargs):
        self.trace.append(("get", key))
        return super().get(key, **kwargs)

    def put(self, key, data):
        self.trace.append(("put", key))
        return super().put(key, data)


def authorize_fixture(operation, original):
    return operation in {"freeze", "read", "restore"} and original.ref in REFS


def authorize_record_fixture(operation, record):
    return (operation in {"freeze-records", "read-record"} and reqctx.current() == CALLER
            and record.ref in {TARGET, REVISED})


def record_access_fixture():
    return CALLER, authorize_record_fixture


def restore_access_fixture():
    def authorize(operation, record):
        return operation == "restore-record-original" and reqctx.current() == CALLER and record.ref == TARGET

    return CALLER, authorize


@pytest.fixture
def archived(env):
    database, store, _ = env
    _collect_single(env)
    with reqctx.use(reqctx.service("frozen-batch-test")):
        yield database, store


def _collect_single(env):
    f, _ = fetcher(env, {LIST_URL: [listing("notice")] * 3,
                          "https://src.example/d/notice": [ok(A), ok(B), ok(A)]})
    for _ in REFS:
        assert f.run(SOURCE, Window(DAY, DAY), max_pages=1, refetch=True).archived == 1


def test_pins_reversion_versions_and_checks_current_rights_around_fetch(archived):
    database, store = archived
    trace = []

    def authorize(operation, original):
        trace.append((operation, original.ref.observation_version))
        return authorize_fixture(operation, original)

    service = FrozenBatches(database, RecordingStore(store.root, trace), authorize=authorize)
    batch = FrozenBatch.parse(json.loads(json.dumps(service.freeze(REFS).dto())))
    assert tuple(item.ref for item in batch.items) == REFS
    assert batch.items[0].content_hash == batch.items[2].content_hash != batch.items[1].content_hash
    assert batch.items[0].dto()["artifact_ref"] == batch.items[0].content_hash.replace("sha256:", "raw/")
    trace.clear()
    assert [service.read(batch, item) for item in (0, 1, 2)] == [A, B, A]
    for index, version in enumerate((1, 2, 3)):
        before, access, after = trace[index * 3:index * 3 + 3]
        assert before == after == ("read", version)
        assert access == ("get", batch.items[index].content_hash.replace("sha256:", "raw/"))


def test_manifest_and_catalog_checks_precede_any_object_access(archived, tmp_path):
    database, store = archived
    batch = FrozenBatches(database, store, authorize=authorize_fixture).freeze(REFS)
    trace = []
    service = FrozenBatches(database, RecordingStore(store.root, trace), authorize=authorize_fixture)
    backup = RecordingStore(tmp_path / "backup", trace)
    original, other, _ = batch.items
    for poisoned in (
        replace(original, content_hash=other.content_hash),
        replace(original, ref=replace(original.ref, notice_key="another-notice")),
        replace(original, ref=replace(original.ref, source_id="another-source")),
        replace(original, ref=replace(original.ref, observation_version=4)),
    ):
        forged = FrozenBatch((poisoned,))
        with pytest.raises(FrozenBatchDenied):
            service.read(forged, 0)
        with pytest.raises(FrozenBatchDenied):
            service.restore(forged, 0, backup=backup)

    with pytest.raises(ValueError):
        FrozenBatch.parse({"schema_version": 1, "items": [{**original.dto(), "artifact_ref": "raw/arbitrary"}]})
    with pytest.raises(ValueError):
        FrozenBatch.parse({"schema_version": 1, "items": [original.dto(), original.dto()]})
    with pytest.raises(ValueError):
        service.read(batch, -1)
    assert trace == []


@pytest.mark.parametrize("decision", [False, 1, RuntimeError("policy unavailable")])
def test_denial_prevents_original_and_backup_reads(archived, tmp_path, decision):
    database, store = archived
    batch = FrozenBatches(database, store, authorize=authorize_fixture).freeze(REFS)
    trace = []

    def authorize(operation, original):
        if isinstance(decision, Exception):
            raise decision
        return decision

    service = FrozenBatches(database, RecordingStore(store.root, trace), authorize=authorize)
    with pytest.raises(FrozenBatchDenied):
        service.freeze(REFS)
    with pytest.raises(FrozenBatchDenied):
        service.read(batch, 0)
    with pytest.raises(FrozenBatchDenied):
        service.restore(batch, 0, backup=RecordingStore(tmp_path / "backup", trace))
    assert trace == []


def test_revocation_after_verification_prevents_return_and_restore_write(archived, tmp_path):
    database, store = archived
    batch = FrozenBatches(database, store, authorize=authorize_fixture).freeze(REFS)
    key = batch.items[0].content_hash.replace("sha256:", "raw/")
    trace = []
    decisions = iter((True, False, True, True, True, False))

    def authorize(operation, original):
        trace.append(("authorize", operation))
        return next(decisions)

    service = FrozenBatches(database, RecordingStore(store.root, trace), authorize=authorize)
    with pytest.raises(FrozenBatchDenied):
        service.read(batch, 0)
    assert trace == [("authorize", "read"), ("get", key), ("authorize", "read")]

    backup = FilesystemObjectStore(tmp_path / "backup")
    backup.put(key, A)
    (store.root / key).unlink()
    trace.clear()
    with pytest.raises(FrozenBatchDenied):
        service.restore(batch, 0, backup=RecordingStore(backup.root, trace))
    assert trace == [("authorize", "restore"), ("get", key), ("authorize", "restore")] * 2
    assert not (store.root / key).exists()


def test_shared_hash_does_not_grant_other_observations_and_batch_rechecks_earlier_items(archived):
    database, store = archived
    batch = FrozenBatches(database, store, authorize=authorize_fixture).freeze(REFS)
    trace = []
    allowed = {REFS[0], REFS[1]}

    def authorize(operation, original):
        if operation == "freeze" and original.ref == REFS[1]:
            allowed.discard(REFS[0])
        return original.ref in allowed

    service = FrozenBatches(database, RecordingStore(store.root, trace), authorize=authorize)
    assert service.read(batch, 0) == A
    trace.clear()
    with pytest.raises(FrozenBatchDenied):
        service.read(batch, 2)
    assert trace == []
    with pytest.raises(FrozenBatchDenied):
        service.freeze(REFS[:2])
    assert len(trace) == 2


def test_restores_hash_verified_bytes_and_preserves_observations(archived, tmp_path):
    database, store = archived
    service = FrozenBatches(database, store, authorize=authorize_fixture)
    batch = service.freeze(REFS)
    before = events(database, NOTICE_ARCHIVED)
    key = batch.items[0].content_hash.replace("sha256:", "raw/")
    backup = FilesystemObjectStore(tmp_path / "backup")
    (store.root / key).unlink()

    with pytest.raises(FrozenObjectError, match="^original_missing$"):
        service.read(batch, 0)
    with pytest.raises(FrozenObjectError, match="^backup_missing$"):
        service.restore(batch, 0, backup=backup)
    backup.put(key, B)
    with pytest.raises(FrozenObjectError, match="^object_integrity$"):
        service.restore(batch, 0, backup=backup)
    assert not (store.root / key).exists()

    (backup.root / key).write_bytes(A)
    (store.root / key).write_bytes(B)
    with pytest.raises(FrozenObjectError, match="^object_integrity$"):
        service.restore(batch, 0, backup=backup)
    assert store.get(key) == B
    (store.root / key).unlink()
    assert service.restore(batch, 0, backup=backup) == "restored"
    assert service.restore(batch, 0, backup=FilesystemObjectStore(tmp_path / "no-backup")) == "already_present"
    assert (service.read(batch, 0), service.read(batch, 2)) == (A, A)
    assert service.freeze(REFS) == batch
    assert events(database, NOTICE_ARCHIVED) == before


def _collect_hunan(env):
    source = sources.load(PACKS)[HUNAN]
    handler = source.handler
    channel = handler.channels[0]
    window = Window(DAY, DAY)
    listing_url = handler.list_request(channel, window, 1).url
    url = handler.group_request(channel, SECTION).url

    def response(url, data):
        return FetchResponse(200, "application/json", json.dumps({"code": 200, "success": True, "data": data}).encode(), url)

    def member(ref, body, *, day=DAY):
        return {"id": ref.notice_key, "constructionSectionId": SECTION, "bulletinType": "ZHAOBIAO_NOTICE",
                "noticeName": f"Title {body}", "noticeContent": f"<p>{body}</p>", "noticeSendTime": f"{day} 17:31:04"}

    def group(body):
        outside = replace(TARGET, notice_key="cccccccc-2222-4333-8444-555555555555" + SECTION)
        return response(url, {"noticeList": [member(TARGET, body), member(NEIGHBOR, "SIBLING_PRIVATE"),
                                            member(outside, "OUTSIDE_WINDOW_PRIVATE", day=DAY - timedelta(days=1))]})

    discovery = response(listing_url, {"current": 1, "size": 10, "total": 1, "pages": 1,
                                      "records": [{"bidSectionId": SECTION}]})
    initial, revision = group("SELECTED_FIRST"), group("SELECTED_REVISED")
    collector, transport = fetcher(env, {listing_url: [discovery] * 2, url: [initial, revision]})
    reports = [collector.run(source, window, max_pages=1, refetch=True) for _ in range(2)]
    assert [report.archived for report in reports] == [2, 1]
    assert [request.url for request in transport.sent] == [listing_url, url] * 2
    return handler.to_selected_html, initial.body


@pytest.fixture
def archived_records(env):
    database, store, _ = env
    select, raw = _collect_hunan(env)
    with reqctx.use(CALLER):
        yield database, store, select, raw


def test_v2_pins_hunan_versions_and_returns_only_one_record(archived_records):
    database, store, select, raw = archived_records
    trace = []

    def authorize(operation, record):
        trace.append((operation, record.ref))
        return authorize_record_fixture(operation, record)

    def project(*args, **kwargs):
        trace.append("select")
        return select(*args, **kwargs)

    service = FrozenBatches(database, RecordingStore(store.root, trace), authorize_record=authorize,
                            record_adapters={HUNAN: project})
    batch = FrozenRecords.parse(service.freeze_records((TARGET, REVISED)).dto())
    first, revised = batch.items
    assert first.original.content_hash != revised.original.content_hash
    assert first.record_digest != revised.record_digest
    assert store.get(first.original.artifact_ref) == raw
    assert set(first.dto()) == {"source_id", "notice_key", "observation_version", "content_hash",
                                 "selector", "record_digest", "published_on"}
    assert first.published_on == DAY and first.selector.group_key == SECTION and first.selector.notice_key == TARGET.notice_key

    trace.clear()
    read = service.read_record(batch, 0).dto()
    assert trace == [("read-record", TARGET), ("get", first.original.artifact_ref), "select", ("read-record", TARGET)]
    assert read == {"schema_version": 1, "kind": "selected_notice", "media_type": "text/html", "source_id": HUNAN,
                    "notice_key": TARGET.notice_key, "observation_version": 1, "published_on": DAY.isoformat(),
                    "html": f"<h1>Title SELECTED_FIRST</h1><p>公告时间：{DAY} 17:31:04</p><div><p>SELECTED_FIRST</p></div>"}
    assert "SELECTED_REVISED" in service.read_record(batch, 1).html
    assert "SIBLING_PRIVATE" in raw.decode() and "OUTSIDE_WINDOW_PRIVATE" in raw.decode()

    all_records = FrozenBatches(database, store, authorize_record=lambda *_: True, record_adapters={HUNAN: select})
    sibling_batch = all_records.freeze_records((NEIGHBOR,))
    assert sibling_batch.items[0].original.content_hash == first.original.content_hash
    trace.clear()
    with pytest.raises(FrozenBatchDenied):
        service.read_record(sibling_batch, 0)
    with pytest.raises(FrozenBatchDenied):
        service.freeze_records((NEIGHBOR,))
    assert trace == [("read-record", NEIGHBOR), ("freeze-records", NEIGHBOR)]


def test_v2_rejects_manifest_and_object_tampering_and_v1_rejects_grouped(archived_records, tmp_path):
    database, store, select, raw = archived_records
    trace = []
    objects = RecordingStore(store.root, trace)
    service = FrozenBatches(database, objects, authorize=lambda *_: True, authorize_record=authorize_record_fixture,
                            record_adapters={HUNAN: select})
    batch = service.freeze_records((TARGET,))
    (item,) = batch.items
    trace.clear()
    entry = item.dto()

    for data in ({"source_id": "other-source"}, {"observation_version": 2}, {"content_hash": "sha256:" + "0" * 64},
                 {"record_digest": "sha256:" + "1" * 64}, {"published_on": (DAY + timedelta(days=1)).isoformat()},
                 {"selector": entry["selector"] | {"group_key": "other-section"}},
                 {"selector": entry["selector"] | {"kind": "other-selector"}},
                 {"selector": entry["selector"] | {"version": 2}},
                 {"notice_key": NEIGHBOR.notice_key, "selector": entry["selector"] | {"notice_key": NEIGHBOR.notice_key}}):
        forged = FrozenRecords.parse({"schema_version": 2, "items": [entry | data]})
        with pytest.raises(FrozenBatchDenied):
            service.read_record(forged, 0)

    for data in ({"part_key": "body"}, {"artifact_ref": item.original.artifact_ref}, {"published_on": "20261111"},
                 {"selector": entry["selector"] | {"version": True}}, {"selector": entry["selector"] | {"extra": 1}},
                 {"selector": entry["selector"] | {"notice_key": NEIGHBOR.notice_key}}):
        with pytest.raises(ValueError):
            FrozenRecords.parse({"schema_version": 2, "items": [entry | data]})
    with pytest.raises(ValueError):
        FrozenRecords.parse({"schema_version": 2, "items": [entry, entry]})
    with pytest.raises(ValueError):
        FrozenBatch.parse(batch.dto())

    disguised = FrozenBatch((FrozenOriginal(TARGET, item.original.content_hash),))
    with pytest.raises(FrozenBatchDenied):
        service.freeze((TARGET,))
    with pytest.raises(FrozenBatchDenied):
        service.read(disguised, 0)
    with pytest.raises(FrozenBatchDenied):
        service.restore(disguised, 0, backup=RecordingStore(tmp_path / "backup", trace))
    without_adapter = FrozenBatches(database, objects, authorize_record=authorize_record_fixture)
    with pytest.raises(FrozenBatchDenied):
        without_adapter.read_record(batch, 0)
    legacy_only = FrozenBatches(database, objects, authorize=lambda *_: True, record_adapters={HUNAN: select})
    with pytest.raises(FrozenBatchDenied):
        legacy_only.read_record(batch, 0)
    assert trace == []

    (store.root / item.original.artifact_ref).write_bytes(raw.replace(b"SELECTED_FIRST", b"FORGED_CONTENT"))
    with pytest.raises(FrozenObjectError, match="^object_integrity$"):
        service.read_record(batch, 0)


def test_v2_rechecks_postgres_revocation_after_member_selection(registry, tmp_path):
    reader, operator, dsns = registry
    database = db.Database(dsns["owner"])
    store = FilesystemObjectStore(tmp_path / "group-originals")
    trace = []
    try:
        select, _ = _collect_hunan((database, store, FakeClock()))

        def authorize(operation, record):
            with reader.snapshot() as rights:
                allowed = rights.public_revocations((HUNAN,)) == {HUNAN: False}
            trace.append((operation, allowed))
            return allowed and authorize_record_fixture(operation, record)

        def select_and_revoke(*args, **kwargs):
            html = select(*args, **kwargs)
            write_command(operator, revoke(HUNAN))
            return html

        with reqctx.use(CALLER):
            objects = RecordingStore(store.root, trace)
            service = FrozenBatches(database, objects, authorize_record=authorize, record_adapters={HUNAN: select})
            batch = service.freeze_records((TARGET,))
            revoked = FrozenBatches(database, objects, authorize_record=authorize,
                                    record_adapters={HUNAN: select_and_revoke})
            trace.clear()
            with pytest.raises(FrozenBatchDenied):
                revoked.read_record(batch, 0)
            assert trace == [("read-record", True), ("get", batch.items[0].original.artifact_ref), ("read-record", False)]
            trace.clear()
            with pytest.raises(FrozenBatchDenied):
                service.read_record(batch, 0)
            with pytest.raises(FrozenBatchDenied):
                service.freeze_records((TARGET,))
            assert trace == [("read-record", False), ("freeze-records", False)]
    finally:
        database.close()


def _run_cli(config, *args, binary=False):
    return subprocess.run([sys.executable, "-m", "miaoqia.runtime_acquisition.frozen_batches", *args],
                          env=config, capture_output=True, text=not binary, timeout=15)


@pytest.mark.parametrize("grouped", [False, True])
def test_deployment_read_policy_pins_batch_and_rechecks_postgres_rights(registry, pg_factory, tmp_path, grouped):
    _, operator, dsns = registry
    with pytest.raises(ValueError, match="separate from rights roles"):
        provision_frozen_reader(dsns["owner"], dsns["reader_role"])
    catalog = urlsplit(pg_factory()["app"])._replace(path=urlsplit(dsns["owner"]).path)
    provision_frozen_reader(dsns["owner"], catalog.username)
    with psycopg.connect(catalog.geturl()) as conn:
        assert conn.execute("SELECT count(*) FROM acq_source_observations").fetchone() == (0,)
        with pytest.raises(psycopg.errors.InsufficientPrivilege):
            conn.execute("DELETE FROM acq_source_observations")

    database = db.Database(dsns["owner"])
    store = FilesystemObjectStore(tmp_path / "originals")
    try:
        env = (database, store, FakeClock())
        with reqctx.use(CALLER):
            if grouped:
                select, _ = _collect_hunan(env)
                batches = FrozenBatches(database, store, record_adapters={HUNAN: select},
                                        authorize_record=lambda op, item: op == "freeze-records"
                                        and item.ref in {TARGET, NEIGHBOR})
                allowed, other = (batches.freeze_records((ref,)).dto() for ref in (TARGET, NEIGHBOR))
                command, source = "read-record", HUNAN
            else:
                _collect_single(env)
                batches = FrozenBatches(database, store, authorize=authorize_fixture)
                allowed, other = (batches.freeze((ref,)).dto() for ref in (REFS[0], REFS[2]))
                command, source = "read", SOURCE.source_id
    finally:
        database.close()

    assert allowed["items"][0]["content_hash"] == other["items"][0]["content_hash"]
    manifest = tmp_path / "batch.json"
    policy = tmp_path / "policy.json"
    config = {
        "MIAOQIA_ACQUISITION_FROZEN_READ_POLICY": str(policy),
        "MIAOQIA_ACQUISITION_FROZEN_READER_DSN": catalog.geturl(),
        "MIAOQIA_ACQUISITION_RIGHTS_READER_DSN": dsns["app"],
        "MIAOQIA_ACQUISITION_OBJECT_STORE_DIR": str(store.root),
        "MIAOQIA_PACKS_DIR": str(PACKS),
    }
    document = {"schema_version": 1, "service": "frozen-test-fixture",
                "expires_at": "2099-01-01T00:00:00Z", "batch": allowed}
    policy.write_text(json.dumps(document))
    flags = (command, "--manifest", str(manifest), "--index", "0")
    manifest.write_text(json.dumps(allowed))
    read = _run_cli(config, *flags)
    assert (read.returncode, read.stderr) == (0, "")
    if grouped:
        assert "SELECTED_FIRST" in read.stdout and "SIBLING_PRIVATE" not in read.stdout
    else:
        assert read.stdout.encode() == A

    restore_command = "restore-record-original" if grouped else "restore"
    disabled = _run_cli(config, restore_command, *flags[1:])
    assert (disabled.returncode, disabled.stdout, json.loads(disabled.stderr)) == (
        1, "", {"error": "configuration_unavailable"},
    )

    manifest.write_text(json.dumps(other))
    denied = _run_cli(config, *flags)
    assert (denied.returncode, denied.stdout, json.loads(denied.stderr)) == (1, "", {"error": "access_denied"})
    manifest.write_text(json.dumps(allowed))
    policy.write_text(json.dumps({**document, "expires_at": "2000-01-01T00:00:00Z"}))
    expired = _run_cli(config, *flags)
    assert (expired.returncode, expired.stdout, json.loads(expired.stderr)) == (1, "", {"error": "access_denied"})
    policy.write_text(json.dumps(document))
    write_command(operator, revoke(source))
    revoked = _run_cli(config, *flags)
    assert (revoked.returncode, revoked.stdout, json.loads(revoked.stderr)) == (1, "", {"error": "access_denied"})


def test_cli_process_outputs_structured_errors_and_restores(archived, tmp_path):
    database, store = archived
    selection = tmp_path / "selection.json"
    selection.write_text(json.dumps([{"source_id": SOURCE.source_id, "notice_key": "notice", "observation_version": 1}]))
    manifest = tmp_path / "batch.json"
    backup_dir = tmp_path / "backup"
    config = {"PYTHONPATH": os.path.dirname(__file__),
              "MIAOQIA_ACQUISITION_FROZEN_AUTHORIZER": f"{__name__}:authorize_fixture",
              "MIAOQIA_ACQUISITION_FROZEN_READER_DSN": database.engine.url.render_as_string(hide_password=False),
              "MIAOQIA_ACQUISITION_OBJECT_STORE_DIR": str(store.root),
              "MIAOQIA_ACQUISITION_FROZEN_BACKUP": str(backup_dir)}

    def command(*args, binary=False):
        return _run_cli(config, *args, binary=binary)

    reference = config.pop("MIAOQIA_ACQUISITION_FROZEN_AUTHORIZER")
    denied = command("freeze", "--selection", str(selection))
    assert (denied.returncode, denied.stdout, json.loads(denied.stderr)) == (1, "", {"error": "configuration_unavailable"})
    config["MIAOQIA_ACQUISITION_FROZEN_AUTHORIZER"] = reference
    frozen = command("freeze", "--selection", str(selection))
    assert (frozen.returncode, frozen.stderr) == (0, "")
    batch = FrozenBatch.parse(json.loads(frozen.stdout))
    assert tuple(item.ref for item in batch.items) == REFS[:1]
    manifest.write_text(frozen.stdout)
    flags = ("--manifest", str(manifest), "--index", "0")
    result = command("read", *flags, binary=True)
    assert (result.returncode, result.stdout, result.stderr) == (0, A, b"")
    key = batch.items[0].content_hash.replace("sha256:", "raw/")
    config["MIAOQIA_ACQUISITION_OBJECT_STORE_DIR"] = str(tmp_path / "missing-primary")
    unavailable = command("verify", *flags)
    assert (unavailable.returncode, unavailable.stdout, json.loads(unavailable.stderr)) == (
        1, "", {"error": "configuration_unavailable"},
    )
    assert not (tmp_path / "missing-primary").exists()
    config["MIAOQIA_ACQUISITION_OBJECT_STORE_DIR"] = str(store.root)
    present = command("restore", *flags)
    assert (present.returncode, json.loads(present.stdout), present.stderr) == (0, {"status": "already_present"}, "")
    assert not backup_dir.exists()
    (store.root / key).unlink()
    absent = command("restore", *flags)
    assert (absent.returncode, absent.stdout, json.loads(absent.stderr)) == (1, "", {"error": "backup_missing"})
    assert not backup_dir.exists()
    FilesystemObjectStore(backup_dir).put(key, A)
    missing = command("verify", *flags)
    assert (missing.returncode, missing.stdout, json.loads(missing.stderr)) == (1, "", {"error": "original_missing"})
    missing = command("read", *flags, binary=True)
    assert (missing.returncode, missing.stdout, json.loads(missing.stderr)) == (1, b"", {"error": "original_missing"})
    restored = command("restore", *flags)
    assert (restored.returncode, json.loads(restored.stdout), restored.stderr) == (0, {"status": "restored"}, "")
    verified = command("verify", *flags)
    assert (verified.returncode, json.loads(verified.stdout), verified.stderr) == (0, {"status": "verified"}, "")
    manifest.write_text('{"schema_version":1,"schema_version":1,"items":[]}')
    invalid = command("verify", *flags)
    assert (invalid.returncode, invalid.stdout, json.loads(invalid.stderr)) == (2, "", {"error": "invalid_input"})


def test_cli_v2_uses_trusted_hunan_pack_and_rejects_legacy_commands(archived_records, tmp_path):
    database, store, _, raw = archived_records
    selection = tmp_path / "selection.json"
    selection.write_text(json.dumps([{key: getattr(TARGET, key) for key in
                                      ("source_id", "notice_key", "observation_version")}]))
    manifest = tmp_path / "records.json"
    flags = ("--manifest", str(manifest), "--index", "0")
    config = {"PYTHONPATH": os.path.dirname(__file__), "MIAOQIA_PACKS_DIR": str(PACKS),
              "MIAOQIA_ACQUISITION_FROZEN_READER_DSN": database.engine.url.render_as_string(hide_password=False),
              "MIAOQIA_ACQUISITION_OBJECT_STORE_DIR": str(store.root),
              "MIAOQIA_ACQUISITION_FROZEN_AUTHORIZER": f"{__name__}:authorize_fixture"}

    def command(*args):
        return _run_cli(config, *args)

    blocked = command("freeze-records", "--selection", str(selection))
    assert (blocked.returncode, blocked.stdout, json.loads(blocked.stderr)) == (1, "", {"error": "configuration_unavailable"})
    config["MIAOQIA_ACQUISITION_FROZEN_RECORD_ACCESS"] = f"{__name__}:record_access_fixture"
    frozen = command("freeze-records", "--selection", str(selection))
    assert (frozen.returncode, frozen.stderr) == (0, "")
    manifest.write_text(frozen.stdout)
    parsed = FrozenRecords.parse(json.loads(frozen.stdout))
    assert tuple(item.ref for item in parsed.items) == (TARGET,)
    assert "raw/" not in frozen.stdout and "SIBLING_PRIVATE" not in frozen.stdout
    checked = command("read-record", *flags)
    assert (checked.returncode, checked.stderr) == (0, "")
    record = json.loads(checked.stdout)
    assert record["notice_key"] == TARGET.notice_key and "SELECTED_FIRST" in record["html"]
    assert "SIBLING_PRIVATE" not in checked.stdout and "OUTSIDE_WINDOW_PRIVATE" not in checked.stdout
    assert parsed.items[0].original.content_hash not in checked.stdout and "raw/" not in checked.stdout

    for command_name in ("read", "verify", "restore"):
        invalid = command(command_name, *flags)
        assert (invalid.returncode, invalid.stdout, json.loads(invalid.stderr)) == (2, "", {"error": "invalid_input"})

    key = parsed.items[0].original.artifact_ref
    backup = FilesystemObjectStore(tmp_path / "backup")
    config["MIAOQIA_ACQUISITION_FROZEN_BACKUP"] = str(backup.root)
    (store.root / key).unlink()
    denied = command("restore-record-original", *flags)
    assert (denied.returncode, denied.stdout, json.loads(denied.stderr)) == (1, "", {"error": "access_denied"})
    assert not (store.root / key).exists()
    config["MIAOQIA_ACQUISITION_FROZEN_RECORD_ACCESS"] = f"{__name__}:restore_access_fixture"
    del config["MIAOQIA_PACKS_DIR"]
    backup.put(key, raw.replace(b"SELECTED_FIRST", b"FORGED_CONTENT"))
    corrupt = command("restore-record-original", *flags)
    assert (corrupt.returncode, corrupt.stdout, json.loads(corrupt.stderr)) == (1, "", {"error": "object_integrity"})
    assert not (store.root / key).exists()
    (backup.root / key).unlink()
    backup.put(key, raw)
    restored = command("restore-record-original", *flags)
    assert (restored.returncode, json.loads(restored.stdout), restored.stderr) == (0, {"status": "restored"}, "")
    assert store.get(key) == raw
    present = command("restore-record-original", *flags)
    assert (present.returncode, json.loads(present.stdout), present.stderr) == (0, {"status": "already_present"}, "")

    manifest.write_text(frozen.stdout.replace('"group_key": ', '"group_key": "x", "group_key": '))
    invalid = command("read-record", *flags)
    assert (invalid.returncode, invalid.stdout, json.loads(invalid.stderr)) == (2, "", {"error": "invalid_input"})


@pytest.fixture
def frozen_s3_buckets(monkeypatch):
    endpoint = os.environ.get("MIAOQIA_TEST_FROZEN_S3_ENDPOINT")
    if not endpoint:
        pytest.skip("set MIAOQIA_TEST_FROZEN_S3_ENDPOINT for a disposable S3-compatible service")
    monkeypatch.setenv("AWS_ENDPOINT_URL_S3", endpoint)
    profile = os.environ.get("MIAOQIA_TEST_FROZEN_BACKUP_S3_PROFILE")
    setup_profile = os.environ.get("MIAOQIA_TEST_FROZEN_BACKUP_SETUP_PROFILE", profile)
    suffix = uuid4().hex

    def remove_bucket(client, name):
        for item in client.list_objects_v2(Bucket=name).get("Contents", ()):
            client.delete_object(Bucket=name, Key=item["Key"])
        client.delete_bucket(Bucket=name)

    config = {"MIAOQIA_ACQUISITION_OBJECT_STORE_DIR": f"s3://mq-frozen-primary-{suffix}",
              "MIAOQIA_ACQUISITION_FROZEN_BACKUP": f"s3://mq-frozen-backup-{suffix}"}
    if profile is not None:
        config["MIAOQIA_ACQUISITION_FROZEN_BACKUP_S3_PROFILE"] = profile
    with ExitStack() as resources:
        stores = []
        for location, binding in ((config["MIAOQIA_ACQUISITION_OBJECT_STORE_DIR"], None),
                                  (config["MIAOQIA_ACQUISITION_FROZEN_BACKUP"], setup_profile)):
            client = resources.enter_context(closing(s3_client(profile=binding)))
            bucket = urlsplit(location).netloc
            client.create_bucket(Bucket=bucket)
            resources.callback(remove_bucket, client, bucket)
            stores.append(S3ObjectStore(bucket, client=client))
        primary, backup = stores
        yield primary, backup, config


@pytest.mark.parametrize("grouped", [False, True])
def test_restores_between_s3_compatible_buckets(request, frozen_s3_buckets, grouped, tmp_path):
    if grouped:
        database, originals, select, original_bytes = request.getfixturevalue("archived_records")
        options = {"record_adapters": {HUNAN: select}, "authorize_record": authorize_record_fixture}
        batch = FrozenBatches(database, originals, **options).freeze_records((TARGET,))
        key = batch.items[0].original.artifact_ref
        options["authorize_record"] = restore_access_fixture()[1]
    else:
        database, originals = request.getfixturevalue("archived")
        options = {"authorize": authorize_fixture}
        batch = FrozenBatches(database, originals, **options).freeze(REFS[:1])
        key = batch.items[0].artifact_ref
        original_bytes = A
    primary, backup, locations = frozen_s3_buckets
    backup.put(key, original_bytes)
    service = FrozenBatches(database, primary, **options)
    restore = service.restore_record_original if grouped else service.restore
    with pytest.raises(FileNotFoundError):
        primary.get(key)
    manifest = tmp_path / "batch.json"
    manifest.write_text(json.dumps(batch.dto()))
    config = {k: v for k, v in os.environ.items() if k.startswith("AWS_")}
    config.update(locations, PYTHONPATH=os.path.dirname(__file__),
                  MIAOQIA_ACQUISITION_FROZEN_READER_DSN=database.engine.url.render_as_string(hide_password=False),
                  MIAOQIA_ACQUISITION_FROZEN_RECORD_ACCESS=f"{__name__}:restore_access_fixture",
                  MIAOQIA_ACQUISITION_FROZEN_AUTHORIZER=f"{__name__}:authorize_fixture")
    result = _run_cli(config, "restore-record-original" if grouped else "restore",
                      "--manifest", str(manifest), "--index", "0")
    assert (result.returncode, result.stderr) == (0, "")
    assert json.loads(result.stdout) == {"status": "restored"}
    assert restore(batch, 0, backup=backup) == "already_present"
    with pytest.raises(ObjectConflict):
        primary.put(key, B)
    assert primary.get(key) == original_bytes
