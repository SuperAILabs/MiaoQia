"""Rebuild a pinned projection from owner ports for every request, before matching."""
from __future__ import annotations

from miaoqia.domain_intelligence.api import Basis
from miaoqia.evidence.api import Closure, ProvenanceIntegrityError, Unbound
from miaoqia.sdk.corpus_source import fixture_manifest
from miaoqia.sdk.derivation import fingerprint
from ..api import FixtureFieldProof, FixtureOrganization, FixtureRegistry
from ..domain.fixtures import FixtureResolver

REFERENCE = "fixture-r2-org-registry"


class TrustedFixtureResolver:
    def __init__(self, *, manifest_path, handles, facts, extractions, evidence, provenance, references):
        # These are explicit deployment-owned ports, never values supplied by a query.
        self.manifest = fixture_manifest(manifest_path)
        self.handles = dict(handles)
        self.facts, self.extractions, self.evidence = facts, extractions, evidence
        self.provenance, self.references = provenance, references
        if set(self.handles) != set(self.manifest["samples"]) or len(self.handles) != 4:
            raise ValueError("four fixed registry handles required")
        if len(set(self.handles.values())) != 4 or any(
            not isinstance(h, tuple) or len(h) != 2 or not isinstance(h[0], str)
            or type(h[1]) is not int or h[1] < 1 for h in self.handles.values()
        ):
            raise ValueError("distinct fixed fact id/version handles required")

    def requirements(self):
        """Metadata-only owner proof, including unselected records and reference."""
        return Closure.merge(*(self.facts.requirements(*h) for h in self.handles.values()),
                             self.provenance.reference(REFERENCE, self.manifest["version"]))

    def resolve(self, name, *, fixture_subject_key=None):
        # Admission is before lookup, including unknown names and missing/corrupt inputs.
        self.references.require_full_requirements(Closure())
        closure = self.requirements()
        self.references.require_full_requirements(closure)
        reference = self.references.reference(REFERENCE, self.manifest["version"])
        if reference is None:
            raise Unbound("registry reference unavailable")
        digest = fingerprint(self.manifest)
        if reference["hash"] != digest or reference["content"] != self.manifest:
            raise ProvenanceIntegrityError("pinned registry manifest mismatch")
        expected_ref = next((r for r in closure.references if r.name == REFERENCE), None)
        if expected_ref is None or expected_ref.content_hash != digest or expected_ref.tags != tuple(sorted(reference["rights_tags"])):
            raise ProvenanceIntegrityError("registry reference proof mismatch")
        records = []
        for record in self.manifest["data"]["records"]:
            sample = record["sample"]
            source = self.manifest["samples"][sample]
            fact_id, version = self.handles[sample]
            selected = self.facts.get_fact(fact_id, Basis("current"), version=version)
            if selected is None:
                raise Unbound("registry fact unavailable")
            fact = selected.fact
            if ("fact", fact_id + ":" + str(version), fingerprint(fact)) not in closure.dependencies:
                raise ProvenanceIntegrityError("registry fact owner hash mismatch")
            revision = self.extractions.revision(fact["revision_ref"])
            tags = {source["rights_tag"], "corpus:" + sample}
            if (fact["id"] != fact_id or fact["version"] != version or fact.get("record_scope") != "fixture"
                    or fact["info_domain"] != "INF-ORG" or fact["schema_version"] != "1.2.0"
                    or revision is None or revision.pack_id != "INF-ORG" or revision.pack_version != "1.1.0"
                    or revision.quality_status != "ok" or revision.source_id != source["source_id"]
                    or revision.source_key != source["source_key"] or revision.snapshot_ref != fact["snapshot_ref"]
                    or set(revision.rights_tags) != tags or set(fact["rights_tags"]) != tags
                    or len(revision.facts) != 1):
                raise ProvenanceIntegrityError("registry source identity mismatch")
            original = revision.facts[0]
            if any(fact.get(k) != v for k, v in original.items()):
                raise ProvenanceIntegrityError("registry extraction mismatch")
            snapshot = self.evidence.snapshot(fact["snapshot_ref"])
            if snapshot is None:
                raise Unbound("registry raw unavailable")
            if (snapshot.source_id != source["source_id"] or snapshot.source_key != source["source_key"]
                    or set(snapshot.rights_tags) != tags or snapshot.raw_sha256 != "sha256:" + source["raw_sha256"]):
                raise ProvenanceIntegrityError("registry raw binding mismatch")
            closures = [closure, self.provenance.revision(revision.id, revision.snapshot_ref)]
            fields = [("/name/value", fact["name"], record["name"])] + [
                (f"/former_names/{i}/value", field, alias)
                for i, (field, alias) in enumerate(zip(fact["former_names"], record["aliases"]))]
            if len(fact["former_names"]) != len(record["aliases"]):
                raise ProvenanceIntegrityError("registry alias count mismatch")
            proofs, confidence = [], []
            for path, field, expected in fields:
                ref = field["prov"]["evidence_ref"]
                resolved = self.evidence.resolve(ref)
                if (field["value"] != expected or resolved is None or resolved.display_level != "full_text"
                        or resolved.snapshot_ref != snapshot.id or resolved.source_id != snapshot.source_id
                        or resolved.source_key != snapshot.source_key or resolved.snippet != expected):
                    raise ProvenanceIntegrityError("registry field evidence mismatch")
                proofs.append(FixtureFieldProof(fact_id, version, path, ref))
                confidence.append(field["prov"]["confidence"])
                closures.append(self.provenance.evidence(ref))
            closure = Closure.merge(*closures)
            records.append(FixtureOrganization(record["fixture_subject_key"], record["org_id"], record["name"],
                                                tuple(record["aliases"]), tuple(proofs), min(confidence)))
        self.references.require_full_requirements(closure)
        registry = FixtureRegistry(self.manifest["namespace"], REFERENCE, self.manifest["version"], digest, tuple(records))
        return FixtureResolver(registry).resolve(name, fixture_subject_key=fixture_subject_key)
