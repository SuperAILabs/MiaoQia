"""显式目标的非生产测试增强；部署单元组装两阶段本地服务。"""
import argparse
import dataclasses
import json
import os
import sys

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
    return "full_text" if all(t == "public-official-review" or t == "test:identity-policy"
                              or t.startswith("corpus:") for t in tags) else "none"


def main(argv=None):
    parser = argparse.ArgumentParser(prog="mq-notice-enrich")
    parser.add_argument("--recipe", required=True)
    parser.add_argument("--enable-test-recipe", action="store_true")
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
            queries = domain_intelligence.queries(database, allowed=lambda tags, purpose: test_display(tags, purpose) == "full_text")
            def fixed(fact_id, version):
                result = queries.get_fact(fact_id, Basis("current"), version=version)
                return result.fact if result else None
            service = evidence.enrichments(evd_database, FilesystemObjectStore(root), facts=fixed,
                                           display=test_display, enable_test_recipe=args.enable_test_recipe)
            if args.recipe not in service.recipes:
                raise ValueError("recipe未登记/未启用（测试recipe须显式启用）")
            service.register_reference(**REFERENCE, content=TEST_CONTENT, rights_tags=["test:identity-policy"])
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
