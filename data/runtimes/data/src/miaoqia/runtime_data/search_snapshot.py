"""Capture the domain owner's public search handle manifest."""

import argparse
from datetime import datetime
from hashlib import sha256
import json
import os
import sys
import time

from sqlalchemy.engine import make_url
from sqlalchemy.exc import ArgumentError, DBAPIError, SQLAlchemyError

from miaoqia.domain_intelligence.application.search_snapshot import (
    DEFAULT_SECONDS,
    MAX_BATCH,
    MAX_HANDLES,
    MAX_SECONDS,
    InvalidSnapshotState,
    SnapshotBudgetExceeded,
    capture,
)
from miaoqia.entity_graph.api.search_projection import ProjectionUnavailable as OrganizationUnavailable
from miaoqia.kernel import db, reqctx


USAGE = """\
在 data/ 下执行。使用已迁移的采购事实主库及具备事实表读取权限的
MIAOQIA_DOMAIN_INTELLIGENCE_DB_DSN。清单仅供受控的搜索构建流程使用。

1. 在启动捕获之前，由运维从同一 Kafka 实时采集并留存 public.acquisition、
   public.domain、public.entity、public.evidence 全部分区的下一位点，保存为
   replay-start.json（topic -> partition -> offset），同时保存采样来源、完成时间
   和集群与主题身份。确保位点仍在保留范围内，且重放使用同一日志。采样须先
   于 owner 读取，随后发布到 Kafka 的事务性发件箱事件由重放覆盖。

2. 捕获并保留原始 manifest（以下假设文件位于操作员的私有工作目录）。

   uv run --locked --all-packages python -m miaoqia.runtime_data.search_snapshot \\
     > owner-snapshot.tmp && mv owner-snapshot.tmp owner-snapshot.json

   未启用搜索分块时，每页默认最多 32 个事实句柄。启用分块时，捕获命令加上
   --page-size 1，每个 pages 元素就是 snapshot-page 所需的单句柄数组。

   启用 MIAOQIA_PUBLIC_SEARCH_ORGANIZATIONS=1 时，命令另读取实体权威库，
   两类页依各自所有者确认。跨所有者构建用快照时刻中较早者作为 as_of，用
   同一份前置 Kafka 位点追赶后续变更。

   as_of 使用 PostgreSQL 固定 MVCC 视图的起始查询时间，作为保守的截至时间。
   命令只生成 owner 数据。Kafka 起点及其真实性由前置采样记录提供。重新构建
   时可复用原清单及其配对的前置位点。重新捕获得到新的 as_of。snapshot_id 是移除该字段
   后取清单的 SHA-256，格式为 sha256:<hex>。合并清单使用包含 owners 字段、
   排序键与紧凑分隔符的 UTF-8 JSON，保留原始整数精度。

3. 生成 Go search-build begin 的输入，保留 Kafka 位点的整数精度。

   python3 - <<'PY' > basis.json
   import json
   from pathlib import Path
   owner = json.loads(Path("owner-snapshot.json").read_bytes())
   replay_start = json.loads(Path("replay-start.json").read_bytes())
   print(json.dumps({"snapshot_id": owner["snapshot_id"], "as_of": owner["as_of"],
                     "replay_start": replay_start}))
   PY

4. 按清单顺序将每个 pages 元素分别交给当前 owner 页导出。读取第 0 页示例。

   jq -c '.pages[0]' owner-snapshot.json | \\
     uv run --locked --all-packages python -m miaoqia.runtime_data.search_projection snapshot-page

   将成功确认的 DTO 交给同一 snapshot_id 对应 generation 的 load，最后一页
   交给 seal。确认 handle_count 与全部页面一致，并保存各页交付状态。清单为
   空时可直接以 {"mutations":[]} 封存。公开权利及投影修订仍由原页出口核验。
   捕获后若事实在其页确认前换版，当前页出口会拒绝旧句柄。终止该构建，从新的
   前置采样和 owner 捕获开始重试。页确认之后发生的变更由重放追赶。重放门禁、
   相关性比较及发布仍由搜索构建流程完成。

退出码 0 表示完整清单，1 表示数据库或当前事实异常，2 表示参数或配置缺失，
3 表示数量或时间上限。超限时请以新的有效前置水位重新捕获，单次无法容纳时
停止构建。命令从参数读取上限，清单与 Kafka 位点在独立步骤组合。
"""


def _capture(database, environ, *, page_size, max_handles, timeout_seconds):
    started = time.monotonic()
    procurement = capture(database, page_size=page_size, max_handles=max_handles, timeout_seconds=timeout_seconds)
    if environ.get("MIAOQIA_PUBLIC_SEARCH_ORGANIZATIONS") != "1":
        return procurement

    from .search_projection import SNAPSHOT_CALLER, _assembly

    with _assembly(environ) as projections, reqctx.use(SNAPSHOT_CALLER):
        remaining = int(started + timeout_seconds - time.monotonic())
        if remaining < 1:
            raise SnapshotBudgetExceeded("combined owner snapshot deadline exceeded")
        organizations = projections.organizations.capture(page_size=page_size, max_handles=max_handles,
                                                          timeout_seconds=remaining)
    count = procurement["handle_count"] + organizations["handle_count"]
    if count > max_handles:
        raise SnapshotBudgetExceeded(f"combined owner snapshot exceeds {max_handles} handles")
    manifest = {
        "as_of": min(procurement["as_of"], organizations["as_of"], key=datetime.fromisoformat),
        "handle_count": count,
        "pages": procurement["pages"] + organizations["pages"],
        "owners": {"procurement": procurement["snapshot_id"], "organizations": organizations["snapshot_id"]},
    }
    raw = json.dumps(manifest, sort_keys=True, separators=(",", ":")).encode()
    return {"snapshot_id": "sha256:" + sha256(raw).hexdigest(), **manifest}


def main(argv=None):
    parser = argparse.ArgumentParser(
        prog="python -m miaoqia.runtime_data.search_snapshot",
        description=__doc__, epilog=USAGE, formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    default_page_size = 1 if os.environ.get("MIAOQIA_PUBLIC_SEARCH_NOTICE_CHUNKS") == "1" else MAX_BATCH
    parser.add_argument("--page-size", type=int, default=default_page_size, metavar=f"1..{MAX_BATCH}")
    parser.add_argument("--max-handles", type=int, default=MAX_HANDLES, metavar=f"1..{MAX_HANDLES}")
    parser.add_argument("--timeout-seconds", type=int, default=DEFAULT_SECONDS, metavar=f"1..{MAX_SECONDS}")
    args = parser.parse_args(argv)
    if default_page_size == 1 and args.page_size != 1:
        parser.error("notice chunk projection requires a snapshot page size of 1")

    raw_dsn = os.environ.get("MIAOQIA_DOMAIN_INTELLIGENCE_DB_DSN")
    if not raw_dsn:
        parser.error("MIAOQIA_DOMAIN_INTELLIGENCE_DB_DSN is required")
    try:
        url = make_url(raw_dsn).update_query_dict({"connect_timeout": str(max(2, min(5, args.timeout_seconds)))})
    except (ArgumentError, ValueError):
        parser.error("invalid PostgreSQL DSN")
    if url.drivername not in {"postgres", "postgresql", "postgresql+psycopg"}:
        parser.error("a PostgreSQL psycopg DSN is required")
    try:
        database = db.Database(url.render_as_string(hide_password=False), pool_size=1, pool_timeout=0.5)
        try:
            manifest = _capture(database, os.environ, page_size=args.page_size, max_handles=args.max_handles,
                                timeout_seconds=args.timeout_seconds)
        finally:
            database.close()
    except ValueError as exc:
        parser.error(str(exc))
    except SnapshotBudgetExceeded as exc:
        print(exc, file=sys.stderr)
        return 3
    except (InvalidSnapshotState, OrganizationUnavailable) as exc:
        print(exc, file=sys.stderr)
        return 1
    except (KeyError, PermissionError):
        print("owner snapshot configuration or state unavailable", file=sys.stderr)
        return 1
    except SQLAlchemyError as exc:
        if isinstance(exc, DBAPIError) and getattr(exc.orig, "sqlstate", None) in {"57014", "25P03"}:
            print("owner snapshot query timed out or was cancelled", file=sys.stderr)
            return 3
        print("owner snapshot database unavailable", file=sys.stderr)
        return 1

    print(json.dumps(manifest, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
