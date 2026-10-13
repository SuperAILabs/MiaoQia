"""语料：用真实来源样本检验解析器与领域 Schema。

目录结构（位于信息领域包内，例如 packs/information_domains/procurement/corpus/）：

  <样本编号>/
    meta.yaml       source_url、source_id、fetched_at、rights_tag、redacted（必须为 true）
    raw.html        已脱敏的原始页面
    expected.yaml   标注：facts: [{fact_type, fields: {JSON Pointer: 期望值}}]

命令：
  mq-corpus redact IN -o OUT        脱敏：手机号、带标签的电话与传真、邮箱、身份证号、带标签的联系人姓名
  mq-corpus lint DIR...             语料中不得残留个人信息，meta.redacted 必须为 true
  mq-corpus check DIR --extractor 模块:类 --schema 相对 contracts/schemas 的路径 [--min-accuracy 0.95]
                                    规范化 → 解析 → Schema 校验 → 与标注比对，输出逐字段准确率
"""
from __future__ import annotations

import argparse
import importlib
import json
import re
import sys
from collections import defaultdict
from pathlib import Path

import yaml
from jsonschema import Draft202012Validator
from referencing import Registry, Resource

from .document import Document
from .extraction import ExtractionContext
from .testing import html_to_document

# ---------------- 脱敏 ----------------
LABEL_PHONE = re.compile(r"((?:联系)?电话|联系方式|传真|手机|座机)(\s*[：:]\s*)([0-9（）()\-－—转、/\s]{5,40})")
LABEL_NAME = re.compile(r"((?:项目)?联系人|负责人|经办人|联系人姓名)(\s*[：:]\s*)(?!某某)([一-龥·]{2,4})(?=[\s，,；;。（(]|$|先生|女士|老师|工)")
MOBILE = re.compile(r"(?<!\d)1[3-9]\d{9}(?!\d)")
EMAIL = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")
ID_CARD = re.compile(r"(?<![0-9A-Za-z])\d{17}[\dXx](?![0-9A-Za-z])")
MASK = "＊"


def redact(text: str) -> tuple[str, int]:
    n = 0

    def sub(pattern, repl, s):
        nonlocal n
        s2, k = pattern.subn(repl, s)
        n += k
        return s2

    text = sub(LABEL_PHONE, lambda m: m.group(1) + m.group(2) + MASK * 4, text)
    text = sub(LABEL_NAME, lambda m: m.group(1) + m.group(2) + "某某", text)
    text = sub(MOBILE, MASK * 4, text)
    text = sub(EMAIL, MASK * 4, text)
    text = sub(ID_CARD, MASK * 4, text)
    return text, n


def lint(dirs: list[Path]) -> list[str]:
    problems = []
    for d in dirs:
        for sample in sorted(p for p in d.iterdir() if p.is_dir()):
            meta = yaml.safe_load((sample / "meta.yaml").read_text(encoding="utf-8")) if (sample / "meta.yaml").exists() else {}
            if meta.get("redacted") is not True:
                problems.append(f"{sample}: meta.yaml 缺少 redacted: true")
            for f in sample.glob("raw.*"):
                text = f.read_text(encoding="utf-8", errors="replace")
                _, residual = redact(text)
                if residual:
                    problems.append(f"{f}: 发现 {residual} 处疑似个人信息，请先运行 mq-corpus redact")
    return problems


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


def check(corpus: Path, extractor_ref: str, schema_rel: str, min_accuracy: float | None) -> int:
    mod, _, cls = extractor_ref.partition(":")
    extractor = getattr(importlib.import_module(mod), cls)()
    validator, schema = _validator(schema_rel)
    field_stats = defaultdict(lambda: {"correct": 0, "wrong": 0, "missing": 0})
    invalid = 0
    samples = sorted(p for p in corpus.iterdir() if p.is_dir())
    for sample in samples:
        raw = next(sample.glob("raw.*")).read_text(encoding="utf-8")
        meta = yaml.safe_load((sample / "meta.yaml").read_text(encoding="utf-8"))
        expected = yaml.safe_load((sample / "expected.yaml").read_text(encoding="utf-8"))
        doc = Document(html_to_document(raw, "snp_" + "0" * 26, meta.get("source_url")))
        ctx = ExtractionContext(doc, f"{extractor.id}@{extractor.version}")
        result = extractor.extract(doc, ctx)
        for i, exp in enumerate(expected["facts"]):
            got = result.facts[i] if i < len(result.facts) else {}
            full = {**_placeholder_meta(schema, sample.name), **got}
            errs = list(validator.iter_errors(full))
            if errs:
                invalid += 1
                print(f"✗ {sample.name}#{i} 不符合 Schema：{errs[0].json_path} {errs[0].message[:160]}")
            for pointer, want in exp["fields"].items():
                try:
                    val = _get(got, pointer)
                except (KeyError, IndexError, ValueError, TypeError):
                    field_stats[pointer]["missing"] += 1
                    continue
                field_stats[pointer]["correct" if val == want else "wrong"] += 1
                if val != want:
                    print(f"  {sample.name}#{i} {pointer}：期望 {want!r}，实际 {val!r}")
    total = sum(sum(s.values()) for s in field_stats.values())
    correct = sum(s["correct"] for s in field_stats.values())
    print(f"\n{len(samples)} 个样本，Schema 不符 {invalid} 个")
    for pointer, s in sorted(field_stats.items()):
        n = sum(s.values())
        print(f"  {pointer:<48} {s['correct']}/{n}  错误 {s['wrong']}  缺失 {s['missing']}")
    acc = correct / total if total else 1.0
    print(f"字段准确率 {acc:.1%}（{correct}/{total}）")
    if invalid or (min_accuracy is not None and acc < min_accuracy):
        return 1
    return 0


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="mq-corpus")
    sub = ap.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("redact")
    r.add_argument("input")
    r.add_argument("-o", "--output", required=True)
    lt = sub.add_parser("lint")
    lt.add_argument("dirs", nargs="+")
    c = sub.add_parser("check")
    c.add_argument("corpus")
    c.add_argument("--extractor", required=True)
    c.add_argument("--schema", required=True)
    c.add_argument("--min-accuracy", type=float)
    a = ap.parse_args(argv)
    if a.cmd == "redact":
        text, n = redact(Path(a.input).read_text(encoding="utf-8", errors="replace"))
        Path(a.output).write_text(text, encoding="utf-8")
        print(f"已脱敏 {n} 处 → {a.output}")
        return 0
    if a.cmd == "lint":
        dirs = [Path(d) for d in a.dirs if Path(d).exists()]
        problems = lint(dirs)
        for p in problems:
            print(f"✗ {p}", file=sys.stderr)
        print(f"语料脱敏检查：{len(dirs)} 个目录，{len(problems)} 个问题")
        return 1 if problems else 0
    return check(Path(a.corpus), a.extractor, a.schema, a.min_accuracy)


if __name__ == "__main__":
    raise SystemExit(main())
