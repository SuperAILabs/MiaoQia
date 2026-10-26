"""Finite owner append preserves the original allowlist and restores its immutable trigger."""
import importlib
import psycopg
import pytest
from alembic import command
from alembic.config import Config
from miaoqia.acquisition import MIGRATIONS
from miaoqia.kernel import db


def test_finite_upgrade_and_failed_insert_restore_all_guards(pg_factory, monkeypatch):
    dsns = pg_factory()
    db.migrate(dsns["owner"])
    cfg = Config()
    cfg.set_main_option("script_location", MIGRATIONS)
    cfg.set_main_option("sqlalchemy.url", db._url(dsns["owner"]))
    command.upgrade(cfg, "0001")
    with psycopg.connect(dsns["owner"]) as conn:
        before = conn.execute("SELECT source_id,tags FROM acq_rights.reviewed ORDER BY tags").fetchall()
        guard = conn.execute("SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgname='reviewed_immutable'").fetchone()[0]
    migration = importlib.import_module("miaoqia.acquisition.migrations.versions.0002_reviewed_building_reference")
    with pytest.raises(psycopg.Error), psycopg.connect(dsns["owner"]) as conn:
        def fail(statement):
            conn.execute(statement)
            conn.execute("SELECT 1/0")
        with monkeypatch.context() as patch:
            patch.setattr(migration.op, "execute", fail)
            migration.upgrade()
    with psycopg.connect(dsns["owner"]) as conn:
        assert conn.execute("SELECT source_id,tags FROM acq_rights.reviewed ORDER BY tags").fetchall() == before
        assert conn.execute("SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgname='reviewed_immutable'").fetchone()[0] == guard
    command.upgrade(cfg, "head")
    with psycopg.connect(dsns["owner"]) as conn:
        after = conn.execute("SELECT source_id,tags FROM acq_rights.reviewed ORDER BY tags").fetchall()
        assert len(after) == 3 and all(r in after for r in before)
        assert ("reference-proc-building-topic-v0-1", ["review:building-topic-pair"]) in after
        assert conn.execute("SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgname='reviewed_immutable'").fetchone()[0] == guard
        for table in ("versions", "aliases", "current", "audit"):
            assert conn.execute("SELECT count(*) FROM acq_rights."+table).fetchone()[0] == 0
    for statement in ("INSERT INTO acq_rights.reviewed VALUES ('official-forged','[]')",
        "UPDATE acq_rights.reviewed SET source_id='official-forged'", "DELETE FROM acq_rights.reviewed", "TRUNCATE acq_rights.reviewed CASCADE"):
        with pytest.raises(psycopg.Error), psycopg.connect(dsns["owner"]) as conn:
            conn.execute(statement)
