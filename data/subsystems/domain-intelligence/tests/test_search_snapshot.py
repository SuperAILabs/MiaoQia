"""Synthetic owner snapshots against isolated, migrated PostgreSQL databases."""

from datetime import datetime, timezone
import json
import os
import subprocess
import sys
from urllib.parse import urlsplit

import psycopg
from psycopg import sql
import pytest
from sqlalchemy import event, text

from miaoqia.domain_intelligence import MIGRATIONS
from miaoqia.domain_intelligence.application import requirements, search_snapshot
from miaoqia.evidence.api.provenance import Closure, source_input
from miaoqia.kernel import db
from miaoqia.sdk.derivation import fingerprint

from test_search_projection import SOURCE, export, plane  # noqa: F401

NOTICE = "tendering.notice"
PROJECT = "tendering.procurement_project"
OWNER_CLI = [sys.executable, "-m", "miaoqia.runtime_data.search_snapshot"]
PAGE_CLI = [sys.executable, "-m", "miaoqia.runtime_data.search_projection", "snapshot-page"]


@pytest.fixture
def store(pg_factory):
    dsns = pg_factory()
    db.migrate(dsns["owner"], MIGRATIONS)
    database = db.Database(dsns["app"])
    try:
        yield database, dsns
    finally:
        database.close()


def _id(prefix, number):
    return f"{prefix}_{number:026d}"


def _seed(conn, fact_id, *, kind=NOTICE, domain="INF-PROC", version=1, proof=False):
    if version == 1:
        conn.execute(text("""
            INSERT INTO dom_facts (id, info_domain, object_type, source_id, source_key, ordinal, current_version)
            VALUES (:id, :domain, :kind, :source, :id, 0, 1)
        """), {"id": fact_id, "domain": domain, "kind": kind, "source": SOURCE})
    else:
        conn.execute(text("UPDATE dom_facts SET current_version=:v WHERE id=:id"), {"id": fact_id, "v": version})
    tags = [f"public:{SOURCE}"]
    fact = {"id": fact_id, "version": version, "info_domain": domain, "classification": "public",
            "rights_tags": tags, "title": {"value": "Synthetic procurement"}}
    revision = _id("exr", version)
    conn.execute(text("""
        INSERT INTO dom_fact_versions (fact_id, version, content, value_hash, schema_version, snapshot_ref,
                                       revision_ref, changed_fields, rights_tags, observed_at)
        VALUES (:id, :v, CAST(:content AS jsonb), :digest, '1.0.0', :snapshot,
                :revision, '["/title"]', CAST(:tags AS jsonb), clock_timestamp())
    """), {"id": fact_id, "v": version, "content": json.dumps(fact), "digest": fingerprint(fact),
           "snapshot": _id("snp", version), "revision": revision, "tags": json.dumps(tags)})
    if proof:
        source = source_input({"source_id": SOURCE, "tags": tags})
        requirements.put(conn, fact, Closure((source,)).with_dependency("revision", revision, fingerprint(fact)))
    return {"id": fact_id, "version": version}


def _cli(command, dsn, *args, payload=None, extra_env=None):
    env = dict(os.environ, MIAOQIA_DOMAIN_INTELLIGENCE_DB_DSN=dsn)
    env.update(extra_env or {})
    return subprocess.run([*command, *args], input=None if payload is None else json.dumps(payload),
                          text=True, env=env, capture_output=True, timeout=12)


def _assert_manifest(manifest, pages):
    assert set(manifest) == {"snapshot_id", "as_of", "handle_count", "pages"}
    assert manifest["pages"] == pages
    assert manifest["handle_count"] == sum(map(len, pages))
    assert manifest["snapshot_id"] == fingerprint({k: v for k, v in manifest.items() if k != "snapshot_id"})


def test_default_pagination_scope_and_hard_cap(store):
    database, dsns = store
    with database.engine.begin() as conn:
        project = _seed(conn, _id("pp", 91), kind=PROJECT)
        notices = [_seed(conn, _id("ntc", index)) for index in reversed(range(33))]
        _seed(conn, _id("ntc", 95), domain="INF-OTHER")
        _seed(conn, _id("bdr", 88), kind="tendering.bid_result")
    ordered = [*reversed(notices), project]
    app_role = sql.Identifier(urlsplit(dsns["app"]).username)
    with psycopg.connect(dsns["owner"]) as conn:
        conn.execute(sql.SQL("REVOKE ALL ON dom_facts, dom_fact_versions, dom_version_requirements FROM {}").format(app_role))
        conn.execute(sql.SQL("GRANT SELECT (id, info_domain, object_type, current_version) ON dom_facts TO {}").format(app_role))
        conn.execute(sql.SQL("GRANT SELECT (fact_id, version) ON dom_fact_versions TO {}").format(app_role))
    result = _cli(OWNER_CLI, dsns["app"], "--max-handles", "34")
    assert (result.returncode, result.stderr) == (0, "")
    _assert_manifest(json.loads(result.stdout), [ordered[:32], ordered[32:]])
    rejected = _cli(OWNER_CLI, dsns["app"], "--max-handles", "33")
    assert (rejected.returncode, rejected.stdout) == (3, "")
    assert "exceeds 33 handles" in rejected.stderr


def test_view_excludes_inserts_and_version_committed_between_pages(store):
    database, _ = store
    with database.engine.begin() as conn:
        notice = _seed(conn, _id("ntc", 1))
        project = _seed(conn, _id("pp", 1), kind=PROJECT)

    with database.engine.connect() as writer, writer.begin() as transaction:
        writer.exec_driver_sql("SET LOCAL lock_timeout = '1s'")
        inserted = _seed(writer, _id("ntc", 2))
        advanced = _seed(writer, project["id"], kind=PROJECT, version=2)
        before = writer.execute(text("SELECT clock_timestamp()")).scalar_one()
        reader_connection = None
        fetches = 0
        committed_at = None

        def commit_between_pages(conn, cursor, statement, parameters, context, executemany):
            nonlocal reader_connection, fetches, committed_at
            if context.execution_options.get("yield_per") == 1:
                reader_connection = conn
            if conn is reader_connection and statement.startswith("SET LOCAL statement_timeout"):
                fetches += 1
                if fetches == 2:
                    transaction.commit()
                    with database.engine.connect() as observer:
                        committed_at = observer.execute(text("SELECT clock_timestamp()")).scalar_one()

        event.listen(database.engine, "before_cursor_execute", commit_between_pages)
        try:
            frozen = search_snapshot.capture(database, page_size=1)
        finally:
            event.remove(database.engine, "before_cursor_execute", commit_between_pages)

    assert committed_at is not None
    as_of = datetime.fromisoformat(frozen["as_of"])
    assert as_of.tzinfo == timezone.utc and before <= as_of <= committed_at
    _assert_manifest(frozen, [[notice], [project]])
    refreshed = search_snapshot.capture(database, page_size=1)
    _assert_manifest(refreshed, [[notice], [inserted], [advanced]])
    assert datetime.fromisoformat(refreshed["as_of"]) >= committed_at
    assert frozen["snapshot_id"] != refreshed["snapshot_id"]


def test_empty_snapshot_and_database_statement_timeout(store):
    database, dsns = store
    dsn = dsns["app"]
    empty = _cli(OWNER_CLI, dsn, "--page-size", "1")
    assert (empty.returncode, empty.stderr) == (0, "")
    _assert_manifest(json.loads(empty.stdout), [])
    with database.engine.begin() as conn:
        conn.exec_driver_sql("LOCK TABLE dom_facts IN ACCESS EXCLUSIVE MODE")
        timed_out = _cli(OWNER_CLI, dsn, "--timeout-seconds", "1")
    assert (timed_out.returncode, timed_out.stdout) == (3, "")
    assert "timed out" in timed_out.stderr or "deadline exceeded" in timed_out.stderr


@pytest.mark.parametrize("invalid", ["missing current version", "type and identity mismatch"])
def test_invalid_current_pointer_fails_without_manifest(store, invalid):
    database, dsns = store
    with database.engine.begin() as conn:
        if invalid == "missing current version":
            notice = _seed(conn, _id("ntc", 1))
            conn.execute(text("UPDATE dom_facts SET current_version=2 WHERE id=:id"), {"id": notice["id"]})
        else:
            _seed(conn, _id("pp", 1))
    result = _cli(OWNER_CLI, dsns["app"])
    assert (result.returncode, result.stdout) == (1, "")
    assert invalid in result.stderr


@pytest.mark.parametrize("args", [("--page-size", "33"), ("--as-of", "2026-01-01T00:00:00Z"),
                                 ("--replay-start", "{}")])
def test_capture_cli_rejects_bounds_and_operator_watermarks(args):
    result = _cli(OWNER_CLI, "postgresql://unused@127.0.0.1:1/unused", *args)
    assert (result.returncode, result.stdout) == (2, "")


def test_singleton_manifest_pages_feed_existing_export_and_stale_page_fails(plane, export):
    _, change_rights, reader = export
    database, dsns = plane[1], plane[4]
    with database.engine.begin() as conn:
        notice = _seed(conn, _id("ntc", 1), proof=True)
        blocked = _seed(conn, _id("ntc", 2))
        project = _seed(conn, _id("pp", 1), kind=PROJECT, proof=True)

    result = _cli(OWNER_CLI, dsns["app"], "--page-size", "1")
    assert (result.returncode, result.stderr) == (0, "")
    manifest = json.loads(result.stdout)
    _assert_manifest(manifest, [[notice], [blocked], [project]])

    def export_page(page):
        return _cli(PAGE_CLI, dsns["app"], payload=page, extra_env={
            "MIAOQIA_ACQUISITION_RIGHTS_READER_DSN": reader.database.engine.url.render_as_string(hide_password=False),
            "MIAOQIA_PUBLIC_SEARCH_DISCOVERY_SOURCES": json.dumps([SOURCE]),
        })

    for page, kind in zip(manifest["pages"], ("notice", None, "fact"), strict=True):
        response = export_page(page)
        assert (response.returncode, response.stderr) == (0, "")
        mutations = json.loads(response.stdout)["mutations"]
        if kind is None:
            assert mutations == []
        else:
            (mutation,) = mutations
            assert mutation["ref"] == {"kind": kind, "domain": "INF-PROC", "id": page[0]["id"], "version": "1"}
            assert mutation["withdrawn"] is False and mutation["revision"] == 1

    change_rights("revoke_public", 0)
    response = export_page(manifest["pages"][0])
    assert response.returncode == 0, response.stderr
    (withdrawn,) = json.loads(response.stdout)["mutations"]
    assert withdrawn["withdrawn"] is True and withdrawn["revision"] == 2

    with database.engine.begin() as conn:
        _seed(conn, project["id"], kind=PROJECT, version=2, proof=True)
    stale = export_page(manifest["pages"][-1])
    assert (stale.returncode, stale.stdout) == (1, "")
    _assert_manifest(json.loads(result.stdout), [[notice], [blocked], [project]])
