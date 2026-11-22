"""Standalone service wiring for the certificate-authenticated public search export."""

from contextlib import ExitStack, contextmanager
import json
import os
import re
import signal
import sys
import time

from miaoqia.acquisition.api import identifier
from miaoqia.acquisition.infrastructure.rights import Registry
from miaoqia.domain_intelligence.api.search_projection import (
    DiscoveryDecision,
    InvalidProjectionEvent,
    ProjectionUnavailable,
    owner,
)
from miaoqia.domain_intelligence.application.search_projection import RIGHTS_CHANGED, _event
from miaoqia.domain_intelligence.api import FACT_UPSERTED
from miaoqia.entity_graph.api.search_projection import MAX_BATCH as ORG_BATCH
from miaoqia.kernel import db, reqctx
from miaoqia.kernel.internal_http import PreparedResponse, Problem, Server
from miaoqia.kernel.jsoncodec import MAX_BODY, loads

PATH = "/internal/v1/search-projection/mutations"
OPERATION = "search_projection.mutations"
CLIENT_PEER = "spiffe://miaoqia/service/platform-public-search-workflow-worker"
SERVER_PEER = "spiffe://miaoqia/service/public-search-export"
_CALLER = reqctx.service(CLIENT_PEER.rsplit("/", 1)[1])
SNAPSHOT_CALLER = reqctx.Context(reqctx.Principal("svc:public-search-export"), purpose="search_projection.snapshot")
_ORG_PHASE = "~org:"
_ORG_ID = re.compile(r"org_[0-9A-HJKMNP-TV-Z]{26}")
_ORG_CREATED = "entity.entity.created.v1"
_ORG_UPDATED = "entity.entity.updated.v1"
_MERGE_APPLIED = "entity.merge.applied.v1"
_MERGE_REVERTED = "entity.merge.reverted.v1"
_ENTITY_EVENTS = {_ORG_CREATED, _ORG_UPDATED, _MERGE_APPLIED, _MERGE_REVERTED}


class RegistryDiscoveryPolicy:
    """Use Acquisition's coherent authority snapshot plus the explicit discovery allowlist."""

    def __init__(self, registry, approved_sources):
        if not isinstance(approved_sources, list):
            raise ValueError("explicit JSON source allowlist required")
        self.approved_sources = frozenset(identifier(s) for s in approved_sources)
        if len(self.approved_sources) != len(approved_sources):
            raise ValueError("duplicate source approval")
        self.registry = registry

    def resolve(self, source_ids, *, deadline):
        if time.monotonic() >= deadline:
            raise ProjectionUnavailable("discovery policy deadline")
        with self.registry.snapshot() as snapshot:
            current = snapshot.public_revocations(source_ids & self.approved_sources)
            approved = frozenset(source for source, revoked in current.items() if revoked is False)
            decision = DiscoveryDecision(snapshot.epoch, approved)
        if time.monotonic() >= deadline:
            raise ProjectionUnavailable("discovery policy deadline")
        return decision


class OwnerExports:
    """Route validated events and fixed handles to their authoritative owners."""

    def __init__(self, procurement, organizations):
        self.procurement = procurement
        self.organizations = organizations

    def prepare(self, event, *, deadline, after=""):
        name, data = _event(event)
        org_phase = after.startswith(_ORG_PHASE)
        org_after = after.removeprefix(_ORG_PHASE) if org_phase else ""
        if org_phase and org_after and not _ORG_ID.fullmatch(org_after):
            raise InvalidProjectionEvent("invalid organization continuation")
        if name == RIGHTS_CHANGED:
            if org_phase:
                page = self.organizations.prepare_source(data["source_id"], after=org_after,
                                                         rights_version=data["rights_version"], deadline=deadline)
                if page.next_after is not None:
                    page.next_after = _ORG_PHASE + page.next_after
                return page
            page = self.procurement.prepare(event, deadline=deadline, after=after)
            if page.next_after is None:
                page.next_after = _ORG_PHASE
            return page
        if name in _ENTITY_EVENTS:
            return self._entity_event(event, name, data, deadline, after, org_phase, org_after)
        page = self.procurement.prepare(event, deadline=deadline, after=after)
        if name == FACT_UPSERTED and data["info_domain"] == "INF-ORG" and data["object_type"] == "organization.registration":
            return self.organizations.prepare_registration(data["fact_id"], deadline=deadline)
        return page

    def _entity_event(self, event, name, data, deadline, after, org_phase, org_after):
        if name in (_ORG_CREATED, _ORG_UPDATED):
            if after or event.get("subject") != data["entity_id"]:
                raise InvalidProjectionEvent("organization event subject or continuation mismatch")
            ids = [data["entity_id"]]
        else:
            if after and (not org_phase or not org_after):
                raise InvalidProjectionEvent("invalid merge event continuation")
            ids = ([data["survivor_id"], *data["absorbed_ids"]] if name == _MERGE_APPLIED else data["restored_ids"])
        ids = sorted({value for value in ids if value > org_after and _ORG_ID.fullmatch(value)})
        if not ids:
            return self.procurement.prepare(event, deadline=deadline)
        page = self.organizations.prepare_ids(ids[:ORG_BATCH], deadline=deadline)
        if len(ids) > ORG_BATCH:
            page.next_after = _ORG_PHASE + ids[ORG_BATCH - 1]
        return page

    def prepare_snapshot_page(self, handles, *, deadline):
        if isinstance(handles, list) and handles and all(
            isinstance(handle, dict) and isinstance(handle.get("id"), str) and _ORG_ID.fullmatch(handle["id"])
            for handle in handles
        ):
            return self.organizations.prepare_snapshot_page(handles, deadline=deadline)
        return self.procurement.prepare_snapshot_page(handles, deadline=deadline)


def route(projection):
    """Adapt the certificate-only kernel request to the owner port."""
    def handle(body, deadline):
        if reqctx.get() != _CALLER:
            raise Problem(403)
        after = ""
        if isinstance(body, dict) and "event" in body:
            if set(body) != {"event", "after"} or not isinstance(body["after"], str) or not body["after"]:
                raise Problem(400)
            after, body = body["after"], body["event"]
        try:
            prepared = projection.prepare(body, deadline=deadline, after=after)
        except InvalidProjectionEvent:
            raise Problem(400) from None
        except Exception:
            raise Problem(503) from None

        def before_send(deadline):
            try:
                prepared.confirm(deadline=deadline)
            except Exception:
                raise Problem(503) from None

        payload = {"mutations": prepared.mutations}
        if prepared.next_after is not None:
            payload["next_after"] = prepared.next_after
        return PreparedResponse(payload, before_send)

    return handle


def server(address, tls, projection):
    return Server(address, tls, (), None, {PATH: (OPERATION, route(projection))},
                  certificate_services={(CLIENT_PEER, OPERATION)})


@contextmanager
def _assembly(environ):
    from miaoqia import evidence
    from miaoqia.kernel.objectstore import open_object_store

    with ExitStack() as stack:
        dom = db.Database(environ["MIAOQIA_DOMAIN_INTELLIGENCE_DB_DSN"], pool_timeout=0.5)
        stack.callback(dom.close)
        registry = Registry(environ["MIAOQIA_ACQUISITION_RIGHTS_READER_DSN"])
        stack.callback(registry.close)
        with reqctx.use(reqctx.service("public-search-export")):
            registry.ensure_reader()
        approved = loads(environ["MIAOQIA_PUBLIC_SEARCH_DISCOVERY_SOURCES"])
        policy = RegistryDiscoveryPolicy(registry, approved)
        chunks = environ.get("MIAOQIA_PUBLIC_SEARCH_NOTICE_CHUNKS") == "1"
        organizations = environ.get("MIAOQIA_PUBLIC_SEARCH_ORGANIZATIONS") == "1"
        evidence_db = storage = None
        if chunks or organizations:
            evidence_db = db.Database(environ["MIAOQIA_EVIDENCE_DB_DSN"], pool_timeout=0.5)
            stack.callback(evidence_db.close)
            storage = open_object_store(environ["MIAOQIA_EVIDENCE_OBJECT_STORE_DIR"])
        procurement = owner(dom, policy, notice_chunks=evidence.notice_chunks(evidence_db, storage) if chunks else None)
        if not organizations:
            yield procurement
            return

        from .organization_registry import registry_authorization
        from .organizations import bind_organization_search, parse_registry_sources

        sources = parse_registry_sources(environ["MIAOQIA_PUBLIC_REGISTRY_SOURCES"])
        organization_policy = RegistryDiscoveryPolicy(registry, sorted(set(sources) & set(approved)))
        authorization = registry_authorization(registry, callers={_CALLER, SNAPSHOT_CALLER}, operation="get_entity",
                                                source_allowed=organization_policy.approved_sources.__contains__)
        entities = db.Database(environ["MIAOQIA_ENTITY_GRAPH_DB_DSN"], pool_timeout=0.5)
        stack.callback(entities.close)
        extractions = evidence.extractions(environ["MIAOQIA_EVIDENCE_DB_DSN"])
        stack.callback(extractions.close)
        projection = bind_organization_search(entities, dom, evidence_db, storage, extractions=extractions,
                                              approved_sources=sources, authorize=authorization,
                                              policy=organization_policy)
        yield OwnerExports(procurement, projection)


def _serve():
    from . import _tls

    host, port = os.environ["MIAOQIA_PUBLIC_SEARCH_EXPORT_LISTEN"].rsplit(":", 1)
    if not host or not 1 <= int(port) <= 65535:
        raise ValueError("explicit listener address required")

    def stop(*_):
        raise SystemExit(0)

    signal.signal(signal.SIGTERM, stop)
    with _assembly(os.environ) as projection, server((host, int(port)), _tls(), projection) as listener:
        listener.serve_forever()


def main():
    args = sys.argv[1:]
    if args == ["serve"]:
        try:
            _serve()
            return 0
        except KeyboardInterrupt:
            return 0
        except Exception:
            print("public search export listener unavailable", file=sys.stderr)
            return 1
    if args != ["snapshot-page"]:
        print("Usage: python -m miaoqia.runtime_data.search_projection {serve|snapshot-page}", file=sys.stderr)
        return 2
    try:
        handles = loads(sys.stdin.buffer.read(MAX_BODY + 1))
        deadline = time.monotonic() + 5
        with _assembly(os.environ) as projection, reqctx.use(SNAPSHOT_CALLER):
            prepared = projection.prepare_snapshot_page(handles, deadline=deadline)
            output = json.dumps({"mutations": prepared.mutations}, ensure_ascii=False, allow_nan=False)
            if len(output.encode()) > MAX_BODY:
                raise ProjectionUnavailable("snapshot page response too large")
            prepared.confirm(deadline=deadline)
            print(output)
        return 0
    except Exception:
        print("public search snapshot page unavailable", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
