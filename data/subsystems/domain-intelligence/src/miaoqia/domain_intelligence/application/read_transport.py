"""Closed internal data-read routes.

The route map is deliberately boring: fixed operations, closed request DTOs,
and the existing query port.  It accepts no SQL, source policy, or JSON paths.
"""
from __future__ import annotations

from datetime import datetime
import re

from miaoqia.kernel.internal_http import PreparedResponse, closed
from miaoqia.kernel import reqctx

from ..api import Basis

_ID = re.compile(r"^(?:ntc|pp|ev)_[A-Za-z0-9_-]{1,128}$")


class ReadAuthorization:
    """Request-local d0 admission/receipt collector.

    It never caches across requests.  The same exact operation and input DTOs
    are used for the final confirmation callback, including an empty input set.
    """
    def __init__(self, rights, identity, *, max_groups=64):
        self.rights, self.identity, self.max_groups = rights, identity, max_groups

    def prepare(self, operation, inputs, deadline):
        if not isinstance(inputs, (list, tuple)) or len(inputs) > 1000:
            raise ValueError("bounded authorization inputs required")
        if len(inputs) > self.max_groups:
            raise ValueError("authorization group limit exceeded")
        ctx = reqctx.current()
        ctx.validate()
        if ctx.principal.kind != "user" or not ctx.tenant or not ctx.cell or ctx.purpose != "interactive":
            raise PermissionError("active user context required")
        self.identity.resolve(deadline=deadline)
        self.rights.status(deadline=deadline)
        request = {"operation": operation, "inputs": list(inputs)}
        decision = self.rights.resolve(request, deadline=deadline)
        if not decision.get("allowed") or "receipt" not in decision:
            raise PermissionError("source groups denied")
        receipt = decision["receipt"]
        def confirm(remaining_deadline):
            self.identity.resolve(deadline=remaining_deadline)
            self.rights.status(deadline=remaining_deadline)
            result = self.rights.confirm({**request, "receipt": receipt}, deadline=remaining_deadline)
            if not result.get("allowed"):
                raise PermissionError("authorization expired or revoked")
        return confirm


def _basis(value):
    if not isinstance(value, dict) or set(value) != {"view", "recorded_cutoff"}:
        raise ValueError("invalid basis")
    view, cutoff = value["view"], value["recorded_cutoff"]
    if view == "current":
        if cutoff is not None:
            raise ValueError("current basis cannot have cutoff")
    elif view == "known_at":
        if not isinstance(cutoff, str):
            raise ValueError("known_at cutoff required")
        cutoff = datetime.fromisoformat(cutoff.replace("Z", "+00:00"))
    else:
        raise ValueError("unsupported basis")
    return Basis(view, cutoff)


def _id(value, prefix=None):
    if not isinstance(value, str) or not _ID.fullmatch(value) or (prefix and not value.startswith(prefix)):
        raise ValueError("invalid identifier")
    return value


def _fact_id(value):
    value = _id(value)
    if not (value.startswith("ntc_") or value.startswith("pp_")):
        raise ValueError("unsupported fact identifier")
    return value


def _page(page):
    return {"basis": {"view": page.basis.view, "recorded_cutoff": page.basis.recorded_cutoff.isoformat() if page.basis.recorded_cutoff else None},
            "items": list(page.items), "next_cursor": page.next_cursor}


def routes(facts, evidence, *, admit=None, before_send=None, collectors=None, collect=None):
    """Build the five fixed POST handlers for one explicitly assembled reader.

    ``collectors`` is a closed operation-to-callable map.  A collector receives
    ``(body, deadline)`` and returns the complete persisted source-group DTOs
    for that operation.  It runs before the query, so authorization cannot be
    based on a response assembled after content access.  ``collect`` is a
    convenience for callers that use one dispatcher.  The legacy body input
    remains available when no collector is supplied for compatibility with
    local callers; production assembly supplies all five collectors.
    """
    if admit is None or not callable(admit) or before_send is None or not callable(before_send):
        raise ValueError("data-read routes require final authorization confirmer")
    if collectors is not None and collect is not None:
        raise ValueError("choose collectors or collect")
    if collectors is not None:
        if not isinstance(collectors, dict):
            raise ValueError("route collectors required")
        unknown = set(collectors) - {"data.get_fact", "data.fact_history", "data.query_facts", "data.bid_result_units", "data.resolve_evidence"}
        required = {"data.get_fact", "data.fact_history", "data.query_facts", "data.bid_result_units", "data.resolve_evidence"}
        if unknown or set(collectors) != required or any(not callable(v) for v in collectors.values()):
            raise ValueError("invalid route collectors")

    def admission(operation, body, deadline):
        collector = collectors.get(operation) if collectors is not None else collect
        inputs = collector(body, deadline) if collector is not None else body
        if not isinstance(inputs, (list, tuple)):
            raise ValueError("source-group collector must return a sequence")
        hook = admit(operation, list(inputs), deadline)
        if not callable(hook):
            raise ValueError("request admission must return final confirmer")
        return hook
    def wrap(value, operation, admission_hook):
        hook = before_send(operation, value, admission_hook)
        if not callable(hook):
            raise ValueError("final authorization confirmer required")
        return PreparedResponse(value, hook)

    def get_fact(body, deadline):
        body = closed(body, {"fact_id", "basis", "version"})
        final = admission("data.get_fact", body, deadline)
        fact_id = _fact_id(body["fact_id"])
        version = body["version"]
        if version is not None and (type(version) is not int or version < 1):
            raise ValueError("invalid version")
        value = facts.get_fact(fact_id, _basis(body["basis"]), version=version)
        return wrap(None if value is None else {"basis": {"view": value.basis.view, "recorded_cutoff": value.basis.recorded_cutoff.isoformat() if value.basis.recorded_cutoff else None}, "fact": value.fact}, "data.get_fact", final)

    def history(body, deadline):
        body = closed(body, {"fact_id", "basis", "after", "limit"})
        final = admission("data.fact_history", body, deadline)
        after, limit = body["after"], body["limit"]
        if type(after) is not int or after < 0 or type(limit) is not int or not 1 <= limit <= 100:
            raise ValueError("invalid page")
        value = facts.get_fact_history(_fact_id(body["fact_id"]), _basis(body["basis"]), after=after, limit=limit)
        return wrap(_page(value), "data.fact_history", final)

    def query(body, deadline):
        body = closed(body, {"info_domain", "object_type", "basis", "after", "limit"})
        final = admission("data.query_facts", body, deadline)
        if not all(isinstance(body[k], str) and body[k] for k in ("info_domain", "object_type", "after")):
            raise ValueError("invalid query")
        if type(body["limit"]) is not int or not 1 <= body["limit"] <= 100:
            raise ValueError("invalid page")
        return wrap(_page(facts.query_facts(body["info_domain"], body["object_type"], _basis(body["basis"]), after=body["after"], limit=body["limit"])), "data.query_facts", final)

    def units(body, deadline):
        body = closed(body, {"fact_id", "basis", "version"})
        final = admission("data.bid_result_units", body, deadline)
        version = body["version"]
        if version is not None and (type(version) is not int or version < 1):
            raise ValueError("invalid version")
        value = facts.bid_result_units(_fact_id(body["fact_id"]), _basis(body["basis"]), version=version)
        return wrap(_page(value), "data.bid_result_units", final)

    def resolve(body, deadline):
        body = closed(body, {"evidence_ref"})
        final = admission("data.resolve_evidence", body, deadline)
        value = evidence.resolve(_id(body["evidence_ref"], "ev_"), deadline=deadline)
        kind = "derivation" if value is not None and value.derivation is not None else "doc"
        if value is None:
            payload = None
        else:
            payload = {"kind": kind, "evidence": {
                "id": value.id, "display_level": value.display_level,
                "snapshot_ref": value.snapshot_ref, "source_id": value.source_id,
                "source_key": value.source_key,
                "observed_at": value.observed_at.isoformat() if value.observed_at else None,
                "locator": value.locator, "snippet": value.snippet,
                "context": value.context, "derivation": value.derivation,
            }}
        return wrap(payload, "data.resolve_evidence", final)

    return {
        "/internal/v1/data/get-fact": ("data.get_fact", get_fact),
        "/internal/v1/data/fact-history": ("data.fact_history", history),
        "/internal/v1/data/query-facts": ("data.query_facts", query),
        "/internal/v1/data/bid-result-units": ("data.bid_result_units", units),
        "/internal/v1/data/resolve-evidence": ("data.resolve_evidence", resolve),
    }
