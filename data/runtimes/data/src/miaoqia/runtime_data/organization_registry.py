"""Confirm an approved fixed registry fact using the public organization authority."""

import argparse
from contextlib import ExitStack
import json
import os
import sys

from miaoqia import evidence
from miaoqia.acquisition.infrastructure.rights import Registry
from miaoqia.entity_graph.api.organizations import RegistrationRef
from miaoqia.kernel import db, reqctx
from miaoqia.kernel.objectstore import open_object_store

from .organizations import bind_organizations, parse_registry_sources

WRITER = reqctx.service("organization-registry-confirmation")


def registry_authorization(registry, *, callers, operation, source_allowed):
    """Bind one public service operation to current exact public source rights."""
    def authorize(caller, requested, groups):
        if caller not in callers or caller.tenant or caller.cell or requested != operation:
            raise PermissionError("public registry operation not admitted")
        source_ids = frozenset(group.source_id for group in groups)
        if any(group.tags != (f"public:{group.source_id}",) or not source_allowed(group.source_id) for group in groups):
            raise PermissionError("registry source not approved for this operation")

        def permitted():
            if reqctx.get() != caller:
                return False
            with registry.snapshot() as snapshot:
                states = snapshot.public_revocations(source_ids) if source_ids else {}
                return states.keys() == source_ids and all(revoked is False for revoked in states.values())

        if not permitted():
            raise PermissionError("registry source not currently readable")
        return permitted

    return authorize


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("fact_id", help="fixed INF-ORG organization.registration fact ID")
    parser.add_argument("version", type=int, help="fixed registration version")
    args = parser.parse_args(argv)
    environ = os.environ
    try:
        sources = parse_registry_sources(environ["MIAOQIA_PUBLIC_REGISTRY_SOURCES"])
        with ExitStack() as stack, reqctx.use(WRITER):
            owner_dbs = []
            for owner in ("ENTITY_GRAPH", "DOMAIN_INTELLIGENCE", "EVIDENCE"):
                owner_db = db.Database(environ[f"MIAOQIA_{owner}_DB_DSN"], pool_timeout=0.5)
                stack.callback(owner_db.close)
                owner_dbs.append(owner_db)
            registry = Registry(environ["MIAOQIA_ACQUISITION_RIGHTS_READER_DSN"])
            stack.callback(registry.close)
            registry.ensure_reader()
            extractions = evidence.extractions(environ["MIAOQIA_EVIDENCE_DB_DSN"])
            stack.callback(extractions.close)
            organizations = bind_organizations(
                *owner_dbs, open_object_store(environ["MIAOQIA_EVIDENCE_OBJECT_STORE_DIR"]),
                extractions=extractions, approved_sources=sources,
                authorize=registry_authorization(registry, callers={WRITER}, operation="confirm_registration",
                                                 source_allowed=frozenset(sources).__contains__),
            )
            result = organizations.confirm_registration(RegistrationRef(args.fact_id, args.version))
    except PermissionError:
        print("organization registration denied", file=sys.stderr)
        return 1
    except Exception:
        print("organization registration unavailable or invalid", file=sys.stderr)
        return 1
    output = {"status": result.status}
    if result.organization is not None:
        output.update(org_id=result.organization.org_id, version=result.organization.version)
    print(json.dumps(output))
    return 0 if result.status == "confirmed" else 3


if __name__ == "__main__":
    raise SystemExit(main())
