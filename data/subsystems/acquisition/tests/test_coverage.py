"""Source admission keeps staged protocols separate from live health and coverage."""
import time
from dataclasses import replace
from datetime import timedelta
from pathlib import Path

import pytest
from sqlalchemy import text

from miaoqia.acquisition import MIGRATIONS
from miaoqia.acquisition.api.handlers import DetailFetch, Window
from miaoqia.acquisition.application import coverage, discovery, grouped_observations, initial_backfill, sources
from miaoqia.acquisition.infrastructure.transport import TransportError
from miaoqia.kernel import db, reqctx
from miaoqia.runtime_acquisition import fetch as fetch_cli

from test_fetch import DAY, LIST_URL, SOURCE, Handler, env, fetcher, listing, ok, rows
from test_grouped_fetch import GROUP_URL, SOURCE_GROUPS, _group, _list

REPO = Path(__file__).resolve().parents[4]


def test_operator_report_requires_a_database_connection(monkeypatch, capsys):
    monkeypatch.delenv("MIAOQIA_ACQUISITION_DB_DSN", raising=False)
    with pytest.raises(SystemExit) as error:
        fetch_cli.main(["--list-sources", "--require-runtime", "--packs", str(REPO / "packs")])
    captured = capsys.readouterr()
    assert error.value.code == 2 and captured.out == ""
    assert "required for the runtime coverage report" in captured.err


def test_operator_report_connection_failure_has_fixed_output(monkeypatch, capsys):
    monkeypatch.setenv("MIAOQIA_ACQUISITION_DB_DSN", "invalid-fixture-with-sensitive-marker")
    assert fetch_cli.main(["--list-sources", "--require-runtime", "--packs", str(REPO / "packs")]) == 1
    captured = capsys.readouterr()
    assert captured.out == ""
    assert captured.err == "runtime coverage report unavailable\n"


def test_inventory_includes_every_source_without_activating_unverified_protocols():
    catalog = sources.catalog(REPO / "packs")
    current = sources.load(REPO / "packs")
    assert len(catalog) == 40
    assert all(catalog[sid].admission == "active" for sid in current)
    assert catalog["ymzec-trade-info"].admission == "authorization_required"
    assert all(sid not in current for sid in catalog if catalog[sid].readiness != "capability_verified")
    reported = {entry["source_id"]: entry["transport"] for entry in coverage.report(catalog, None)}
    assert all(reported[sid] == source.transport for sid, source in current.items())


def test_live_admission_requires_verified_inventory_and_complete_group_contract(monkeypatch, tmp_path):
    pack = tmp_path / "information_domains" / "example"
    pack.mkdir(parents=True)
    (pack / "pack.yaml").write_text("pack:\n  provides:\n    sources: sources.yaml\n")
    inventory = pack / "source-inventory.yaml"
    inventory.write_text("sources:\n- source_id: demo\n  readiness: candidate\n")
    declaration = ("sources:\n- source_id: demo\n  handler: demo_pack.module:Handler\n"
                   "  access: {min_interval_seconds: 2, throttle_backoff_seconds: 60}\n")
    path = pack / "sources.yaml"
    path.write_text(declaration + "  admission: staged\n")
    from types import SimpleNamespace

    class Handler:
        source_id = "demo"
        channels = ("announcements",)
        windowed = False
        list_request = parse_list = detail_fetches = check_detail = None

    monkeypatch.setattr(sources, "load_pack_module", lambda path, module: SimpleNamespace(Handler=Handler))
    staged = sources.catalog(tmp_path)["demo"]
    assert staged.admission == "staged" and staged.declaration.handler.channels == ("announcements",)
    assert sources.load(tmp_path) == {}
    path.write_text(declaration)
    with pytest.raises(ValueError, match="not capability_verified"):
        sources.load(tmp_path)
    inventory.write_text(inventory.read_text().replace("candidate", "capability_verified"))
    Handler.expand_group = lambda *_: ()
    with pytest.raises(ValueError, match="grouped source requires group_request"):
        sources.load(tmp_path)
    Handler.group_request = lambda *_: None
    with pytest.raises(ValueError, match="grouped source requires to_selected_html"):
        sources.load(tmp_path)
    Handler.to_selected_html = lambda *_: "<p>Fixture</p>"
    assert list(sources.load(tmp_path)) == ["demo"]


def test_coverage_preserves_blocked_reason_and_separate_incremental_and_backfill_progress(pg_factory):
    dsns = pg_factory()
    db.migrate(dsns["owner"], MIGRATIONS)
    database = db.Database(dsns["app"])
    sid = "anhui-ggzy-construction"
    try:
        with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
            conn.execute(text("""
                INSERT INTO acq_sources (source_id, handler, access, state, state_reason)
                VALUES (:sid, 'fixture', '{}', 'blocked', 'http_403')
            """), {"sid": sid})
            conn.execute(text("""
                INSERT INTO acq_initial_backfill (source_id, oldest_day, next_day)
                VALUES (:sid, '2026-10-21', '2026-11-12')
            """), {"sid": sid})
            incremental_id = conn.execute(text("""
                INSERT INTO acq_collection_runs
                    (source_id, window_start, window_end, max_pages, refetch, started_at, mode, generation)
                VALUES (:sid, '2026-11-18', '2026-11-19', 2, true, now(), 'incremental', 2) RETURNING id
            """), {"sid": sid}).scalar_one()
            conn.execute(text("""
                UPDATE acq_collection_runs SET pending_report = '{"deferred":"archive_spool_full"}'
                WHERE source_id = :sid
            """), {"sid": sid})
            conn.execute(text("""
                INSERT INTO acq_request_tasks (task_key, run_id, kind, scope, fingerprint)
                SELECT repeat('a',64), id, 'list', '[]', 'sha256:' || repeat('b',64)
                FROM acq_collection_runs WHERE source_id = :sid
            """), {"sid": sid})
            run_id = conn.execute(text("""
                INSERT INTO acq_collection_runs
                    (source_id, window_start, window_end, max_pages, refetch, started_at, mode)
                VALUES (:sid, '2026-11-13', '2026-11-13', 5, false, now(), 'backfill') RETURNING id
            """), {"sid": sid}).scalar_one()
            conn.execute(text("""
                INSERT INTO acq_initial_backfill_gaps (source_id, day, last_run_id)
                VALUES (:sid, '2026-11-13', :run)
            """), {"sid": sid, "run": run_id})
        states = {entry["source_id"]: entry for entry in coverage.report(sources.catalog(REPO / "packs"), database)}
        assert states[sid]["admission"] == "active"
        runtime = states[sid]["runtime"]
        assert runtime["state"] == "blocked" and runtime["blocked_reason"] == "http_403"
        assert runtime["initial_backfill_pending"] == "2026-11-12"
        assert runtime["initial_backfill_gaps"] == [
            {"day": "2026-11-13", "run_id": run_id, "wait_for_source_change": False}]
        assert runtime["window_start"] == "2026-11-18" and runtime["finished_at"] is None
        assert runtime["incremental_report"] == {"deferred": "archive_spool_full"}
        assert runtime["pending_incremental_count"] == 1
        assert runtime["pending_incremental_windows"] == [
            {"run_id": incremental_id, "since": "2026-11-18", "until": "2026-11-19", "generation": 2}
        ]
        assert runtime["incremental_request_tasks"] == {"queued": 1}
        assert states["ymzec-trade-info"]["runtime"] is None
    finally:
        database.close()


def _history(database, source=SOURCE, *, window=None):
    catalog = {source.source_id: sources.CatalogSource(
        source.source_id, "capability_verified", "public", source, "active",
    )}
    entry, = coverage.report(catalog, database, window=window)
    return entry["runtime"]["historical_coverage"]


@pytest.mark.parametrize("corrupt", [
    pytest.param("""UPDATE acq_collection_runs SET report = report - 'historical_scan'
                     WHERE id = :run""", id="legacy-no-marker"),
    pytest.param("""UPDATE acq_collection_runs
                     SET report = jsonb_set(report, '{historical_scan,generation}', to_jsonb('invalid'::text))
                     WHERE id = :run""", id="malformed-marker"),
    pytest.param("DELETE FROM acq_discovery_pages WHERE run_id = :run", id="missing-receipt"),
    pytest.param("""UPDATE acq_discovery_pages SET window_start = window_start - 1, window_end = window_end - 1
                     WHERE run_id = :run""", id="receipt-from-wrong-day"),
])
def test_historical_coverage_downgrades_untrustworthy_latest_run(env, corrupt):
    database, _, _ = env
    day = DAY - timedelta(days=1)
    collector, _ = fetcher(env, {LIST_URL: [listing(), listing()]})
    collector.run(SOURCE, Window(day, day), max_pages=1, mode="backfill")
    latest = collector.run(SOURCE, Window(day, day), max_pages=1)
    _, (attempt_id,) = rows(database, "SELECT attempt_id FROM acq_discovery_pages ORDER BY run_id")

    before, = _history(database)
    assert (before["run_id"], before["list_status"], before["required_originals_status"],
            before["list_receipt_attempt_ids"]) == (latest.run_id, "proven_empty", "not_required", [attempt_id])

    with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
        assert conn.execute(text(corrupt), {"run": latest.run_id}).rowcount == 1

    after, = _history(database)
    expected = {"day": day.isoformat(), "channel": "all", "run_id": latest.run_id, "mode": "manual",
                "run_finished": True, "list_status": "unknown", "required_originals_status": "unknown",
                "list_receipt_attempt_ids": []}
    assert {key: after[key] for key in expected} == expected


def test_historical_coverage_keeps_channels_and_publication_days_distinct(env):
    database, _, _ = env
    handler = Handler()
    handler.channels = ("construction", "government")
    source = replace(SOURCE, handler=handler)
    yesterday = DAY - timedelta(days=1)
    previous = yesterday - timedelta(days=1)
    collector, _ = fetcher(env, {
        LIST_URL: [
            listing(f"notice@{previous}"), listing(),  # previous day's backfill
            listing(), listing(),  # yesterday's backfill
            listing(f"notice@{yesterday}"), listing("outside", has_next=True),  # yesterday's manual recheck
            listing(), listing(),  # incremental
            listing(), listing(),  # multi-day manual
            listing(), listing(),  # same-day manual
        ],
        "https://src.example/d/notice": [ok(b"original for the previous day"), ok(b"empty")],
    })
    older = collector.run(source, Window(previous, previous), max_pages=1, mode="backfill")
    collector.run(source, Window(yesterday, yesterday), max_pages=1, mode="backfill")
    latest = collector.run(source, Window(yesterday, yesterday), max_pages=1, refetch=True)
    collector.run(source, Window(yesterday, yesterday), max_pages=1, mode="incremental")
    collector.run(source, Window(previous, yesterday), max_pages=1)
    collector.run(source, Window(DAY, DAY), max_pages=1)

    attempts = {(run, channel): attempt for run, channel, attempt in rows(database, """
        SELECT run_id, channel, attempt_id FROM acq_discovery_pages
    """)}
    history = _history(database, source)
    assert len(history) == 4
    assert {(row["day"], row["channel"]): (row["run_id"], row["mode"], row["list_status"],
                                          row["required_originals_status"]) for row in history} == {
        (yesterday.isoformat(), "construction"): (latest.run_id, "manual", "complete", "missing"),
        (yesterday.isoformat(), "government"): (latest.run_id, "manual", "incomplete", "unknown"),
        (previous.isoformat(), "construction"): (older.run_id, "backfill", "complete", "archived"),
        (previous.isoformat(), "government"): (older.run_id, "backfill", "proven_empty", "not_required"),
    }
    assert all(row["run_finished"] is True for row in history)
    assert all(row["list_receipt_attempt_ids"] == [attempts[row["run_id"], row["channel"]]] for row in history)


def test_historical_coverage_uses_pending_report_for_its_own_generation(env):
    database, _, _ = env
    day = DAY - timedelta(days=1)
    collector, _ = fetcher(env, {LIST_URL: [listing()]})
    outcome = collector.run(SOURCE, Window(day, day), max_pages=1, mode="backfill")

    with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
        conn.execute(text("""
            UPDATE acq_collection_runs SET pending_report = report, report = NULL, finished_at = NULL
            WHERE id = :run
        """), {"run": outcome.run_id})
    result, = _history(database)
    assert not result["run_finished"] and result["list_status"] == "proven_empty"

    with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
        conn.execute(text("""
            UPDATE acq_collection_runs SET generation = generation + 1 WHERE id = :run
        """), {"run": outcome.run_id})
    result, = _history(database)
    assert not result["run_finished"] and result["list_status"] == "unknown"


def test_historical_coverage_bounds_distinct_days_and_receipt_ids(env):
    database, _, clock = env
    yesterday = DAY - timedelta(days=1)
    window = Window(yesterday, yesterday)
    page_count = 51
    collector, _ = fetcher(env, {
        SOURCE.handler.list_request("all", window, page).url: [listing(f"outside-{page}", has_next=page < page_count)]
        for page in range(1, page_count + 1)
    })
    latest = collector.run(SOURCE, window, max_pages=page_count)
    with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
        # Legacy retries exercise the date bound without fetching pages for every old run.
        conn.execute(text("""
            INSERT INTO acq_collection_runs
                (source_id, window_start, window_end, max_pages, refetch, started_at, finished_at, report, mode)
            SELECT :source, CAST(:today AS date) - days_ago, CAST(:today AS date) - days_ago,
                   1, false, :now, :now, '{}'::jsonb, 'backfill'
            FROM generate_series(2, 35) AS days(days_ago) CROSS JOIN generate_series(1, 2)
        """), {"source": SOURCE.source_id, "today": DAY, "now": clock.now()})
    attempts = [attempt for attempt, in rows(database, "SELECT attempt_id FROM acq_discovery_pages ORDER BY page")]
    assert len(attempts) == page_count

    history = _history(database)
    assert [row["day"] for row in history] == [(DAY - timedelta(days=i)).isoformat() for i in range(1, 33)]
    newest = history[0]
    assert newest["run_id"] == latest.run_id
    assert newest["list_status"] == "proven_empty"
    assert newest["list_receipt_count"] == page_count
    assert newest["list_receipt_attempt_ids"] == attempts[:50]

    # The output cap must still allow validation of the terminal receipt.
    with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
        conn.execute(text("DELETE FROM acq_discovery_pages WHERE attempt_id = :attempt"), {"attempt": attempts[-1]})
    assert _history(database)[0]["list_status"] == "unknown"


def test_ordered_cutoff_still_proves_empty_after_list_bytes_expire(env):
    database, store, _ = env
    yesterday = DAY - timedelta(days=1)
    two_days_ago = yesterday - timedelta(days=1)
    collector, _ = fetcher(env, {LIST_URL: [listing(f"older@{two_days_ago}", has_next=True)]})
    collector.run(SOURCE, Window(yesterday, yesterday), max_pages=1, mode="backfill")

    with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
        attempt, ref = conn.execute(text("""
            UPDATE acq_discovery_pages SET expires_at = now() - interval '1 second' RETURNING attempt_id, object_ref
        """)).one()
    assert store.get(ref)
    assert discovery.expire(database, store, deadline=time.monotonic() + 5) == 1
    with pytest.raises(FileNotFoundError):
        store.get(ref)
    result, = _history(database)
    assert (result["list_status"], result["required_originals_status"], result["list_receipt_attempt_ids"]) == (
        "proven_empty", "not_required", [attempt],
    )


@pytest.mark.parametrize(("attachment", "status"), [
    ("required", "missing"), ("optional", "archived"), ("invalid_manifest", "unknown"),
])
def test_original_status_accounts_for_required_attachment_manifest(env, attachment, status):
    database, _, _ = env

    class WithAttachment(Handler):
        def attachment_fetches(self, channel, ref, body):
            if attachment == "invalid_manifest":
                raise ValueError("invalid fixture manifest")
            return (DetailFetch("part", kind="attachment", required=attachment == "required"),)

    source = replace(SOURCE, handler=WithAttachment())
    collector, _ = fetcher(env, {LIST_URL: [listing("notice")], "https://src.example/d/notice": [ok(b"fixture body")]})
    collector.run(source, Window(DAY, DAY), max_pages=1, mode="backfill")
    result, = _history(database, source)
    assert (result["list_status"], result["required_originals_status"]) == ("complete", status)


def test_backfill_keeps_undated_gap_until_same_original_is_linked_to_the_day(env):
    database, _, _ = env

    class Undated(Handler):
        def parse_list(self, channel, response):
            parsed = super().parse_list(channel, response)
            if b"@" in response.body:
                return parsed
            return replace(parsed, refs=tuple(replace(ref, published=None) for ref in parsed.refs),
                           ordered_by_publication=False)

    source = replace(SOURCE, handler=Undated())
    previous_day = DAY - timedelta(days=1)
    collector, _ = fetcher(env, {LIST_URL: [listing("notice"), listing(f"notice@{DAY}"),
                                           listing(f"notice@{previous_day}"), listing("notice")],
                                 "https://src.example/d/notice": [ok(b"body")] * 4})
    with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
        sources.register(conn, source)
        initial_backfill.next_day(conn, source.source_id, DAY + timedelta(days=1), 2, "fixture")

    run = collector.run(source, Window(DAY, DAY), max_pages=1, mode="backfill")
    assert run.archived == 1 and run.gaps[0]["reason"] == initial_backfill.LIST_DATE_UNAVAILABLE
    assert rows(database, "SELECT day, last_run_id FROM acq_initial_backfill_gaps") == [(DAY, run.run_id)]
    record, = _history(database, source)
    assert (record["list_status"], record["required_originals_status"]) == ("incomplete", "unknown")

    retry = collector.run(source, Window(DAY, DAY), max_pages=1, mode="backfill")
    record, = _history(database, source)
    assert (record["list_status"], record["required_originals_status"]) == ("complete", "archived")
    assert retry.archived == 1 and not retry.gaps
    assert rows(database, "SELECT day FROM acq_initial_backfill_gaps") == []
    assert rows(database, "SELECT next_day FROM acq_initial_backfill") == [(None,)]
    first, second = rows(database, "SELECT version, published_on, artifact_id FROM acq_source_observations ORDER BY version")
    assert first == (1, None, second[2]) and second[:2] == (2, DAY)

    corrected = collector.run(source, Window(previous_day, DAY), max_pages=1)
    assert corrected.archived == 1 and not corrected.gaps
    undated = collector.run(source, Window(previous_day, DAY), max_pages=1, refetch=True)
    assert undated.unchanged == 1
    assert rows(database, """
        SELECT version, published_on, artifact_id FROM acq_detail_checks
        JOIN acq_source_observations USING (source_id, notice_key, version)
    """) == [(3, previous_day, first[2])]


@pytest.mark.parametrize("mode", ["manual", "backfill"])
def test_day_settlement_requires_persisted_list_receipt(env, monkeypatch, mode):
    database, _, _ = env
    day = DAY - timedelta(days=1)
    window = Window(day, day)
    collector, _ = fetcher(env, {LIST_URL: [listing(), listing()]})
    with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
        sources.register(conn, SOURCE)
        initial_backfill.next_day(conn, SOURCE.source_id, DAY, 2, "fixture")

    finish = collector._health

    def lose_receipt(source, run):
        with database.tx() as conn:
            assert conn.execute(text("DELETE FROM acq_discovery_pages WHERE run_id = :run"), {"run": run.id}).rowcount == 1
        finish(source, run)

    with monkeypatch.context() as patch:
        patch.setattr(collector, "_health", lose_receipt)
        attempt = collector.run(SOURCE, window, max_pages=1, mode=mode)
    assert attempt.pages == 1 and attempt.health == "degraded"
    assert [gap["reason"] for gap in attempt.gaps] == ["coverage_unverified"]
    assert rows(database, "SELECT day, last_run_id FROM acq_initial_backfill_gaps") == [(day, attempt.run_id)]
    assert rows(database, "SELECT report->'gaps' FROM acq_collection_runs") == [(attempt.gaps,)]
    history, = _history(database)
    assert history["list_status"] == "unknown"

    retry = collector.run(SOURCE, window, max_pages=1, mode=mode)
    assert retry.health == "healthy" and not retry.gaps
    assert rows(database, "SELECT day FROM acq_initial_backfill_gaps") == []
    history, = _history(database)
    assert (history["list_status"], history["required_originals_status"]) == ("proven_empty", "not_required")


def test_backfill_retry_rechecks_unverified_attachment_manifest(env):
    database, _, _ = env

    class RecoverableManifest(Handler):
        calls = 0

        def attachment_fetches(self, channel, ref, body):
            self.calls += 1
            if self.calls == 1:
                raise ValueError("invalid fixture manifest")
            return ()

    handler = RecoverableManifest()
    source = replace(SOURCE, handler=handler)
    collector, _ = fetcher(env, {LIST_URL: [listing("notice")] * 3,
                                 "https://src.example/d/notice": [ok(b"body")] * 2})
    with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
        sources.register(conn, source)
        initial_backfill.next_day(conn, source.source_id, DAY + timedelta(days=1), 2, "fixture")

    window = Window(DAY, DAY)
    first = collector.run(source, window, max_pages=1, mode="backfill")
    assert rows(database, "SELECT day, last_run_id FROM acq_initial_backfill_gaps") == [(DAY, first.run_id)]
    retry = collector.run(source, window, max_pages=1, mode="backfill")
    assert handler.calls == 2 and retry.unchanged == 1 and not retry.gaps
    assert rows(database, "SELECT day FROM acq_initial_backfill_gaps") == []
    third = collector.run(source, window, max_pages=1, mode="backfill")
    assert handler.calls == 2 and third.skipped == 1
    record, = _history(database, source)
    assert record["required_originals_status"] == "archived"


def test_fixed_historical_window_includes_days_without_a_run(env):
    database, _, _ = env
    start = DAY - timedelta(days=2)
    collector, _ = fetcher(env, {LIST_URL: [listing()] * 3})
    for day in (start, DAY, DAY + timedelta(days=1)):
        collector.run(SOURCE, Window(day, day), max_pages=1, mode="backfill")

    history = _history(database, window=Window(start, DAY))
    assert [(row["day"], row["list_status"]) for row in history] == [
        (DAY.isoformat(), "proven_empty"),
        ((DAY - timedelta(days=1)).isoformat(), "unknown"),
        (start.isoformat(), "proven_empty"),
    ]
    assert history[1]["required_originals_status"] == "unknown"
    assert history[1]["run_id"] is None and history[1]["list_receipt_count"] == 0


@pytest.mark.parametrize(("group", "expected"), [
    (_group(), ("complete", "archived")),
    (TransportError("network_error", retryable=False), ("incomplete", "missing")),
])
def test_group_index_requires_a_committed_expansion_for_notice_coverage(env, group, expected):
    database, _, _ = env
    collector, _ = fetcher(env, {LIST_URL: [_list("section")], GROUP_URL: [group]})
    collector.run(SOURCE_GROUPS, Window(DAY, DAY), max_pages=1, mode="backfill")
    result, = _history(database, SOURCE_GROUPS)
    assert (result["list_status"], result["required_originals_status"]) == expected


def test_stale_group_members_do_not_settle_required_originals(env, monkeypatch):
    database, _, _ = env

    def stale_members(_conn, _params, window, notices, members):
        for notice, member in zip(notices, members, strict=True):
            member["outcome"] = "stale" if notice.ref.published == window.since else "outside_window"
        return []

    monkeypatch.setattr(grouped_observations, "_plan_members", stale_members)
    collector, _ = fetcher(env, {LIST_URL: [_list("section")], GROUP_URL: [_group()]})
    with reqctx.use(reqctx.service("acquisition")), database.tx() as conn:
        sources.register(conn, SOURCE_GROUPS)
        initial_backfill.next_day(conn, SOURCE_GROUPS.source_id, DAY + timedelta(days=1), 2, "fixture")
    run = collector.run(SOURCE_GROUPS, Window(DAY, DAY), max_pages=1, mode="backfill")

    assert run.gaps[0]["reason"] == "stale_group_member_requires_recheck"
    assert rows(database, "SELECT day FROM acq_initial_backfill_gaps") == [(DAY,)]
    result, = _history(database, SOURCE_GROUPS)
    assert (result["list_status"], result["required_originals_status"]) == ("complete", "unknown")
