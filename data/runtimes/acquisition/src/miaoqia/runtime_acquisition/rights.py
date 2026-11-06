"""Local operator CLI and explicitly enabled acq-admin read listener. Safe startup/CLI failures."""

from __future__ import annotations

import argparse
import os
import re
import signal
import sys
import threading
from pathlib import Path

from miaoqia.acquisition.application.http import routes
from miaoqia.acquisition.application.rights import Decisions
from miaoqia.acquisition.infrastructure.privileges import provision
from miaoqia.acquisition.infrastructure.rights import Registry, validate_command
from miaoqia.kernel import reqctx
from miaoqia.kernel.internal_http import Client, Identity, Server, TLS, loads


def tls():
    return TLS(
        *(
            os.environ.get("MIAOQIA_INTERNAL_TLS_" + k, "")
            for k in ("CERT", "KEY", "CA")
        )
    )


def serve():
    # Dedicated rights reader connection; never a generic acquisition writer/owner DSN.
    registry = Registry(os.environ["MIAOQIA_ACQUISITION_RIGHTS_READER_DSN"])
    try:
        with reqctx.use(reqctx.service("acquisition-rights-startup")):
            registry.ensure_reader()
        identity = Identity(
            Client(
                os.environ["MIAOQIA_IDENTITY_URL"],
                tls(),
                os.environ["MIAOQIA_IDENTITY_PEER"],
            )
        )
        value = loads(os.environ["MIAOQIA_ACQUISITION_RIGHTS_DELEGATIONS"])
        if not isinstance(value, list) or not value:
            raise ValueError("explicit delegation list required")
        delegations = []
        for item in value:
            if (
                not isinstance(item, dict)
                or set(item) != {"peer", "operation", "purpose"}
                or item["operation"]
                not in {"rights.resolve", "rights.confirm", "rights.status"}
            ):
                raise ValueError("invalid delegation")
            delegations.append((item["peer"], item["operation"], item["purpose"]))
        host, port = os.environ["MIAOQIA_ACQUISITION_RIGHTS_LISTEN"].rsplit(":", 1)
        if not host or not 1 <= int(port) <= 65535:
            raise ValueError("explicit address required")
        server = Server(
            (host, int(port)), tls(), delegations, identity, routes(Decisions(registry))
        )
        stop = threading.Event()
        signal.signal(signal.SIGTERM, lambda *_: stop.set())
        signal.signal(signal.SIGINT, lambda *_: stop.set())
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        try:
            stop.wait()
        finally:
            server.shutdown()
            server.server_close()
            worker.join(5)
    finally:
        registry.close()


def main():
    parser = argparse.ArgumentParser(prog="mq-source-rights")
    sub = parser.add_subparsers(dest="command", required=True)
    apply = sub.add_parser("apply")
    apply.add_argument("--file", required=True)
    apply.add_argument("--actor", required=True)
    apply.add_argument("--reason", required=True)
    grant = sub.add_parser("grant-roles")
    grant.add_argument("--reader", required=True)
    grant.add_argument("--operator", required=True)
    a = parser.parse_args()
    try:
        if a.command == "grant-roles":
            provision(
                os.environ["MIAOQIA_ACQUISITION_DB_OWNER_DSN"], a.reader, a.operator
            )
        else:
            if (
                not re.fullmatch(r"[a-zA-Z0-9_-]{1,96}", a.actor)
                or not 1 <= len(a.reason) <= 256
                or any(ord(c) < 32 for c in a.reason)
            ):
                raise ValueError("invalid audit metadata")
            with Path(a.file).open("rb") as stream:
                commands = loads(stream.read(65537))
            if not isinstance(commands, list) or not 1 <= len(commands) <= 16:
                raise ValueError("bounded command list required")
            for cmd in commands:
                validate_command(cmd)
            registry = Registry(os.environ["MIAOQIA_ACQUISITION_RIGHTS_OPERATOR_DSN"])
            try:
                # One transaction for the whole explicit batch; failures cannot partially apply it.
                import json
                from sqlalchemy import text

                with reqctx.use(
                    reqctx.service("source-rights-operator")
                ), registry.database.tx() as c:
                    c.execute(text("SET LOCAL statement_timeout = '3000ms'"))
                    for cmd in commands:
                        c.execute(
                            text("SELECT acq_rights.command(CAST(:v AS jsonb),:a,:r)"),
                            {"v": json.dumps(cmd), "a": a.actor, "r": a.reason},
                        )
            finally:
                registry.close()
        print("source-rights operation complete; no source activated")
    except Exception:
        print("source-rights operation failed", file=sys.stderr)
        raise SystemExit(1) from None
