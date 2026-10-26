"""项目补物化命令的部署单元本地装配。"""
import argparse
import json
import os
import sys

from miaoqia import evidence
from miaoqia.domain_intelligence.api import NAME
from miaoqia.domain_intelligence.application.backfill import backfill
from miaoqia.kernel import db, reqctx
from miaoqia.kernel.runtime import env_key


def main(argv=None):
    parser = argparse.ArgumentParser(prog="mq-project-backfill")
    parser.add_argument("notice_ids", nargs="+", help="显式指定当前固定版本的ntc_标识；单次至多100个")
    args = parser.parse_args(argv)
    dom_dsn, evd_dsn = os.environ.get(env_key(NAME, "DB_DSN")), os.environ.get(env_key("evidence", "DB_DSN"))
    if not dom_dsn or not evd_dsn:
        print("未配置MIAOQIA_DOMAIN_INTELLIGENCE_DB_DSN或MIAOQIA_EVIDENCE_DB_DSN", file=sys.stderr)
        return 2
    database, reader = db.Database(dom_dsn), evidence.extractions(evd_dsn)
    try:
        with reqctx.use(reqctx.service(NAME)):
            results = backfill(database, reader, args.notice_ids)
        print(json.dumps(results, ensure_ascii=False))
        return 0
    finally:
        reader.close()
        database.close()


if __name__ == "__main__":
    raise SystemExit(main())
