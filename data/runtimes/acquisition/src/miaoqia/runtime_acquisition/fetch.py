"""Operator one-shot collection of one declared source over a publication window.

  mq-acquisition-fetch --source SOURCE_ID [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--max-pages N]
                       [--refetch] [--unblock] [--packs packs]

Configuration: MIAOQIA_ACQUISITION_DB_DSN (migrated acq database), MIAOQIA_ACQUISITION_OBJECT_STORE_DIR.
The window defaults to today. Prints the run report as JSON; exits 2 when the source is blocked.
"""
from __future__ import annotations

import argparse
import json
import os
import sys
from dataclasses import asdict
from datetime import date

from miaoqia.acquisition.api.handlers import Window
from miaoqia.acquisition.application import sources
from miaoqia.acquisition.application.fetch import Fetcher, SourceStopped
from miaoqia.acquisition.infrastructure.transport import UrllibTransport
from miaoqia.kernel import db
from miaoqia.kernel.objectstore import FilesystemObjectStore


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="mq-acquisition-fetch")
    ap.add_argument("--source", required=True)
    ap.add_argument("--since", type=date.fromisoformat)
    ap.add_argument("--until", type=date.fromisoformat)
    ap.add_argument("--max-pages", type=int, default=1)
    ap.add_argument("--refetch", action="store_true", help="fetch details already archived again")
    ap.add_argument("--unblock", action="store_true", help="resume a source an operator has reviewed")
    ap.add_argument("--packs", default=os.environ.get("MIAOQIA_PACKS_DIR", "packs"))
    a = ap.parse_args(argv)
    dsn, objects = os.environ.get("MIAOQIA_ACQUISITION_DB_DSN"), os.environ.get("MIAOQIA_ACQUISITION_OBJECT_STORE_DIR")
    if not dsn or not objects:
        ap.error("MIAOQIA_ACQUISITION_DB_DSN and MIAOQIA_ACQUISITION_OBJECT_STORE_DIR are required")
    declared = sources.load(a.packs)
    if a.source not in declared:
        ap.error(f"source {a.source!r} has no declaration; declared: {', '.join(sorted(declared)) or 'none'}")
    until = a.until or date.today()
    window = Window(a.since or until, until)
    database = db.Database(dsn, pool_size=1)
    try:
        fetcher = Fetcher(database, FilesystemObjectStore(objects), UrllibTransport())
        try:
            report = fetcher.run(declared[a.source], window, max_pages=a.max_pages, refetch=a.refetch, unblock=a.unblock)
        except SourceStopped:
            print(f"source {a.source} is blocked; review it and rerun with --unblock", file=sys.stderr)
            return 2
    finally:
        database.close()
    print(json.dumps(asdict(report), ensure_ascii=False, indent=2))
    return 2 if report.blocked else 0


if __name__ == "__main__":
    raise SystemExit(main())
