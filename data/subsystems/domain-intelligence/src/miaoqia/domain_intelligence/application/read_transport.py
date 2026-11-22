"""Internal data-api endpoints and their request-local read admission."""
from __future__ import annotations

from contextvars import ContextVar
from datetime import datetime
import re

from miaoqia.acquisition.api import DISPLAYS, inputs as source_inputs
from miaoqia.evidence.api import Unbound
from miaoqia.evidence.api.chunks import MAX_NOTICE_CHUNKS
from miaoqia.kernel import reqctx
from miaoqia.kernel.internal_http import INTERACTIVE, SERVICE_PURPOSES, PreparedResponse, Problem, closed

from ..api import Basis

_FACT_ID = re.compile(r"(?:ntc|pp)_[A-Za-z0-9_-]{1,128}")
_NOTICE_ID = re.compile(r"ntc_[0-9A-HJKMNP-TV-Z]{26}")
_ORG_ID = re.compile(r"org_[0-9A-HJKMNP-TV-Z]{26}")
_EVIDENCE_ID = re.compile(r"ev_[A-Za-z0-9_-]{1,128}")
_INSTANT = re.compile(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})", re.ASCII | re.IGNORECASE)
_MAX_INT64 = (1 << 63) - 1
_GET_PUBLIC_ORGANIZATION = "data.get_public_organization"
RIGHTS_OPERATIONS = {
    "data.get_fact": "fact_read", "data.fact_history": "fact_read", "data.query_facts": "fact_read",
    "data.bid_result_units": "fact_read", "data.resolve_evidence": "evidence_resolve",
    "data.get_notice_chunk": "evidence_resolve",
    _GET_PUBLIC_ORGANIZATION: "evidence_resolve",
}

_admitted = ContextVar("miaoqia_admitted_evidence", default=(frozenset(), "none"))


def admitted_display(groups, purpose):
    """Only evidence from the groups explicitly admitted in this request can display."""
    admitted, display = _admitted.get()
    if not groups or not set(groups) <= admitted:
        return "none"
    return display


class ReadAuthorization:
    """Confirm the same operation, caller, exact source inputs and display."""

    def __init__(self, rights, identity):
        self.rights, self.identity = rights, identity

    def prepare(self, operation, inputs, deadline):
        _admitted.set((frozenset(), "none"))
        ctx = reqctx.current()
        ctx.validate()
        if not ctx.tenant or not ctx.cell:
            raise PermissionError("tenant context required")
        if ctx.purpose == INTERACTIVE:
            if ctx.principal.kind != "user":
                raise PermissionError("active user context required")
        elif ctx.purpose in SERVICE_PURPOSES:
            if ctx.principal.kind != "service" or not ctx.principal.id.startswith("svc:"):
                raise PermissionError("delegated service context required")
        else:
            raise PermissionError("unsupported purpose")

        def current(remaining_deadline):
            if ctx.purpose == INTERACTIVE:
                self.identity.resolve(deadline=remaining_deadline)
            self.rights.status(deadline=remaining_deadline)

        if operation not in RIGHTS_OPERATIONS:
            raise ValueError("unsupported data-read operation")
        current(deadline)
        if not inputs:
            return current

        try:
            groups = source_inputs(inputs)
        except ValueError:
            raise Problem(503) from None
        request = {"operation": RIGHTS_OPERATIONS[operation], "inputs": [source.dto() for source in groups]}
        decision = self.rights.resolve(request, deadline=deadline)
        if decision.get("allowed") is not True:
            raise PermissionError("source groups denied")
        display = decision.get("display")
        if "receipt" not in decision or display not in DISPLAYS[1:]:
            raise Problem(503)
        if operation == _GET_PUBLIC_ORGANIZATION:
            if display != "full_text":
                raise PermissionError("full public organization proof access required")
        elif RIGHTS_OPERATIONS[operation] == "evidence_resolve":
            _admitted.set((frozenset(groups), display))

        def confirm(remaining_deadline):
            current(remaining_deadline)
            result = self.rights.confirm({**request, "receipt": decision["receipt"]}, deadline=remaining_deadline)
            if result.get("allowed") is not True or result.get("display") != display:
                raise PermissionError("authorization expired or revoked")

        return confirm


def _basis(value):
    value = closed(value, {"view", "recorded_cutoff"})
    view, cutoff = value["view"], value["recorded_cutoff"]
    if view == "known_at":
        if not isinstance(cutoff, str) or not _INSTANT.fullmatch(cutoff):
            raise ValueError("known_at cutoff required")
        cutoff = datetime.fromisoformat(cutoff.upper().replace("Z", "+00:00"))
    return Basis(view, cutoff)


def _basis_dto(basis):
    return {"view": basis.view, "recorded_cutoff": basis.recorded_cutoff.isoformat() if basis.recorded_cutoff else None}


def _identifier(value, pattern):
    if not isinstance(value, str) or not pattern.fullmatch(value):
        raise ValueError("invalid identifier")
    return value


def _bounded(value, minimum, maximum):
    if type(value) is not int or not minimum <= value <= maximum:
        raise ValueError("integer out of bounds")
    return value


def _fact_request(body):
    body = closed(body, {"fact_id", "basis", "version"})
    version = body["version"]
    return (_identifier(body["fact_id"], _FACT_ID), _basis(body["basis"]),
            None if version is None else _bounded(version, 1, _MAX_INT64))


def _page(page):
    return {"basis": _basis_dto(page.basis), "items": list(page.items), "next_cursor": page.next_cursor}


class _NonPublicOrganization(Exception):
    pass


def _public_organization(bind, admit):
    operation = _GET_PUBLIC_ORGANIZATION

    def read(body, deadline):
        body = closed(body, {"org_id", "version"})
        org_id = _identifier(body["org_id"], _ORG_ID)
        version = _bounded(body["version"], 1, _MAX_INT64)
        request_confirmation = admit(operation, [], deadline)
        admitted_groups = ()
        source_confirmation = None

        def select(read_deadline):
            def authorize(caller, owner_operation, groups):
                nonlocal admitted_groups, source_confirmation
                if caller != reqctx.current() or owner_operation != "get_entity":
                    raise PermissionError("public organization read scope required")
                if not groups:
                    return lambda: True  # The request already passed its separate admission.
                if any(group.tags != (f"public:{group.source_id}",) for group in groups):
                    raise _NonPublicOrganization
                if source_confirmation is None:
                    admitted_groups = groups
                    source_confirmation = admit(operation, [group.dto() for group in groups], read_deadline)
                elif admitted_groups != groups:
                    raise Problem(503)

                def confirm():
                    source_confirmation(read_deadline)
                    return True

                return confirm

            try:
                return bind(authorize, read_deadline).get_entity(org_id, version=version, require_current=True)
            except _NonPublicOrganization:
                return None
            except (Problem, PermissionError):
                raise
            except Exception:
                raise Problem(503) from None

        result = select(deadline)
        if result is None or result.organization.identifier.namespace != "cn-uscc":
            # A selected head might cease to be current during proof. Reconfirm any grant already used.
            return PreparedResponse(None, source_confirmation or request_confirmation)
        organization = result.organization
        if source_confirmation is None or (organization.org_id, organization.version) != (org_id, version):
            raise Problem(503)

        def confirm_current(final_deadline):
            if select(final_deadline) != result:
                raise Problem(503)
            source_confirmation(final_deadline)

        return PreparedResponse({
            "org_id": org_id, "version": version, "name": organization.name, "uscc": organization.identifier.value,
            "rights_tags": sorted({group.tags[0] for group in admitted_groups}),
        }, confirm_current)

    return read


def routes(facts, evidence, *, admit, evidence_groups, notice_chunks=None, bind_public_organizations=None):
    """Authorize a frozen fact DTO or a fixed evidence handle, then confirm before sending."""

    def prepare_fact(operation, select, dto, deadline):
        try:
            value, groups = facts.prepare_read(select, deadline=deadline)
        except (ValueError, TypeError):
            # After request validation these errors originate in owner data/closure.
            raise Problem(503) from None
        confirm = admit(operation, groups, deadline)
        return PreparedResponse(dto(value), confirm)

    def get_fact(body, deadline):
        fact_id, basis, version = _fact_request(body)
        return prepare_fact("data.get_fact",
                            lambda query: query.get_fact(fact_id, basis, version=version, deadline=deadline),
                            lambda value: None if value is None else {"basis": _basis_dto(value.basis), "fact": value.fact},
                            deadline)

    def history(body, deadline):
        body = closed(body, {"fact_id", "basis", "after", "limit"})
        fact_id, basis = _identifier(body["fact_id"], _FACT_ID), _basis(body["basis"])
        after, limit = _bounded(body["after"], 0, _MAX_INT64), _bounded(body["limit"], 1, 100)
        return prepare_fact("data.fact_history",
                            lambda query: query.get_fact_history(fact_id, basis, after=after, limit=limit, deadline=deadline),
                            _page, deadline)

    def query_facts(body, deadline):
        body = closed(body, {"info_domain", "object_type", "basis", "after", "limit"})
        domain, object_type, after = body["info_domain"], body["object_type"], body["after"]
        if not all(isinstance(v, str) and v for v in (domain, object_type)) or not isinstance(after, str):
            raise ValueError("invalid query")
        basis, limit = _basis(body["basis"]), _bounded(body["limit"], 1, 100)
        return prepare_fact("data.query_facts",
                            lambda query: query.query_facts(domain, object_type, basis, after=after, limit=limit, deadline=deadline),
                            _page, deadline)

    def units(body, deadline):
        fact_id, basis, version = _fact_request(body)
        return prepare_fact("data.bid_result_units",
                            lambda query: query.bid_result_units(fact_id, basis, version=version, deadline=deadline),
                            _page, deadline)

    def resolve(body, deadline):
        ref = _identifier(closed(body, {"evidence_ref"})["evidence_ref"], _EVIDENCE_ID)
        token = _admitted.set((frozenset(), "none"))
        try:
            try:
                groups = evidence_groups(ref, deadline)
            except Unbound:
                groups = []
            confirm = admit("data.resolve_evidence", groups, deadline)
            value = evidence.resolve(ref, deadline=deadline) if groups else None
            if value is None:
                return PreparedResponse(None, confirm)
            _, display = _admitted.get()
            if (value.id != ref or value.derivation is not None or value.display_level != display
                    or value.source_id not in {group["source_id"] for group in groups}):
                raise Problem(503)
            return PreparedResponse({"kind": "doc", "evidence": {
                "id": value.id, "display_level": value.display_level,
                "snapshot_ref": value.snapshot_ref, "source_id": value.source_id,
                "source_key": value.source_key, "observed_at": value.observed_at.isoformat(),
                "locator": value.locator, "snippet": value.snippet,
                "context": value.context, "derivation": None,
            }}, confirm)
        except (ValueError, TypeError, AttributeError):
            raise Problem(503) from None
        finally:
            _admitted.reset(token)

    def chunk(body, deadline):
        from .search_projection import _metadata

        operation = "data.get_notice_chunk"
        body = closed(body, {"fact_id", "version", "ordinal"})
        fact_id = _identifier(body["fact_id"], _NOTICE_ID)
        version = _bounded(body["version"], 1, _MAX_INT64)
        ordinal = _bounded(body["ordinal"], 0, MAX_NOTICE_CHUNKS - 1)

        def select(query):
            value = query.get_fact(fact_id, Basis("current"), deadline=deadline)
            if value is not None and value.fact["version"] == version:
                return value
            return None

        token = _admitted.set((frozenset(), "none"))
        try:
            value, groups = facts.prepare_read(select, deadline=deadline)
            confirm = admit(operation, groups if value else [], deadline)
            if value is None:
                return PreparedResponse(None, confirm)
            fact = value.fact
            if fact["info_domain"] != "INF-PROC" or fact["classification"] != "public":
                return PreparedResponse(None, confirm)
            fixed_groups = source_inputs(groups)
            tags = tuple(f"public:{source.source_id}" for source in fixed_groups)
            if (any(source.tags != (tag,) for source, tag in zip(fixed_groups, tags))
                    or sorted(fact["rights_tags"]) != sorted(tags)):
                return PreparedResponse(None, confirm)

            result = notice_chunks.read(
                fact["snapshot_ref"], lambda sources: admitted_display(sources, reqctx.current().purpose) == "full_text",
                deadline=deadline,
            )
            if result is None or ordinal >= len(result.spans):
                return PreparedResponse(None, confirm)
            span = result.spans[ordinal]

            def confirm_current(final_deadline):
                def check(query):
                    return query.get_fact(fact_id, Basis("current"), deadline=final_deadline)

                head, current_groups = facts.prepare_read(check, deadline=final_deadline)
                if head is None or head.fact != fact or current_groups != groups:
                    raise Problem(503)
                confirm(final_deadline)

            return PreparedResponse({
                "fact_id": fact_id, "version": version, "ordinal": ordinal,
                "snapshot_ref": result.snapshot, "locator": span.locator(), "text": span.text,
                "rights_tags": fact["rights_tags"], "metadata": _metadata(fact),
            }, confirm_current)
        except (ValueError, TypeError, KeyError, AttributeError):
            raise Problem(503) from None
        finally:
            _admitted.reset(token)

    result = {
        "/internal/v1/data/get-fact": ("data.get_fact", get_fact),
        "/internal/v1/data/fact-history": ("data.fact_history", history),
        "/internal/v1/data/query-facts": ("data.query_facts", query_facts),
        "/internal/v1/data/bid-result-units": ("data.bid_result_units", units),
        "/internal/v1/data/resolve-evidence": ("data.resolve_evidence", resolve),
    }
    if notice_chunks is not None:
        result["/internal/v1/data/get-notice-chunk"] = ("data.get_notice_chunk", chunk)
    if bind_public_organizations is not None:
        result["/internal/v1/data/get-public-organization"] = (
            _GET_PUBLIC_ORGANIZATION, _public_organization(bind_public_organizations, admit),
        )
    return result
