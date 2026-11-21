from __future__ import annotations

from datetime import datetime, timezone

import pytest

from miaoqia.acquisition.api import SourceInput
from miaoqia.domain_intelligence.api import FactRead, Page
from miaoqia.domain_intelligence.application.read_transport import ReadAuthorization, admitted_display, routes
from miaoqia.kernel import reqctx
from miaoqia.kernel.internal_http import Problem


GROUP = SourceInput("fixture", ("public:fixture",))
INPUTS = [GROUP.dto()]


class Facts:
    def __init__(self):
        self.calls = []

    def prepare_read(self, select, *, deadline):
        return select(self), INPUTS

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
        return type("Evidence", (), {"id": evidence_ref, "display_level": "full_text",
            "snapshot_ref": "s1", "source_id": GROUP.source_id, "source_key": "k",
            "observed_at": datetime(2026, 1, 1, tzinfo=timezone.utc), "locator": {"block": 0}, "snippet": "text", "context": "text",
            "derivation": None})()


def _routes():
    facts, evidence, finals = Facts(), Evidence(), []

    def admit(operation, inputs, deadline):
        finals.append(("admit", operation, inputs))
        if operation == "data.resolve_evidence":
            _recording_auth([]).prepare(operation, inputs, deadline)
        return lambda remaining: finals.append(("confirm", operation, remaining))

    return routes(facts, evidence, admit=admit, evidence_groups=lambda ref, deadline: INPUTS), facts, finals


def test_all_five_routes_prepare_and_finalize():
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
            assert finals[-2][0] == "admit" and finals[-1][0] == "confirm"
    assert [v[1] for v in finals if v[0] == "admit"] == [op for _, (op, _) in handlers.items()]


def test_query_facts_accepts_empty_cursor_for_first_page():
    handlers, facts, _ = _routes()
    basis = {"view": "current", "recorded_cutoff": None}
    body = {"info_domain": "INF-PROC", "object_type": "tendering.notice", "basis": basis, "after": "", "limit": 100}
    with reqctx.use(reqctx.Context(reqctx.Principal("u", "user"), "tenant", "cell", "interactive")):
        handlers["/internal/v1/data/query-facts"][1](body, 10.0)
        for missing in ("info_domain", "object_type"):
            with pytest.raises(ValueError):
                handlers["/internal/v1/data/query-facts"][1](dict(body, **{missing: ""}), 10.0)
    assert facts.calls[-1][4] == ""


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
        def resolve(self, request, *, deadline): calls.append(("resolve", request, deadline)); return {"allowed": True, "display": "full_text", "receipt": "r"}
        def status(self, *, deadline): calls.append(("status", deadline))
        def confirm(self, request, *, deadline): calls.append(("confirm", request, deadline)); return {"allowed": True, "display": "full_text"}
    auth = ReadAuthorization(Rights(), Identity())
    with reqctx.use(reqctx.Context(reqctx.Principal("u", "user"), "tenant", "cell", "interactive")):
        confirm = auth.prepare("data.get_fact", INPUTS, 10.0)
        confirm(9.0)
    assert calls[-1] == ("confirm", {"operation": "fact_read", "inputs": INPUTS, "receipt": "r"}, 9.0)
    with reqctx.use(reqctx.Context(reqctx.Principal("svc", "service"), "tenant", "cell", "interactive")):
        with pytest.raises(PermissionError):
            auth.prepare("data.get_fact", [], 10.0)


def test_known_at_basis_requires_timezone_and_preserves_cutoff():
    handlers, facts, _ = _routes()
    cutoff = "2026-01-01T00:00:00Z"
    with reqctx.use(reqctx.Context(reqctx.Principal("u", "user"), "tenant", "cell", "interactive")):
        handlers["/internal/v1/data/get-fact"][1]({"fact_id": "ntc_1", "basis": {"view": "known_at", "recorded_cutoff": cutoff}, "version": None}, 10.0)
    assert facts.calls[-1][2].recorded_cutoff == datetime(2026, 1, 1, tzinfo=timezone.utc)


def _recording_auth(calls):
    class Identity:
        def resolve(self, *, deadline): calls.append(("identity", deadline))
    class Rights:
        def resolve(self, request, *, deadline): calls.append(("resolve", request, deadline)); return {"allowed": True, "display": "full_text", "receipt": "r"}
        def status(self, *, deadline): calls.append(("status", deadline))
        def confirm(self, request, *, deadline): calls.append(("confirm", request, deadline)); return {"allowed": True, "display": "full_text"}
    return ReadAuthorization(Rights(), Identity())


@pytest.mark.parametrize("purpose, principal", [("monitoring", "svc:detection"), ("simulation", "svc:learning")])
def test_read_authorization_service_purpose_skips_identity_but_confirms_rights(purpose, principal):
    calls = []
    auth = _recording_auth(calls)
    with reqctx.use(reqctx.Context(reqctx.Principal(principal, "service"), "tenant", "cell", purpose)):
        confirm = auth.prepare("data.bid_result_units", INPUTS, 10.0)
        confirm(9.0)
        auth.prepare("data.bid_result_units", [], 10.0)(9.0)
    assert not any(c[0] == "identity" for c in calls)
    assert ("confirm", {"operation": "fact_read", "inputs": INPUTS, "receipt": "r"}, 9.0) in calls


@pytest.mark.parametrize("context", [
    reqctx.Context(reqctx.Principal("u", "user"), "tenant", "cell", "monitoring"),
    reqctx.Context(reqctx.Principal("svc:learning", "service"), "tenant", "cell", "interactive"),
    reqctx.Context(reqctx.Principal("svc:detection", "service_account"), "tenant", "cell", "monitoring"),
    reqctx.Context(reqctx.Principal("detection", "service"), "tenant", "cell", "monitoring"),
    reqctx.Context(reqctx.Principal("svc:detection", "service"), "", "", "monitoring"),
    reqctx.Context(reqctx.Principal("svc:detection", "service"), "tenant", "cell", "research"),
])
def test_read_authorization_rejects_principal_purpose_mismatch_before_any_call(context):
    calls = []
    auth = _recording_auth(calls)
    with reqctx.use(context):
        with pytest.raises(PermissionError):
            auth.prepare("data.bid_result_units", INPUTS, 10.0)
    assert calls == []


def test_read_authorization_maps_routes_to_closed_rights_operations():
    calls = []
    auth = _recording_auth(calls)
    with reqctx.use(reqctx.Context(reqctx.Principal("u", "user"), "tenant", "cell", "interactive")):
        auth.prepare("data.resolve_evidence", INPUTS, 10.0)
        with pytest.raises(ValueError):
            auth.prepare("data.unknown", INPUTS, 10.0)
    assert [c[1]["operation"] for c in calls if c[0] == "resolve"] == ["evidence_resolve"]


def test_admitted_display_is_exact_and_fails_closed():
    import contextvars

    auth = _recording_auth([])
    user = reqctx.Context(reqctx.Principal("u", "user"), "tenant", "cell", "interactive")

    def admitted():
        with reqctx.use(user):
            auth.prepare("data.resolve_evidence", INPUTS, 10.0)
        assert admitted_display((GROUP,), "interactive") == "full_text"
        assert admitted_display((), "interactive") == "none"
        assert admitted_display((SourceInput("other", GROUP.tags),), "interactive") == "none"

    contextvars.Context().run(admitted)
    assert contextvars.Context().run(admitted_display, (GROUP,), "interactive") == "none"


def test_missing_display_rejects_an_allowed_evidence_decision(monkeypatch):
    import contextvars

    auth = _recording_auth([])
    monkeypatch.setattr(auth.rights, "resolve", lambda *args, **kw: {"allowed": True, "receipt": "r"})
    user = reqctx.Context(reqctx.Principal("u", "user"), "tenant", "cell", "interactive")

    def verify():
        with reqctx.use(user), pytest.raises(Problem) as rejected:
            auth.prepare("data.resolve_evidence", INPUTS, 10.0)
        assert rejected.value.status == 503
        assert admitted_display((GROUP,), "interactive") == "none"

    contextvars.Context().run(verify)
