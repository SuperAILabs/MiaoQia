"""显式目标的冻结语料增强；部署单元组装两阶段本地服务。"""
import argparse
import dataclasses
import json
import os
import sys
from pathlib import Path

from miaoqia import domain_intelligence, evidence
from miaoqia.domain_intelligence.api import Basis
from miaoqia.domain_intelligence.application.enrichments import apply, prepare
from miaoqia.evidence.application.enrichments import REFERENCE, TEST_CONTENT
from miaoqia.evidence.infrastructure.objectstore import FilesystemObjectStore
from miaoqia.kernel import db, reqctx
from miaoqia.kernel.runtime import env_key


def test_display(tags, purpose):
    """仅限本地显式测试服务；不作为远程用户认证或真实来源授权。"""
    ctx = reqctx.current()
    if (ctx.principal.id != "svc:notice-enrich-test" or ctx.principal.kind != "service"
            or ctx.tenant or ctx.cell or purpose != "enrichment_test"):
        raise PermissionError("非本地测试服务上下文")
    return "full_text" if all(t == "public-official-review" or t in ("test:identity-policy", "review:building-topic-pair")
                              or t.startswith("corpus:") for t in tags) else "none"


def assemble(database, evd_database, root, *, packs, enable_test_recipe=False, enable_building_topic=False):
    """Explicit offline local recipe assembly shared by enrichment and audited backfill."""
    queries = domain_intelligence.queries(database, allowed=lambda tags, purpose: test_display(tags, purpose) == "full_text")
    def fixed(fact_id, version):
        result = queries.get_fact(fact_id, Basis("current"), version=version)
        return result.fact if result else None
    recipes = []
    if enable_building_topic:
        recipes.append(evidence.load_recipe(Path(packs) / "industries/building-materials",
            "miaoqia_pack_building_materials.topic:build_recipe", packs))
    service = evidence.enrichments(evd_database, FilesystemObjectStore(root), facts=fixed,
                                   display=test_display, enable_test_recipe=enable_test_recipe, recipes=recipes, fact_requirements=queries.requirements)
    return service, recipes


def main(argv=None):
    parser = argparse.ArgumentParser(prog="mq-notice-enrich")
    parser.add_argument("--recipe", required=True)
    parser.add_argument("--enable-test-recipe", action="store_true")
    parser.add_argument("--enable-building-topic", action="store_true", help="仅启用复核冻结官方pair")
    parser.add_argument("--packs", default=os.environ.get("MIAOQIA_PACKS_DIR", "packs"))
    parser.add_argument("notice_ids", nargs="+")
    args = parser.parse_args(argv)
    dom_dsn = os.environ.get(env_key("domain-intelligence", "DB_DSN"))
    evd_dsn = os.environ.get(env_key("evidence", "DB_DSN"))
    root = os.environ.get("MIAOQIA_EVIDENCE_OBJECT_STORE_DIR")
    if not dom_dsn or not evd_dsn or not root:
        print("未配置domain/evidence DB_DSN或MIAOQIA_EVIDENCE_OBJECT_STORE_DIR", file=sys.stderr)
        return 2
    database, evd_database, reader = db.Database(dom_dsn), db.Database(evd_dsn), evidence.extractions(evd_dsn)
    try:
        with reqctx.use(dataclasses.replace(reqctx.service("notice-enrich-test"), purpose="enrichment_test")):
            service, recipes = assemble(database, evd_database, root, packs=args.packs,
                enable_test_recipe=args.enable_test_recipe, enable_building_topic=args.enable_building_topic)
            if args.recipe not in service.recipes:
                raise ValueError("recipe未登记/未启用（测试recipe须显式启用）")
            if args.enable_test_recipe:
                service.register_reference(**REFERENCE, content=TEST_CONTENT, rights_tags=["test:identity-policy"])
            for recipe in recipes:
                service.register_reference(**recipe.reference, content=recipe.content, rights_tags=["review:building-topic-pair"])
            results = []
            for target in prepare(database, reader, args.notice_ids):
                run_key = service.register(target["source"], args.recipe)
                results.append(apply(database, reader, service, run_key, expected_parent=target["expected_parent"]))
        print(json.dumps(results, ensure_ascii=False))
        return 0
    except (ValueError, PermissionError) as error:
        print(str(error), file=sys.stderr)
        return 2
    finally:
        reader.close()
        evd_database.close()
        database.close()


if __name__ == "__main__":
    raise SystemExit(main())
