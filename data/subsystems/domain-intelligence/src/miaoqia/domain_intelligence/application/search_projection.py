"""Reconcile current owner versions into a minimal public discovery DTO."""

from dataclasses import dataclass
from datetime import datetime, timezone
from functools import lru_cache
import re
import time

from sqlalchemy import text

from miaoqia.acquisition.api import ID as SOURCE_ID
from miaoqia.evidence.api import ProvenanceIntegrityError, Unbound
from miaoqia.kernel import outbox, payloads
from miaoqia.sdk.derivation import fingerprint

from ..api import FACT_UPSERTED
from ..api.search_projection import DiscoveryDecision, InvalidProjectionEvent, ProjectionUnavailable
from . import requirements

DOMAIN = "INF-PROC"
KINDS = {"tendering.notice": ("notice", "ntc"), "tendering.procurement_project": ("fact", "pp")}
RIGHTS_CHANGED = "acquisition.source.rights_changed.v1"
EVENT_SOURCES = {
    FACT_UPSERTED: "domain-intelligence",
    RIGHTS_CHANGED: "acquisition",
    "evidence.snapshot.created.v1": "evidence",
    "evidence.extraction.revised.v1": "evidence",
    "evidence.enrichment.completed.v1": "evidence",
    "evidence.enricher.promoted.v1": "evidence",
    "entity.entity.created.v1": "entity-graph",
    "entity.entity.updated.v1": "entity-graph",
    "entity.merge.applied.v1": "entity-graph",
    "entity.merge.reverted.v1": "entity-graph",
}
MAX_BATCH = 32
_FACT_ID = re.compile(r"(ntc|pp)_[0-9A-HJKMNP-TV-Z]{26}")
_CURRENT = """
SELECT f.id, f.object_type, f.current_version, v.content
FROM dom_facts f JOIN dom_fact_versions v ON (v.fact_id,v.version) = (f.id,f.current_version)
"""
_TYPES = tuple(KINDS)


@lru_cache
def _validator(name):
    return payloads.validator(name, {})


def _event(value):
    try:
        outbox.validate(value)
    except outbox.InvalidEnvelope as exc:
        raise InvalidProjectionEvent("invalid CloudEvent envelope") from exc
    name = value["type"]
    producer = EVENT_SOURCES.get(name)
    if producer is None or value["miaoqiazone"] != "public" or value["source"] != f"miaoqia/{producer}":
        raise InvalidProjectionEvent("unexpected projection event producer or scope")
    data = value["data"]
    if not _validator(name).is_valid(data):
        raise InvalidProjectionEvent("invalid projection event payload")
    subject_field = {FACT_UPSERTED: "fact_id", RIGHTS_CHANGED: "source_id"}.get(name)
    if subject_field and value.get("subject") != data[subject_field]:
        raise InvalidProjectionEvent("owner event subject mismatch")
    return name, data


def _kind(object_type, fact_id):
    kind, prefix = KINDS[object_type]
    match = _FACT_ID.fullmatch(fact_id)
    if match is None or match[1] != prefix:
        raise InvalidProjectionEvent("fact type and identity mismatch")
    return kind


def _sources(conn, fact):
    if fact["classification"] != "public":
        return frozenset()
    try:
        closure = requirements.read(conn, fact)
    except Unbound:
        return frozenset()
    # Pure public source input only. References and derivations with special tags
    # require a separately reviewed discovery contract.
    if closure.references or not 1 <= len(closure.sources) <= 64 or not closure.dependencies:
        return frozenset()
    if any(not SOURCE_ID.fullmatch(g.source_id) or g.tags != (f"public:{g.source_id}",) for g in closure.sources):
        return frozenset()
    sources = frozenset(g.source_id for g in closure.sources)
    if sorted(fact["rights_tags"]) != sorted(f"public:{source}" for source in sources):
        raise ProjectionUnavailable("fact metadata disagrees with its source closure")
    return sources


def _wrapped(fact, field, key="value"):
    return fact.get(field, {}).get(key)


def _metadata(fact):
    fields = {}
    identifiers = [fact["id"]]
    for value in (_wrapped(fact, "notice_code"), _wrapped(fact, "project_code")):
        if value and value not in identifiers:
            identifiers.append(value)
    fields["identifiers"] = identifiers
    for key, value in (
        ("title", _wrapped(fact, "title")),
        ("notice_type", fact.get("notice_type")),
        ("region", _wrapped(fact, "location", "code")),
    ):
        if value:
            fields[key] = value
    # The project's first_published_at is a different field from the notice's publication.
    published = _wrapped(fact, "published_at")
    if published:
        dt = datetime.fromisoformat(published.replace("Z", "+00:00"))
        if dt.tzinfo is None:
            raise ProjectionUnavailable("stored publication time lacks timezone")
        fields["published_at"] = dt.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")
    return fields


def _budget(conn, deadline):
    remaining = int((deadline - time.monotonic()) * 1000)
    if remaining <= 0:
        raise ProjectionUnavailable("projection deadline")
    conn.execute(
        text("SELECT set_config('statement_timeout', :ms, true), "
             "set_config('idle_in_transaction_session_timeout', :ms, true)"),
        {"ms": str(remaining)},
    )


@dataclass
class Prepared:
    mutations: list[dict]
    owner: "Projection"
    versions: dict
    revisions: dict
    source_ids: frozenset[str]
    decision: DiscoveryDecision | None
    next_after: str | None = None

    def confirm(self, *, deadline):
        if self.decision is not None:
            self.owner.confirm(self, deadline=deadline)


class Projection:
    def __init__(self, database, policy, *, notice_chunks=None):
        self.database = database
        self.policy = policy
        self.notice_chunks = notice_chunks

    @property
    def page_size(self):
        # Opening one normalized notice can expand a fact into twelve chunks.
        return 1 if self.notice_chunks is not None else MAX_BATCH

    def _decide(self, source_ids, deadline):
        decision = self.policy.resolve(source_ids, deadline=deadline)
        if (
            not isinstance(decision, DiscoveryDecision)
            or type(decision.epoch) is not int or decision.epoch < 0
            or not isinstance(decision.approved, frozenset)
            or not decision.approved <= source_ids
        ):
            raise ProjectionUnavailable("invalid discovery policy decision")
        return decision

    def prepare(self, event, *, deadline, after=""):
        name, data = _event(event)
        if after and (name != RIGHTS_CHANGED or not _FACT_ID.fullmatch(after)):
            raise InvalidProjectionEvent("invalid source refresh continuation")
        if name not in (FACT_UPSERTED, RIGHTS_CHANGED):
            # These registered event families do not own notice/project revisions.
            return Prepared([], self, {}, {}, frozenset(), None)
        with self.database.tx() as conn:
            self._lock(conn, deadline)
            if name == FACT_UPSERTED:
                return self._prepare(conn, self._for_fact(conn, data), deadline)
            source_id = data["source_id"]
            trigger = self._decide(frozenset({source_id}), deadline)
            if trigger.epoch < data["rights_version"]:
                raise ProjectionUnavailable("current rights have not caught up with the event")
            page_size = self.page_size
            rows = self._for_source(conn, source_id, after=after, include_new=source_id in trigger.approved)
            prepared = self._prepare(conn, rows[:page_size], deadline, trigger=(source_id, trigger))
            if len(rows) > page_size:
                prepared.next_after = rows[page_size - 1]["id"]
            return prepared

    def prepare_snapshot_page(self, handles, *, deadline):
        if not isinstance(handles, list) or not 1 <= len(handles) <= self.page_size:
            raise ValueError("snapshot page requires a bounded list of fixed handles")
        expected = {}
        for handle in handles:
            if not isinstance(handle, dict) or set(handle) != {"id", "version"}:
                raise ValueError("invalid snapshot handle")
            fact_id, version = handle["id"], handle["version"]
            if (not isinstance(fact_id, str) or not _FACT_ID.fullmatch(fact_id)
                    or type(version) is not int or not 1 <= version < 2**31 or fact_id in expected):
                raise ValueError("invalid or repeated snapshot handle")
            expected[fact_id] = version
        with self.database.tx() as conn:
            self._lock(conn, deadline)
            rows = conn.execute(
                text(_CURRENT + " WHERE f.id=ANY(:ids) AND f.info_domain=:domain "
                     'AND f.object_type=ANY(:types) ORDER BY f.id COLLATE "C"'),
                {"ids": sorted(expected), "domain": DOMAIN, "types": list(_TYPES)},
            ).mappings().all()
            if {r["id"]: r["current_version"] for r in rows} != expected:
                raise ProjectionUnavailable("snapshot page no longer matches the current fixed versions")
            return self._prepare(conn, rows, deadline)

    def _lock(self, conn, deadline):
        _budget(conn, deadline)
        # Serialize exported revisions across owner processes. A rights read
        # performed before another reconciliation cannot overwrite it.
        conn.execute(text("SELECT pg_advisory_xact_lock(hashtext('dom.public-search-projection'))"))
        _budget(conn, deadline)

    def _for_fact(self, conn, data):
        fact_id = data["fact_id"]
        supported = data["info_domain"] == DOMAIN and data["object_type"] in KINDS
        if supported:
            _kind(data["object_type"], fact_id)
        fixed = conn.execute(
            text("""SELECT v.snapshot_ref,v.revision_ref,v.schema_version,v.changed_fields,v.rights_tags
                    FROM dom_fact_versions v WHERE v.fact_id=:id AND v.version=:version"""),
            {"id": fact_id, "version": data["fact_version"]},
        ).mappings().one_or_none()
        row = conn.execute(
            text(_CURRENT + " WHERE f.id=:id AND f.info_domain=:domain AND f.object_type=:type"),
            {"id": fact_id, "domain": data["info_domain"], "type": data["object_type"]},
        ).mappings().one_or_none()
        if row is None or fixed is None:
            raise ProjectionUnavailable("fact referenced by event is not available")
        if any(data[key] != value for key, value in fixed.items()):
            raise InvalidProjectionEvent("event does not match the fixed fact")
        if row["current_version"] > data["fact_version"]:
            return []
        if row["current_version"] != data["fact_version"]:
            raise ProjectionUnavailable("current version has not caught up with the event")
        return [row] if supported else []

    def _for_source(self, conn, source_id, *, after, include_new):
        return conn.execute(text(_CURRENT + """
          WHERE f.id IN (
              SELECT fact_id FROM dom_search_projection WHERE source_ids @> ARRAY[:source]::text[]
              UNION
              SELECT r.fact_id FROM dom_version_requirements r
              JOIN dom_facts cf ON (cf.id,cf.current_version)=(r.fact_id,r.version)
              WHERE :include_new AND (r.closure->'sources') @> jsonb_build_array(jsonb_build_object('source_id', :source))
          )
          AND f.info_domain=:domain AND f.object_type=ANY(:types)
          AND f.id COLLATE "C" > :after
          ORDER BY f.id COLLATE "C" LIMIT :limit
        """), {"source": source_id, "include_new": include_new, "domain": DOMAIN,
               "after": after, "types": list(_TYPES), "limit": self.page_size + 1}).mappings().all()

    def _prepare(self, conn, rows, deadline, *, trigger=None):
        versions, candidates = {}, []
        for row in rows:
            fact = row["content"]
            fact_id, version = row["id"], row["current_version"]
            if fact["id"] != fact_id or fact["version"] != version or fact["info_domain"] != DOMAIN:
                raise ProjectionUnavailable("fixed fact identity mismatch")
            kind = _kind(row["object_type"], fact_id)
            versions[fact_id] = version
            try:
                sources = _sources(conn, fact)
            except ProvenanceIntegrityError as exc:
                raise ProjectionUnavailable("fixed source closure is inconsistent") from exc
            candidates.append((fact, kind, sources))
        source_ids = frozenset().union(*(groups for _, _, groups in candidates))
        if trigger is not None:
            source_id, previous = trigger
            source_ids |= {source_id}
        decision = self._decide(source_ids, deadline) if rows or trigger is not None else None
        if trigger is not None and (
            decision.epoch != previous.epoch or decision.approved & {source_id} != previous.approved
        ):
            raise ProjectionUnavailable("source rights changed during source refresh")
        mutations, revisions = [], {}
        for fact, kind, sources in candidates:
            live = bool(sources and sources <= decision.approved)
            projected = self._record(conn, fact, kind, sources, live, deadline)
            if projected:
                mutations.extend(projected)
                revisions[fact["id"]] = projected[0]["revision"]
        _budget(conn, deadline)
        return Prepared(mutations, self, versions, revisions, source_ids, decision)

    def _record(self, conn, fact, kind, sources, live, deadline):
        fact_id = fact["id"]
        old = conn.execute(text("SELECT * FROM dom_search_projection WHERE fact_id=:id"),
                           {"id": fact_id}).mappings().one_or_none()
        if not live and old is None:
            return []
        version = fact["version"] if live else old["public_version"]
        mutation = {
            "ref": {"kind": kind, "domain": DOMAIN, "id": fact_id, "version": str(version)},
            "withdrawn": not live,
        }
        if live:
            mutation.update(_metadata(fact))
        texts = ()
        if live and kind == "notice" and self.notice_chunks is not None:
            document = self.notice_chunks.read(
                fact["snapshot_ref"], lambda groups: bool(groups) and all(
                    group.source_id in sources and group.tags == (f"public:{group.source_id}",) for group in groups
                ), deadline=deadline,
            )
            if document is None:
                raise ProjectionUnavailable("approved notice chunk snapshot unavailable")
            texts = tuple(span.text for span in document.spans)
        chunk_slots = max(old["chunk_slots"] if old else 0, len(texts))
        chunk_mutations = []
        for ordinal in range(chunk_slots):
            piece = {"ref": {**mutation["ref"], "kind": "chunk", "id": f"{fact_id}~{ordinal:02d}"},
                     "withdrawn": ordinal >= len(texts)}
            if not piece["withdrawn"]:
                piece.update({k: mutation[k] for k in ("identifiers", "region", "notice_type", "published_at")
                              if k in mutation})
                piece["identifiers"] = [*piece["identifiers"], piece["ref"]["id"]]
                piece["text"] = texts[ordinal]
            chunk_mutations.append(piece)
        digest = fingerprint([mutation, *chunk_mutations]) if chunk_slots else fingerprint(mutation)
        revision = old["revision"] if old else 0
        if old is None or old["digest"] != digest:
            revision += 1
            conn.execute(text("""
                INSERT INTO dom_search_projection(fact_id,public_version,revision,digest,source_ids,chunk_slots)
                VALUES(:id,:version,:revision,:digest,:sources,:slots)
                ON CONFLICT(fact_id) DO UPDATE SET public_version=excluded.public_version,
                    revision=excluded.revision,digest=excluded.digest,source_ids=excluded.source_ids,
                    chunk_slots=excluded.chunk_slots
            """), {"id": fact_id, "version": version, "revision": revision,
                    "digest": digest, "sources": sorted(sources) if live else old["source_ids"], "slots": chunk_slots})
        return [{**row, "revision": revision} for row in (mutation, *chunk_mutations)]

    def confirm(self, prepared, *, deadline):
        if prepared.versions:
            with self.database.tx(deadline=deadline) as conn:
                rows = conn.execute(text("""
                  SELECT f.id, f.current_version, p.revision
                  FROM dom_facts f LEFT JOIN dom_search_projection p ON p.fact_id=f.id
                  WHERE f.id=ANY(:ids)
                """), {"ids": list(prepared.versions)}).all()
            if {row.id: row.current_version for row in rows} != prepared.versions or any(
                row.revision != prepared.revisions.get(row.id) for row in rows
            ):
                raise ProjectionUnavailable("current fact or export revision changed during export")
        if self._decide(prepared.source_ids, deadline) != prepared.decision:
            raise ProjectionUnavailable("current discovery rights changed during export")
