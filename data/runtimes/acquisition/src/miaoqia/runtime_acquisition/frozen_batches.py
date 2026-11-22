"""Operator commands for curated, authorized batches of archived notice observations.

    python -m miaoqia.runtime_acquisition.frozen_batches freeze --selection selection.json > batch.json
    python -m miaoqia.runtime_acquisition.frozen_batches read --manifest batch.json --index 0
    python -m miaoqia.runtime_acquisition.frozen_batches verify --manifest batch.json --index 0
    python -m miaoqia.runtime_acquisition.frozen_batches restore --manifest batch.json --index 0
    python -m miaoqia.runtime_acquisition.frozen_batches freeze-records --selection selection.json > records.json
    python -m miaoqia.runtime_acquisition.frozen_batches read-record --manifest records.json --index 0
    python -m miaoqia.runtime_acquisition.frozen_batches restore-record-original --manifest records.json --index 0
    python -m miaoqia.runtime_acquisition.frozen_batches freeze-browser --selection selection.json > browser.json
    python -m miaoqia.runtime_acquisition.frozen_batches read-projection --manifest browser.json --index 0
    python -m miaoqia.runtime_acquisition.frozen_batches verify-browser --manifest browser.json --index 0
    python -m miaoqia.runtime_acquisition.frozen_batches read-browser-original --manifest browser.json --index 0
    python -m miaoqia.runtime_acquisition.frozen_batches restore-browser-original --manifest browser.json --index 0
    python -m miaoqia.runtime_acquisition.frozen_batches restore-browser-projection --manifest browser.json --index 0

Selection is an array of source_id, notice_key and observation_version records,
with an optional part_key for a particular attachment.
The index is zero-based. Read writes verified original bytes to stdout.
Read-record and read-projection emit typed JSON. Read-browser-original emits exact original bytes.
Set MIAOQIA_ACQUISITION_FROZEN_AUTHORIZER to a trusted deployment module:callable
implementing AuthorizeOriginal. The callback must enforce current permissions for
the operator or service and operation. Set MIAOQIA_ACQUISITION_FROZEN_READER_DSN
to a catalog reader for the observation and artifact tables, and use
MIAOQIA_ACQUISITION_OBJECT_STORE_DIR for primary storage. Restore also needs
MIAOQIA_ACQUISITION_FROZEN_BACKUP configured as a separate backup location.
For a separate S3 connection, MIAOQIA_ACQUISITION_FROZEN_BACKUP_S3_PROFILE selects
a named AWS profile with its own endpoint_url, region and read credentials.
For grouped record commands, MIAOQIA_ACQUISITION_FROZEN_RECORD_ACCESS names a trusted
module:callable returning (caller Context with purpose, AuthorizeRecord callback).
MIAOQIA_PACKS_DIR identifies the trusted installed packs used for member selection.
MIAOQIA_ACQUISITION_FROZEN_BROWSER_ACCESS names a trusted module:callable
returning (caller Context with purpose, AuthorizeBrowser callback) for operator browser commands.
Each browser command has its own operation permission. Browser manifests pin the
pair of objects and the binding digest. The browser read policy permits
read-projection and verify-browser for its pinned items.
MIAOQIA_ACQUISITION_FROZEN_READ_POLICY enables the built-in read-only deployment
authorizer for an operator-approved batch and a dedicated testing process.
"""
from __future__ import annotations

import argparse
import json
import os
import sys
from contextlib import ExitStack
from dataclasses import replace
from pathlib import Path

from miaoqia.acquisition.api import identifier, instant
from miaoqia.acquisition.api.frozen_batches import (
    MAX_BATCH_ITEMS, FrozenBatch, FrozenBatchDenied, FrozenBrowserBatch, FrozenObjectError, FrozenRecords,
    ObservationRef,
)
from miaoqia.acquisition.application import sources
from miaoqia.acquisition.application.frozen_batches import FrozenBatches
from miaoqia.acquisition.infrastructure.rights import Registry
from miaoqia.kernel import db, reqctx
from miaoqia.kernel.jsoncodec import MAX_BODY, closed, loads
from miaoqia.kernel.objectstore import open_object_store

from .bindings import operator_callable as _callback

_BROWSER_RESTORES = ("restore-browser-original", "restore-browser-projection")
_BROWSER_POLICY_READS = ("read-projection", "verify-browser")
_BROWSER_READS = (*_BROWSER_POLICY_READS, "read-browser-original")
_BROWSER_ACCESS = "MIAOQIA_ACQUISITION_FROZEN_BROWSER_ACCESS"
_RECORD_ACCESS = "MIAOQIA_ACQUISITION_FROZEN_RECORD_ACCESS"
_ORIGINAL_ACCESS = "MIAOQIA_ACQUISITION_FROZEN_AUTHORIZER"


def _load(path: Path):
    with path.open("rb") as stream:
        return loads(stream.read(MAX_BODY + 1))


def _error(code: str, status: int) -> int:
    print(json.dumps({"error": code}), file=sys.stderr)
    return status


def _record_adapters(packs_dir: Path) -> dict:
    adapters = {}
    for entry in sources.catalog(packs_dir).values():
        declared = entry.declaration
        if declared is None or entry.access_mode != "public" or entry.admission not in {"staged", "active"}:
            continue
        select = getattr(declared.handler, "to_selected_html", None)
        if callable(select) and callable(getattr(declared.handler, "expand_group", None)):
            adapters[declared.source_id] = select
    return adapters


def _read_policy(path: Path, batch_type):
    policy = closed(_load(path), {"schema_version", "service", "expires_at", "batch"})
    if type(policy["schema_version"]) is not int or policy["schema_version"] != 1:
        raise ValueError("invalid frozen read policy")
    context = replace(reqctx.service(identifier(policy["service"])), purpose="frozen-testing")
    return context, instant(policy["expires_at"]), batch_type.parse(policy["batch"])


def _scoped_access(variable: str):
    context, authorize = _callback(variable)()
    if not isinstance(context, reqctx.Context) or not context.purpose.strip() or not callable(authorize):
        raise ValueError("trusted caller and authorizer required")
    context.validate()
    return context, authorize


def _policy_authorizer(resources: ExitStack, command: str, path: Path, batch_type):
    operations = {"read": "read", "verify": "read", "read-record": "read-record",
                  **{name: name for name in _BROWSER_POLICY_READS}}
    if command not in operations or any(name in os.environ for name in (_ORIGINAL_ACCESS, _RECORD_ACCESS, _BROWSER_ACCESS)):
        raise ValueError("read policy requires an exclusive read-only configuration")
    context, _, _ = _read_policy(path, batch_type)
    registry = Registry(os.environ["MIAOQIA_ACQUISITION_RIGHTS_READER_DSN"])
    resources.callback(registry.close)
    with reqctx.use(context):
        registry.ensure_reader()
    expected_operation = operations[command]

    def authorize(operation, item):
        if operation != expected_operation or reqctx.get() != context:
            return False
        current, expiry, batch = _read_policy(path, batch_type)
        if current != context or item not in batch.items:
            return False
        source = item.ref.source_id
        with registry.snapshot() as snapshot:
            return (snapshot.current_time() < expiry
                    and snapshot.public_revocations([source]).get(source) is False)

    return context, authorize


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="mq-acquisition-frozen-batches", description=__doc__.splitlines()[0])
    commands = parser.add_subparsers(dest="command", required=True)
    freezes = ("freeze", "freeze-records", "freeze-browser")
    restores = ("restore", "restore-record-original", *_BROWSER_RESTORES)
    for name in freezes:
        commands.add_parser(name).add_argument("--selection", type=Path, required=True)
    for name in ("read", "verify", "read-record", *_BROWSER_READS, *restores):
        command = commands.add_parser(name)
        command.add_argument("--manifest", type=Path, required=True)
        command.add_argument("--index", type=int, required=True, help="zero-based manifest entry index")
    args = parser.parse_args(argv)
    browser_command = args.command in ("freeze-browser", *_BROWSER_READS, *_BROWSER_RESTORES)
    record_command = args.command in {"freeze-records", "read-record", "restore-record-original"}
    if browser_command:
        batch_type, access, authorize_option = FrozenBrowserBatch, _BROWSER_ACCESS, "authorize_browser"
    elif record_command:
        batch_type, access, authorize_option = FrozenRecords, _RECORD_ACCESS, "authorize_record"
    else:
        batch_type, access, authorize_option = FrozenBatch, _ORIGINAL_ACCESS, "authorize"

    try:
        if args.command in freezes:
            selection = _load(args.selection)
            if not isinstance(selection, list) or not 1 <= len(selection) <= MAX_BATCH_ITEMS:
                raise ValueError("invalid selection")
            refs = tuple(ObservationRef.parse(item) for item in selection)
        else:
            batch = batch_type.parse(_load(args.manifest))
    except (OSError, ValueError, RecursionError):
        return _error("invalid_input", 2)

    resources = ExitStack()
    try:
        if policy := os.environ.get("MIAOQIA_ACQUISITION_FROZEN_READ_POLICY"):
            context, authorize = _policy_authorizer(resources, args.command, Path(policy), batch_type)
        elif browser_command or record_command:
            context, authorize = _scoped_access(access)
        else:
            context = reqctx.service("acquisition-frozen-batches")
            authorize = _callback(access)
        options = {authorize_option: authorize}
        if args.command in {"freeze-records", "read-record"}:
            options["record_adapters"] = _record_adapters(Path(os.environ["MIAOQIA_PACKS_DIR"]))
        restoring = args.command in restores
        store = open_object_store(os.environ["MIAOQIA_ACQUISITION_OBJECT_STORE_DIR"], read_only=not restoring)
        backup = None
        if restoring:
            try:
                backup = open_object_store(
                    os.environ["MIAOQIA_ACQUISITION_FROZEN_BACKUP"], read_only=True,
                    s3_profile=os.environ.get("MIAOQIA_ACQUISITION_FROZEN_BACKUP_S3_PROFILE"),
                )
            except FileNotFoundError:
                # Restore checks authorization and the primary before it needs backup bytes.
                pass
        database = db.Database(os.environ["MIAOQIA_ACQUISITION_FROZEN_READER_DSN"], pool_size=1)
        resources.callback(database.close)
    except Exception:
        resources.close()
        return _error("configuration_unavailable", 1)

    content = None
    try:
        service = FrozenBatches(database, store, **options)
        with reqctx.use(context):
            if args.command == "freeze":
                result = service.freeze(refs).dto()
            elif args.command == "freeze-records":
                result = service.freeze_records(refs).dto()
            elif args.command == "read-record":
                result = service.read_record(batch, args.index).dto()
            elif args.command == "restore":
                result = {"status": service.restore(batch, args.index, backup=backup)}
            elif args.command == "restore-record-original":
                result = {"status": service.restore_record_original(batch, args.index, backup=backup)}
            elif args.command == "freeze-browser":
                result = service.freeze_browser(refs).dto()
            elif args.command == "read-projection":
                result = service.read_projection(batch, args.index).dto()
            elif args.command == "verify-browser":
                service.verify_browser(batch, args.index)
                result = {"status": "verified"}
            elif args.command == "read-browser-original":
                content = service.read_browser_original(batch, args.index)
            elif args.command == "restore-browser-original":
                result = {"status": service.restore_browser_original(batch, args.index, backup=backup)}
            elif args.command == "restore-browser-projection":
                result = {"status": service.restore_browser_projection(batch, args.index, backup=backup)}
            else:
                raw = service.read(batch, args.index)
                if args.command == "read":
                    content = raw
                else:
                    result = {"status": "verified"}
    except FrozenBatchDenied:
        return _error("access_denied", 1)
    except FrozenObjectError as exc:
        return _error(exc.code, 1)
    except ValueError:
        return _error("invalid_input", 2)
    except Exception:
        return _error("operation_unavailable", 1)
    finally:
        resources.close()
    if content is None:
        print(json.dumps(result, ensure_ascii=False))
    else:
        sys.stdout.buffer.write(content)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
