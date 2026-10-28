"""Owner metadata-only provenance. Binding availability is never persisted in closure."""
import json
from sqlalchemy import text
from miaoqia.acquisition.api import SourceInput, inputs
from miaoqia.sdk.derivation import fingerprint
from ..api.provenance import Closure, ReferenceInput, Unbound, ProvenanceIntegrityError, source_input, revision_proof


def put(conn, kind, key, subject_hash, closure, *, actor="writer", reason="fixed input closure"):
    closure = Closure.parse(closure.dto())
    proof = closure.proof(subject_hash)
    old = conn.execute(text("SELECT * FROM evd_requirement_sets WHERE kind=:k AND key=:i"), {"k": kind, "i": key}).mappings().one_or_none()
    if old:
        if old["subject_hash"] != subject_hash or old["closure"] != closure.dto() or old["proof_hash"] != proof:
            raise ProvenanceIntegrityError("conflicting source closure")
        return
    conn.execute(text("INSERT INTO evd_requirement_sets(kind,key,subject_hash,closure,proof_hash,actor,reason) VALUES (:k,:i,:h,CAST(:c AS jsonb),:p,:a,:r)"),
                 {"k": kind, "i": key, "h": subject_hash, "c": json.dumps(closure.dto()), "p": proof, "a": actor, "r": reason})


def checked(row, subject_hash):
    if row is None:
        raise Unbound("source closure unavailable")
    closure = Closure.parse(row["closure"])
    if row["subject_hash"] != subject_hash or closure.proof(subject_hash) != row["proof_hash"]:
        raise ProvenanceIntegrityError("subject closure mismatch")
    return closure


class Provenance:
    def __init__(self, database):
        self.database = database

    def snapshot(self, snapshot_ref, *, deadline=None):
        with self.database.tx(deadline=deadline) as conn:
            row = conn.execute(text("SELECT * FROM evd_snapshots WHERE id=:i"), {"i": snapshot_ref}).mappings().one_or_none()
        if row is None:
            raise Unbound("snapshot unavailable")
        source = source_input({"source_id": row["source_id"], "tags": row["rights_tags"]})
        digest = fingerprint({k: row[k] for k in ("id", "source_id", "source_key", "rights_tags", "normalized_hash", "raw_sha256")})
        return Closure((source,)).with_dependency("snapshot", row["id"], digest)

    def revision(self, revision_ref, snapshot_ref, *, deadline=None):
        with self.database.tx(deadline=deadline) as conn:
            row = conn.execute(text("SELECT r.snapshot_id,r.facts,s.source_id,s.source_key,s.rights_tags FROM evd_extraction_revisions r JOIN evd_snapshots s ON s.id=r.snapshot_id WHERE r.id=:i"), {"i": revision_ref}).mappings().one_or_none()
        if row is None:
            raise Unbound("revision unavailable")
        if row["snapshot_id"] != snapshot_ref:
            raise ProvenanceIntegrityError("真实解析修订与快照不一致")
        return self.snapshot(snapshot_ref, deadline=deadline).with_dependency("revision", revision_ref, revision_proof(row["snapshot_id"], row["source_id"], row["source_key"], row["rights_tags"], row["facts"]))

    def reference(self, name, version, *, deadline=None):
        with self.database.tx(deadline=deadline) as conn:
            row = conn.execute(text("SELECT content_hash,rights_tags FROM evd_reference_data WHERE name=:n AND version=:v"), {"n": name, "v": version}).mappings().one_or_none()
        if row is None:
            raise Unbound("reference unavailable")
        ref = ReferenceInput.parse({"name": name, "version": version, "content_hash": row["content_hash"], "tags": row["rights_tags"]})
        return Closure(references=(ref,)).with_dependency("reference", name+"@"+version, row["content_hash"])

    def recorded(self, kind, key, *, deadline=None):
        table, column = {"derivation": ("evd_derivations", "id"), "enrichment": ("evd_enrichments", "run_key")}[kind]
        with self.database.tx(deadline=deadline) as conn:
            subject = conn.execute(text(f"SELECT content_hash FROM {table} WHERE {column}=:i"), {"i": key}).scalar_one_or_none()
            row = conn.execute(text("SELECT * FROM evd_requirement_sets WHERE kind=:k AND key=:i"), {"k": kind, "i": key}).mappings().one_or_none()
        if subject is None:
            raise Unbound("subject unavailable")
        return checked(row, subject)

    def evidence(self, evidence_ref, *, deadline=None):
        with self.database.tx(deadline=deadline) as conn:
            row = conn.execute(text("SELECT * FROM evd_evidence_refs WHERE id=:i"), {"i": evidence_ref}).mappings().one_or_none()
        return self.snapshot(row["snapshot_id"], deadline=deadline).with_dependency("evidence", evidence_ref, fingerprint(dict(row))) if row else self.recorded("derivation", evidence_ref, deadline=deadline)

    def resolve(self, closure, *, deadline=None):
        """Resolve exact immutable reference handles each time; no positive cache."""
        closure = Closure.parse(closure.dto())
        groups = list(closure.sources)
        with self.database.tx(deadline=deadline) as conn:
            for ref in closure.references:
                row = conn.execute(text("SELECT b.*,r.content_hash AS actual_hash,r.rights_tags AS actual_tags FROM evd_provenance.bindings b LEFT JOIN evd_reference_data r USING(name,version) WHERE b.name=:n AND b.version=:v"), {"n": ref.name, "v": ref.version}).mappings().one_or_none()
                if row is None:
                    raise Unbound("reference unbound")
                if row["source_kind"] != "reference" or row["content_hash"] != ref.content_hash or tuple(sorted(row["rights_tags"])) != ref.tags or row["actual_hash"] != ref.content_hash or row["actual_tags"] is None or tuple(sorted(row["actual_tags"])) != ref.tags:
                    raise ProvenanceIntegrityError("reference binding mismatch")
                groups.append(SourceInput(row["source_id"], ref.tags))
        groups = tuple(sorted(set(groups), key=lambda s: (s.source_id, s.tags)))
        try:
            # Wire representability is deliberately checked ONLY at structured authorization.
            return inputs([s.dto() for s in groups])
        except ValueError:
            raise Unbound("source requirements unsupported by rights transport") from None
