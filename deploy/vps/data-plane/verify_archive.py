"""Verify current archive objects from the frozen public database references."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sys
import time
from datetime import datetime, timezone

import psycopg

from miaoqia.evidence.domain.snapshot import normalized_hash, raw_hash
from miaoqia.kernel.objectstore import open_object_store, versioned
from miaoqia.sdk.derivation import fingerprint


ACQUISITION = {
    # Includes originals of attachments, groups and pending identity by foreign key.
    "acq_original": """
        SELECT artifact_ref, content_hash, size_bytes::bigint, NULL::text, NULL::text
        FROM public.acq_raw_artifacts
    """,
    "acq_browser_original": """
        SELECT payload->>'artifact_ref', payload->>'content_hash', (payload->>'size_bytes')::bigint, NULL, NULL
        FROM public.acq_browser_receipts
    """,
    "acq_browser_projection": """
        SELECT payload #>> '{projection,artifact_ref}', payload #>> '{projection,content_hash}',
               (payload #>> '{projection,size_bytes}')::bigint, NULL, NULL
        FROM public.acq_browser_receipts
    """,
    "acq_discovery": """
        SELECT object_ref, response_hash,
               CASE WHEN browser_binding IS NOT NULL THEN coalesce((browser_binding->>'size_bytes')::bigint, -1) END,
               NULL, NULL
        FROM public.acq_discovery_pages
        WHERE object_ref IS NOT NULL AND expired_at IS NULL AND expires_at > %(cutoff)s
    """,
}

EVIDENCE = {
    "evd_original": """
        SELECT artifact_ref, raw_sha256, NULL::bigint, NULL::text, NULL::text
        FROM public.evd_snapshots
    """,
    "evd_normalized": """
        SELECT document_ref, normalized_hash, NULL::bigint, id, pipeline_version
        FROM public.evd_snapshots
    """,
    "evd_attachment": """
        SELECT part->>'artifact_ref', part->>'content_hash', (part->>'size_bytes')::bigint, NULL, NULL
        FROM public.evd_snapshot_attachments
        CROSS JOIN LATERAL jsonb_array_elements(receipts) AS parts(part)
        WHERE (part->>'status') IS DISTINCT FROM 'pending'
    """,
    "evd_group_original": """
        SELECT 'raw/' || substr(raw_sha256, 8), raw_sha256, size_bytes::bigint, NULL, NULL
        FROM public.evd_grouped_observations
    """,
    "evd_browser_original": """
        SELECT event->>'artifact_ref', event->>'content_hash', (event->>'size_bytes')::bigint, NULL, NULL
        FROM public.evd_browser_observations
    """,
    "evd_browser_projection": """
        SELECT event #>> '{projection,artifact_ref}', event #>> '{projection,content_hash}',
               (event #>> '{projection,size_bytes}')::bigint, NULL, NULL
        FROM public.evd_browser_observations
    """,
    "evd_pending_original": """
        SELECT envelope #>> '{data,artifact_ref}', envelope #>> '{data,content_hash}',
               (envelope #>> '{data,size_bytes}')::bigint, NULL, NULL
        FROM public.evd_pending_live_events
    """,
    "evd_pending_attachment": """
        SELECT part->>'artifact_ref', part->>'content_hash', (part->>'size_bytes')::bigint, NULL, NULL
        FROM public.evd_pending_live_events
        CROSS JOIN LATERAL jsonb_array_elements(coalesce(envelope #> '{data,attachments}', '[]'::jsonb)) AS parts(part)
        WHERE (part->>'status') IS DISTINCT FROM 'pending'
    """,
    "evd_pending_projection": """
        SELECT envelope #>> '{data,projection,artifact_ref}', envelope #>> '{data,projection,content_hash}',
               (envelope #>> '{data,projection,size_bytes}')::bigint, NULL, NULL
        FROM public.evd_pending_live_events
        WHERE envelope->>'type' = 'acquisition.browser_notice.archived.v1'
    """,
    **{
        f"evd_{table}": f"SELECT object_ref, content_hash, NULL::bigint, NULL::text, NULL::text FROM public.evd_{table}"
        for table in ("reference_data", "derivations", "enrichments")
    },
}

PREFIXES = {
    "acq_browser_projection": "projection",
    "acq_discovery": "discovery",
    "evd_normalized": "normalized",
    "evd_browser_projection": "projection",
    "evd_pending_projection": "projection",
    "evd_reference_data": "enrichment",
    "evd_derivations": "enrichment",
    "evd_enrichments": "enrichment",
}
SIZED = {
    "acq_original", "acq_browser_original", "acq_browser_projection", "evd_attachment", "evd_group_original",
    "evd_browser_original", "evd_browser_projection", "evd_pending_attachment", "evd_pending_projection",
}
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")


class VerificationError(Exception):
    def __init__(self, section: str, reason: str):
        self.section = section
        self.reason = reason


def verify_reference(store, row, section, max_bytes, deadline):
    key, expected, size, snapshot, pipeline = row
    prefix = PREFIXES.get(section, "raw")
    if (not key or not key.startswith(prefix + "/") or not expected or not DIGEST.fullmatch(expected)
            or (size is None and section in SIZED) or (size is not None and size < 0)):
        raise ValueError("invalid reference")
    if prefix in {"raw", "projection"} and key != f"{prefix}/{expected[7:]}":
        raise ValueError("invalid digest in object key")
    if prefix == "discovery" and key.rsplit("/", 1)[-1] != expected[7:]:
        raise ValueError("invalid discovery digest in object key")
    if prefix == "enrichment" and key.rsplit("/", 1)[-1] != expected[7:] + ".json":
        raise ValueError("invalid enrichment digest in object key")
    data = store.get(key, max_bytes=max_bytes if size is None else min(size, max_bytes), deadline=deadline)
    if size is not None and len(data) != size:
        raise ValueError("invalid size")
    byte_hash = raw_hash(data)
    actual = byte_hash
    if prefix == "normalized":
        document = json.loads(data)
        if (not isinstance(document, dict) or document.get("snapshot_ref") != snapshot
                or document.get("pipeline_version") != pipeline
                or key != f"normalized/{versioned(pipeline)}/{snapshot}.json"):
            raise ValueError("invalid snapshot binding")
        actual = normalized_hash(document)
    elif prefix == "enrichment":
        actual = fingerprint(json.loads(data))
    if actual != expected:
        raise ValueError("invalid object hash")
    return len(data), byte_hash


def scan(conn, store, section, query, cutoff, max_bytes, deadline):
    checksum = hashlib.sha256()
    count = total = 0
    try:
        with conn.cursor(name=section) as cursor:
            cursor.itersize = 512
            # A stable per-section set permits exact source/restore comparisons across cluster locales.
            cursor.execute(f"""
                SELECT DISTINCT key COLLATE "C", digest COLLATE "C", size, snapshot COLLATE "C", pipeline COLLATE "C"
                FROM ({query}) AS refs(key, digest, size, snapshot, pipeline)
                ORDER BY 1, 2, 3, 4, 5
            """, {"cutoff": cutoff} if section == "acq_discovery" else None)
            for row in cursor:
                try:
                    length, byte_hash = verify_reference(store, row, section, max_bytes, deadline)
                except FileNotFoundError:
                    raise VerificationError(section, "object_missing") from None
                except TimeoutError:
                    raise VerificationError(section, "deadline_exceeded") from None
                except (ValueError, OverflowError, RecursionError):
                    raise VerificationError(section, "object_or_reference_invalid") from None
                except Exception:
                    raise VerificationError(section, "object_read_failed") from None
                checksum.update(json.dumps([*row, length, byte_hash], separators=(",", ":")).encode("ascii") + b"\n")
                count += 1
                total += length
    except psycopg.Error:
        raise VerificationError(section, "reference_scan_failed") from None
    return {"references": count, "bytes": total, "sha256": checksum.hexdigest()}


def verify(phase, cutoff, store, max_bytes, timeout):
    deadline = time.monotonic() + timeout
    sections = {}
    discovery_policy = {}
    for database, queries in (("acq", ACQUISITION), ("evd", EVIDENCE)):
        try:
            with psycopg.connect(service=f"mq-{phase}-{database}", connect_timeout=5,
                                 application_name="mq-verify-archive") as conn:
                conn.read_only = True
                conn.isolation_level = psycopg.IsolationLevel.REPEATABLE_READ
                if conn.execute("SELECT current_database()").fetchone() != (database,):
                    raise VerificationError(database, "wrong_database")
                for section, query in queries.items():
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        raise VerificationError(section, "deadline_exceeded")
                    conn.execute("SELECT set_config('statement_timeout', %s, true)",
                                 (str(max(1, int(1000 * min(remaining, 60)))),))
                    sections[section] = scan(conn, store, section, query, cutoff, max_bytes, deadline)
                    if time.monotonic() >= deadline:
                        raise VerificationError(section, "deadline_exceeded")
                if database == "acq":
                    without_object, expired = conn.execute("""
                        SELECT count(*) FILTER (WHERE object_ref IS NULL),
                               count(*) FILTER (WHERE object_ref IS NOT NULL AND
                                                (expired_at IS NOT NULL OR expires_at <= %s))
                        FROM public.acq_discovery_pages
                    """, (cutoff,)).fetchone()
                    discovery_policy = {"without_object_ref": without_object, "outside_retention": expired}
                    if time.monotonic() >= deadline:
                        raise VerificationError(database, "deadline_exceeded")
        except psycopg.Error:
            raise VerificationError(database, "database_unavailable") from None
    return {"schema_version": 1, "status": "verified", "cutoff": cutoff.isoformat(),
            "sections": sections, "discovery_policy": discovery_policy}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--phase", required=True, choices=("backup", "restore"))
    parser.add_argument("--cutoff", required=True, help="Frozen recovery point as ISO-8601 with timezone")
    parser.add_argument("--max-object-mib", type=int, default=64, choices=range(1, 257), metavar="1..256")
    parser.add_argument("--timeout-seconds", type=int, default=3600, metavar="SECONDS")
    options = parser.parse_args()
    try:
        cutoff = datetime.fromisoformat(options.cutoff)
        if cutoff.utcoffset() is None or not 1 <= options.timeout_seconds <= 86400:
            raise ValueError
        cutoff = cutoff.astimezone(timezone.utc)
    except (ValueError, OverflowError):
        parser.error("a timezone-aware cutoff and 1..86400 timeout seconds are required")
    try:
        if not os.environ.get("PGSERVICEFILE"):
            raise VerificationError("configuration", "service_file_required")
        store = open_object_store(os.environ["MQ_RECOVERY_OBJECT_STORE"], read_only=True)
        receipt = verify(options.phase, cutoff, store, options.max_object_mib * 1024**2, options.timeout_seconds)
    except VerificationError as exc:
        print(json.dumps({"status": "failed", "section": exc.section, "reason": exc.reason}), file=sys.stderr)
        return 1
    except Exception:
        print('{"status":"failed","reason":"archive_verification_unavailable"}', file=sys.stderr)
        return 1
    print(json.dumps(receipt, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
