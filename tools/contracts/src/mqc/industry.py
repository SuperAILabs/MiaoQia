"""行业包一致性检查（格式见 contracts/packs/industry.schema.json，语义见 docs/03-architecture/04-extensibility.md 2.4）。

  I1  pack.yaml 的种类与标识合规，provides 只登记已知种类，定义文件存在并符合格式；品类体系为 industry:<包目录名>
  I2  品类代码唯一、上级存在；category.any_match 只比较代码，因此全部行业包之间代码不重复、互不为上下级
  I3  词表、规格、映射、品牌与资质模板引用的品类存在；产品词与同义词不重复对应两个品类；标签代码唯一
  I4  资质代码唯一；默认词表展开后的词数不超过 CEL 规模上界（contracts/policy/cel-environment.yaml）
  I5  应用领域包策略模板引用的 vocab.<名> 由某个行业包提供；模板样例中行业体系的品类代码在对应行业包中存在
"""
from __future__ import annotations

import json
import re
from pathlib import Path

from jsonschema import Draft202012Validator

from .repo import Repo, load_yaml
from .report import Report

KINDS = {"taxonomy": "taxonomy", "vocabulary": "vocabulary", "qualifications": "qualifications", "defaults": "defaults"}
PACK_ID = re.compile(r"^IND-[A-Z]+(-[A-Z]+)*$")
VOCAB_REF = re.compile(r"vocab\.([A-Za-z0-9_]+)")


def _validator(repo: Repo) -> Draft202012Validator:
    schema = json.loads((repo.contracts / "packs" / "industry.schema.json").read_text(encoding="utf-8"))
    Draft202012Validator.check_schema(schema)
    return Draft202012Validator(schema, format_checker=Draft202012Validator.FORMAT_CHECKER)


def _parent(code: str) -> str | None:
    head, _, _ = code.rpartition("-")
    return head if head != "CAT" else None


def check_industry_pack(repo: Repo, pack_dir: Path, v: Draft202012Validator) -> tuple[Report, dict]:
    """返回报告与本包的声明摘要（品类代码、词表名），供跨包检查。"""
    r = Report()
    rel = lambda f: str((pack_dir / f).relative_to(repo.root))
    pack = load_yaml(pack_dir / "pack.yaml")["pack"]
    out = {"id": pack.get("id"), "codes": set(), "vocab": {}, "scheme": None}
    if not PACK_ID.match(str(pack.get("id", ""))):
        r.error(rel("pack.yaml"), f"I1 行业包标识 {pack.get('id')!r} 不合法（IND-<大写>）")
    prov = pack.get("provides", {})
    for k in prov:
        if k not in KINDS:
            r.error(rel("pack.yaml"), f"I1 provides.{k} 不是行业包的提供项（{'、'.join(KINDS)}）")
    if "taxonomy" not in prov:
        r.error(rel("pack.yaml"), "I1 行业包必须登记品类分类 provides.taxonomy")
    docs = {}
    for k, key in KINDS.items():
        f = prov.get(k)
        if f is None:
            continue
        if not (pack_dir / f).exists():
            r.error(rel(f), "I1 定义文件不存在")
            continue
        doc = load_yaml(pack_dir / f)
        errs = sorted(v.iter_errors(doc), key=lambda e: list(e.absolute_path))
        for e in errs:
            r.error(rel(f), f"I1 /{'/'.join(map(str, e.absolute_path))}：{e.message}")
        if not errs and key not in doc:
            r.error(rel(f), f"I1 顶层键应为 {key}")
        elif not errs:
            docs[k] = (f, doc[key])

    codes: set[str] = set()
    if "taxonomy" in docs:
        f, t = docs["taxonomy"]
        out["scheme"] = t["scheme"]
        if t["scheme"] != f"industry:{pack_dir.name}":
            r.error(rel(f), f"I1 品类体系应为 industry:{pack_dir.name}")
        for c in t["categories"]:
            if c["code"] in codes:
                r.error(rel(f), f"I2 品类代码 {c['code']} 重复")
            codes.add(c["code"])
        for c in t["categories"]:
            p = _parent(c["code"])
            if p and p not in codes:
                r.error(rel(f), f"I2 品类 {c['code']} 的上级 {p} 不存在")
        for m in t.get("mappings", []):
            for e in m.get("entries", []):
                if e["code"] not in codes:
                    r.error(rel(f), f"I3 映射 {m['scheme']} 引用不存在的品类 {e['code']}")
    out["codes"] = codes

    def cats(f: str, label: str, xs) -> None:
        for x in xs:
            if x not in codes:
                r.error(rel(f), f"I3 {label} 引用不存在的品类 {x}")

    if "vocabulary" in docs:
        f, voc = docs["vocabulary"]
        seen: dict[str, str] = {}
        for p in voc["products"]:
            cats(f, f"产品词 {p['term']}", [p["category"]])
            for w in [p["term"], *p.get("synonyms", [])]:
                if w in seen and seen[w] != p["category"]:
                    r.error(rel(f), f"I3 产品词 {w} 同时对应 {seen[w]} 与 {p['category']}")
                seen[w] = p["category"]
        for s in voc["specs"]:
            cats(f, f"规格词 {s['term']}", s.get("categories", []))
        tags = [t["code"] for t in voc["tags"]]
        if len(tags) != len(set(tags)):
            r.error(rel(f), "I3 标签代码重复")
        for b in voc["brands"]:
            cats(f, f"品牌 {b['name']}", b["categories"])
        if not voc["brands"] and "brands_pending" not in voc:
            r.error(rel(f), "I3 品牌词表为空时须以 brands_pending 登记交付任务")
    if "qualifications" in docs:
        f, q = docs["qualifications"]
        qc = [t["code"] for t in q["templates"]]
        if len(qc) != len(set(qc)):
            r.error(rel(f), "I4 资质代码重复")
        for t in q["templates"]:
            cats(f, f"资质 {t['code']}", t.get("categories", []))
    if "defaults" in docs:
        f, dft = docs["defaults"]
        bound = _vocab_bound(repo)
        for name, lst in dft["vocab"].items():
            words = {w for g in lst["groups"] for w in g["terms"]}
            out["vocab"][name] = words
            if bound is not None and len(words) > bound:
                r.error(rel(f), f"I4 词表 {name} 有 {len(words)} 个词，超过 CEL 规模上界 {bound}")
    return r, out


def _vocab_bound(repo: Repo) -> int | None:
    env = repo.contracts / "policy" / "cel-environment.yaml"
    if not env.exists():
        return None
    return load_yaml(env).get("size_bounds", {}).get("paths", {}).get("vocab")


def check_industries(repo: Repo) -> Report:
    r = Report()
    v = _validator(repo)
    infos = []
    for pf in sorted((repo.root / "packs").glob("*/*/pack.yaml")):
        if load_yaml(pf)["pack"].get("kind") != "industry":
            continue
        rep, info = check_industry_pack(repo, pf.parent, v)
        r.extend(rep)
        info["dir"] = pf.parent
        infos.append(info)
    # I2 跨包：代码不重复、互不为上下级
    for i, a in enumerate(infos):
        for b in infos[i + 1:]:
            for x in a["codes"]:
                for y in b["codes"]:
                    if x == y or y.startswith(x + "-") or x.startswith(y + "-"):
                        r.error(str(b["dir"].relative_to(repo.root)),
                                f"I2 品类 {y} 与 {a['id']} 的 {x} 重复或互为上下级（category.any_match 不区分行业体系）")
    # I5 应用领域包的词表引用与样例品类
    vocab = set().union(*(i["vocab"] for i in infos)) if infos else set()
    by_scheme = {i["scheme"]: i["codes"] for i in infos if i["scheme"]}
    for pf in sorted((repo.root / "packs").glob("*/*/pack.yaml")):
        pack = load_yaml(pf)["pack"]
        if pack.get("kind") != "application_domain":
            continue
        for tf in pack.get("provides", {}).get("policy", {}).get("templates", []):
            path = pf.parent / tf
            where = str(path.relative_to(repo.root))
            for name in sorted(set(VOCAB_REF.findall(path.read_text(encoding="utf-8")))):
                if name not in vocab:
                    r.error(where, f"I5 词表 vocab.{name} 没有行业包提供")
            for ex in load_yaml(path)["template"].get("examples", []):
                for c in _categories_in(ex.get("given", {}).get("fact", {})):
                    codes = by_scheme.get(c.get("scheme"))
                    if codes is not None and c.get("code") not in codes:
                        r.error(where, f"I5 样例「{ex.get('name')}」的品类 {c.get('code')} 不在 {c.get('scheme')} 中")
    r.note(f"行业包检查：{len(infos)} 个包")
    return r


def _categories_in(node):
    if isinstance(node, dict):
        if isinstance(node.get("scheme"), str) and node["scheme"].startswith("industry:") and "code" in node:
            yield node
        for v in node.values():
            yield from _categories_in(v)
    elif isinstance(node, list):
        for v in node:
            yield from _categories_in(v)
