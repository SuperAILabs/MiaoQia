"""回放投放：把仓库内冻结语料中的官方公告原件送入公共文档管线（A02 贯通验收 L1，docs/05-delivery/09-a02-end-to-end-acceptance.md）。

  mq-evidence-replay SAMPLE_DIR [SAMPLE_DIR ...] [--packs packs]

配置：MIAOQIA_EVIDENCE_DB_DSN、MIAOQIA_EVIDENCE_OBJECT_STORE_DIR。
语料作为测试材料处理：权利标签追加 corpus:<样本>，来源对象键取样本登记的来源地址；不构成来源启用，不改变来源门禁。
投放前核对原件（与经复核的规范化表示）哈希，与登记不符的样本拒绝投放。
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import secrets
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path

import yaml

from opentelemetry import context as otel_context

from miaoqia.kernel import db, reqctx, tracing

from .api import NAME
from .application.pipeline import Artifact, Ingested, Pipeline
from .extensions import packs
from .infrastructure.objectstore import FilesystemObjectStore

CST = timezone(timedelta(hours=8))


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
    if not meta.get("source_id") or not meta.get("rights_tag") or not str(meta.get("source_url", "")).startswith("http"):
        raise ValueError(f"{sample.name}: 样本缺少来源、来源地址或权利标签")
    fetched = datetime.fromisoformat(str(meta["fetched_at"]))
    observed = fetched if fetched.tzinfo else fetched.replace(tzinfo=CST)
    return Artifact(source_id=str(meta["source_id"]), source_key=str(meta["source_url"]),
                    media_type="application/pdf" if pdf else "text/html", raw=raw,
                    rights_tags=(str(meta["rights_tag"]), f"corpus:{sample.name}"), observed_at=observed,
                    source_url=str(meta["source_url"]), normalized=normalized)


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


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="mq-evidence-replay")
    ap.add_argument("samples", nargs="+", type=Path)
    ap.add_argument("--packs", default=os.environ.get("MIAOQIA_PACKS_DIR", "packs"))
    a = ap.parse_args(argv)
    dsn, objects = os.environ.get("MIAOQIA_EVIDENCE_DB_DSN"), os.environ.get("MIAOQIA_EVIDENCE_OBJECT_STORE_DIR")
    if not dsn or not objects:
        print("未配置 MIAOQIA_EVIDENCE_DB_DSN 或 MIAOQIA_EVIDENCE_OBJECT_STORE_DIR", file=sys.stderr)
        return 2
    database = db.Database(dsn)
    try:
        results = replay(database, Pipeline(FilesystemObjectStore(objects), packs.load(a.packs)), a.samples)
    finally:
        database.close()
    for sample, r in zip(a.samples, results):
        print(json.dumps({"sample": sample.name, "snapshot_ref": r.snapshot_ref, "snapshot_created": r.snapshot_created,
                          "revisions": list(r.revisions), "existing_revisions": list(r.existing_revisions)}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
