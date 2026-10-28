"""公共事实的本地读取绑定。先选取时间口径下的版本，再校验当前来源权利，不回退到有权的旧版本。

权利判定由部署单元注入，回调通过 reqctx.current() 取得调用主体、租户、单元与用途；缺省拒绝。
全部查询在数据库只读事务内运行；不接收 SQL、任意字段路径或任意查询模板。
"""
from __future__ import annotations

from collections.abc import Callable
from datetime import datetime

from sqlalchemy import text

from miaoqia.kernel import db, reqctx
from miaoqia.sdk.derivation import fingerprint

from ..api import Basis, FactRead, Page
from ..domain.views import bid_result_units
from .facts import fact_version


class Queries:
    def __init__(self, database: db.Database, allowed: Callable[[tuple[str, ...], str], bool] | None = None, *, authorize=None, provenance=None):
        if allowed is not None and authorize is not None:
            raise ValueError("choose one authorization mode")
        self.authorize, self.provenance = authorize, provenance
        self.database = database
        self.allowed = allowed

    def _require_access(self):
        context = reqctx.current()
        context.validate()
        if not context.purpose or (self.allowed is None and self.authorize is None):
            raise PermissionError("未装配来源权利判定或缺少用途")
        return context

    def requirements(self, fact_id, version, *, deadline=None):
        """Return the owner-verified immutable closure for one fixed version."""
        from . import requirements
        from miaoqia.evidence.api import Unbound
        if type(version) is not int or version < 1:
            raise ValueError("fixed positive version required")
        with self.database.tx(deadline=deadline) as conn:
            conn.execute(text("SET TRANSACTION READ ONLY"))
            fact = fact_version(conn, fact_id, version)
            if fact is None:
                raise Unbound("fact unavailable")
            return requirements.read(conn, fact).with_dependency("fact", fact["id"]+":"+str(fact["version"]), fingerprint(fact))

    def _allowed(self, conn, fact, *, deadline=None):
        context = self._require_access()
        if self.authorize is not None:
            from miaoqia.evidence.api import Unbound
            if self.provenance is None:
                return False
            try:
                from . import requirements
                groups = self.provenance.resolve(requirements.read(conn, fact), deadline=deadline)
            except Unbound:
                return False
            return self.authorize(groups, context.purpose) is True
        return self.allowed(tuple(fact["rights_tags"]), context.purpose) is True

    @staticmethod
    def _limit(limit):
        if type(limit) is not int or not 1 <= limit <= 100:
            raise ValueError("limit 必须在 1–100 之间")

    def _select_fact(self, conn, fact_id, basis, version, *, deadline=None):
        if version is not None and (type(version) is not int or version < 1):
            raise ValueError("version 必须为正整数")
        if version is not None or basis.view == "current":
            fact = fact_version(conn, fact_id, version)
        else:
            fact = conn.execute(text("SELECT content FROM dom_fact_versions WHERE fact_id=:f AND recorded_at<=:r ORDER BY version DESC LIMIT 1"),
                                {"f": fact_id, "r": basis.recorded_cutoff}).scalar_one_or_none()
        if fact is not None and basis.view == "known_at":
            if datetime.fromisoformat(fact["recorded_at"].replace("Z", "+00:00")) > basis.recorded_cutoff:
                return None
        return fact if fact is not None and self._allowed(conn, fact, deadline=deadline) else None

    def get_fact(self, fact_id: str, basis: Basis, *, version: int | None = None, deadline=None) -> FactRead | None:
        self._require_access()
        with self.database.tx(deadline=deadline) as conn:
            conn.execute(text("SET TRANSACTION READ ONLY"))
            fact = self._select_fact(conn, fact_id, basis, version, deadline=deadline)
            return FactRead(basis, fact) if fact is not None else None

    def _page(self, conn, statement, params, basis, limit, *, history=False, deadline=None):
        permitted = []
        # 单条 SELECT 的一致快照；流式扫描先按权利过滤，再按可见结果分页，游标不暴露被过滤的对象标识。
        result = conn.execution_options(stream_results=True).execute(text(statement), params)
        try:
            while len(permitted) <= limit:
                rows = result.fetchmany(100)
                for row in rows:
                    if self._allowed(conn, row[0], deadline=deadline):
                        permitted.append(row)
                        if len(permitted) > limit:
                            break
                if len(rows) < 100:
                    break
        finally:
            result.close()
        selected = permitted[:limit]
        items = tuple({"fact": r[0], "changed_fields": r[1]} if history else r[0] for r in selected)
        cursor = str(selected[-1][0]["version" if history else "id"]) if len(permitted) > limit else None
        return Page(basis, items, cursor)

    def get_fact_history(self, fact_id: str, basis: Basis, *, after: int = 0, limit: int = 50, deadline=None) -> Page:
        self._require_access()
        self._limit(limit)
        if type(after) is not int or after < 0:
            raise ValueError("历史游标必须为非负版本号")
        with self.database.tx(deadline=deadline) as conn:
            conn.execute(text("SET TRANSACTION READ ONLY"))
            return self._page(conn, """SELECT content, changed_fields FROM dom_fact_versions
                                      WHERE fact_id = :f AND version > :a
                                        AND (CAST(:r AS timestamptz) IS NULL OR recorded_at <= :r)
                                      ORDER BY version""",
                              {"f": fact_id, "a": after, "r": basis.recorded_cutoff}, basis, limit, history=True, deadline=deadline)

    def query_facts(self, info_domain: str, object_type: str, basis: Basis, *, after: str = "", limit: int = 50, deadline=None) -> Page:
        self._require_access()
        self._limit(limit)
        with self.database.tx(deadline=deadline) as conn:
            conn.execute(text("SET TRANSACTION READ ONLY"))
            return self._page(conn, """SELECT selected.content FROM dom_facts f
                                      JOIN LATERAL (
                                          SELECT content FROM dom_fact_versions v WHERE v.fact_id = f.id
                                            AND (CAST(:r AS timestamptz) IS NULL OR v.recorded_at <= :r)
                                          ORDER BY v.version DESC LIMIT 1
                                      ) selected ON true
                                      WHERE f.info_domain = :d AND f.object_type = :t AND f.id > :a
                                      ORDER BY f.id""",
                              {"d": info_domain, "t": object_type, "a": after, "r": basis.recorded_cutoff}, basis, limit, deadline=deadline)

    def _topic_context(self, conn, fact, basis, *, deadline=None):
        if not fact.get("subject_categories"):
            return None
        link = conn.execute(text("SELECT * FROM dom_notice_links WHERE notice_id=:n AND notice_version=:v"),
                            {"n": fact["id"], "v": fact["version"]}).mappings().one_or_none()
        if not link or link["status"] != "confirmed" or not link["project_id"]:
            return None
        project = self._select_fact(conn, link["project_id"], basis, link["reason"].get("project_version"), deadline=deadline) if link["reason"].get("project_version") else None
        if not project:
            return None
        deps = conn.execute(text("SELECT notice_id,notice_version FROM dom_project_dependencies WHERE project_id=:p AND project_version=:v"),
                            {"p": project["id"], "v": project["version"]}).all()
        if ({(d[0], d[1]) for d in deps} != {(d["id"], d["version"]) for d in project.get("notice_refs", [])}
                or (fact["id"], fact["version"]) not in deps):
            return None
        if any(self._select_fact(conn, d[0], basis, d[1], deadline=deadline) is None for d in deps):
            return None
        receipts = conn.execute(text("""SELECT * FROM dom_enrichment_receipts WHERE notice_id=:n AND output_version<=:v
            AND recipe='proc.building-topic@0.1.0' AND (CAST(:r AS timestamptz) IS NULL OR recorded_at<=:r)
            ORDER BY output_version DESC, recorded_at DESC"""),
            {"n": fact["id"], "v": fact["version"], "r": basis.recorded_cutoff}).mappings().all()
        for receipt in receipts:
            source = self._select_fact(conn, fact["id"], basis, receipt["source_base_version"], deadline=deadline)
            output_fact = self._select_fact(conn, fact["id"], basis, receipt["output_version"], deadline=deadline)
            outputs = receipt["outputs"]
            if not source or not output_fact or len(outputs) != 1:
                continue
            output = outputs[0]
            guard = output.get("scope_check", {})
            if (set(output) != {"path", "value", "evidence_ref", "scope_check", "diagnostics"}
                    or output["path"] != "/subject_categories" or not output["value"]
                    or output["value"] != fact["subject_categories"] or output["value"] != output_fact.get("subject_categories")
                    or source["revision_ref"] != fact["revision_ref"] or source["snapshot_ref"] != fact["snapshot_ref"]
                    or fingerprint(source) != receipt["source_hash"] or guard.get("source_hash") != receipt["source_hash"]
                    or guard.get("reference_hash") != receipt["reference_hash"] or guard.get("snapshot_ref") != fact["snapshot_ref"]
                    or guard.get("eligible") is not True or not guard.get("review_ref") or not guard.get("review_sha256")
                    or guard.get("title_evidence_ref") != source["title"]["prov"]["evidence_ref"]
                    or fingerprint({"fact": output_fact, "outputs": outputs}) != receipt["output_hash"]):
                continue
            return {"project": project, "categories": output["value"]}
        return None

    def collect_source_groups(self, operation, body, deadline=None):
        """Collect the persisted source-group union before content admission."""
        from . import requirements
        from miaoqia.evidence.api.provenance import Closure
        basis = body.get("basis")
        if isinstance(basis, dict):
            from datetime import datetime
            basis = Basis(basis["view"], datetime.fromisoformat(basis["recorded_cutoff"].replace("Z", "+00:00")) if basis.get("recorded_cutoff") else None)
        facts = []
        with self.database.tx(deadline=deadline) as conn:
            conn.execute(text("SET TRANSACTION READ ONLY"))
            if operation in ("data.get_fact", "data.bid_result_units"):
                # Collector runs before rights admission; read persisted closure
                # directly and never apply the response authorization callback.
                version = body.get("version")
                if version is None:
                    fact = fact_version(conn, body["fact_id"], None)
                else:
                    fact = fact_version(conn, body["fact_id"], version)
                if fact is not None and basis.view == "known_at" and basis.recorded_cutoff is not None:
                    if datetime.fromisoformat(fact["recorded_at"].replace("Z", "+00:00")) > basis.recorded_cutoff:
                        fact = None
                if fact is not None: facts.append(fact)
            elif operation == "data.fact_history":
                rows = conn.execute(text("SELECT content FROM dom_fact_versions WHERE fact_id=:f AND version>:a AND (CAST(:r AS timestamptz) IS NULL OR recorded_at<=:r) ORDER BY version"), {"f": body["fact_id"], "a": body.get("after", 0), "r": basis.recorded_cutoff}).all()
                if len(rows) > 1000:
                    raise ValueError("source-group collector result too large")
                facts = [r[0] for r in rows]
            elif operation == "data.query_facts":
                rows = conn.execute(text("""SELECT selected.content FROM dom_facts f JOIN LATERAL (SELECT content FROM dom_fact_versions v WHERE v.fact_id=f.id AND (CAST(:r AS timestamptz) IS NULL OR v.recorded_at<=:r) ORDER BY v.version DESC LIMIT 1) selected ON true WHERE f.info_domain=:d AND f.object_type=:t AND f.id>:a ORDER BY f.id"""), {"d": body["info_domain"], "t": body["object_type"], "a": body.get("after", ""), "r": basis.recorded_cutoff}).all()
                if len(rows) > 1000:
                    raise ValueError("source-group collector result too large")
                facts = [r[0] for r in rows]
            closures = [requirements.read(conn, f) for f in facts]
        merged = Closure.merge(*closures) if closures else None
        return [] if merged is None else [x.dto() for x in merged.sources]

    def bid_result_units(self, fact_id: str, basis: Basis, *, version: int | None = None, deadline=None) -> Page:
        self._require_access()
        with self.database.tx(deadline=deadline) as conn:
            conn.execute(text("SET TRANSACTION READ ONLY"))
            fact = self._select_fact(conn, fact_id, basis, version, deadline=deadline)
            context = self._topic_context(conn, fact, basis, deadline=deadline) if fact else None
            return Page(basis, tuple(bid_result_units(fact, context)) if fact is not None else ())
