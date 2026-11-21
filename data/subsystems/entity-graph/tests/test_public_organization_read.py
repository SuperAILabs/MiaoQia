"""Authenticated public organization reads over TLS with synthetic PostgreSQL owners."""

import json
import os
from pathlib import Path
import subprocess

import pytest

from miaoqia.acquisition.api.remote import RightsClient
from miaoqia.acquisition.application.http import routes as rights_routes
from miaoqia.acquisition.application.rights import Decisions
from miaoqia.kernel import ids, reqctx
from miaoqia.kernel.internal_http import Client, Problem, Server
from miaoqia.runtime_data import POOL_TIMEOUT, _delegations, read_routes
from miaoqia.runtime_data.organization_read import configured_public_organizations
from miaoqia.runtime_data.organizations import bind_organizations

from test_registration_runtime import NAME, READER, SOURCE, USCC, WRITER, document, plane  # noqa: F401


PATH = "/internal/v1/data/get-public-organization"
OP = "data.get_public_organization"
DATA_PEER = "spiffe://miaoqia/service/data-api"
ACQ_PEER = "spiffe://miaoqia/service/acq-admin"
CALLER_PEER = "spiffe://miaoqia/service/synthetic-org-caller"
OTHER_PEER = "spiffe://miaoqia/service/synthetic-fact-caller"
MONITOR = reqctx.Context(reqctx.Principal("svc:org-reader", "service"), READER.tenant, READER.cell, "monitoring")


class IdentityFixture:
    active = True

    def resolve(self, *, deadline):
        if not self.active or reqctx.current() != READER:
            raise Problem(403)


class RightsFixture:
    display = None
    on_confirm = None

    def __init__(self, client):
        self.client = client
        self.requests = []
        self.confirmations = []

    def status(self, *, deadline):
        return self.client.status(deadline=deadline)

    def resolve(self, request, *, deadline):
        result = self.client.resolve(request, deadline=deadline)
        self.requests.append((request, result))
        return {**result, "display": self.display} if self.display else result

    def confirm(self, request, *, deadline):
        self.confirmations.append(request)
        if self.on_confirm is not None:
            self.on_confirm()
        return self.client.confirm(request, deadline=deadline)


@pytest.fixture
def listener(plane, tls_factory, running_server):
    identity = IdentityFixture()
    data_tls = tls_factory("data-org", DATA_PEER)
    rights_grants = {(DATA_PEER, op, "interactive") for op in ("rights.resolve", "rights.confirm", "rights.status")}
    rights_grants.update((DATA_PEER, op, "monitoring", MONITOR.principal.id)
                         for op in ("rights.resolve", "rights.confirm", "rights.status"))
    acq = Server(("127.0.0.1", 0), tls_factory("acq-org", ACQ_PEER), rights_grants, identity,
                 rights_routes(Decisions(plane.rights)))
    acq_url = running_server(acq)
    rights = RightsFixture(RightsClient(Client(acq_url + "/internal/v1", data_tls, ACQ_PEER)))
    settings = {"MIAOQIA_ENTITY_GRAPH_DB_DSN": plane.entity_dsn, "MIAOQIA_PUBLIC_REGISTRY_SOURCES": json.dumps([SOURCE])}
    with configured_public_organizations(settings, plane.dom, plane.evd, plane.store, plane.evidence_dsn,
                                         pool_timeout=POOL_TIMEOUT) as bind:
        handlers = read_routes(plane.dom, plane.evd, plane.store, rights, identity, bind_public_organizations=bind)
        delegations = _delegations(json.dumps([
            {"peer": CALLER_PEER, "operation": OP, "purpose": "interactive"},
            {"peer": CALLER_PEER, "operation": OP, "purpose": "monitoring", "principal": MONITOR.principal.id},
            {"peer": OTHER_PEER, "operation": "data.get_fact", "purpose": "interactive"},
        ]))
        data = Server(("127.0.0.1", 0), data_tls, delegations, identity, handlers)
        url = running_server(data)
        caller_tls = tls_factory("caller-org", CALLER_PEER)
        caller = Client(url, caller_tls, DATA_PEER)
        yield caller, url, caller_tls, identity, rights


@pytest.mark.parametrize("settings", [
    {},
    {"MIAOQIA_ENTITY_GRAPH_DB_DSN": "postgresql://example.invalid/db"},
    {"MIAOQIA_ENTITY_GRAPH_DB_DSN": "postgresql://example.invalid/db", "MIAOQIA_PUBLIC_REGISTRY_SOURCES": ""},
    {"MIAOQIA_ENTITY_GRAPH_DB_DSN": "", "MIAOQIA_PUBLIC_REGISTRY_SOURCES": '["synthetic-registry"]'},
    {"MIAOQIA_ENTITY_GRAPH_DB_DSN": "", "MIAOQIA_PUBLIC_REGISTRY_SOURCES": ""},
])
def test_unset_or_empty_configuration_disables_the_route(settings):
    with configured_public_organizations(settings, None, None, None, None, pool_timeout=1) as bind:
        assert bind is None
        assert PATH not in read_routes(None, None, None, None, None, bind_public_organizations=bind)


@pytest.mark.parametrize("raw", ['[]', '"synthetic-registry"', '[false]', '["*"]', '["repeat","repeat"]', '{invalid'])
def test_nonempty_invalid_allowlist_fails_closed(raw):
    settings = {"MIAOQIA_ENTITY_GRAPH_DB_DSN": "postgresql://example.invalid/db", "MIAOQIA_PUBLIC_REGISTRY_SOURCES": raw}
    with pytest.raises(ValueError), configured_public_organizations(settings, None, None, None, None, pool_timeout=1):
        pytest.fail("invalid allowlist enabled a read binding")


def fetch(client, org_id, version, context=READER):
    with reqctx.use(context):
        return client.request("POST", PATH, {"org_id": org_id, "version": version})


def test_public_organization_read_current_version_and_go(listener, plane):
    caller, url, tls, _, rights = listener
    first = plane.ingest(SOURCE, "registration:alpha", document())
    org = plane.confirm(first).organization
    expected = {"org_id": org.org_id, "version": 1, "name": NAME, "uscc": USCC.value, "rights_tags": [f"public:{SOURCE}"]}
    assert fetch(caller, org.org_id, 1) == expected
    assert len(rights.requests) == 1 and rights.confirmations
    request, decision = rights.requests[0]
    assert request == {"operation": "evidence_resolve", "inputs": [{"source_id": SOURCE, "tags": [f"public:{SOURCE}"]}]}
    assert all(confirmed == {**request, "receipt": decision["receipt"]} for confirmed in rights.confirmations)
    assert fetch(caller, org.org_id, 1, MONITOR) == expected
    assert fetch(caller, org.org_id, 2) is None
    assert fetch(caller, ids.new("org"), 1) is None
    with reqctx.use(READER):
        for invalid in ({"org_id": org.org_id, "version": None}, {"org_id": org.org_id, "version": True},
                        {"org_id": org.org_id, "version": 0}, {"org_id": org.org_id, "version": 1 << 63},
                        {"org_id": first.fact_id, "version": 1}, {"org_id": org.org_id, "version": 1, "rights_tags": []}):
            with pytest.raises(Problem) as problem:
                caller.request("POST", PATH, invalid)
            assert problem.value.status == 400

    if os.environ.get("MIAOQIA_TEST_DATAREAD_GO") == "1":
        result = subprocess.run(
            ["go", "test", "-count=1", "-v", "./kernel/dataread", "-run", "^TestPublicOrganizationAgainstPythonListener$"],
            cwd=Path(__file__).resolve().parents[4] / "services",
            env={**os.environ, "MIAOQIA_TEST_PUBLIC_ORG_URL": url, "MIAOQIA_TEST_PUBLIC_ORG_ID": org.org_id,
                 "MIAOQIA_INTERNAL_TLS_CERT": tls.cert, "MIAOQIA_INTERNAL_TLS_KEY": tls.key, "MIAOQIA_INTERNAL_TLS_CA": tls.ca},
            text=True, capture_output=True, timeout=60,
        )
        assert result.returncode == 0, result.stdout + result.stderr
        print(result.stdout, end="")

    updated = plane.ingest(SOURCE, "registration:alpha", document(name=NAME + "更名"))
    assert fetch(caller, org.org_id, 1) is None
    assert plane.confirm(updated).organization.version == 2
    assert fetch(caller, org.org_id, 1) is None
    assert fetch(caller, org.org_id, 2)["name"] == NAME + "更名"
    conflict = plane.ingest(SOURCE, "registration:another", document(name=NAME + "冲突"))
    assert plane.confirm(conflict).status == "candidate"
    assert fetch(caller, org.org_id, 2) is None


def test_public_organization_denies_clipped_display_rejected_callers_and_revocation(listener, plane, tls_factory):
    caller, url, _, identity, rights = listener
    org = plane.confirm(plane.ingest(SOURCE, "registration:alpha", document())).organization
    reads = plane.store.reads

    def rejected(client=caller, context=READER):
        with pytest.raises(Problem) as problem:
            fetch(client, org.org_id, 1, context)
        assert problem.value.status == 403

    other = tls_factory("undelegated-org", OTHER_PEER)
    rejected(Client(url, other, DATA_PEER))
    rejected(context=reqctx.Context(reqctx.Principal("svc:other", "service"), READER.tenant, READER.cell, "monitoring"))
    identity.active = False
    rejected()
    identity.active = True
    for display in ("citation_only", "excerpt"):
        rights.display = display
        rejected()
    rights.display = None
    assert plane.store.reads == reads

    def revoke_after_grant():
        rights.on_confirm = None
        assert plane.change_rights("revoke_public", 0) == 1

    rights.on_confirm = revoke_after_grant
    rejected()
    rejected()
    assert plane.store.reads == reads


def test_public_organization_withholds_a_head_superseded_during_read(listener, plane):
    caller, _, _, _, rights = listener
    org = plane.confirm(plane.ingest(SOURCE, "registration:alpha", document())).organization

    def advance_after_proof():
        if len(rights.confirmations) == 2:
            rights.on_confirm = None
            plane.ingest(SOURCE, "registration:alpha", document(name=NAME + "更名"))

    rights.on_confirm = advance_after_proof
    with pytest.raises(Problem) as problem:
        fetch(caller, org.org_id, 1)
    assert problem.value.status == 503
    assert fetch(caller, org.org_id, 1) is None


def test_final_public_organization_confirmation_rechecks_after_proof(listener, plane):
    caller, _, _, _, rights = listener
    org = plane.confirm(plane.ingest(SOURCE, "registration:alpha", document())).organization

    def revoke_at_final_confirmation():
        # Each owner read confirms before and after its field proofs. The last
        # check uses the original receipt just before the HTTP body is released.
        if len(rights.confirmations) == 5:
            rights.on_confirm = None
            assert plane.change_rights("revoke_public", 0) == 1

    rights.on_confirm = revoke_at_final_confirmation
    with pytest.raises(Problem) as problem:
        fetch(caller, org.org_id, 1)
    assert problem.value.status == 403
    assert len(rights.requests) == 1 and len(rights.confirmations) == 5


def test_mixed_closure_cannot_be_used_by_public_read(listener, plane):
    caller, _, _, _, rights = listener
    mixed = plane.ingest(SOURCE, "registration:mixed", document(), rights_tags=(f"public:{SOURCE}", "restricted:synthetic"))
    # Materialize an existing registration admitted for a separate, tighter synthetic purpose.
    writer = bind_organizations(plane.ent, plane.dom, plane.evd, plane.store, extractions=plane.extractions,
                                approved_sources=[SOURCE], authorize=lambda *_: lambda: True)
    with reqctx.use(WRITER):
        org = writer.confirm_registration(mixed).organization
    reads = plane.store.reads
    assert fetch(caller, org.org_id, 1) is None
    assert not rights.requests and plane.store.reads == reads
