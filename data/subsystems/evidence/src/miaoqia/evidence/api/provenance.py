"""Immutable input closure, separate from current binding and permission."""
from dataclasses import dataclass
from miaoqia.acquisition.api import SourceInput
from miaoqia.sdk.derivation import fingerprint

def source_input(value):
    """Provenance preserves local contracts; d0 wire bounds apply only at authorization."""
    if not isinstance(value, dict) or set(value) != {"source_id", "tags"}:
        raise ProvenanceIntegrityError("invalid source input")
    if not isinstance(value["source_id"], str) or not value["source_id"] or not isinstance(value["tags"], list) or any(not isinstance(t, str) for t in value["tags"]):
        raise ProvenanceIntegrityError("invalid source provenance")
    return SourceInput(value["source_id"], tuple(sorted(value["tags"])))

class Unbound(PermissionError):
    """Complete proof exists, but a reference has no approved source binding."""

class ProvenanceIntegrityError(ValueError):
    """Persisted subject or dependency proof is inconsistent."""

@dataclass(frozen=True)
class ReferenceInput:
    name: str
    version: str
    content_hash: str
    tags: tuple[str, ...]

    def dto(self):
        return {"name": self.name, "version": self.version, "content_hash": self.content_hash, "tags": list(self.tags)}

    @classmethod
    def parse(cls, value):
        import re
        if not isinstance(value, dict) or set(value) != {"name", "version", "content_hash", "tags"}:
            raise ProvenanceIntegrityError("invalid reference handle")
        if any(not isinstance(value[k], str) for k in ("name", "version")):
            raise ProvenanceIntegrityError("invalid reference identity")
        if not isinstance(value["content_hash"], str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", value["content_hash"]):
            raise ProvenanceIntegrityError("invalid reference hash")
        tags = source_input({"source_id": "reference", "tags": value["tags"]}).tags
        return cls(value["name"], value["version"], value["content_hash"], tags)

@dataclass(frozen=True)
class Closure:
    sources: tuple[SourceInput, ...] = ()
    references: tuple[ReferenceInput, ...] = ()
    dependencies: tuple[tuple[str, str, str], ...] = ()

    def dto(self):
        return {"schema_version": 1, "sources": [s.dto() for s in self.sources],
                "references": [r.dto() for r in self.references],
                "dependencies": [list(d) for d in self.dependencies]}

    @classmethod
    def parse(cls, value):
        import re
        if not isinstance(value, dict) or set(value) != {"schema_version", "sources", "references", "dependencies"} or type(value["schema_version"]) is not int or value["schema_version"] != 1:
            raise ProvenanceIntegrityError("invalid closure")
        if any(not isinstance(value[k], list) for k in ("sources", "references", "dependencies")):
            raise ProvenanceIntegrityError("closure bound")
        sources = tuple(source_input(s) for s in value["sources"])
        refs = tuple(ReferenceInput.parse(r) for r in value["references"])
        deps = []
        for d in value["dependencies"]:
            if not isinstance(d, list) or len(d) != 3 or any(not isinstance(x, str) or not x for x in d) or not re.fullmatch(r"sha256:[0-9a-f]{64}", d[2]):
                raise ProvenanceIntegrityError("invalid dependency")
            deps.append(tuple(d))
        return cls.merge(cls(sources, refs, tuple(deps)))

    @classmethod
    def merge(cls, *closures):
        sources, refs, deps = set(), set(), set()
        for c in closures:
            if not isinstance(c, cls):
                raise Unbound("missing input closure")
            sources.update(c.sources); refs.update(c.references); deps.update(c.dependencies)
        dependencies = {}
        for kind, key, digest in deps:
            if (kind, key) in dependencies and dependencies[(kind, key)] != digest:
                raise ProvenanceIntegrityError("conflicting dependency proof")
            dependencies[(kind, key)] = digest
        identities = {}
        for r in refs:
            key = (r.name, r.version)
            if key in identities and identities[key] != r:
                raise ProvenanceIntegrityError("conflicting reference identity")
            identities[key] = r
        return cls(tuple(sorted(sources, key=lambda s: (s.source_id, s.tags))),
                   tuple(sorted(refs, key=lambda r: (r.name, r.version, r.content_hash, r.tags))), tuple(sorted(deps)))

    def with_dependency(self, kind, key, digest):
        return self.merge(self, Closure(dependencies=((kind, key, digest),)))

    def proof(self, subject_hash):
        return fingerprint({"subject_hash": subject_hash, "closure": self.dto()})


def revision_proof(snapshot, source, source_key, tags, facts):
    """One owner-shared canonical dependency fingerprint for an immutable extraction."""
    return fingerprint({"snapshot": snapshot, "source": source, "source_key": source_key,
                        "tags": list(tags), "facts": list(facts)})
