"""Frozen browser replay against the acquisition ledger and current PostgreSQL rights."""
from __future__ import annotations

import json
from dataclasses import asdict, replace
from pathlib import Path
from urllib.parse import urlsplit

import psycopg
import pytest
from psycopg.types.json import Jsonb

from miaoqia.acquisition.api.browser_pairs import seal_browser_event
from miaoqia.acquisition.api.frozen_batches import (
    FrozenBatch, FrozenBatchDenied, FrozenBrowserBatch, FrozenObjectError, FrozenRecords, ObservationRef,
)
from miaoqia.acquisition.application import collection_leases
from miaoqia.acquisition.application.browser_observations import store_browser_pair
from miaoqia.acquisition.application.frozen_batches import FrozenBatches
from miaoqia.acquisition.infrastructure.privileges import provision_frozen_reader
from miaoqia.kernel import db, reqctx
from miaoqia.kernel.objectstore import FilesystemObjectStore
from miaoqia.runtime_acquisition import frozen_batches as cli

from test_browser_observations import DAY, NOTICE_KEY, NOW, SOURCE_ID, WINDOW, _stage, _submit
from test_frozen_batches import CALLER, RecordingStore, _run_cli, frozen_s3_buckets  # noqa: F401
from test_public_source_rights import restore, revoke, write_command
from test_rights import declare_public


REFS = tuple(ObservationRef(SOURCE_ID, NOTICE_KEY, version) for version in (1, 2))
RAW = json.dumps({"id": NOTICE_KEY, "private_marker": "RAW_ONLY"}).encode()


def browser_operator():
    return CALLER, lambda operation, item: reqctx.current() == CALLER and item.ref in REFS and operation in {
        "freeze-browser", "read-browser-original", "restore-browser-original",
    }


@pytest.fixture
def archive(registry, tmp_path):
    _, _, dsns = registry
    declare_public(dsns, SOURCE_ID)
    database = db.Database(dsns["owner"])
    store = FilesystemObjectStore(tmp_path / "objects")
    try:
        with reqctx.use(CALLER):
            with database.tx() as conn:
                lease = collection_leases.claim(conn, SOURCE_ID, WINDOW, max_pages=2, refetch=True,
                                                mode="manual", started_at=NOW, recover=False)
            ledger = (database, lease, dsns["owner"])
            for index, ref in enumerate(REFS):
                html = (f'<article data-notice-id="{NOTICE_KEY}"><p>Notice {index}</p>'
                        '<script>ACTIVE_SCRIPT</script><img src="https://src.example/ACTIVE_IMAGE"></article>')
                receipt, projection = _stage(ledger, RAW, html)
                store_browser_pair(store, receipt, RAW, projection)
                assert _submit(ledger, store, receipt).version == ref.observation_version
            yield database, store
    finally:
        database.close()


def _frozen(database, store):
    return FrozenBatches(database, store, authorize_browser=browser_operator()[1]).freeze_browser(REFS)


def test_pair_pins_both_descriptors_and_cannot_enter_legacy_batches(archive):
    database, store = archive
    trace = []

    def authorize(operation, item):
        trace.append((operation, item.ref))
        return True

    service = FrozenBatches(database, RecordingStore(store.root, trace), authorize_browser=authorize,
                            authorize=lambda *_: True, authorize_record=lambda *_: True)
    batch = service.freeze_browser(REFS)
    assert batch == FrozenBrowserBatch.parse(json.loads(json.dumps(batch.dto())))
    first, second = batch.items
    assert first.ref == REFS[0] and second.ref == REFS[1]
    assert first.original == second.original
    assert first.original.media_type == first.projection.media_type == "application/json"
    assert first.original.size_bytes == len(RAW)
    assert first.projection_version == 1
    assert first.projection != second.projection and first.binding_digest != second.binding_digest
    assert trace == [
        ("freeze-browser", first.ref), ("get", first.original.artifact_ref), ("get", first.projection.artifact_ref),
        ("freeze-browser", first.ref),
        ("freeze-browser", second.ref), ("get", second.original.artifact_ref),
        ("get", second.projection.artifact_ref), ("freeze-browser", second.ref), ("freeze-browser", first.ref),
    ]
    trace.clear()
    projection = service.read_projection(batch, 0).dto()
    assert projection.keys() == {
        "schema_version", "kind", "media_type", "source_id", "notice_key", "observation_version", "published_on",
        "document",
    }
    assert projection["kind"] == "browser_projection"
    assert projection["published_on"] == DAY.isoformat()
    assert projection["document"]["blocks"] == [{"index": 0, "kind": "paragraph", "text": "Notice 0"}]
    assert trace == [
        ("read-projection", first.ref), ("get", first.original.artifact_ref),
        ("get", first.projection.artifact_ref), ("read-projection", first.ref),
    ]
    with pytest.raises(ValueError):
        FrozenBatch.parse(batch.dto())
    with pytest.raises(ValueError):
        FrozenRecords.parse(batch.dto())
    trace.clear()
    for freeze in (service.freeze, service.freeze_records):
        with pytest.raises(FrozenBatchDenied):
            freeze(REFS[:1])
    with pytest.raises(ValueError):
        service.freeze_browser((replace(first.ref, part_key="attachment"),))
    assert trace == []


def test_tampered_manifest_or_catalog_is_denied_before_authorization_or_storage(archive, registry):
    database, store = archive
    batch = _frozen(database, store)
    first, second = batch.items
    trace = []
    service = FrozenBatches(database, RecordingStore(store.root, trace),
                            authorize_browser=lambda *args: trace.append(args) or True)
    for item in (
        replace(first, projection=second.projection, binding_digest=second.binding_digest),
        replace(first, original=replace(first.original, size_bytes=first.original.size_bytes + 1)),
        replace(first, original=replace(first.original, media_type="text/html")),
        replace(first, binding_digest="sha256:" + "a" * 64),
        replace(first, ref=replace(first.ref, source_id="other-source")),
    ):
        with pytest.raises(FrozenBatchDenied):
            service.read_projection(FrozenBrowserBatch((item,)), 0)

    for field, value in (("part_key", "attachment"), ("binding", {})):
        manifest = batch.dto()
        manifest["items"][0][field] = value
        with pytest.raises(ValueError):
            FrozenBrowserBatch.parse(manifest)
    manifest = batch.dto()
    manifest["items"][0]["original"], manifest["items"][0]["projection"] = (
        manifest["items"][0]["projection"], manifest["items"][0]["original"],
    )
    with pytest.raises(ValueError):
        FrozenBrowserBatch.parse(manifest)

    # Only this disposable fixture's owner can simulate a mismatched immutable receipt.
    with psycopg.connect(registry[2]["owner"]) as conn:
        event = conn.execute("SELECT payload FROM acq_browser_receipts ORDER BY attempt_id LIMIT 1").fetchone()[0]
        conn.execute("ALTER TABLE acq_browser_receipts DISABLE TRIGGER acq_browser_receipts_append_only")
        for changes in (
            {"binding": {**event["binding"], "task_key": "f" * 64}},
            {"fetched_at": "2025-01-02T00:00:00Z"},
            {"size_bytes": first.original.size_bytes + 1},
            {"observation_version": 2},
        ):
            fields = {key: value for key, value in event.items() if key != "binding_digest"}
            forged = seal_browser_event({**fields, **changes})
            conn.execute("UPDATE acq_browser_receipts SET payload = %s WHERE attempt_id = %s",
                         (Jsonb(forged), event["binding"]["attempt_id"]))
            conn.commit()
            with pytest.raises(FrozenBatchDenied):
                service.freeze_browser(REFS[:1])
    assert trace == []


@pytest.mark.parametrize("role", ["original", "projection"])
def test_loss_and_restore_of_each_half(archive, tmp_path, role):
    database, store = archive
    batch = _frozen(database, store)
    item = batch.items[0]
    key = getattr(item, role).artifact_ref
    data = store.get(key)
    path = store.root / key
    path.unlink()
    trace = []
    backup = RecordingStore(tmp_path / "backup", trace)
    primary = RecordingStore(store.root, trace)
    permissions = {"freeze-browser", "read-projection", "verify-browser", f"restore-browser-{role}"}
    service = FrozenBatches(database, primary, authorize_browser=lambda operation, _: operation in permissions)
    for action in (lambda: service.freeze_browser(REFS[:1]), lambda: service.read_projection(batch, 0),
                   lambda: service.verify_browser(batch, 0)):
        with pytest.raises(FrozenObjectError, match="browser_object_missing"):
            action()
    restore_object = getattr(service, f"restore_browser_{role}")
    other_role = "projection" if role == "original" else "original"
    trace.clear()
    with pytest.raises(FrozenBatchDenied):
        getattr(service, f"restore_browser_{other_role}")(batch, 0, backup=backup)
    with pytest.raises(FrozenBatchDenied):
        service.read_browser_original(batch, 0)
    assert trace == []

    with pytest.raises(FrozenObjectError, match="backup_missing"):
        restore_object(batch, 0, backup=backup)
    wrong = bytes([data[0] ^ 1]) + data[1:]
    backup.put(key, wrong)
    with pytest.raises(FrozenObjectError, match="object_integrity"):
        restore_object(batch, 0, backup=backup)
    assert not path.exists()
    (backup.root / key).write_bytes(data)
    path.write_bytes(wrong)
    trace.clear()
    with pytest.raises(FrozenObjectError, match="object_integrity"):
        restore_object(batch, 0, backup=backup)
    assert trace == [("get", key)]
    assert path.read_bytes() == wrong
    path.unlink()

    trace.clear()
    assert restore_object(batch, 0, backup=backup) == "restored"
    assert trace == [("get", key), ("get", key), ("put", key), ("get", key)]
    trace.clear()
    assert restore_object(batch, 0, backup=backup) == "already_present"
    assert trace == [("get", key)]
    service.verify_browser(batch, 0)


def test_restore_rechecks_authorization_after_backup_read(archive, tmp_path, monkeypatch):
    database, store = archive
    batch = _frozen(database, store)
    key = batch.items[0].projection.artifact_ref
    backup = FilesystemObjectStore(tmp_path / "backup")
    backup.put(key, store.get(key))
    (store.root / key).unlink()
    allowed = True
    read = backup.get

    def read_then_revoke(*args, **kwargs):
        nonlocal allowed
        content = read(*args, **kwargs)
        allowed = False
        return content

    monkeypatch.setattr(backup, "get", read_then_revoke)
    service = FrozenBatches(database, store, authorize_browser=lambda op, _: op == "restore-browser-projection" and allowed)
    with pytest.raises(FrozenBatchDenied):
        service.restore_browser_projection(batch, 0, backup=backup)
    assert not (store.root / key).exists()


def test_restores_browser_pair_between_s3_compatible_buckets(frozen_s3_buckets, archive):
    database, store = archive
    batch = _frozen(database, store)
    item = batch.items[0]
    primary, backup, _ = frozen_s3_buckets
    for descriptor in (item.original, item.projection):
        backup.put(descriptor.artifact_ref, store.get(descriptor.artifact_ref))

    roles = {"restore-browser-projection"}
    service = FrozenBatches(database, primary, authorize_browser=lambda op, _: op in roles)
    with pytest.raises(FrozenBatchDenied):
        service.restore_browser_original(batch, 0, backup=backup)
    assert service.restore_browser_projection(batch, 0, backup=backup) == "restored"
    assert service.restore_browser_projection(batch, 0, backup=backup) == "already_present"

    roles.update({"verify-browser", "restore-browser-original"})
    with pytest.raises(FrozenObjectError, match="browser_object_missing"):
        service.verify_browser(batch, 0)
    assert service.restore_browser_original(batch, 0, backup=backup) == "restored"
    service.verify_browser(batch, 0)


@pytest.fixture
def deployment(archive, registry, pg_factory, tmp_path):
    database, store = archive
    _, _, dsns = registry
    for role in (dsns["reader_role"], dsns["operator_role"]):
        with pytest.raises(ValueError, match="separate from rights roles"):
            provision_frozen_reader(dsns["owner"], role)
    catalog = urlsplit(pg_factory()["app"])._replace(path=urlsplit(dsns["owner"]).path)
    provision_frozen_reader(dsns["owner"], catalog.username)
    batch = _frozen(database, store)
    manifest = tmp_path / "batch.json"
    manifest.write_text(json.dumps(batch.dto()))
    config = {
        "MIAOQIA_ACQUISITION_FROZEN_READER_DSN": catalog.geturl(),
        "MIAOQIA_ACQUISITION_RIGHTS_READER_DSN": dsns["app"],
        "MIAOQIA_ACQUISITION_OBJECT_STORE_DIR": str(store.root),
    }
    return batch, manifest, config


def test_frozen_reader_has_exact_table_and_sequence_grants(deployment, registry):
    _, _, config = deployment
    with psycopg.connect(config["MIAOQIA_ACQUISITION_FROZEN_READER_DSN"]) as conn:
        assert conn.execute("""
            SELECT table_name FROM information_schema.table_privileges
            WHERE table_schema = 'public' AND grantee = current_user AND privilege_type = 'SELECT'
            ORDER BY table_name
        """).fetchall() == [
            ("acq_browser_receipts",), ("acq_observation_attachments",),
            ("acq_raw_artifacts",), ("acq_source_observations",),
        ]
        assert conn.execute("""
            SELECT
                has_schema_privilege(current_user, 'acq_rights', 'USAGE'),
                EXISTS(SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND has_table_privilege(
                    current_user, format('%I.%I', schemaname, tablename), 'INSERT,UPDATE,DELETE,TRUNCATE')),
                EXISTS(SELECT 1 FROM pg_sequences WHERE schemaname = 'public' AND has_sequence_privilege(
                    current_user, format('%I.%I', schemaname, sequencename), 'USAGE,SELECT,UPDATE'))
        """).fetchone() == (False, False, False)
    for dsn in (registry[2]["app"], registry[2]["operator"]):
        with psycopg.connect(dsn) as conn, pytest.raises(psycopg.errors.InsufficientPrivilege):
            conn.execute("SELECT payload FROM public.acq_browser_receipts")


def _policy(path, batch):
    grant = {"schema_version": 1, "service": "frozen-test-fixture",
             "expires_at": "2099-01-01T00:00:00Z", "batch": batch.dto()}
    _replace_policy(path, grant)
    return grant


def _replace_policy(path, grant):
    replacement = path.with_suffix(".tmp")
    replacement.write_text(json.dumps(grant))
    replacement.replace(path)


def test_cli_policy_pins_observation_expiry_and_current_source_rights(deployment, registry, tmp_path):
    batch, manifest, config = deployment
    _, operator, _ = registry
    policy = tmp_path / "policy.json"
    pinned = FrozenBrowserBatch(batch.items[:1])
    grant = _policy(policy, pinned)
    config["MIAOQIA_ACQUISITION_FROZEN_READ_POLICY"] = str(policy)
    flags = ("--manifest", str(manifest), "--index", "0")

    def denied(command, expected, *item_flags):
        result = _run_cli(config, command, *(item_flags or flags))
        assert (result.returncode, result.stdout, json.loads(result.stderr)) == (1, "", {"error": expected})

    projection = _run_cli(config, "read-projection", *flags)
    assert (projection.returncode, projection.stderr) == (0, "")
    assert "Notice 0" in projection.stdout
    assert all(marker not in projection.stdout for marker in ("RAW_ONLY", "ACTIVE_SCRIPT", "ACTIVE_IMAGE", "binding_digest"))
    verified = _run_cli(config, "verify-browser", *flags)
    assert (verified.returncode, json.loads(verified.stdout), verified.stderr) == (0, {"status": "verified"}, "")
    for command in ("read-browser-original", "restore-browser-original", "restore-browser-projection"):
        denied(command, "configuration_unavailable")
    denied("verify-browser", "access_denied", *flags[:-1], "1")
    _replace_policy(policy, {**grant, "expires_at": "2000-01-01T00:00:00Z"})
    denied("read-projection", "access_denied")
    _replace_policy(policy, grant)
    assert _run_cli(config, "read-projection", *flags).returncode == 0
    write_command(operator, revoke(SOURCE_ID))
    denied("read-projection", "access_denied")
    denied("verify-browser", "access_denied")
    write_command(operator, restore(SOURCE_ID, 1))
    assert _run_cli(config, "verify-browser", *flags).returncode == 0


@pytest.mark.parametrize("change", ["policy", "source"])
def test_policy_rechecked_after_projected_bytes_are_read(deployment, registry, tmp_path, monkeypatch, capsys, change):
    batch, manifest, config = deployment
    policy = tmp_path / "policy.json"
    grant = _policy(policy, batch)
    monkeypatch.setenv("MIAOQIA_ACQUISITION_FROZEN_READ_POLICY", str(policy))
    for name, value in config.items():
        monkeypatch.setenv(name, value)
    store = FilesystemObjectStore(config["MIAOQIA_ACQUISITION_OBJECT_STORE_DIR"])
    read = store.get

    def get_then_revoke(key, **kwargs):
        data = read(key, **kwargs)
        if key == batch.items[0].projection.artifact_ref:
            if change == "policy":
                _replace_policy(policy, {**grant, "batch": FrozenBrowserBatch(batch.items[1:]).dto()})
            else:
                write_command(registry[1], revoke(SOURCE_ID))
        return data

    monkeypatch.setattr(store, "get", get_then_revoke)
    def open_reader(location, *, read_only):
        assert location == config["MIAOQIA_ACQUISITION_OBJECT_STORE_DIR"] and read_only
        return store

    monkeypatch.setattr(cli, "open_object_store", open_reader)
    assert cli.main(["read-projection", "--manifest", str(manifest), "--index", "0"]) == 1
    captured = capsys.readouterr()
    assert captured.out == ""
    assert json.loads(captured.err) == {"error": "access_denied"}


def test_cli_operator_permissions_are_role_specific(deployment, tmp_path):
    batch, manifest, config = deployment
    selection = tmp_path / "selection.json"
    selection.write_text(json.dumps([asdict(ref) for ref in REFS]))
    config.update(PYTHONPATH=str(Path(__file__).parent),
                  MIAOQIA_ACQUISITION_FROZEN_BROWSER_ACCESS=f"{__name__}:browser_operator",
                  MIAOQIA_ACQUISITION_FROZEN_BACKUP=str(tmp_path / "backup"))
    frozen = _run_cli(config, "freeze-browser", "--selection", str(selection))
    assert (frozen.returncode, json.loads(frozen.stdout), frozen.stderr) == (0, batch.dto(), "")
    flags = ("--manifest", str(manifest), "--index", "0")
    raw = _run_cli(config, "read-browser-original", *flags, binary=True)
    assert (raw.returncode, raw.stdout, raw.stderr) == (0, RAW, b"")
    for command in ("read-projection", "verify-browser", "restore-browser-projection"):
        denied = _run_cli(config, command, *flags)
        assert (denied.returncode, denied.stdout, json.loads(denied.stderr)) == (1, "", {"error": "access_denied"})
    key = batch.items[0].original.artifact_ref
    FilesystemObjectStore(config["MIAOQIA_ACQUISITION_FROZEN_BACKUP"]).put(key, RAW)
    (Path(config["MIAOQIA_ACQUISITION_OBJECT_STORE_DIR"]) / key).unlink()
    restored = _run_cli(config, "restore-browser-original", *flags)
    assert (restored.returncode, json.loads(restored.stdout), restored.stderr) == (0, {"status": "restored"}, "")
