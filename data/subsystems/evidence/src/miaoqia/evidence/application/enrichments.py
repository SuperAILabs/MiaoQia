"""固定输入、不可变参考数据、可重算结果的本地登记；与domain为可重试的两阶段。"""
from __future__ import annotations

import json

from sqlalchemy import text

from miaoqia.kernel import ids, outbox, payloads, reqctx
from miaoqia.sdk.derivation import canonical, evaluate, field_input, fingerprint

from ..api import NAME, ResolvedEvidence
from .queries import load_document

LEVELS = ("none", "citation_only", "excerpt", "full_text")
REFERENCE = {"name": "test-identity-policy", "version": "1"}
TEST_CONTENT = {"non_production": True, "mode": "identity"}
RECIPES = {"test.buyer-provenance@0.1.0": "/buyer/name_raw",
           "test.winner-provenance@0.1.0": "/lots/0/bid_results/0/bidder/name_raw",
           "test.buyer-provenance@0.1.1": "/buyer/name_raw"}


def identity(inputs, references):
    if len(inputs) != 1 or references != [TEST_CONTENT]:
        raise ValueError("测试identity转换输入或参考版本错误")
    return inputs[0]


class EnrichmentStore:
    def __init__(self, database, store, *, facts, display=None, enable_test_recipe=False):
        self.database, self.store, self.facts, self.display = database, store, facts, display
        self.transforms = {"test.identity@0.1.0": identity} if enable_test_recipe else {}
        self.recipes = RECIPES if enable_test_recipe else {}

    def level(self, requirements=()):
        ctx = reqctx.current()
        ctx.validate()
        if not ctx.purpose or self.display is None:
            raise PermissionError("派生读取缺少用途或当前权利策略")
        levels = [self.display((), ctx.purpose)]  # 全局认证/租户/purpose策略先于数据匹配
        levels += [self.display(tuple(tags), ctx.purpose) for tags in requirements]
        if any(level not in LEVELS for level in levels):
            raise PermissionError("未知展示级别")
        return min(levels, key=LEVELS.index)

    def require_full(self, rights_tags):
        if self.level([rights_tags]) != "full_text":
            raise PermissionError("当前增强输入权利拒绝")

    def _put(self, value):
        digest = fingerprint(value)
        key = "enrichment/" + digest.removeprefix("sha256:") + ".json"
        self.store.put(key, canonical(value))
        return key, digest

    def _load(self, row):
        value = json.loads(self.store.get(row["object_ref"]))
        if fingerprint(value) != row["content_hash"]:
            raise ValueError("派生/参考对象被篡改")
        return value

    def register_reference(self, name, version, content, rights_tags):
        if self.level([rights_tags]) != "full_text":
            raise PermissionError("参考登记需要完整权限")
        key, digest = self._put(content)
        with self.database.tx() as conn:
            conn.execute(text("SELECT pg_advisory_xact_lock(hashtext(:k))"), {"k": "ref:" + name + ":" + version})
            old = conn.execute(text("SELECT * FROM evd_reference_data WHERE name=:n AND version=:v"),
                               {"n": name, "v": version}).mappings().one_or_none()
            if old:
                if old["content_hash"] != digest or old["rights_tags"] != list(rights_tags):
                    raise ValueError("同参考版本不得改内容或权利")
                self._load(old)
            else:
                conn.execute(text("INSERT INTO evd_reference_data VALUES (:n,:v,:o,:h,CAST(:r AS jsonb))"),
                             {"n": name, "v": version, "o": key, "h": digest, "r": json.dumps(list(rights_tags))})

    def reference(self, name, version):
        if self.level() == "none":
            return None
        with self.database.tx() as conn:
            row = conn.execute(text("SELECT * FROM evd_reference_data WHERE name=:n AND version=:v"),
                               {"n": name, "v": version}).mappings().one_or_none()
        if not row or self.level([row["rights_tags"]]) not in ("excerpt", "full_text"):
            return None
        return {"content": self._load(row), "hash": row["content_hash"], "rights_tags": row["rights_tags"]}

    def _ref(self, ref_id):
        with self.database.tx() as conn:
            row = conn.execute(text("""SELECT r.*,s.rights_tags FROM evd_evidence_refs r JOIN evd_snapshots s ON s.id=r.snapshot_id WHERE r.id=:id"""),
                               {"id": ref_id}).mappings().one_or_none()
            derived = None if row else conn.execute(text("SELECT * FROM evd_derivations WHERE id=:id"),
                                                    {"id": ref_id}).mappings().one_or_none()
        if row:
            if self.level([row["rights_tags"]]) == "none":
                raise PermissionError("派生输入权利拒绝")
            return {"id": row["id"], "snapshot_ref": row["snapshot_id"], "pipeline_version": row["pipeline_version"],
                    "locator": row["locator"], "quote_hash": row["quote_hash"]}
        if derived:
            if self.level(derived["rights_requirements"]) not in ("excerpt", "full_text"):
                raise PermissionError("派生输入不允许读取内容")
            return self._load(derived)
        return None

    def _document(self, snapshot_ref):
        with self.database.tx() as conn:
            row = conn.execute(text("SELECT * FROM evd_snapshots WHERE id=:id"), {"id": snapshot_ref}).mappings().one()
        if self.level([row["rights_tags"]]) not in ("excerpt", "full_text"):
            raise PermissionError("只引证不得打开正文")
        return load_document(self.store, row)

    def _evaluate(self, root):
        return evaluate(root, get_ref=self._ref, get_document=self._document, get_fact=self.facts,
                        get_reference=self.reference, transforms=self.transforms)

    def register(self, source, recipe):
        if self.level([source["rights_tags"]]) != "full_text":
            raise PermissionError("增强登记需要完整权限")
        if recipe not in self.recipes:
            raise ValueError("recipe未登记/未启用（测试recipe须显式启用）")
        if self.facts(source["id"], source["version"]) != source:
            raise ValueError("固定source事实不一致")
        path = self.recipes[recipe]
        value, leaf = field_input(source, path)
        reference = self.reference(**REFERENCE)
        if reference is None:
            raise ValueError("参考内容不存在或不可用")
        requirements = [source["rights_tags"], reference["rights_tags"]]
        ref = {"kind": "derivation", "transform": "test.identity@0.1.0",
               "inputs": [{"evidence_ref": leaf, "fact_ref": {"id": source["id"], "version": source["version"]}, "field_path": path}],
               "reference_data": [REFERENCE], "value_hash": fingerprint(value)}
        key = fingerprint({"source": [source["id"], source["version"]], "recipe": recipe,
                           "reference_hash": reference["hash"], "derivation": ref, "rights": requirements})
        with self.database.tx() as conn:
            revision = conn.execute(text("SELECT snapshot_id,facts FROM evd_extraction_revisions WHERE id=:r"),
                                    {"r": source["revision_ref"]}).mappings().one_or_none()
            if not revision or revision["snapshot_id"] != source["snapshot_ref"]:
                raise ValueError("真实解析修订与快照不一致")
            if len(revision["facts"]) != 1:
                raise ValueError("c0测试recipe仅支持单事实解析修订")
            conn.execute(text("SELECT pg_advisory_xact_lock(hashtext(:k))"), {"k": "enrich:" + key})
            old = conn.execute(text("SELECT run_key FROM evd_enrichments WHERE run_key=:k"), {"k": key}).first()
            if old:
                return key
            deriv_digest = fingerprint({"ref": ref, "rights": requirements})
            conn.execute(text("SELECT pg_advisory_xact_lock(hashtext(:k))"), {"k": "derive:" + deriv_digest})
            derived = conn.execute(text("SELECT * FROM evd_derivations WHERE fingerprint=:h"), {"h": deriv_digest}).mappings().one_or_none()
            if derived:
                ref = self._load(derived)
            else:
                ref["id"] = ids.new("ev")
                # 临时根通过同一SDK求值器验证；所有子输入仍由权威登记读取。
                actual_get = self._ref
                result = evaluate(ref["id"], get_ref=lambda i: ref if i == ref["id"] else actual_get(i),
                                  get_document=self._document, get_fact=self.facts, get_reference=self.reference,
                                  transforms=self.transforms)
                if result != value:
                    raise ValueError("重算输出不一致")
                object_ref, content_hash = self._put(ref)
                conn.execute(text("INSERT INTO evd_derivations VALUES (:id,:f,:o,:h,CAST(:r AS jsonb))"),
                             {"id": ref["id"], "f": deriv_digest, "o": object_ref, "h": content_hash, "r": json.dumps(requirements)})
            prov_path = path.rsplit("/", 1)[0] + "/prov"
            from miaoqia.sdk.derivation import pointer
            source_prov = pointer(source, prov_path)
            bundle = {"run_key": key, "recipe": recipe, "test_recipe": True, "source": {"id": source["id"], "version": source["version"]},
                      "source_hash": fingerprint(source), "snapshot_ref": source["snapshot_ref"], "revision_ref": source["revision_ref"],
                      "input_hash": fingerprint(ref["inputs"]), "reference_hash": reference["hash"],
                      "rights_tags": sorted({t for tags in requirements for t in tags}),
                      "outputs": [{"path": prov_path, "value": {**source_prov, "evidence_ref": ref["id"], "method": "rule", "extractor": ref["transform"]},
                                   "evidence_ref": ref["id"]}]}
            object_ref, content_hash = self._put(bundle)
            conn.execute(text("INSERT INTO evd_enrichments(run_key,snapshot_ref,revision_ref,recipe,object_ref,content_hash,rights_requirements) VALUES (:k,:s,:r,:p,:o,:h,CAST(:q AS jsonb))"),
                         {"k": key, "s": source["snapshot_ref"], "r": source["revision_ref"], "p": recipe, "o": object_ref, "h": content_hash, "q": json.dumps(requirements)})
            name, version = recipe.split("@")
            data = {"snapshot_ref": source["snapshot_ref"], "revision_ref": source["revision_ref"], "enricher_id": name,
                    "enricher_version": version, "quality_status": "ok"}
            payloads.validator("evidence.enrichment.completed.v1", {}).validate(data)
            outbox.Writer(NAME, "evidence").append(conn, outbox.Event("evidence.enrichment.completed.v1", source["snapshot_ref"], data))
        return key

    def get(self, run_key):
        if self.level() == "none":
            return None
        with self.database.tx() as conn:
            row = conn.execute(text("SELECT * FROM evd_enrichments WHERE run_key=:k"), {"k": run_key}).mappings().one_or_none()
        if row is None or self.level(row["rights_requirements"]) != "full_text":
            return None
        bundle = self._load(row)
        if bundle["recipe"] not in self.recipes or bundle["run_key"] != run_key:
            raise ValueError("未启用的结果recipe或run不一致")
        source = self.facts(bundle["source"]["id"], bundle["source"]["version"])
        if source is None or fingerprint(source) != bundle["source_hash"]:
            raise ValueError("固定source事实哈希不一致")
        if (row["recipe"] != bundle["recipe"] or row["snapshot_ref"] != bundle["snapshot_ref"]
                or row["revision_ref"] != bundle["revision_ref"]
                or source["snapshot_ref"] != bundle["snapshot_ref"] or source["revision_ref"] != bundle["revision_ref"]):
            raise ValueError("登记元数据与source不一致")
        path = self.recipes[bundle["recipe"]]
        value, leaf = field_input(source, path)
        reference = self.reference(**REFERENCE)
        if reference is None or bundle["reference_hash"] != reference["hash"]:
            raise ValueError("参考指纹不一致")
        requirements = [source["rights_tags"], reference["rights_tags"]]
        if row["rights_requirements"] != requirements or bundle["rights_tags"] != sorted({t for tags in requirements for t in tags}):
            raise ValueError("输入权利不一致")
        if len(bundle["outputs"]) != 1:
            raise ValueError("测试recipe输出数错误")
        output = bundle["outputs"][0]
        ref = self._ref(output["evidence_ref"])
        expected_inputs = [{"evidence_ref": leaf, "fact_ref": bundle["source"], "field_path": path}]
        if (not ref or ref["inputs"] != expected_inputs or ref["reference_data"] != [REFERENCE]
                or ref["transform"] != "test.identity@0.1.0"
                or bundle["input_hash"] != fingerprint(expected_inputs)
                or self._evaluate(output["evidence_ref"]) != value):
            raise ValueError("输出与固定输入不一致")
        from miaoqia.sdk.derivation import pointer
        prov_path = path.rsplit("/", 1)[0] + "/prov"
        expected_prov = {**pointer(source, prov_path), "evidence_ref": ref["id"], "method": "rule", "extractor": ref["transform"]}
        if output != {"path": prov_path, "value": expected_prov, "evidence_ref": ref["id"]}:
            raise ValueError("输出补丁与已登记转换不一致")
        return bundle

    def resolve(self, evidence_ref):
        if self.level() == "none":
            return None
        with self.database.tx() as conn:
            row = conn.execute(text("SELECT * FROM evd_derivations WHERE id=:id"), {"id": evidence_ref}).mappings().one_or_none()
        if row is None:
            return None
        level = self.level(row["rights_requirements"])
        if level == "none":
            return None
        # citation_only全程不打开正文/结果对象；只有各输入都可摘录时才校验并展示转换引用。
        if level == "citation_only":
            return ResolvedEvidence(evidence_ref, level)
        ref = self._load(row)
        self._evaluate(evidence_ref)
        return ResolvedEvidence(evidence_ref, level, derivation=ref)
