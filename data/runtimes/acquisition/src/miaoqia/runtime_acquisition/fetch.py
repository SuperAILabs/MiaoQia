"""Operator collection, catalog reporting, and public source engineering validation.

  mq-acquisition-fetch --source SOURCE_ID [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--max-pages N]
                       [--refetch] [--unblock] [--packs packs]
  mq-acquisition-fetch --list-sources [--require-runtime] [--since YYYY-MM-DD --until YYYY-MM-DD] [--packs packs]
  mq-acquisition-fetch --validate-source SOURCE_ID --channel CHANNEL
                       [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--max-pages N] [--max-details N]

Collection accepts active HTTP sources and requires MIAOQIA_ACQUISITION_DB_DSN and
MIAOQIA_ACQUISITION_OBJECT_STORE_DIR.
Validation uses anonymous HTTP and reports observations as JSON without collection storage.
"""
from __future__ import annotations

import argparse
import json
import os
import sys
from dataclasses import asdict
from datetime import date

from miaoqia.acquisition.api.handlers import Window
from miaoqia.acquisition.application import coverage, source_validation, sources
from miaoqia.acquisition.application.fetch import Fetcher, SourceStopped, SystemClock, china_today
from miaoqia.acquisition.infrastructure.archive_spool import ArchiveSpool
from miaoqia.acquisition.infrastructure.transport import UrllibTransport
from miaoqia.kernel import db
from miaoqia.kernel.objectstore import open_object_store

_VALIDATION_EXIT = {"validated": 0, "failed": 1, "blocked": 2, "limited": 3}


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(
        prog="mq-acquisition-fetch",
        epilog="Validation runs one channel with anonymous HTTP. Use an authorized environment and coordinate "
        "with collection operators before sending requests to shared sites. Validation exits 0 for validated samples, "
        "1 for technical failures, 2 for access blocks or invalid arguments, and 3 for access or sample limits. "
        "Collection exits 3 when deferred.",
    )
    mode = ap.add_mutually_exclusive_group(required=True)
    mode.add_argument("--source", help="collect one active HTTP source")
    mode.add_argument("--list-sources", action="store_true", help="report source admission and current acquisition coverage")
    mode.add_argument("--validate-source", metavar="SOURCE_ID", help="validate one declared public HTTP source")
    ap.add_argument("--require-runtime", action="store_true", help="require the dedicated coverage reader for --list-sources")
    ap.add_argument("--channel", help="declared channel for --validate-source")
    ap.add_argument("--since", type=date.fromisoformat)
    ap.add_argument("--until", type=date.fromisoformat)
    ap.add_argument("--max-pages", type=int, help="page limit (collection default 1, validation default 2)")
    ap.add_argument("--max-details", type=int, help="body or group response sample limit for --validate-source (default 1)")
    ap.add_argument("--refetch", action="store_true", help="fetch details already archived again")
    ap.add_argument("--unblock", action="store_true", help="resume a source an operator has reviewed")
    ap.add_argument("--packs", default=os.environ.get("MIAOQIA_PACKS_DIR", "packs"))
    a = ap.parse_args(argv)
    if a.require_runtime and not a.list_sources:
        ap.error("--require-runtime requires --list-sources")
    if a.validate_source is not None:
        return _validate(ap, a)
    if a.channel is not None or a.max_details is not None:
        ap.error("--channel and --max-details require --validate-source")
    dsn, objects = os.environ.get("MIAOQIA_ACQUISITION_DB_DSN"), os.environ.get("MIAOQIA_ACQUISITION_OBJECT_STORE_DIR")
    if a.list_sources:
        if a.refetch or a.unblock or a.max_pages not in (None, 1):
            ap.error("fetch options require --source")
        window = None
        if a.since or a.until:
            if not a.since or not a.until or not 0 <= (a.until - a.since).days < coverage.HISTORY_DAYS:
                ap.error(f"coverage requires --since and --until within {coverage.HISTORY_DAYS} inclusive days")
            window = Window(a.since, a.until)
        if (a.require_runtime or window is not None) and not dsn:
            ap.error("MIAOQIA_ACQUISITION_DB_DSN is required for the runtime coverage report")
        database = None
        try:
            if dsn:
                database = db.Database(dsn, pool_size=1)
            report = coverage.report(sources.catalog(a.packs), database, window=window, operator=a.require_runtime)
            result = json.dumps(report, ensure_ascii=False, indent=2)
        except Exception:
            if not a.require_runtime:
                raise
            print("runtime coverage report unavailable", file=sys.stderr)
            return 1
        finally:
            if database is not None:
                database.close()
        print(result)
        return 0
    declared = sources.load(a.packs, transport="http")
    if a.source not in declared:
        ap.error(f"source {a.source!r} has no active HTTP declaration; use --list-sources")
    if not dsn or not objects:
        ap.error("MIAOQIA_ACQUISITION_DB_DSN and MIAOQIA_ACQUISITION_OBJECT_STORE_DIR are required")
    until = a.until or china_today()
    window = Window(a.since or until, until)
    database = db.Database(dsn, pool_size=1)
    try:
        spool = ArchiveSpool.configured(lambda key: os.environ.get("MIAOQIA_ACQUISITION_" + key))
        fetcher = Fetcher(database, open_object_store(objects), UrllibTransport(), spool=spool)
        try:
            report = fetcher.run(declared[a.source], window, max_pages=1 if a.max_pages is None else a.max_pages,
                                 refetch=a.refetch, unblock=a.unblock)
        except SourceStopped:
            print(f"source {a.source} is blocked; review it and rerun with --unblock", file=sys.stderr)
            return 2
    finally:
        database.close()
    print(json.dumps(asdict(report), ensure_ascii=False, indent=2))
    if report.blocked:
        return 2
    return 3 if report.deferred else 0


def _validate(ap: argparse.ArgumentParser, args: argparse.Namespace) -> int:
    if not args.channel:
        ap.error("--validate-source requires --channel; use --list-sources")
    if args.refetch or args.unblock:
        ap.error("--refetch and --unblock require --source")
    max_pages = 2 if args.max_pages is None else args.max_pages
    max_details = 1 if args.max_details is None else args.max_details
    until = args.until or args.since or china_today()
    try:
        entry, source = sources.for_validation(args.packs, args.validate_source)
        report = source_validation.validate(
            entry, source, args.channel, Window(args.since or until, until), max_pages=max_pages,
            max_details=max_details, transport_factory=UrllibTransport, clock=SystemClock(),
        )
    except ValueError as exc:
        ap.error(str(exc))
    print(json.dumps(asdict(report), ensure_ascii=False, indent=2))
    return _VALIDATION_EXIT[report.outcome]


if __name__ == "__main__":
    raise SystemExit(main())
