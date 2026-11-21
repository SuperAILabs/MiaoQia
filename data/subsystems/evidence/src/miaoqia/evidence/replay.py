"""回放投放：把仓库内冻结语料中的公开原件或显式合成夹具送入公共文档管线（A02 贯通验收 L1，docs/05-delivery/09-a02-end-to-end-acceptance.md）。

  mq-evidence-replay SAMPLE_DIR [SAMPLE_DIR ...] [--packs packs]
  mq-evidence-replay --pending-source SOURCE_ID [--limit N] [--packs packs]
  mq-evidence-replay --assess-pending-source SOURCE_ID [--limit N] [--packs packs]

配置：MIAOQIA_EVIDENCE_DB_DSN、MIAOQIA_EVIDENCE_OBJECT_STORE_DIR。
语料作为测试材料处理：权利标签追加 corpus:<样本>，公开来源对象键取登记地址；合成夹具取 manifest 绑定的本地键，不声明抓取地址；不构成来源启用，不改变来源门禁。
投放前核对原件（与经复核的规范化表示）哈希，与登记不符的样本拒绝投放。
待解析来源经独立验收并启用后，可用原采集观察重放，无需下载或改写原件。
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import secrets
import sys
from pathlib import Path
from time import monotonic

import yaml

from opentelemetry import context as otel_context

from miaoqia.acquisition.api import identifier
from miaoqia.kernel import db, reqctx, tracing
from miaoqia.kernel.objectstore import open_object_store
from miaoqia.sdk.corpus_source import corpus_source

from .api import NAME
from .application.live import LiveIngest, pending_events
from .application.pipeline import Artifact, Ingested, Pipeline
from .extensions import packs

MAX_ASSESSMENT_ITEMS = 16


class SourceNotPaused(ValueError):
    """Assessment requires explicit pending admission for a configured source."""


def corpus_artifact(sample: Path) -> Artifact:
    meta = yaml.safe_load((sample / "meta.yaml").read_text(encoding="utf-8"))
    pdf = (sample / "raw.pdf").exists()
    raw = (sample / ("raw.pdf" if pdf else "raw.html")).read_bytes()
    if meta.get("raw_sha256") and hashlib.sha256(raw).hexdigest() != meta["raw_sha256"]:
        raise ValueError(f"{sample.name}: 原件哈希不符")
    normalized = None
    if pdf:
        body = (sample / "normalized.json").read_bytes()
        if hashlib.sha256(body).hexdigest() != meta.get("normalized_sha256"):
            raise ValueError(f"{sample.name}: 规范化表示哈希不符")
        normalized = json.loads(body)
    source = corpus_source(sample, meta, raw, for_replay=True)
    return Artifact(source_id=source.source_id, source_key=source.source_key,
                    media_type="application/pdf" if pdf else "text/html", raw=raw,
                    rights_tags=(source.rights_tag, f"corpus:{sample.name}"), observed_at=source.observed_at,
                    source_url=source.source_url, normalized=normalized)



def root_traceparent() -> str:
    """回放是一条链路的起点：显式给出 W3C 第1级的根 traceparent（标志 01）。

    Python SDK 自建根 span 时追加随机标志位（03），不符合事件信封契约的 0[01]，下游处理器沿用它写发件箱时会被拒绝。
    """
    return f"00-{secrets.token_hex(16)}-{secrets.token_hex(8)}-01"


def replay(database: db.Database, pipeline: Pipeline, samples: list[Path]) -> list[Ingested]:
    out = []
    with reqctx.use(reqctx.service(NAME)):
        for sample in samples:
            art = corpus_artifact(sample)
            token = otel_context.attach(tracing.context_from(root_traceparent()))
            try:
                with database.tx() as conn:
                    out.append(pipeline.ingest(conn, art))
            finally:
                otel_context.detach(token)
    return out


def replay_pending(database: db.Database, live: LiveIngest, source_id: str, limit: int) -> int:
    with reqctx.use(reqctx.service(NAME)):
        for processed in range(limit):
            token = otel_context.attach(tracing.context_from(root_traceparent()))
            try:
                with database.tx() as conn:
                    if not live.drain_one(conn, source_id):
                        return processed
            finally:
                otel_context.detach(token)
    return limit


def _require_paused(adapters: dict[str, packs.DocumentAdapter], source_id: str) -> None:
    adapter = adapters.get(source_id)
    if adapter is None or adapter.live_parse is not False:
        raise SourceNotPaused


def assess_pending(database: db.Database, live: LiveIngest, source_id: str, limit: int) -> list[dict]:
    _require_paused(live.adapters, source_id)
    if not 1 <= limit <= MAX_ASSESSMENT_ITEMS:
        raise ValueError("invalid assessment limit")
    with reqctx.use(reqctx.service("evidence-pending-assessment")):
        with database.tx(deadline=monotonic() + 15) as conn:
            events = pending_events(conn, source_id, limit)
        return [live.assess_pending(source_id, event) for event in events]


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="mq-evidence-replay")
    ap.add_argument("samples", nargs="*", type=Path)
    ap.add_argument("--pending-source", metavar="SOURCE_ID")
    ap.add_argument("--assess-pending-source", metavar="SOURCE_ID", type=identifier,
                    help="read-only field evidence assessment for a paused source (default 8, maximum 16)")
    ap.add_argument("--limit", type=int, help="maximum pending observations per run")
    ap.add_argument("--packs", default=os.environ.get("MIAOQIA_PACKS_DIR", "packs"))
    a = ap.parse_args(argv)
    if sum(map(bool, (a.pending_source, a.assess_pending_source, a.samples))) != 1:
        ap.error("provide sample directories, --pending-source or --assess-pending-source")
    if a.limit is not None:
        maximum = MAX_ASSESSMENT_ITEMS if a.assess_pending_source else 1000
        if a.samples or not 1 <= a.limit <= maximum:
            ap.error(f"--limit requires a pending source and must be between 1 and {maximum}")
    dsn, objects = os.environ.get("MIAOQIA_EVIDENCE_DB_DSN"), os.environ.get("MIAOQIA_EVIDENCE_OBJECT_STORE_DIR")
    if not dsn or not objects:
        print("未配置 MIAOQIA_EVIDENCE_DB_DSN 或 MIAOQIA_EVIDENCE_OBJECT_STORE_DIR", file=sys.stderr)
        return 2
    if a.assess_pending_source:
        try:
            adapters = packs.load_documents(a.packs)
            _require_paused(adapters, a.assess_pending_source)
            database = db.Database(dsn, pool_size=1)
            try:
                store = open_object_store(objects, read_only=True)
                live = LiveIngest(store, Pipeline(store, packs.load(a.packs)), adapters)
                reports = assess_pending(database, live, a.assess_pending_source, a.limit or 8)
            finally:
                database.close()
        except SourceNotPaused:
            print(json.dumps({"error": "source_not_paused"}), file=sys.stderr)
            return 2
        except Exception:
            print(json.dumps({"error": "assessment_unavailable"}), file=sys.stderr)
            return 1
        print(json.dumps({"source_id": a.assess_pending_source, "observations": reports}, ensure_ascii=False))
        return int(not reports or any(report.get("field_evidence") != "available" for report in reports))

    database = db.Database(dsn)
    try:
        store = open_object_store(objects)
        pipeline = Pipeline(store, packs.load(a.packs))
        if a.pending_source:
            adapters = packs.load_documents(a.packs)
            adapter = adapters.get(a.pending_source)
            if adapter is None or adapter.live_parse is not True:
                print("该来源尚未启用待解析观察的重放", file=sys.stderr)
                return 2
            processed = replay_pending(database, LiveIngest(store, pipeline, adapters), a.pending_source, a.limit or 100)
            print(json.dumps({"source_id": a.pending_source, "processed": processed}, ensure_ascii=False))
            return 0
        results = replay(database, pipeline, a.samples)
    finally:
        database.close()
    for sample, r in zip(a.samples, results):
        print(json.dumps({"sample": sample.name, "snapshot_ref": r.snapshot_ref, "snapshot_created": r.snapshot_created,
                          "revisions": list(r.revisions), "existing_revisions": list(r.existing_revisions)}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
