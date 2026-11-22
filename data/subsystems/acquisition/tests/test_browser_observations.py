"""Exercise the browser-pair ledger without contacting a publisher."""
from __future__ import annotations

import json
from dataclasses import replace

import pytest
from sqlalchemy import text

from miaoqia.acquisition.api.browser_pairs import BROWSER_NOTICE_ARCHIVED, read_browser_pair
from miaoqia.acquisition.api.handlers import BrowserSelection, DetailRef, FetchRequest, FetchResponse
from miaoqia.acquisition.application.browser_observations import (
    BrowserConflict, archive_browser_pair, prepare_browser_pair, stage_browser_pair, store_browser_pair,
    submit_browser_pair,
)
from miaoqia.acquisition.infrastructure.archive_spool import ArchiveSpool
from miaoqia.acquisition.infrastructure.browser_capture import BrowserCapture
from miaoqia.kernel.objectstore import FilesystemObjectStore

from test_grouped_observations import CHANNEL, DAY, LIST_URL, NOW, SOURCE_ID, WINDOW, _attempt, ledger, rows  # noqa: F401


NOTICE_KEY = "90071992547409931"
NOTICE_URL = f"https://src.example/notice?id={NOTICE_KEY}"
BODY_URL = "https://src.example/body"


def _stage(ledger, original, html, *, listed_url=NOTICE_URL, loader_override=None):
    database, lease, _ = ledger
    request = FetchRequest("GET", BODY_URL + "?id=" + NOTICE_KEY)
    with database.tx() as conn:
        _, listing = _attempt(conn, lease, "list", FetchRequest("GET", LIST_URL), "settled")
        refs = [{"notice_key": NOTICE_KEY, "source_url": listed_url, "published_on": DAY.isoformat()}]
        conn.execute(text("""
            INSERT INTO acq_discovery_pages (attempt_id, run_id, channel, window_start, window_end, page,
                response_hash, refs, has_next)
            VALUES (:attempt, :run, :channel, :day, :day, 1, :hash, CAST(:refs AS jsonb), FALSE)
        """), {"attempt": listing, "run": lease.run_id, "channel": CHANNEL, "day": DAY,
               "hash": "sha256:" + "1" * 64, "refs": json.dumps(refs)})
        task, attempt = _attempt(conn, lease, "browser", request, "queued", recorded_url=BODY_URL)
        response = FetchResponse(200, "application/json", original, request.url)
        capture = BrowserCapture(response, "GET", f"{attempt}.1", "main", f"loader-{attempt}", "192.0.2.1", 443)
        selection = BrowserSelection(html, f"navigation-{attempt}", capture.frame_id, capture.loader_id)
        if loader_override is not None:
            selection = replace(selection, loader_id=loader_override)

        def validate(ref, raw, selected):
            assert json.loads(raw.body)["id"] == ref.source_key and ref.source_key in selected.html
            return DAY

        checkpoint, projection = prepare_browser_pair(
            lease=lease, ref=DetailRef(NOTICE_KEY, NOTICE_URL, DAY), request=request, capture=capture,
            selection=selection, validate=validate, task_key=task, attempt_id=attempt,
            list_attempt_id=listing, received_at=NOW,
        )
        scope = [CHANNEL, DAY.isoformat(), DAY.isoformat(), NOTICE_KEY, listing]
        conn.execute(text("UPDATE acq_request_tasks SET scope = CAST(:scope AS jsonb) WHERE task_key = :task"),
                     {"scope": json.dumps(scope), "task": task})
        stage_browser_pair(conn, lease=lease, channel=CHANNEL, window=WINDOW, checkpoint=checkpoint)
    return checkpoint, projection


def _submit(ledger, store, checkpoint):
    database, lease, _ = ledger
    with database.tx() as conn:
        return submit_browser_pair(conn, store, lease=lease, channel=CHANNEL, window=WINDOW, checkpoint=checkpoint)


def test_browser_attempts_bind_pair_keep_history_and_replay(ledger, tmp_path):
    store = FilesystemObjectStore(tmp_path)
    original = json.dumps({"id": NOTICE_KEY, "opaque": True}).encode()
    fragments = [f'<article data-notice-id="{NOTICE_KEY}"><h1>Notice</h1><p>Edition {edition}</p></article>'
                 for edition in ("A", "A", "B", "A")]

    def submit(fragment):
        checkpoint, projection = _stage(ledger, original, fragment)
        store_browser_pair(store, checkpoint, original, projection)
        result = _submit(ledger, store, checkpoint)
        replay = _submit(ledger, store, checkpoint)
        assert replay.replayed and (replay.version, replay.changed) == (result.version, result.changed)
        return result

    assert [(result.version, result.changed) for result in map(submit, fragments)] == [
        (1, True), (1, False), (2, True), (3, True),
    ]
    assert rows(ledger[0], "SELECT count(*), count(DISTINCT artifact_id) FROM acq_browser_receipts") == [(4, 1)]
    events = rows(ledger[0], "SELECT partition_key, envelope FROM kernel_outbox ORDER BY seq")
    for version, (partition, envelope) in enumerate(events, 1):
        data = envelope["data"]
        assert partition == f"{SOURCE_ID}|{NOTICE_KEY}"
        assert envelope["type"] == BROWSER_NOTICE_ARCHIVED and envelope["subject"] == data["artifact_ref"]
        assert data["observation_version"] == version
        archived, document = read_browser_pair(store, data)
        assert archived == original
        assert document["blocks"][-1]["text"] == f"Edition {'ABA'[version - 1]}"
    assert len(events) == 3


def test_missing_projection_and_cross_discovery_do_not_advance(ledger, tmp_path):
    store = FilesystemObjectStore(tmp_path)
    original = json.dumps({"id": NOTICE_KEY, "opaque": True}).encode()
    fragment = f'<article data-notice-id="{NOTICE_KEY}"><p>Selected body</p></article>'
    with pytest.raises(BrowserConflict, match="not from the approved navigation"):
        _stage(ledger, original, fragment, loader_override="old-loader")
    checkpoint, projection = _stage(ledger, original, fragment)
    store.put(checkpoint["artifact_ref"], original)
    with pytest.raises(FileNotFoundError):
        _submit(ledger, store, checkpoint)

    with pytest.raises(BrowserConflict, match="does not match the accepted attempt and discovery"):
        _stage(ledger, original, fragment, listed_url="https://src.example/other")
    with pytest.raises(ValueError, match="digest mismatch"):
        _submit(ledger, store, {**checkpoint, "binding": {**checkpoint["binding"], "loader_id": "other"}})
    assert rows(ledger[0], "SELECT count(*) FROM acq_browser_receipts") == [(0,)]
    assert rows(ledger[0], "SELECT count(*) FROM acq_source_observations") == [(0,)]
    assert rows(ledger[0], "SELECT count(*) FROM kernel_outbox") == [(0,)]
    recovered, = rows(ledger[0], "SELECT scope->5 FROM acq_request_tasks WHERE kind = 'browser' AND state = 'staged'")
    assert recovered[0] == checkpoint
    store_browser_pair(store, recovered[0], original, projection)
    assert _submit(ledger, store, recovered[0]).changed


def test_recovers_same_pair_after_storage_fails_between_writes(ledger, tmp_path):
    class OneInterruptedWrite(FilesystemObjectStore):
        def put(self, key, data):
            if key.startswith("projection/") and self.interrupt_projection:
                raise OSError("projection unavailable")
            super().put(key, data)

    database, lease, _ = ledger
    store = OneInterruptedWrite(tmp_path / "objects")
    store.interrupt_projection = True
    spool = ArchiveSpool(tmp_path / "spool")
    original = json.dumps({"id": NOTICE_KEY, "opaque": True}).encode()
    checkpoint, projection = _stage(ledger, original, f'<article data-notice-id="{NOTICE_KEY}"><p>Body</p></article>')
    scope = {"lease": lease, "channel": CHANNEL, "window": WINDOW, "checkpoint": checkpoint}

    with pytest.raises(OSError, match="projection unavailable"):
        archive_browser_pair(database, store, spool, **scope, captured=(original, projection))
    assert rows(database, "SELECT count(*) FROM kernel_outbox") == [(0,)]
    assert len([path for path in spool.root.iterdir() if path.is_file() and path.name != ".lock"]) == 1
    assert store.get(checkpoint["artifact_ref"]) == original
    with pytest.raises(FileNotFoundError):
        store.get(checkpoint["projection"]["artifact_ref"])

    store.interrupt_projection = False
    result = archive_browser_pair(database, store, ArchiveSpool(spool.root), **scope)
    assert result.changed and not result.replayed
    assert rows(database, "SELECT count(*) FROM kernel_outbox") == [(1,)]
    assert len(list(spool.root.iterdir())) == 1
    assert archive_browser_pair(database, store, spool, **scope).replayed
