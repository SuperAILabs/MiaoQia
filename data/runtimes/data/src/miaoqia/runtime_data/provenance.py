"""Offline owner/binder operations; never enables a listener or grants source permission."""
import argparse
import dataclasses
import json
import os
from pathlib import Path
import sys

import psycopg
from sqlalchemy.exc import SQLAlchemyError
from miaoqia import domain_intelligence, evidence
from miaoqia.evidence.api.audit import audit, batch
from miaoqia.evidence.infrastructure.privileges import provision
from miaoqia.kernel import db, reqctx
from .enrich import assemble


def _dsn(name):
    value = os.environ.get(name)
    if not value:
        raise ValueError("missing " + name)
    return value


def main(argv=None):
    parser = argparse.ArgumentParser(prog="mq-provenance")
    commands = parser.add_subparsers(dest="command", required=True)
    setup = commands.add_parser("provision")
    setup.add_argument("--reader", required=True)
    setup.add_argument("--binder", required=True)
    for name in ("bind", "backfill-evidence", "backfill-domain"):
        command = commands.add_parser(name)
        command.add_argument("--batch", type=Path, required=True)
        command.add_argument("--actor", required=True)
        command.add_argument("--reason", required=True)
        if name != "bind":
            command.add_argument("--packs", default=os.environ.get("MIAOQIA_PACKS_DIR", "packs"))
            command.add_argument("--enable-test-recipe", action="store_true")
            command.add_argument("--enable-building-topic", action="store_true")
    args = parser.parse_args(argv)
    try:
        if args.command == "provision":
            provision(_dsn("MIAOQIA_EVIDENCE_OWNER_DSN"), args.reader, args.binder)
        else:
            audit(args.actor, args.reason)
            if args.batch.stat().st_size > 65536:
                raise ValueError("batch exceeds 64 KiB")
            values = json.loads(args.batch.read_text())
            batch(values)
            if args.command == "bind":
                if any(not isinstance(v, dict) or set(v) != {"name", "version"}
                       or any(not isinstance(x, str) for x in v.values()) for v in values):
                    raise ValueError("binding accepts only reviewed name/version handles")
                with psycopg.connect(_dsn("MIAOQIA_EVIDENCE_BINDER_DSN")) as conn:
                    for value in sorted(values, key=lambda v: (v["name"], v["version"])):
                        conn.execute("SELECT evd_provenance.bind_reference(%s,%s,%s,%s)",
                                     (value["name"], value["version"], args.actor, args.reason))
            else:
                evd = db.Database(_dsn("MIAOQIA_EVIDENCE_OWNER_DSN"))
                dom = db.Database(_dsn("MIAOQIA_DOMAIN_INTELLIGENCE_OWNER_DSN"))
                reader = evidence.extractions(_dsn("MIAOQIA_EVIDENCE_OWNER_DSN"))
                try:
                    with reqctx.use(dataclasses.replace(reqctx.service("notice-enrich-test"), purpose="enrichment_test")):
                        service, _ = assemble(dom, evd, _dsn("MIAOQIA_EVIDENCE_OBJECT_STORE_DIR"),
                            packs=args.packs, enable_test_recipe=args.enable_test_recipe,
                            enable_building_topic=args.enable_building_topic)
                        if args.command == "backfill-evidence":
                            service.backfill(values, actor=args.actor, reason=args.reason)
                        else:
                            domain_intelligence.backfill_requirements(dom, reader, service, values,
                                actor=args.actor, reason=args.reason)
                finally:
                    reader.close()
                    dom.close()
                    evd.close()
        print("provenance operation complete; source permission unchanged")
        return 0
    except (ValueError, PermissionError, OSError, psycopg.Error, SQLAlchemyError):
        print("provenance operation failed", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
