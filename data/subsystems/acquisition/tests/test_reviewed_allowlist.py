"""The owner-reviewed allowlist ships finite and stays immutable for every account, the owner included."""
import psycopg
import pytest
from miaoqia.acquisition import MIGRATIONS
from miaoqia.kernel import db


def test_reviewed_allowlist_is_finite_and_immutable(pg_factory):
    dsns = pg_factory()
    db.migrate(dsns["owner"], MIGRATIONS)
    with psycopg.connect(dsns["owner"]) as conn:
        assert conn.execute("SELECT source_id,tags FROM acq_rights.reviewed ORDER BY source_id,tags").fetchall() == [
            ("official-qzygjy", ["corpus:official-building-award-20261026", "public-official-review"]),
            ("official-qzygjy", ["corpus:official-building-tender-20261026", "public-official-review"]),
            ("reference-proc-building-topic-v0-1", ["review:building-topic-pair"]),
        ]
        guard = conn.execute("SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgname='reviewed_immutable'").fetchone()[0]
        assert "BEFORE INSERT OR DELETE OR UPDATE OR TRUNCATE" in guard
        for table in ("versions", "aliases", "current", "audit"):
            assert conn.execute("SELECT count(*) FROM acq_rights."+table).fetchone()[0] == 0
    for statement in ("INSERT INTO acq_rights.reviewed VALUES ('official-forged','[]')",
        "UPDATE acq_rights.reviewed SET source_id='official-forged'", "DELETE FROM acq_rights.reviewed", "TRUNCATE acq_rights.reviewed CASCADE"):
        with pytest.raises(psycopg.Error), psycopg.connect(dsns["owner"]) as conn:
            conn.execute(statement)
