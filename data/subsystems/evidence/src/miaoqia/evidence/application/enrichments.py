"""固定输入、不可变参考数据、可重算结果的本地登记；与domain为可重试的两阶段。"""
from __future__ import annotations

import json

from sqlalchemy import text

from miaoqia.kernel import ids, outbox, payloads, reqctx
from miaoqia.sdk.derivation import canonical, evaluate, field_input, fingerprint

from ..api import NAME, ResolvedEvidence, Recipe
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


def identity_outputs(source, value, ref, snapshot):
    from miaoqia.sdk.derivation import pointer
    path = ref["inputs"][0]["field_path"].rsplit("/", 1)[0] + "/prov"
    return [{"path": path, "value": {**pointer(source, path), "evidence_ref": ref["id"],
             "method": "rule", "extractor": ref["transform"]}, "evidence_ref": ref["id"]}]


class EnrichmentStore:
    def __init__(self, database, store, *, facts, display=None, enable_test_recipe=False, recipes=(), authorize=None, fact_requirements=None):
        if display is not None and authorize is not None:
            raise ValueError("choose one authorization mode")
        from .provenance import Provenance
        self.provenance = Provenance(database)
        self.authorize, self.fact_requirements = authorize, fact_requirements
        self.database, self.store, self.facts, self.display = database, store, facts, display
        registered = list(recipes)
        if enable_test_recipe:
            registered += [Recipe(name, path, "test.identity@0.1.0", REFERENCE, TEST_CONTENT,
                                  identity, identity_outputs, True) for name, path in RECIPES.items()]
        self.recipes = {r.id: r for r in registered}
        if len(self.recipes) != len(registered):
            raise ValueError("recipe重复")
        self.transforms = {}
        for recipe in registered:
            if recipe.transform_id in self.transforms and self.transforms[recipe.transform_id] is not recipe.transform:
                raise ValueError("转换版本重复")
            self.transforms[recipe.transform_id] = recipe.transform

    def level(self, requirements=(), *, closure=None):
        ctx = reqctx.current()
        ctx.validate()
        if not ctx.purpose or (self.display is None and self.authorize is None):
            raise PermissionError("派生读取缺少用途或当前权利策略")
        if self.authorize is not None:
            if closure is None:
                if requirements:
                    raise PermissionError("structured authorization needs source closure")
                return "full_text"  # request admission belongs to the deployment; no content grant
            from ..api import Unbound
            try:
                groups = self.provenance.resolve(closure)
            except Unbound:
                return "none"
            value = self.authorize(groups, ctx.purpose)
            if value not in LEVELS:
                raise PermissionError("unknown structured display")
            return value
        levels = [self.display((), ctx.purpose)]  # 全局认证/租户/purpose策略先于数据匹配
        levels += [self.display(tuple(tags), ctx.purpose) for tags in requirements]
        if any(level not in LEVELS for level in levels):
            raise PermissionError("未知展示级别")
        return min(levels, key=LEVELS.index)

    def _level(self, requirements, closure):
        """Do not impose new metadata requirements on the legacy callback path."""
        if self.authorize is None:
            return self.level(requirements)
        from ..api import Unbound
        try:
            return self.level(requirements, closure=closure())
        except Unbound:
            return "none"

    def require_full(self, rights_tags):
        if self.level([rights_tags]) != "full_text":
            raise PermissionError("当前增强输入权利拒绝")

    def require_full_requirements(self, closure):
        if self._level([s.tags for s in closure.sources] + [r.tags for r in closure.references], lambda: closure) != "full_text":
            raise PermissionError("current grouped input rights denied")

    def requirements(self, run_key):
        return self.provenance.recorded("enrichment", run_key)

    def _closure(self, source, reference, *, evidence=None):
        from ..api import Closure
        from ..api import Unbound
        if self.fact_requirements is None:
            raise Unbound("fixed source fact proof unavailable")
        revision = self.provenance.revision(source["revision_ref"], source["snapshot_ref"])
        parent = self.fact_requirements(source["id"], source["version"])
        from ..api import ProvenanceIntegrityError
        handle = ("fact", source["id"]+":"+str(source["version"]), fingerprint(source))
        if handle not in parent.dependencies:
            raise ProvenanceIntegrityError("fixed source fact hash does not match owner proof")
        closures = [parent, revision]
        def visit(node):
            if isinstance(node, dict):
                for k, v in node.items():
                    if k == "evidence_ref" and isinstance(v, str):
                        closures.append((evidence or self.provenance.evidence)(v))
                    else:
                        visit(v)
            elif isinstance(node, list):
                for v in node:
                    visit(v)
        visit(source)
        closures.append(self.provenance.reference(**reference))
        return Closure.merge(*closures).with_dependency("fact", source["id"]+":"+str(source["version"]), fingerprint(source))

    def backfill(self, run_keys, *, actor, reason):
        """Explicit audited owner batch; content verification precedes immutable append."""
        from ..api.audit import audit, batch
        from ..api import Closure, ProvenanceIntegrityError, Unbound
        from .provenance import put
        audit(actor, reason)
        batch(run_keys)
        import re
        if any(not isinstance(key, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", key) for key in run_keys):
            raise ValueError("fixed enrichment run keys required")
        with self.database.tx() as conn:
            if not conn.execute(text("SELECT current_user=pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname='evd_provenance'")).scalar_one():
                raise PermissionError("evidence owner required")
        if self.authorize is not None:
            raise PermissionError("backfill requires explicit offline local verification")
        closures, active = {}, set()
        def evidence(ref_id):
            ref = self._ref(ref_id)
            if ref is None:
                raise ProvenanceIntegrityError("missing evidence dependency")
            if ref.get("kind") != "derivation":
                return self.provenance.evidence(ref_id)
            if ref_id in active or len(active) >= 64 or len(closures) + len(active) >= 1024:
                raise ProvenanceIntegrityError("backfill dependency bound or cycle")
            active.add(ref_id)
            try:
                self._evaluate(ref_id)  # existing SDK enforces complete fixed DAG verification
                inputs = []
                for item in ref["inputs"]:
                    f = item["fact_ref"]
                    source = self.facts(f["id"], f["version"])
                    if source is None:
                        raise ProvenanceIntegrityError("missing fixed source")
                    inputs.extend(self._closure(source, reference, evidence=evidence) for reference in ref["reference_data"])
                closure = Closure.merge(*inputs)
                closures[("derivation", ref_id)] = (fingerprint(ref), closure)
                return closure
            finally:
                active.remove(ref_id)
        for key in run_keys:
            result = self.get(key)
            if result is None:
                raise PermissionError("backfill subject unavailable")
            source = self.facts(result["source"]["id"], result["source"]["version"])
            closure = self._closure(source, self.recipes[result["recipe"]].reference, evidence=evidence)
            for output in result["outputs"]:
                evidence(output["evidence_ref"])
            closures[("enrichment", key)] = (fingerprint(result), closure)
        with self.database.tx() as conn:
            if not conn.execute(text("SELECT current_user=pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname='evd_provenance'")).scalar_one():
                raise PermissionError("evidence owner required")
            for (kind, key), (digest, closure) in sorted(closures.items()):
                conn.execute(text("SELECT pg_advisory_xact_lock(hashtext(:k))"), {"k": "backfill:"+kind+":"+key})
                put(conn, kind, key, digest, closure, actor=actor, reason=reason)
        return len(closures)

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
        if self.authorize is not None:
            raise PermissionError("structured readers cannot register references")
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
        if not row or self._level([row["rights_tags"]], lambda: self.provenance.reference(name, version)) not in ("excerpt", "full_text"):
            return None
        return {"content": self._load(row), "hash": row["content_hash"], "rights_tags": row["rights_tags"]}

    def _ref(self, ref_id):
        with self.database.tx() as conn:
            row = conn.execute(text("""SELECT r.*,s.rights_tags FROM evd_evidence_refs r JOIN evd_snapshots s ON s.id=r.snapshot_id WHERE r.id=:id"""),
                               {"id": ref_id}).mappings().one_or_none()
            derived = None if row else conn.execute(text("SELECT * FROM evd_derivations WHERE id=:id"),
                                                    {"id": ref_id}).mappings().one_or_none()
        if row:
            if self._level([row["rights_tags"]], lambda: self.provenance.evidence(ref_id)) == "none":
                raise PermissionError("派生输入权利拒绝")
            return {"id": row["id"], "snapshot_ref": row["snapshot_id"], "pipeline_version": row["pipeline_version"],
                    "locator": row["locator"], "quote_hash": row["quote_hash"]}
        if derived:
            if self._level(derived["rights_requirements"], lambda: self.provenance.recorded("derivation", ref_id)) not in ("excerpt", "full_text"):
                raise PermissionError("派生输入不允许读取内容")
            ref = self._load(derived)
            if self.authorize is not None:
                self._check_derivation(ref)
            return ref
        return None

    def _check_derivation(self, ref):
        from ..api import Closure, ProvenanceIntegrityError
        closures = []
        for item in ref["inputs"]:
            handle = item["fact_ref"]
            source = self.facts(handle["id"], handle["version"])
            if source is None:
                raise PermissionError("fixed derivation fact unavailable")
            for reference in ref["reference_data"]:
                closures.append(self._closure(source, reference))
        expected = Closure.merge(*closures)
        if self.provenance.recorded("derivation", ref["id"]) != expected:
            raise ProvenanceIntegrityError("derivation input closure mismatch")

    def _snapshot(self, snapshot_ref):
        with self.database.tx() as conn:
            row = conn.execute(text("SELECT * FROM evd_snapshots WHERE id=:id"), {"id": snapshot_ref}).mappings().one()
        if self.authorize is not None:
            self.require_full_requirements(self.provenance.snapshot(snapshot_ref))
        else:
            self.require_full(row["rights_tags"])
        return dict(row)

    def _document(self, snapshot_ref):
        with self.database.tx() as conn:
            row = conn.execute(text("SELECT * FROM evd_snapshots WHERE id=:id"), {"id": snapshot_ref}).mappings().one()
        if self._level([row["rights_tags"]], lambda: self.provenance.snapshot(snapshot_ref)) not in ("excerpt", "full_text"):
            raise PermissionError("只引证不得打开正文")
        return load_document(self.store, row)

    def _evaluate(self, root):
        return evaluate(root, get_ref=self._ref, get_document=self._document, get_fact=self.facts,
                        get_reference=self.reference, transforms=self.transforms)

    def register(self, source, recipe):
        if self.authorize is None and self.level([source["rights_tags"]]) != "full_text":
            raise PermissionError("增强登记需要完整权限")
        if recipe not in self.recipes:
            raise ValueError("recipe未登记/未启用")
        from ..api import Unbound
        try:
            closure = self._closure(source, self.recipes[recipe].reference)
        except Unbound:
            if self.authorize is not None:
                raise
            closure = None  # Unprovable legacy stays local-only; no partial placeholder.
        if self.level([source["rights_tags"]], closure=closure) != "full_text":
            raise PermissionError("增强登记需要完整权限")
        if recipe not in self.recipes:
            raise ValueError("recipe未登记/未启用（测试recipe须显式启用）")
        if self.facts(source["id"], source["version"]) != source:
            raise ValueError("固定source事实不一致")
        descriptor = self.recipes[recipe]
        path = descriptor.path
        input_value, leaf = field_input(source, path)
        reference = self.reference(**descriptor.reference)
        if reference is None or reference["content"] != descriptor.content:
            raise ValueError("参考内容不存在或与已登记recipe不一致")
        value = descriptor.transform([input_value], [reference["content"]])
        requirements = [source["rights_tags"], reference["rights_tags"]]
        ref = {"kind": "derivation", "transform": descriptor.transform_id,
               "inputs": [{"evidence_ref": leaf, "fact_ref": {"id": source["id"], "version": source["version"]}, "field_path": path}],
               "reference_data": [descriptor.reference], "value_hash": fingerprint(value)}
        key = fingerprint({"source": [source["id"], source["version"]], "recipe": recipe,
                           "reference_hash": reference["hash"], "derivation": ref, "rights": requirements})
        with self.database.tx() as conn:
            revision = conn.execute(text("SELECT snapshot_id,facts FROM evd_extraction_revisions WHERE id=:r"),
                                    {"r": source["revision_ref"]}).mappings().one_or_none()
            if not revision or revision["snapshot_id"] != source["snapshot_ref"]:
                raise ValueError("真实解析修订与快照不一致")
            if len(revision["facts"]) != 1:
                raise ValueError("当前recipe仅支持单事实解析修订")
            conn.execute(text("SELECT pg_advisory_xact_lock(hashtext(:k))"), {"k": "enrich:" + key})
            old = conn.execute(text("SELECT run_key FROM evd_enrichments WHERE run_key=:k"), {"k": key}).first()
            if old:
                return key
            deriv_digest = fingerprint({"ref": ref, "rights": requirements})
            conn.execute(text("SELECT pg_advisory_xact_lock(hashtext(:k))"), {"k": "derive:" + deriv_digest})
            derived = conn.execute(text("SELECT * FROM evd_derivations WHERE fingerprint=:h"), {"h": deriv_digest}).mappings().one_or_none()
            if derived:
                ref = self._load(derived)
                # Reuse never silently backfills a historical derivative in an ordinary run.
                from .provenance import checked
                recorded = conn.execute(text("SELECT * FROM evd_requirement_sets WHERE kind='derivation' AND key=:i"),
                                        {"i": ref["id"]}).mappings().one_or_none()
                if recorded is None:
                    if self.authorize is not None:
                        raise Unbound("legacy derivation requires explicit backfill")
                    closure = None
                else:
                    checked(recorded, derived["content_hash"])
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
            from .provenance import put
            if closure is not None:
                put(conn, "derivation", ref["id"], fingerprint(ref), closure)
            snapshot = self._snapshot(source["snapshot_ref"])
            bundle = {"run_key": key, "recipe": recipe, "test_recipe": descriptor.test_recipe, "source": {"id": source["id"], "version": source["version"]},
                      "source_hash": fingerprint(source), "snapshot_ref": source["snapshot_ref"], "revision_ref": source["revision_ref"],
                      "input_hash": fingerprint(ref["inputs"]), "reference_hash": reference["hash"],
                      "rights_tags": sorted({t for tags in requirements for t in tags}),
                      "outputs": descriptor.outputs(source, value, ref, snapshot)}
            object_ref, content_hash = self._put(bundle)
            conn.execute(text("INSERT INTO evd_enrichments(run_key,snapshot_ref,revision_ref,recipe,object_ref,content_hash,rights_requirements) VALUES (:k,:s,:r,:p,:o,:h,CAST(:q AS jsonb))"),
                         {"k": key, "s": source["snapshot_ref"], "r": source["revision_ref"], "p": recipe, "o": object_ref, "h": content_hash, "q": json.dumps(requirements)})
            if closure is not None:
                put(conn, "enrichment", key, content_hash, closure)
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
        if row is None or self._level(row["rights_requirements"], lambda: self.provenance.recorded("enrichment", run_key)) != "full_text":
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
        if self.authorize is not None:
            expected_closure = self._closure(source, self.recipes[bundle["recipe"]].reference)
            if self.requirements(run_key) != expected_closure:
                raise ValueError("fixed input closure mismatch")
        descriptor = self.recipes[bundle["recipe"]]
        path = descriptor.path
        input_value, leaf = field_input(source, path)
        reference = self.reference(**descriptor.reference)
        if reference is None or reference["content"] != descriptor.content or bundle["reference_hash"] != reference["hash"]:
            raise ValueError("参考指纹不一致")
        value = descriptor.transform([input_value], [reference["content"]])
        requirements = [source["rights_tags"], reference["rights_tags"]]
        if row["rights_requirements"] != requirements or bundle["rights_tags"] != sorted({t for tags in requirements for t in tags}):
            raise ValueError("输入权利不一致")
        if len(bundle["outputs"]) != 1 or bundle["test_recipe"] is not descriptor.test_recipe:
            raise ValueError("recipe输出数或类型错误")
        output = bundle["outputs"][0]
        ref = self._ref(output["evidence_ref"])
        expected_inputs = [{"evidence_ref": leaf, "fact_ref": bundle["source"], "field_path": path}]
        if (not ref or ref["inputs"] != expected_inputs or ref["reference_data"] != [descriptor.reference]
                or ref["transform"] != descriptor.transform_id
                or bundle["input_hash"] != fingerprint(expected_inputs)
                or self._evaluate(output["evidence_ref"]) != value):
            raise ValueError("输出与固定输入不一致")
        if bundle["outputs"] != descriptor.outputs(source, value, ref, self._snapshot(source["snapshot_ref"])):
            raise ValueError("输出补丁与已登记转换不一致")
        return bundle

    def resolve(self, evidence_ref):
        if self.level() == "none":
            return None
        with self.database.tx() as conn:
            row = conn.execute(text("SELECT * FROM evd_derivations WHERE id=:id"), {"id": evidence_ref}).mappings().one_or_none()
        if row is None:
            return None
        level = self._level(row["rights_requirements"], lambda: self.provenance.recorded("derivation", evidence_ref))
        if level == "none":
            return None
        # citation_only全程不打开正文/结果对象；只有各输入都可摘录时才校验并展示转换引用。
        if level == "citation_only":
            return ResolvedEvidence(evidence_ref, level)
        ref = self._load(row)
        self._evaluate(evidence_ref)
        return ResolvedEvidence(evidence_ref, level, derivation=ref)
