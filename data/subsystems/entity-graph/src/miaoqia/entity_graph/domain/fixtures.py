"""固定四记录夹具的纯精确解析；不做来源授权，不写入实体或领域事实。"""
from __future__ import annotations

import math
import re

from ..api import FixtureRegistry, FixtureResolution


class FixtureResolver:
    def __init__(self, registry: FixtureRegistry):
        if (registry.namespace != "fixture:r2-org" or registry.reference_name != "fixture-r2-org-registry"
                or not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", registry.reference_version)
                or not re.fullmatch(r"sha256:[0-9a-f]{64}", registry.reference_hash)
                or not isinstance(registry.records, tuple) or len(registry.records) != 4):
            raise ValueError("无效固定组织夹具投影")
        keys, ids = set(), set()
        for record in registry.records:
            if (not re.fullmatch(r"fixture:r2-org:[a-d]", record.fixture_subject_key)
                    or not re.fullmatch(r"org_[0-9A-HJKMNP-TV-Z]{26}", record.org_id)
                    or record.fixture_subject_key in keys or record.org_id in ids):
                raise ValueError("重复或无效组织夹具主体键/ID")
            keys.add(record.fixture_subject_key); ids.add(record.org_id)
            if (not isinstance(record.aliases, tuple) or len(set(record.aliases)) != len(record.aliases)
                    or any(not isinstance(n, str) or not n.startswith("【夹具】") or n != n.strip() for n in (record.name, *record.aliases))):
                raise ValueError("组织夹具名称无效")
            if (isinstance(record.confidence, bool) or not math.isfinite(record.confidence)
                    or not 0.9 <= record.confidence <= 1):
                raise ValueError("组织夹具证据置信度低于0.9")
            expected = {"/name/value", *[f"/former_names/{i}/value" for i in range(len(record.aliases))]}
            if not isinstance(record.proofs, tuple) or {p.field_path for p in record.proofs} != expected or len(record.proofs) != len(expected):
                raise ValueError("组织夹具缺少完整字段证明")
            for proof in record.proofs:
                if (not re.fullmatch(r"ogr_[0-9A-HJKMNP-TV-Z]{26}", proof.fact_id)
                        or type(proof.version) is not int or proof.version < 1
                        or not re.fullmatch(r"ev_[0-9A-HJKMNP-TV-Z]{26}", proof.evidence_ref)):
                    raise ValueError("组织夹具固定证明无效")
            if len({(p.fact_id, p.version) for p in record.proofs}) != 1:
                raise ValueError("同一登记记录必须来自同一固定事实版本")
        self.registry = registry
        self.records = tuple(sorted(registry.records, key=lambda r: r.org_id))

    def resolve(self, name: str, *, fixture_subject_key: str | None = None) -> FixtureResolution:
        if not isinstance(name, str) or (fixture_subject_key is not None and not isinstance(fixture_subject_key, str)):
            raise ValueError("组织输入必须为原文名称和可选夹具键")
        name = name.strip()
        matches = tuple(r for r in self.records if name == r.name or name in r.aliases)
        selected = None
        if fixture_subject_key is not None:
            method = "exact_strong_key"
            keyed = next((r for r in self.records if r.fixture_subject_key == fixture_subject_key.strip()), None)
            if keyed is None:
                status, candidates = "unknown", ()
            elif name and name != keyed.name and name not in keyed.aliases:
                status, candidates = "conflict", tuple(sorted({keyed.org_id, *[r.org_id for r in matches]}))
            else:
                status, candidates, selected = "resolved", (keyed.org_id,), keyed.org_id
        else:
            method = "exact_name" if any(r.name == name for r in matches) else "exact_alias"
            candidates = tuple(r.org_id for r in matches)
            status = "unknown" if not matches else "ambiguous" if len(matches) > 1 else "resolved"
            if status == "resolved":
                selected = candidates[0]
        return FixtureResolution(True, self.registry.namespace, self.registry.reference_name,
            self.registry.reference_version, self.registry.reference_hash, status, method, candidates, selected,
            min(r.confidence for r in self.records), tuple(p for r in self.records for p in r.proofs))
