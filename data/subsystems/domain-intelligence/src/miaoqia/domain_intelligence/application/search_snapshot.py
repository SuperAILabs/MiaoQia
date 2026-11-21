"""Freeze the owner handles used to prepare a public search generation."""

from datetime import timezone
import time

from sqlalchemy import text

from miaoqia.sdk.derivation import fingerprint

from ..api.search_projection import InvalidProjectionEvent
from .search_projection import DOMAIN, KINDS, MAX_BATCH, _kind

MAX_HANDLES = 100_000
MAX_SECONDS = 60
DEFAULT_SECONDS = 30

_HANDLES = text("""
    SELECT f.id, f.object_type, v.version
    FROM dom_facts f
    LEFT JOIN dom_fact_versions v ON (v.fact_id, v.version) = (f.id, f.current_version)
    WHERE f.info_domain = :domain AND f.object_type = ANY(:types)
    ORDER BY f.id COLLATE "C"
    LIMIT :limit
""")


class SnapshotBudgetExceeded(RuntimeError):
    """The capture ran out of time or exceeded its whole-snapshot handle limit."""


class InvalidSnapshotState(RuntimeError):
    """The owner database cannot supply an authoritative, valid snapshot."""


def _remaining_ms(deadline):
    remaining = int((deadline - time.monotonic()) * 1000)
    if remaining <= 0:
        raise SnapshotBudgetExceeded("owner snapshot deadline exceeded")
    return remaining


def _statement_budget(conn, deadline):
    conn.exec_driver_sql(f"SET LOCAL statement_timeout = {_remaining_ms(deadline)}")


def capture(database, *, page_size=MAX_BATCH, max_handles=MAX_HANDLES, timeout_seconds=DEFAULT_SECONDS):
    """Return a complete manifest from one read-only PostgreSQL MVCC snapshot.

    The first snapshot-taking SELECT supplies the database statement start as
    as_of, a conservative wall-clock label for the captured view. Keep the
    manifest for reuse. Page export still checks current versions and rights.
    """
    if type(page_size) is not int or not 1 <= page_size <= MAX_BATCH:
        raise ValueError(f"page_size must be in 1..{MAX_BATCH}")
    if type(max_handles) is not int or not 1 <= max_handles <= MAX_HANDLES:
        raise ValueError(f"max_handles must be in 1..{MAX_HANDLES}")
    if type(timeout_seconds) is not int or not 1 <= timeout_seconds <= MAX_SECONDS:
        raise ValueError(f"timeout_seconds must be in 1..{MAX_SECONDS}")

    deadline = time.monotonic() + timeout_seconds
    count, pages = 0, []
    # Set isolation before the first SELECT, which pins the view reused by
    # the server cursor. This local owner operation only reads fixed handles.
    with database.engine.begin() as conn:
        conn.exec_driver_sql("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY")
        conn.exec_driver_sql(f"SET LOCAL idle_in_transaction_session_timeout = {_remaining_ms(deadline)}")
        _statement_budget(conn, deadline)
        row = conn.execute(text("""
            SELECT statement_timestamp() AS as_of, pg_current_snapshot(), pg_is_in_recovery() AS standby
        """)).one()
        if row.standby:
            raise InvalidSnapshotState("owner snapshot requires the primary PostgreSQL database")
        as_of = row.as_of.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")

        _statement_budget(conn, deadline)
        stream = _HANDLES.execution_options(yield_per=page_size)
        with conn.execute(stream, {"domain": DOMAIN, "types": list(KINDS), "limit": max_handles + 1}) as result:
            while True:
                _statement_budget(conn, deadline)
                batch = result.fetchmany(page_size)
                if not batch:
                    break
                count += len(batch)
                if count > max_handles:
                    raise SnapshotBudgetExceeded(f"owner snapshot exceeds {max_handles} handles")
                page = []
                for row in batch:
                    try:
                        _kind(row.object_type, row.id)
                    except InvalidProjectionEvent as exc:
                        raise InvalidSnapshotState("owner snapshot contains a fact type and identity mismatch") from exc
                    if row.version is None:
                        raise InvalidSnapshotState("owner snapshot contains a missing current version")
                    page.append({"id": row.id, "version": row.version})
                pages.append(page)

    manifest = {"as_of": as_of, "handle_count": count, "pages": pages}
    snapshot_id = fingerprint(manifest)
    _remaining_ms(deadline)
    return {"snapshot_id": snapshot_id, **manifest}
