"""语料：用公开来源或显式合成夹具检验解析器与领域 Schema。

目录结构（位于信息领域包内，例如 packs/information_domains/procurement/corpus/）：

  <样本编号>/
    meta.yaml       公开来源：source_url/source_id/fetched_at/rights_tag；显式夹具：artifact_origin/manifest/本地键/observed_at
    raw.html        原始页面（公开来源信息按公开数据保留原文）
    expected.yaml   标注：facts: [{fact_type, fields: {JSON Pointer: 期望值}}]；只标注确定无疑的字段

命令：
  mq-corpus check DIR --extractor 模块:类 --schema 相对 contracts/schemas 的路径
                      [--path 额外的导入路径] [--min-accuracy 0.95]
      规范化 → 解析 → Schema/语义校验 → 与标注比对，输出逐字段准确率
  mq-corpus check DIR --reviewed --schema PATH [--export DIR]
      人工映射的原件哈希、证据、Schema与语义验证；不计算解析准确率。
      人工样本使用reviewed.json（facts/evidence），扫描PDF另提供经复核的normalized.json。
  mq-corpus check DIR --extractor 模块:类 --gold reviewed --schema PATH [--min-accuracy 0.95]
      以人工映射（reviewed.json）为黄金标注评测解析器：只取 notice_type 属于解析器 gold_notice_types 的人工样本，
      逐个比对解析器 gold_fields 声明的关键字段（JSON Pointer，* 匹配数组下标）中人工映射给出的值；
      标识、证据与人工映射未给出的字段不计分，十进制金额按数值比较。解析结果同样经过 Schema、证据与领域语义校验。
"""
from __future__ import annotations

import argparse
import importlib
import hashlib
import json
import re
import sys
from collections import defaultdict
from decimal import Decimal, InvalidOperation
from pathlib import Path

import yaml
from jsonschema import Draft202012Validator
from referencing import Registry, Resource

from .document import Document
from .corpus_source import corpus_source
from .entrypoints import resolve_semantic_validator, validate_loaded_semantic_validator
from .extraction import ExtractionContext
from .testing import html_to_document
from .validation import validate_extraction

# ---------------- 评测 ----------------
def _repo_root() -> Path:
    for d in [Path.cwd(), *Path.cwd().parents]:
        if (d / "contracts" / "identifiers.yaml").exists():
            return d
    raise SystemExit("找不到仓库根目录")


def _validator(schema_rel: str) -> tuple[Draft202012Validator, dict]:
    sd = _repo_root() / "contracts" / "schemas"
    reg = Registry()
    target = None
    for f in sd.rglob("*.schema.json"):
        s = json.loads(f.read_text(encoding="utf-8"))
        reg = reg.with_resource(s["$id"], Resource.from_contents(s))
        if f.relative_to(sd).as_posix() == schema_rel:
            target = s
    if target is None:
        raise SystemExit(f"Schema 不存在：{schema_rel}")
    return Draft202012Validator(target, registry=reg, format_checker=Draft202012Validator.FORMAT_CHECKER), target


def _semantic_validator(schema_rel: str):
    """评测组装入口：按manifest登记加载领域校验，SDK不静态依赖领域包。"""
    owners = []
    for manifest in sorted((_repo_root() / "packs/information_domains").glob("*/pack.yaml")):
        provides = yaml.safe_load(manifest.read_text())["pack"].get("provides", {})
        if f"contracts/schemas/{schema_rel}" in provides.get("schemas", []):
            owners.append((manifest.parent, provides))
    if len(owners) > 1:
        raise ValueError(f"Schema有多个领域包所有者：{schema_rel}")
    if not owners:
        if schema_rel.startswith("domains/"):
            raise ValueError(f"领域Schema没有登记所有者：{schema_rel}")
        return None
    pack_dir, provides = owners[0]
    if "semantic_validator" not in provides:
        # 含代码的包必须显式声明校验，避免迁移后漏跑；纯声明包只执行Schema校验。
        if (pack_dir / "src").exists():
            raise ValueError(f"代码型领域包未登记semantic_validator：{pack_dir.name}")
        return None
    ref = provides["semantic_validator"]
    module, name, expected = resolve_semantic_validator(pack_dir, ref)
    source = str(pack_dir / "src")
    sys.path.insert(0, source)
    try:
        loaded = importlib.import_module(module)
        if not getattr(loaded, "__file__", None) or Path(loaded.__file__).resolve() != expected:
            raise ValueError(f"领域校验模块来源与登记包不一致：{ref}")
        callback = getattr(loaded, name)
    finally:
        sys.path.remove(source)
    validate_loaded_semantic_validator(callback, expected, ref)
    return callback


def load_reviewed_sample(sample: Path):
    """共用原件/人工映射加载，保留来源种类及原件、规范化制品哈希验证。"""
    meta = yaml.safe_load((sample / "meta.yaml").read_text())
    doc = _load_document(sample, meta)
    review = json.loads((sample / "reviewed.json").read_text())
    return doc, review["facts"], review["evidence"]


def _placeholder_meta(schema: dict, sample: str) -> dict:
    """平台在入库时补齐的公共字段；评测时用占位值，只为让领域字段接受完整 Schema 校验。"""
    pat = schema["properties"].get("id", {}).get("pattern", "^xx_")
    prefix = pat[1:pat.index("_")]
    pad = lambda p: f"{p}_" + "0" * 26  # noqa: E731
    meta = {"id": pad(prefix), "schema_version": schema.get("x-version", "0.0.0"), "version": 1,
            "snapshot_ref": pad("snp"), "revision_ref": pad("exr"), "rights_tags": [f"corpus:{sample}"],
            "classification": "public", "observed_at": "2000-01-01T00:00:00Z", "recorded_at": "2000-01-01T00:00:00Z"}
    const = schema["properties"].get("info_domain", {}).get("const")
    if const:
        meta["info_domain"] = const
    return meta


def _get(doc, pointer: str):
    node = doc
    for part in pointer.lstrip("/").split("/"):
        if isinstance(node, list):
            node = node[int(part)]
        else:
            node = node[part]
    return node


def _load_document(sample: Path, meta: dict) -> Document:
    raw = sample / ("raw.pdf" if (sample / "raw.pdf").exists() else "raw.html")
    source = corpus_source(sample, meta, raw.read_bytes())
    for name, key in ((raw.name, "raw_sha256"), ("response.html", "response_sha256")):
        if meta.get(key):
            if hashlib.sha256((sample / name).read_bytes()).hexdigest() != meta[key]:
                raise ValueError(f"{sample.name}/{name}: 原件哈希不符")
    if (sample / "normalized.json").exists():
        data = json.loads((sample / "normalized.json").read_text())
        if meta.get("normalized_sha256") != hashlib.sha256((sample / "normalized.json").read_bytes()).hexdigest():
            raise ValueError(f"{sample.name}: 人工规范化制品哈希不符")
        return Document(data)
    if raw.suffix != ".html":
        raise ValueError(f"{sample.name}: PDF未提供经复核的规范化表示")
    # 样本快照稳定且按原件区分；不是生产标识分配或采集时间证明。
    from .extraction import _ulid_like
    snapshot = _ulid_like("snp", hashlib.sha256(raw.read_bytes()).hexdigest())
    return Document(html_to_document(raw.read_text(), snapshot, source.source_url))


def _gold_pointers(fact, patterns: tuple[str, ...]) -> dict[str, object]:
    """人工映射中与关键字段模式匹配、且有取值的叶子（模式中的 * 展开为实际下标）。"""
    out: dict[str, object] = {}

    def walk(node, parts: list[str], pointer: str) -> None:
        if not parts:
            if not isinstance(node, (dict, list)):
                out[pointer] = node
            return
        head, rest = parts[0], parts[1:]
        if head == "*":
            if isinstance(node, list):
                for i, item in enumerate(node):
                    walk(item, rest, f"{pointer}/{i}")
        elif isinstance(node, dict) and head in node:
            walk(node[head], rest, f"{pointer}/{head}")

    for pattern in patterns:
        walk(fact, pattern.lstrip("/").split("/"), "")
    return out


def _same(got, want) -> bool:
    if got == want:
        return True
    if isinstance(got, str) and isinstance(want, str):
        try:
            return Decimal(got) == Decimal(want)
        except InvalidOperation:
            return False
    return False


def check(corpus: Path, extractor_ref: str | None, schema_rel: str,
          min_accuracy: float | None, reviewed: bool = False, export: Path | None = None,
          gold_reviewed: bool = False) -> int:
    extractor = None
    if gold_reviewed and (reviewed or not extractor_ref):
        raise ValueError("--gold reviewed 评测解析器，必须指定extractor且不能与--reviewed混用")
    if not reviewed:
        if not extractor_ref:
            raise ValueError("自动解析评测必须指定extractor")
        mod, _, cls = extractor_ref.partition(":")
        extractor = getattr(importlib.import_module(mod), cls)()
    validator, schema = _validator(schema_rel)
    try:
        semantic_validator = _semantic_validator(schema_rel)
    except (ValueError, ImportError, AttributeError, SyntaxError) as exc:
        print(f"✗ {exc}")
        return 1
    document_validator, _ = _validator("evidence/normalized-document.schema.json")
    evidence_validator, _ = _validator("evidence/evidence-ref.schema.json")
    field_stats = defaultdict(lambda: {"correct": 0, "wrong": 0, "missing": 0})
    invalid = 0
    samples = []
    for sample in sorted(p for p in corpus.iterdir() if p.is_dir()):
        meta = yaml.safe_load((sample / "meta.yaml").read_text())
        if meta.get("schema", schema_rel) != schema_rel:
            continue
        if gold_reviewed:
            if meta.get("review_mode") == "manual":
                types = {f.get("notice_type") for f in json.loads((sample / "reviewed.json").read_text())["facts"]}
                if types & set(getattr(extractor, "gold_notice_types", ())):
                    samples.append((sample, meta))
        elif (meta.get("review_mode") == "manual") == reviewed:
            samples.append((sample, meta))
    if not samples:
        print("✗ 没有匹配模式的样本，不能空跑通过")
        return 1
    fact_count = 0
    instances = []
    for sample, meta in samples:
        try:
            doc = _load_document(sample, meta)
            docerrs = list(document_validator.iter_errors(doc.data))
            if docerrs:
                raise ValueError(docerrs[0].message)
            if reviewed:
                annotation = json.loads((sample / "reviewed.json").read_text())
                facts, evidence = annotation["facts"], annotation["evidence"]
                if not facts or not evidence:
                    raise ValueError("人工映射必须包含事实与可验证证据")
                expected = None
            else:
                if gold_reviewed:
                    gold = json.loads((sample / "reviewed.json").read_text())["facts"]
                    expected = {"facts": [{"fields": _gold_pointers(f, tuple(extractor.gold_fields))} for f in gold]}
                    if not extractor.applies(doc, meta):
                        raise ValueError("解析器不接受该样本")
                else:
                    expected = yaml.safe_load((sample / "expected.yaml").read_text())
                if not expected.get("facts") or not any(f.get("fields") for f in expected["facts"]):
                    raise ValueError("黄金标注为空，不能证明准确率")
                ctx = ExtractionContext(doc, f"{extractor.id}@{extractor.version}")
                result = extractor.extract(doc, ctx)
                facts, evidence = result.facts, ctx.evidence
                if len(facts) != len(expected["facts"]):
                    raise ValueError("解析事实数与标注数不符")
            for ref in evidence.values():
                ref_errors = list(evidence_validator.iter_errors(ref))
                if ref_errors:
                    raise ValueError(ref_errors[0].message)
            errors = validate_extraction(doc, facts, evidence, semantic_validator=semantic_validator)
            if errors:
                raise ValueError(errors[0])
            for i, got in enumerate(facts):
                full = {**_placeholder_meta(schema, sample.name), **got}
                from .extraction import _ulid_like
                prefix = schema["properties"]["id"]["pattern"].split("_")[0].lstrip("^")
                full["id"] = got.get("id", _ulid_like(prefix, f"{sample.name}|{i}"))
                full["snapshot_ref"] = doc.snapshot_ref
                errs = list(validator.iter_errors(full))
                if errs:
                    invalid += 1
                    print(f"✗ {sample.name}#{i}: {errs[0].json_path} {errs[0].message[:160]}")
                if not errs:
                    instances.append((sample.name, i, full))
                fact_count += 1
                if expected is None:
                    continue
                for pointer, want in expected["facts"][i]["fields"].items():
                    key = re.sub(r"/\d+(?=/|$)", "/*", pointer) if gold_reviewed else pointer
                    try:
                        val = _get(got, pointer)
                    except (KeyError, IndexError, ValueError, TypeError):
                        field_stats[key]["missing"] += 1
                        print(f"  {sample.name}#{i} {pointer}：期望 {want!r}，缺失")
                        continue
                    ok = _same(val, want)
                    field_stats[key]["correct" if ok else "wrong"] += 1
                    if not ok:
                        print(f"  {sample.name}#{i} {pointer}：期望 {want!r}，实际 {val!r}")
        except (ValueError, KeyError, IndexError, TypeError, OSError) as exc:
            invalid += 1
            print(f"✗ {sample.name}: {exc}")
    if reviewed and not invalid and export is not None:
        export.mkdir(parents=True, exist_ok=True)
        type_name = schema["title"].split()[0]
        for name, i, full in instances:
            (export / f"{type_name}__reviewed_{name}_{i}.json").write_text(json.dumps(full, ensure_ascii=False))
    if reviewed:
        print(f"人工契约映射：{len(samples)}个原件、{fact_count}条事实，{invalid}个失败；不计算自动解析准确率")
        return 1 if invalid else 0
    total = sum(sum(s.values()) for s in field_stats.values())
    correct = sum(s["correct"] for s in field_stats.values())
    print(f"\n{len(samples)} 个样本，Schema/语义不符 {invalid} 个")
    for pointer, stats in sorted(field_stats.items()):
        n = sum(stats.values())
        print(f"  {pointer:<48} {stats['correct']}/{n}  错误 {stats['wrong']}  缺失 {stats['missing']}")
    acc = correct / total if total else 0.0
    print(f"字段准确率 {acc:.1%}（{correct}/{total}）")
    return 1 if invalid or not total or (min_accuracy is not None and acc < min_accuracy) else 0


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="mq-corpus")
    sub = ap.add_subparsers(dest="cmd", required=True)
    c = sub.add_parser("check")
    c.add_argument("corpus")
    c.add_argument("--extractor")
    c.add_argument("--reviewed", action="store_true", help="验证人工事实映射与证据，不评估解析器准确率")
    c.add_argument("--gold", choices=["reviewed"], help="以人工映射为黄金标注评测解析器")
    c.add_argument("--export", type=Path, help="人工映射全部通过后导出正例，用于现有契约测试")
    c.add_argument("--schema", required=True, action="append", help="人工语料可重复指定，以验证各Schema分组")
    c.add_argument("--path", action="append", default=[], help="解析器所在的导入路径（可重复）")
    c.add_argument("--min-accuracy", type=float)
    a = ap.parse_args(argv)
    for p in a.path:
        sys.path.insert(0, str(Path(p).resolve()))
    if a.reviewed and (a.extractor or a.min_accuracy is not None):
        ap.error("--reviewed不能与extractor/min-accuracy混用")
    if a.export is not None and not a.reviewed:
        ap.error("--export只用于人工契约映射")
    if not a.reviewed and not a.extractor:
        ap.error("自动解析评测需要--extractor")
    if a.gold and (a.reviewed or not a.extractor):
        ap.error("--gold reviewed 需要--extractor，且不能与--reviewed混用")
    if not a.reviewed and len(a.schema) > 1:
        ap.error("多个Schema只用于按元数据分组的人工映射")
    gold = {"gold_reviewed": True} if a.gold == "reviewed" else {}
    return max(check(Path(a.corpus), a.extractor, schema, a.min_accuracy, a.reviewed, a.export, **gold)
               for schema in a.schema)


if __name__ == "__main__":
    raise SystemExit(main())
