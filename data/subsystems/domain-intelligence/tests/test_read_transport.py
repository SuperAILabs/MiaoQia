from __future__ import annotations

from datetime import datetime, timezone

import pytest

from miaoqia.domain_intelligence.api import FactRead, Page
from miaoqia.domain_intelligence.application.read_transport import ReadAuthorization, routes
from miaoqia.kernel import reqctx


class Facts:
    def __init__(self):
        self.calls = []

    def requirements(self, fact_id, version):
        self.calls.append(("requirements", fact_id, version))
        return {"source_group": "g1"}

    def get_fact(self, fact_id, basis, *, version=None, deadline=None):
        self.calls.append(("get", fact_id, basis, version))
        return FactRead(basis, {"id": fact_id, "version": version or 2})

    def get_fact_history(self, fact_id, basis, *, after=0, limit=50, deadline=None):
        self.calls.append(("history", fact_id, basis, after, limit))
        return Page(basis, ({"version": 1},), "next")

    def query_facts(self, info_domain, object_type, basis, *, after="", limit=50, deadline=None):
        self.calls.append(("query", info_domain, object_type, basis, after, limit))
        return Page(basis, ({"fact_id": "ntc_1"},), None)

    def bid_result_units(self, fact_id, basis, *, version=None, deadline=None):
        self.calls.append(("bid", fact_id, basis, version))
        return Page(basis, ({"key": "lot-1"},), None)


class Evidence:
    def resolve(self, evidence_ref, *, deadline=None):
        return type("Evidence", (), {"id": evidence_ref, "display_level": "full",
            "snapshot_ref": "s1", "source_id": "src1", "source_key": "k",
            "observed_at": None, "locator": {}, "snippet": "text", "context": {},
            "derivation": None})()


def _routes(collectors=None):
    facts, evidence, finals = Facts(), Evidence(), []
    if collectors is None:
        collectors = {name: (lambda body, deadline, name=name: [{"group": name}]) for name in (
            "data.get_fact", "data.fact_history", "data.query_facts", "data.bid_result_units", "data.resolve_evidence")}

    def admit(operation, inputs, deadline):
        finals.append(("admit", operation, inputs))
        return lambda remaining: finals.append(("confirm", operation, remaining))

    def before_send(operation, value, hook):
        finals.append(("prepare", operation, value))
        return hook

    return routes(facts, evidence, admit=admit, before_send=before_send, collectors=collectors), facts, finals


def test_all_five_routes_collect_before_read_and_finalize():
    handlers, facts, finals = _routes()
    basis = {"view": "current", "recorded_cutoff": None}
    requests = [
        ("/internal/v1/data/get-fact", {"fact_id": "ntc_1", "basis": basis, "version": 1}),
        ("/internal/v1/data/fact-history", {"fact_id": "ntc_1", "basis": basis, "after": 0, "limit": 10}),
        ("/internal/v1/data/query-facts", {"info_domain": "procurement", "object_type": "notice", "basis": basis, "after": "cursor", "limit": 10}),
        ("/internal/v1/data/bid-result-units", {"fact_id": "ntc_1", "basis": basis, "version": None}),
        ("/internal/v1/data/resolve-evidence", {"evidence_ref": "ev_1"}),
    ]
    with reqctx.use(reqctx.Context(reqctx.Principal("u", "user"), "tenant", "cell", "interactive")):
        for path, body in requests:
            op, fn = handlers[path]
            response = fn(body, 10.0)
            assert response.payload is not None
            response.before_send(9.0)
            assert finals[-2][0] == "prepare" and finals[-1][0] == "confirm"
    assert [v[1] for v in finals if v[0] == "admit"] == [op for _, (op, _) in handlers.items()]


@pytest.mark.parametrize("body", [
    {"fact_id": "ntc_1", "basis": {"view": "current", "recorded_cutoff": None}, "version": 1, "extra": 1},
    {"fact_id": "bad_1", "basis": {"view": "current", "recorded_cutoff": None}, "version": 1},
    {"fact_id": "ntc_1", "basis": {"view": "current", "recorded_cutoff": "2026-01-01T00:00:00Z"}, "version": 1},
])
def test_route_dtos_are_closed_and_basis_is_strict(body):
    handlers, _, _ = _routes()
    with reqctx.use(reqctx.Context(reqctx.Principal("u", "user"), "tenant", "cell", "interactive")):
        with pytest.raises((ValueError, TypeError)):
            handlers["/internal/v1/data/get-fact"][1](body, 10.0)


def test_read_authorization_requires_active_user_and_confirms_same_request():
    calls = []
    class Identity:
        def resolve(self, *, deadline): calls.append(("identity", deadline))
    class Rights:
        def resolve(self, request, *, deadline): calls.append(("resolve", request, deadline)); return {"allowed": True, "receipt": "r"}
        def status(self, *, deadline): calls.append(("status", deadline))
        def confirm(self, request, *, deadline): calls.append(("confirm", request, deadline)); return {"allowed": True}
    auth = ReadAuthorization(Rights(), Identity())
    with reqctx.use(reqctx.Context(reqctx.Principal("u", "user"), "tenant", "cell", "interactive")):
        confirm = auth.prepare("data.get_fact", [{"group": "g"}], 10.0)
        confirm(9.0)
    assert calls[-1] == ("confirm", {"operation": "data.get_fact", "inputs": [{"group": "g"}], "receipt": "r"}, 9.0)
    with reqctx.use(reqctx.Context(reqctx.Principal("svc", "service"), "tenant", "cell", "interactive")):
        with pytest.raises(PermissionError):
            auth.prepare("data.get_fact", [], 10.0)


def test_known_at_basis_requires_timezone_and_preserves_cutoff():
    handlers, facts, _ = _routes()
    cutoff = "2026-01-01T00:00:00Z"
    with reqctx.use(reqctx.Context(reqctx.Principal("u", "user"), "tenant", "cell", "interactive")):
        handlers["/internal/v1/data/get-fact"][1]({"fact_id": "ntc_1", "basis": {"view": "known_at", "recorded_cutoff": cutoff}, "version": None}, 10.0)
    assert facts.calls[-1][2].recorded_cutoff == datetime(2026, 1, 1, tzinfo=timezone.utc)
