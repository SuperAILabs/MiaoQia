"""Group receipt and member history tests on isolated PostgreSQL, without source network access."""

from __future__ import annotations

import hashlib
import json
from dataclasses import replace
from datetime import date, timedelta
from uuid import uuid4

import psycopg
import pytest
from alembic import command
from alembic.config import Config
from sqlalchemy import text
from sqlalchemy.exc import DBAPIError, IntegrityError

from miaoqia.acquisition import MIGRATIONS
from miaoqia.acquisition.api.handlers import (
    DetailRef, FetchRequest, FetchResponse, GroupedNotice, GroupSelector, NoticeGroup, Window,
)
from miaoqia.acquisition.application import collection_leases
from miaoqia.acquisition.application.fetch import _instant
from miaoqia.acquisition.application.grouped_observations import COMPLETED, GroupConflict, submit_group, validate_group
from miaoqia.kernel import db, payloads, reqctx

from test_fetch import DAY, LIST_URL, SOURCE, FakeClock, rows

SOURCE_ID = SOURCE.source_id
WINDOW = Window(DAY, DAY)
CHANNEL = "all"
NOW = FakeClock().now()


def _hash(value):
    return f"sha256:{hashlib.sha256(value).hexdigest()}"


class Handler:
    def __init__(self, version=1):
        self.version = version

    def expand_group(self, channel, group, response):
        data = json.loads(response.body)
        if data["group"] != group.group_key:
            raise ValueError("wrong publisher group")
        return tuple(GroupedNotice(
            DetailRef(key, f"https://src.example/d/{key}", date.fromisoformat(day), content),
            GroupSelector("test_selector", self.version, group.group_key, key),
            _hash(json.dumps([day, content]).encode()),
        ) for key, day, content in data["members"])


def _group(key="package-1"):
    return NoticeGroup(key, FetchRequest.post_json("https://src.example/group", {"id": key}))


def _response(group, members, *, wrapper=1):
    data = {"group": group.group_key, "wrapper": wrapper, "members": members}
    return FetchResponse(200, "application/json", json.dumps(data).encode(), group.request.url)


def _member(key, content, day=DAY):
    return key, day.isoformat(), content


@pytest.fixture
def ledger(pg_factory):
    dsns = pg_factory()
    db.migrate(dsns["owner"], MIGRATIONS)
    database = db.Database(dsns["app"])
    with reqctx.use(reqctx.service("acquisition")):
        try:
            with database.tx() as conn:
                conn.execute(text("INSERT INTO acq_sources (source_id, handler, access) VALUES (:source, 'test', '{}')"),
                             {"source": SOURCE_ID})
                lease = collection_leases.claim(conn, SOURCE_ID, WINDOW, max_pages=2, refetch=True,
                                                mode="manual", started_at=NOW, recover=False)
            yield database, lease, dsns["owner"]
        finally:
            database.close()


def _attempt(conn, lease, kind, request, state, *, recorded_url=None):
    params = {"task": uuid4().hex * 2, "run": lease.run_id, "generation": lease.generation,
              "kind": kind, "fingerprint": request.fingerprint(), "method": request.method,
              "url": recorded_url or request.url, "source": lease.source_id, "state": state}
    conn.execute(text("""
        INSERT INTO acq_request_tasks (task_key, run_id, kind, scope, fingerprint)
        VALUES (:task, :run, :kind, '[]', :fingerprint)
    """), params)
    params["download"] = conn.execute(text("""
        INSERT INTO acq_download_starts (run_id, generation, fingerprint, site, task_key)
        VALUES (:run, :generation, :fingerprint, 'src.example', :task) RETURNING id
    """), params).scalar_one()
    params["attempt"] = conn.execute(text("""
        INSERT INTO acq_fetch_attempts (source_id, run_id, download_id, fingerprint, method, url, status, outcome, duration_ms)
        VALUES (:source, :run, :download, :fingerprint, :method, :url, 200, 'ok', 1) RETURNING id
    """), params).scalar_one()
    conn.execute(text("UPDATE acq_request_tasks SET state = :state, last_attempt = :attempt WHERE task_key = :task"),
                 params)
    return params["task"], params["attempt"]


def _stage(ledger, group, response, *, handler=None, listed_group=None):
    database, lease, _ = ledger
    capture = validate_group(handler or Handler(), CHANNEL, group, response)
    listed_group = listed_group or group
    refs = json.dumps([{"group_key": listed_group.group_key, "request_fingerprint": listed_group.request.fingerprint()}])
    with database.tx() as conn:
        _, listing = _attempt(conn, lease, "list", FetchRequest("GET", LIST_URL), "settled")
        conn.execute(text("""
            INSERT INTO acq_discovery_pages (attempt_id, run_id, channel, window_start, window_end, page,
                response_hash, refs, has_next)
            VALUES (:attempt, :run, :channel, :day, :day, 1, :hash, CAST(:refs AS jsonb), FALSE)
        """), {"attempt": listing, "run": lease.run_id, "channel": CHANNEL, "day": DAY, "hash": _hash(b"list"),
               "refs": refs})
        task, attempt = _attempt(conn, lease, "group", group.request, "staged")
        checkpoint = {"attempt_id": attempt, "content_hash": capture.content_hash, "media_type": capture.media_type,
                      "size_bytes": capture.size_bytes, "received_at": _instant(NOW)}
        scope = json.dumps([CHANNEL, DAY.isoformat(), DAY.isoformat(), group.group_key, listing, 1, checkpoint])
        conn.execute(text("UPDATE acq_request_tasks SET scope = CAST(:scope AS jsonb) WHERE task_key = :task"),
                     {"task": task, "scope": scope})
    return dict(lease=lease, window=WINDOW, list_attempt_id=listing, task_key=task, attempt_id=attempt,
                received_at=NOW, capture=capture)


def _submit(database, staged):
    with database.tx() as conn:
        return submit_group(conn, **staged)


def _counts(database):
    return rows(database, """
        SELECT (SELECT count(*) FROM acq_raw_artifacts), (SELECT count(*) FROM acq_group_receipts),
               (SELECT count(*) FROM acq_source_observations), (SELECT count(*) FROM acq_detail_checks),
               (SELECT count(*) FROM kernel_outbox), (SELECT count(*) FROM acq_request_tasks
                                                      WHERE kind = 'group' AND state = 'settled')
    """)[0]


def test_shared_original_member_versions_replay_and_closed_outbox(ledger):
    database, _, _ = ledger
    group = _group()
    first_members = [_member("a", "X"), _member("b", "B"), _member("old", "old", DAY - timedelta(days=1))]
    first_response = _response(group, first_members)
    first = _stage(ledger, group, first_response)
    submitted = _submit(database, first)
    assert (submitted.archived, submitted.unchanged, submitted.outside_window, submitted.replayed) == (2, 0, 1, False)
    assert _submit(database, first) == replace(submitted, replayed=True)
    changed_selector = validate_group(Handler(version=2), CHANNEL, group, first_response)
    with pytest.raises(GroupConflict, match="replay disagrees"):
        _submit(database, {**first, "capture": changed_selector})
    assert _counts(database) == (1, 1, 2, 2, 2, 1)

    order_only = _stage(ledger, group, _response(group, first_members[::-1], wrapper=2))
    same = _submit(database, order_only)
    assert (same.archived, same.unchanged) == (0, 2)
    assert same.artifact_id != submitted.artifact_id
    assert rows(database, "SELECT DISTINCT attempt_id FROM acq_detail_checks") == [(order_only["attempt_id"],)]

    neighbor_only = _stage(ledger, group, _response(group, [_member("a", "X"), _member("b", "B2"), _member("new", "N")]))
    neighbor = _submit(database, neighbor_only)
    assert (neighbor.archived, neighbor.unchanged) == (2, 1)
    changed = _stage(ledger, group, _response(group, [_member("a", "Y"), _member("b", "B2")]))
    middle = _submit(database, changed)
    assert (middle.archived, middle.unchanged) == (1, 1)
    reversion = _stage(ledger, group, first_response)
    reverted = _submit(database, reversion)
    assert (reverted.artifact_id, reverted.archived) == (submitted.artifact_id, 2)
    assert rows(database, """
        SELECT version, artifact_id, attempt_id FROM acq_source_observations WHERE notice_key = 'a' ORDER BY version
    """) == [(1, submitted.artifact_id, first["attempt_id"]), (2, middle.artifact_id, changed["attempt_id"]),
            (3, submitted.artifact_id, reversion["attempt_id"])]
    assert _counts(database) == (4, 5, 7, 3, 7, 5)

    receipt = rows(database, """
        SELECT attempt_id, list_attempt_id, artifact_id, window_start, window_end, members
        FROM acq_group_receipts ORDER BY attempt_id LIMIT 1
    """)[0]
    assert receipt[:5] == (first["attempt_id"], first["list_attempt_id"], submitted.artifact_id, DAY, DAY)
    assert [(item["selector"]["notice_key"], item["outcome"]) for item in receipt[5]] == [
        ("a", "archived"), ("b", "archived"), ("old", "outside_window"),
    ]
    assert rows(database, """
        SELECT count(DISTINCT artifact_id), count(DISTINCT attempt_id) FROM acq_source_observations
        WHERE version = 1 AND notice_key IN ('a', 'b')
    """) == [(1, 1)]
    all_events = rows(database, "SELECT partition_key, envelope FROM kernel_outbox ORDER BY seq")
    for partition, envelope in all_events:
        assert envelope["type"] == COMPLETED
        data = envelope["data"]
        payloads.validator(COMPLETED, {}).validate(data)
        assert data["selector"]["notice_key"] == data["notice_key"] != "old"
        assert partition == f"{SOURCE_ID}|{data['notice_key']}"
        assert envelope["subject"] == data["artifact_ref"] == f"raw/{data['content_hash'][7:]}"
    assert all_events[0][1]["data"]["content_hash"] == _hash(first_response.body)


def test_wrong_group_duplicate_and_cross_group_conflict_leave_ledger_unchanged(ledger):
    database, _, _ = ledger
    first, second = _group(), _group("package-2")
    a = _member("a", "X")
    different_a = _member("a", "Y")
    with pytest.raises(ValueError, match="duplicate notice key"):
        validate_group(Handler(), CHANNEL, first, _response(first, [a, different_a]))
    with pytest.raises(ValueError, match="wrong publisher group"):
        validate_group(Handler(), CHANNEL, first, _response(second, [a]))
    wrong_target = replace(_response(first, [a]), url=first.request.url + "?other=1")
    with pytest.raises(ValueError, match="invalid group response"):
        validate_group(Handler(), CHANNEL, first, wrong_target)
    unlisted = _stage(ledger, first, _response(first, [a]), listed_group=second)
    with pytest.raises(GroupConflict, match="not staged against the recorded listing"):
        _submit(database, unlisted)
    assert _counts(database) == (0, 0, 0, 0, 0, 0)

    initial = _stage(ledger, first, _response(first, [a]))
    original = _submit(database, initial)
    for claimed in (a, different_a, _member("a", "Outside", DAY - timedelta(days=1))):
        conflict = _stage(ledger, second, _response(second, [_member("0-first-in-sort", "new"), claimed]))
        with pytest.raises(GroupConflict, match="already bound to a different group"):
            _submit(database, conflict)
    assert _counts(database) == (1, 1, 1, 1, 1, 1)
    assert rows(database, "SELECT attempt_id FROM acq_detail_checks") == [(initial["attempt_id"],)]
    assert rows(database, "SELECT group_key, artifact_id FROM acq_source_observations") == [
        (first.group_key, original.artifact_id),
    ]


def test_failure_on_second_outbox_row_rolls_back_even_when_outer_transaction_completes(ledger):
    database, _, owner = ledger
    group = _group()
    staged = _stage(ledger, group, _response(group, [_member("a", "X"), _member("b", "B")]))
    with psycopg.connect(owner) as conn:
        conn.execute("""
            CREATE FUNCTION fail_second_member() RETURNS trigger LANGUAGE plpgsql AS $$
            BEGIN
                IF NEW.envelope->'data'->>'notice_key' = 'b' THEN RAISE EXCEPTION 'second member failed'; END IF;
                RETURN NEW;
            END $$;
            CREATE TRIGGER fail_second_member BEFORE INSERT ON kernel_outbox
                FOR EACH ROW EXECUTE FUNCTION fail_second_member();
        """)
    with database.tx() as conn:
        with pytest.raises(DBAPIError, match="second member failed"):
            submit_group(conn, **staged)
        assert conn.execute(text("SELECT 1")).scalar_one() == 1
    assert _counts(database) == (0, 0, 0, 0, 0, 0)
    assert rows(database, "SELECT state, last_attempt FROM acq_request_tasks WHERE kind = 'group'") == [
        ("staged", staged["attempt_id"]),
    ]
    with psycopg.connect(owner) as conn:
        conn.execute("DROP TRIGGER fail_second_member ON kernel_outbox")
    assert _submit(database, staged).archived == 2
    assert _counts(database) == (1, 1, 2, 2, 2, 1)


def test_lease_takeover_accepts_original_attempt_and_stale_capture_cannot_move_check_back(ledger):
    database, lease, _ = ledger
    group = _group()
    before = _stage(ledger, group, _response(group, [_member("a", "X")]))
    later = _stage(ledger, group, _response(group, [_member("a", "Y")]))
    with database.tx() as conn:
        conn.execute(text("UPDATE acq_sources SET lease_until = clock_timestamp() - interval '1 second'"))
        new_lease = collection_leases.claim(conn, SOURCE_ID, WINDOW, max_pages=2, refetch=True, mode="manual",
                                            started_at=NOW, recover=True)
    assert (new_lease.run_id, new_lease.generation) == (lease.run_id, lease.generation + 1)
    with pytest.raises(collection_leases.LeaseLost):
        _submit(database, later)
    assert _submit(database, {**later, "lease": new_lease}).archived == 1
    assert _submit(database, {**before, "lease": new_lease}).stale == 1
    assert rows(database, "SELECT version, attempt_id FROM acq_detail_checks") == [(1, later["attempt_id"])]
    assert _counts(database) == (2, 2, 1, 1, 1, 2)


def test_blocked_source_and_mismatched_attempt_cannot_settle_group(ledger):
    database, _, _ = ledger
    group = _group()
    staged = _stage(ledger, group, _response(group, [_member("a", "X")]))
    with pytest.raises(GroupConflict, match="not staged against the recorded listing"):
        _submit(database, {**staged, "attempt_id": staged["list_attempt_id"]})
    replaced_bytes = validate_group(Handler(), CHANNEL, group, _response(group, [_member("a", "Y")]))
    with pytest.raises(GroupConflict, match="not staged against the recorded listing"):
        _submit(database, {**staged, "capture": replaced_bytes})
    with database.tx() as conn:
        conn.execute(text("UPDATE acq_sources SET state = 'blocked', state_reason = 'fixture'"))
    with pytest.raises(GroupConflict, match="source cannot archive a group"):
        _submit(database, staged)
    assert _counts(database) == (0, 0, 0, 0, 0, 0)


def test_empty_group_settles_and_selector_version_alone_versions_digest_namespace(ledger):
    database, _, _ = ledger
    group = _group()
    empty = _submit(database, _stage(ledger, group, _response(group, [])))
    assert (empty.archived, empty.unchanged, empty.outside_window) == (0, 0, 0)
    assert _counts(database) == (1, 1, 0, 0, 0, 1)

    a = _response(group, [_member("a", "X")])
    first = _submit(database, _stage(ledger, group, a))
    updated = _submit(database, _stage(ledger, group, a, handler=Handler(version=2)))
    assert (updated.archived, updated.artifact_id) == (1, first.artifact_id)
    assert rows(database, "SELECT version, selector->>'version' FROM acq_source_observations ORDER BY version") == [
        (1, "1"), (2, "2"),
    ]
    assert _counts(database) == (2, 3, 2, 1, 2, 3)


def _migration(owner):
    cfg = Config()
    cfg.set_main_option("script_location", MIGRATIONS)
    cfg.set_main_option("sqlalchemy.url", db._url(owner))
    return cfg


def test_migration_keeps_legacy_observations_and_refuses_history_loss(ledger):
    database, lease, owner = ledger
    cfg = _migration(owner)
    command.downgrade(cfg, "0014")
    with database.tx() as conn:
        request = FetchRequest("GET", "https://src.example/d/legacy")
        _, attempt = _attempt(conn, lease, "body", request, "settled")
        digest = _hash(b"legacy")
        artifact = conn.execute(text("""
            INSERT INTO acq_raw_artifacts (source_id, source_key, content_hash, artifact_ref, media_type, size_bytes,
                source_url, request_fingerprint, fetched_at)
            VALUES (:source, 'legacy/body', :hash, :ref, 'text/html', 6, :url, :fingerprint, :now) RETURNING id
        """), {"source": SOURCE_ID, "hash": digest, "ref": f"raw/{digest[7:]}", "url": request.url,
               "fingerprint": request.fingerprint(), "now": NOW}).scalar_one()
        conn.execute(text("""
            INSERT INTO acq_source_observations (source_id, notice_key, version, artifact_id, attempt_id, source_url, observed_at)
            VALUES (:source, 'legacy', 1, :artifact, :attempt, :url, :now)
        """), {"source": SOURCE_ID, "artifact": artifact, "attempt": attempt, "url": request.url, "now": NOW})
    command.upgrade(cfg, "0015")
    assert rows(database, "SELECT notice_key, selector, record_digest FROM acq_source_observations") == [
        ("legacy", None, None),
    ]
    with database.tx() as conn:
        with pytest.raises(IntegrityError, match="acq_source_observations_selector"), conn.begin_nested():
            conn.execute(text("""
                INSERT INTO acq_source_observations (source_id, notice_key, version, artifact_id, attempt_id,
                    source_url, published_on, observed_at, record_digest)
                SELECT source_id, notice_key, version + 1, artifact_id, attempt_id, source_url, :day, observed_at, :digest
                FROM acq_source_observations WHERE notice_key = 'legacy'
            """), {"day": DAY, "digest": digest})
    command.downgrade(cfg, "0014")
    command.upgrade(cfg, "head")
    head = rows(database, "SELECT version_num FROM alembic_version")

    group = _group()
    staged = _stage(ledger, group, _response(group, [_member("a", "X")]))
    with pytest.raises(DBAPIError, match="cannot downgrade with grouped acquisition history"):
        command.downgrade(cfg, "0014")
    assert _submit(database, staged).archived == 1
    with pytest.raises(DBAPIError, match="cannot downgrade with grouped acquisition history"):
        command.downgrade(cfg, "0014")
    assert rows(database, "SELECT version_num FROM alembic_version") == head
    assert rows(database, "SELECT notice_key FROM acq_source_observations ORDER BY notice_key") == [("a",), ("legacy",)]
