"""Reconcile proof-gated organization versions into public search mutations."""

from collections.abc import Callable
from dataclasses import dataclass
from datetime import timezone
from hashlib import sha256
import json
import time

from sqlalchemy import text

from miaoqia.acquisition.api import identifier
from miaoqia.kernel import reqctx
from miaoqia.sdk.derivation import fingerprint

from ..api.organizations import OrganizationProofError
from ..api.search_projection import MAX_BATCH, ProjectionUnavailable
from ..domain.organizations import check_id, check_ref, check_version
from ..infrastructure import organizations, search_projection as store
from .organizations import _confirm

_MAX_HANDLES = 100_000
_MAX_SECONDS = 60


def _remaining_ms(deadline):
    remaining = int((deadline - time.monotonic()) * 1000)
    if remaining <= 0:
        raise ProjectionUnavailable("organization projection deadline")
    return remaining


def _budget(conn, deadline):
    conn.execute(text("SELECT set_config('statement_timeout', :ms, true), "
                      "set_config('idle_in_transaction_session_timeout', :ms, true)"),
                 {"ms": str(_remaining_ms(deadline))})


def _exact_ids(ids):
    if not isinstance(ids, (list, tuple)) or not 1 <= len(ids) <= MAX_BATCH:
        raise ValueError("a bounded list of organization IDs is required")
    for org_id in ids:
        check_id(org_id, "org")
    if len(set(ids)) != len(ids):
        raise ValueError("duplicate organization IDs")
    return tuple(sorted(ids))


def _bounded(value, maximum, name):
    if type(value) is not int or not 1 <= value <= maximum:
        raise ValueError(f"{name} must be in 1..{maximum}")


@dataclass
class Prepared:
    mutations: list[dict]
    next_after: str | None
    _verify: Callable[[float], None]

    def confirm(self, *, deadline):
        self._verify(deadline)


class Projection:
    def __init__(self, authority, policy, admit_public):
        self.authority = authority
        self.database = authority.database
        self.policy = policy
        self.admit_public = admit_public

    def _request(self, deadline):
        _remaining_ms(deadline)
        caller = reqctx.current()
        if caller.tenant or caller.cell or caller.principal.kind != "service" or not caller.principal.id.startswith("svc:"):
            raise PermissionError("organization projection requires a public service principal")
        receipt = self.authority.admit("get_entity", ())
        _confirm(receipt)
        return caller, receipt

    def _decision(self, sources, deadline):
        _remaining_ms(deadline)
        decision = self.policy.resolve(sources, deadline=deadline)
        epoch, approved = getattr(decision, "epoch", None), getattr(decision, "approved", None)
        if type(epoch) is not int or epoch < 0 or not isinstance(approved, frozenset) or not approved <= sources:
            raise ProjectionUnavailable("invalid organization discovery decision")
        _remaining_ms(deadline)
        return epoch, approved

    def prepare_ids(self, ids, *, deadline):
        org_ids = _exact_ids(ids)
        return self._prepare(lambda _: org_ids, deadline=deadline)

    def prepare_registration(self, fact_id, *, deadline):
        check_id(fact_id, "ogr")
        return self._prepare(lambda conn: store.for_registration(conn, fact_id), deadline=deadline)

    def prepare_source(self, source_id, *, after="", rights_version, deadline):
        identifier(source_id)
        if after != "":
            check_id(after, "org")
        check_version(rights_version)
        return self._prepare(lambda conn: store.for_source(conn, source_id, after, MAX_BATCH + 1),
                             source=(source_id, rights_version), deadline=deadline)

    def prepare_snapshot_page(self, handles, *, deadline):
        if not isinstance(handles, list) or not 1 <= len(handles) <= MAX_BATCH:
            raise ValueError("a bounded list of fixed organization handles is required")
        expected = {}
        for handle in handles:
            if not isinstance(handle, dict) or set(handle) != {"id", "version"}:
                raise ValueError("invalid organization snapshot handle")
            org_id, version = handle["id"], handle["version"]
            check_id(org_id, "org")
            check_version(version)
            if org_id in expected:
                raise ValueError("duplicate organization snapshot handle")
            expected[org_id] = version
        return self._prepare(lambda _: tuple(sorted(expected)), expected=expected, deadline=deadline)

    def _prepare(self, select_ids, *, deadline, source=None, expected=None):
        caller, admission = self._request(deadline)
        receipts = [admission]
        checked_registrations = {}
        denied_admissions = []
        with self.database.tx() as conn:
            _budget(conn, deadline)
            # Serialize projection revisions across the worker and rebuild loaders.
            conn.execute(text("SELECT pg_advisory_xact_lock(hashtext('ent.public-search-projection'))"))
            _budget(conn, deadline)
            selected = select_ids(conn)
            org_ids = selected[:MAX_BATCH]
            next_after = org_ids[-1] if len(selected) > MAX_BATCH else None
            stored = organizations.projection_heads(conn, org_ids)
            heads = tuple((org, undisputed) for org, undisputed, _ in stored)
            if tuple(org.org_id for org, _ in heads) != org_ids:
                raise ProjectionUnavailable("current organization version unavailable")
            if expected is not None and {org.org_id: org.version for org, _ in heads} != expected:
                raise ProjectionUnavailable("snapshot page no longer matches current organization versions")

            sources = frozenset(org.source.source_id for org, _ in heads)
            if source is not None:
                sources |= {source[0]}
            decision = self._decision(sources, deadline) if sources else None
            if source is not None and decision[0] < source[1]:
                raise ProjectionUnavailable("organization source policy has not caught up with the event")

            saved_revisions = {}
            mutations = []
            for org, undisputed, old in stored:
                receipt = None
                if undisputed and org.identifier.namespace == "cn-uscc" and org.source.source_id in decision[1]:
                    ref = org.registration
                    head = self._registration_head(ref.fact_id, deadline)
                    checked_registrations[ref.fact_id] = head
                    if head == ref:
                        receipt = self.admit_public(ref, org.source.source_id, deadline=deadline)
                        if receipt is None:
                            denied_admissions.append((ref, org.source.source_id))
                        else:
                            _confirm(receipt)
                            self._prove(org)
                            receipts.append(receipt)
                mutation = self._record(conn, org, old, live=receipt is not None, deadline=deadline)
                if mutation is not None:
                    mutations.append(mutation)
                    saved_revisions[org.org_id] = mutation["revision"]
            _confirm(*receipts)
            _remaining_ms(deadline)

        def verify(confirm_deadline):
            _remaining_ms(confirm_deadline)
            if reqctx.current() != caller:
                raise PermissionError("organization projection caller changed")
            _confirm(*receipts)
            for fact_id, observed in checked_registrations.items():
                if self._registration_head(fact_id, confirm_deadline) != observed:
                    raise ProjectionUnavailable("current registration changed during export")
            for ref, source_id in denied_admissions:
                if self.admit_public(ref, source_id, deadline=confirm_deadline) is not None:
                    raise ProjectionUnavailable("organization discovery admission changed during export")
            if decision is not None and self._decision(sources, confirm_deadline) != decision:
                raise ProjectionUnavailable("current organization source rights changed during export")
            with self.database.tx(deadline=confirm_deadline) as conn:
                latest = organizations.projection_heads(conn, org_ids)
            confirmed_heads = tuple((org, undisputed) for org, undisputed, _ in latest)
            confirmed_revisions = {org.org_id: row.revision for org, _, row in latest if row is not None}
            if confirmed_heads != heads or confirmed_revisions != saved_revisions:
                raise ProjectionUnavailable("current organization or projection revision changed during export")
            _confirm(*receipts)
            _remaining_ms(confirm_deadline)

        return Prepared(mutations, next_after, verify)

    def _registration_head(self, fact_id, deadline):
        _remaining_ms(deadline)
        head = self.authority.registrations.current_registration(fact_id)
        if head is not None:
            try:
                check_ref(head)
            except ValueError as exc:
                raise ProjectionUnavailable("invalid current registration reference") from exc
            if head.fact_id != fact_id:
                raise ProjectionUnavailable("current registration reference mismatch")
        return head

    def _prove(self, org):
        try:
            self.authority._check_read(org, current=False)
        except OrganizationProofError as exc:
            raise ProjectionUnavailable("fixed organization proof is inconsistent") from exc

    @staticmethod
    def _record(conn, org, old, *, live, deadline):
        if not live and old is None:
            return None
        version = org.version if live else old.public_version
        dto = {"ref": {"kind": "entity", "domain": "INF-ORG", "id": org.org_id, "version": str(version)},
               "withdrawn": not live}
        if live:
            dto.update(title=org.name, identifiers=[org.org_id, org.identifier.value])
        digest = fingerprint(dto)
        revision = old.revision if old else 0
        if old is None or old.digest != digest:
            revision += 1
            _budget(conn, deadline)
            store.record(conn, org.org_id, version, revision, digest)
        return {**dto, "revision": revision}

    def capture(self, *, page_size=MAX_BATCH, max_handles=_MAX_HANDLES, timeout_seconds=30):
        """Freeze all committed heads. Kafka starts are captured independently by the runtime."""
        _bounded(page_size, MAX_BATCH, "page_size")
        _bounded(max_handles, _MAX_HANDLES, "max_handles")
        _bounded(timeout_seconds, _MAX_SECONDS, "timeout_seconds")
        deadline = time.monotonic() + timeout_seconds
        _, admission = self._request(deadline)
        pages, as_of, count = [], None, 0
        with self.database.tx(deadline=deadline) as conn, store.snapshot_rows(conn, limit=max_handles + 1) as rows:
            for batch in rows.partitions(page_size):
                _remaining_ms(deadline)
                page = []
                for row in batch:
                    if row.standby:
                        raise ProjectionUnavailable("organization snapshot requires primary PostgreSQL")
                    if as_of is None:
                        as_of = row.as_of.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")
                    if row.org_id is not None:
                        if row.version is None:
                            raise ProjectionUnavailable("organization has no committed version")
                        page.append({"id": row.org_id, "version": row.version})
                count += len(page)
                if count > max_handles:
                    raise ProjectionUnavailable("organization snapshot exceeds handle limit")
                if page:
                    pages.append(page)
        _confirm(admission)
        manifest = {"as_of": as_of, "handle_count": count, "pages": pages}
        # The manifest contains ASCII identifiers, timestamp and signed bigint
        # versions. JSON serialization preserves integer precision beyond 2**53.
        data = json.dumps(manifest, sort_keys=True, separators=(",", ":")).encode()
        _remaining_ms(deadline)
        return {"snapshot_id": "sha256:" + sha256(data).hexdigest(), **manifest}
