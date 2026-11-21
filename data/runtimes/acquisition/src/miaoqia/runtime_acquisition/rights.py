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
from miaoqia.acquisition.infrastructure.privileges import provision, provision_frozen_reader
from miaoqia.acquisition.infrastructure.rights import Registry
from miaoqia.kernel import reqctx
from miaoqia.kernel.internal_http import Client, Identity, Server, TLS, loads
from miaoqia.kernel.internal_http import delegations as parse_delegations


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
        delegations = parse_delegations(
            loads(os.environ["MIAOQIA_ACQUISITION_RIGHTS_DELEGATIONS"]),
            {"rights.resolve", "rights.confirm", "rights.status"},
        )
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
    apply = sub.add_parser(
        "apply",
        description=(
            "Apply a reviewed JSON command batch atomically. Public source actions "
            "revoke_public and restore_public require source_id and expected_epoch "
            "(the current epoch for that source, or 0 before its first revocation). "
            "restore_public also requires evidence_hash (sha256:<64 lowercase hex>)."
        ),
    )
    apply.add_argument("--file", required=True)
    apply.add_argument("--actor", required=True)
    apply.add_argument("--reason", required=True)
    grant = sub.add_parser("grant-roles")
    grant.add_argument("--reader", required=True)
    grant.add_argument("--operator", required=True)
    sub.add_parser("grant-frozen-reader").add_argument("--reader", required=True)
    a = parser.parse_args()
    try:
        if a.command == "grant-roles":
            provision(
                os.environ["MIAOQIA_ACQUISITION_DB_OWNER_DSN"], a.reader, a.operator
            )
        elif a.command == "grant-frozen-reader":
            provision_frozen_reader(os.environ["MIAOQIA_ACQUISITION_DB_OWNER_DSN"], a.reader)
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
            registry = Registry(os.environ["MIAOQIA_ACQUISITION_RIGHTS_OPERATOR_DSN"])
            try:
                with reqctx.use(reqctx.service("source-rights-operator")):
                    registry.command_batch(commands, a.actor, a.reason)
            finally:
                registry.close()
        print("source-rights operation complete")
    except Exception:
        print("source-rights operation failed", file=sys.stderr)
        raise SystemExit(1) from None
