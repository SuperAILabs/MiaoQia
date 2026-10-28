"""Domain-owned fixed-version closure; never infer sources from aggregate tags."""
import json
from sqlalchemy import text
from miaoqia.evidence.api.provenance import Closure, Unbound, ProvenanceIntegrityError, source_input, revision_proof
from miaoqia.sdk.derivation import fingerprint


def base(revision):
    return Closure((source_input({"source_id": revision.source_id, "tags": list(revision.rights_tags)}),)).with_dependency(
        "revision", revision.id, revision_proof(revision.snapshot_ref, revision.source_id, revision.source_key, revision.rights_tags, revision.facts))


def read(conn, fact):
    if conn.execute(text("SELECT to_regclass('public.dom_version_requirements')")).scalar_one() is None:
        raise Unbound("fact closure schema unavailable")
    row = conn.execute(text("SELECT * FROM dom_version_requirements WHERE fact_id=:i AND version=:v"),
                       {"i": fact["id"], "v": fact["version"]}).mappings().one_or_none()
    if row is None:
        raise Unbound("fact closure unavailable")
    closure = Closure.parse(row["closure"])
    digest = fingerprint(fact)
    if row["subject_hash"] != digest or row["proof_hash"] != closure.proof(digest):
        raise ProvenanceIntegrityError("fact closure mismatch")
    return closure


def put(conn, fact, closure, *, actor="writer", reason="fixed input closure"):
    closure = Closure.parse(closure.dto())
    digest = fingerprint(fact)
    proof = closure.proof(digest)
    old = conn.execute(text("SELECT * FROM dom_version_requirements WHERE fact_id=:i AND version=:v"),
                       {"i": fact["id"], "v": fact["version"]}).mappings().one_or_none()
    if old:
        if old["subject_hash"] != digest or old["closure"] != closure.dto() or old["proof_hash"] != proof:
            raise ProvenanceIntegrityError("conflicting fact closure")
        return
    conn.execute(text("INSERT INTO dom_version_requirements(fact_id,version,subject_hash,closure,proof_hash,actor,reason) VALUES (:i,:v,:h,CAST(:c AS jsonb),:p,:a,:r)"),
                 {"i": fact["id"], "v": fact["version"], "h": digest, "c": json.dumps(closure.dto()), "p": proof, "a": actor, "r": reason})


def members(conn, facts):
    return Closure.merge(*(read(conn, f).with_dependency("fact", f['id']+':'+str(f['version']), fingerprint(f)) for f in facts))


def backfill(database, extractions, enrichments, handles, *, actor, reason):
    """Prove explicit fixed versions offline and append one atomic owner audit batch."""
    from miaoqia.evidence.api.audit import audit, batch
    from .facts import fact_version
    from .source import validate_source
    audit(actor, reason)
    batch(handles)
    if any(not isinstance(h, (tuple, list)) or len(h) != 2 or not isinstance(h[0], str)
           or type(h[1]) is not int or h[1] < 1 for h in handles):
        raise ValueError("fixed fact id/version handles required")
    done, active = {}, set()
    with database.tx() as conn:
        if not conn.execute(text("SELECT current_user=pg_get_userbyid(relowner) FROM pg_class WHERE oid='dom_version_requirements'::regclass")).scalar_one():
            raise PermissionError("domain owner required")
        def prove(handle):
            handle = tuple(handle)
            if handle in done:
                return done[handle][1]
            if handle in active or len(active) >= 64 or len(done) + len(active) >= 1024:
                raise ProvenanceIntegrityError("backfill dependency bound or cycle")
            fact = fact_version(conn, *handle)
            if fact is None:
                raise Unbound("fixed fact unavailable")
            active.add(handle)
            try:
                row = conn.execute(text("SELECT * FROM dom_facts WHERE id=:i"), {"i": handle[0]}).mappings().one()
                if row["object_type"] == "tendering.procurement_project":
                    deps = conn.execute(text("SELECT notice_id,notice_version FROM dom_project_dependencies WHERE project_id=:i AND project_version=:v"), {"i": handle[0], "v": handle[1]}).all()
                    refs = {(r["id"], r["version"]) for r in fact["notice_refs"]}
                    if not deps or set(deps) != refs:
                        raise ProvenanceIntegrityError("project dependency mismatch")
                    closure = Closure.merge(*(prove(d).with_dependency("fact", d[0]+":"+str(d[1]), fingerprint(fact_version(conn, *d))) for d in sorted(deps)))
                else:
                    receipts = conn.execute(text("SELECT * FROM dom_enrichment_receipts WHERE notice_id=:i AND output_version=:v AND parent_version<output_version"), {"i": handle[0], "v": handle[1]}).mappings().all()
                    if receipts:
                        if len(receipts) != 1:
                            raise ProvenanceIntegrityError("ambiguous enrichment history")
                        receipt = receipts[0]
                        result = enrichments.get(receipt["run_key"])
                        if (result is None or result["source"] != {"id": handle[0], "version": receipt["source_base_version"]}
                                or result["input_hash"] != receipt["input_hash"]
                                or result["outputs"] != receipt["outputs"]
                                or result["source_hash"] != receipt["source_hash"]
                                or result["reference_hash"] != receipt["reference_hash"]
                                or fingerprint({"fact": fact, "outputs": receipt["outputs"]}) != receipt["output_hash"]):
                            raise ProvenanceIntegrityError("enrichment receipt mismatch")
                        parent_handle = (handle[0], receipt["parent_version"])
                        parent = fact_version(conn, *parent_handle)
                        closure = Closure.merge(prove(parent_handle).with_dependency("fact", parent_handle[0]+":"+str(parent_handle[1]), fingerprint(parent)), enrichments.requirements(receipt["run_key"]))
                    else:
                        revision = extractions.revision(fact["revision_ref"])
                        from types import SimpleNamespace
                        validate_source(fact, SimpleNamespace(**row), revision)
                        closure = base(revision)
                done[handle] = (fact, closure)
                return closure
            finally:
                active.remove(handle)
        for handle in sorted(handles):
            prove(handle)
        for (fact_id, version), (fact, closure) in sorted(done.items()):
            conn.execute(text("SELECT pg_advisory_xact_lock(hashtext(:k))"), {"k": "requirements:"+fact_id+":"+str(version)})
            put(conn, fact, closure, actor=actor, reason=reason)
    return len(done)
