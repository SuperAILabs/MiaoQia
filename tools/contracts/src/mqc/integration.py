"""工程横切检查；API样例复用Schema/事件的正反例执行器。"""
import re
from .repo import load_yaml
from .report import Report
from .fixtures import check_cases
from .lint import SEMVER
from jsonschema import Draft202012Validator


def check_integration(repo):
    r = Report()
    ids = load_yaml(repo.contracts / "identifiers.yaml")
    if not SEMVER.fullmatch(str(ids.get("version", ""))):
        r.error("identifiers.yaml", "I1 缺少标识登记版本")
    for prefix, entry in ids["prefixes"].items():
        if not prefix.isalpha() or not prefix.islower() or not entry.get("object") or not entry.get("owner"):
            r.error("identifiers.yaml", f"I1 非法登记 {prefix}")
    for directory in ("services", "data"):
        for path in (repo.root / directory).rglob("*"):
            if path.suffix not in {".go", ".py"} or ".venv" in path.parts:
                continue
            for prefix in re.findall(r'ids\.(?:New|new)\(["\']([a-z]+)["\']\)', path.read_text()):
                if prefix not in ids["prefixes"]:
                    r.error(str(path), f"I4 代码分配未登记前缀 {prefix}")
    profile = load_yaml(repo.contracts / "integration.yaml")
    storage = load_yaml(repo.contracts / "storage/kernel.yaml")
    for name, doc in [("integration.yaml", profile), ("storage/kernel.yaml", storage)]:
        if not SEMVER.fullmatch(str(doc.get("version", ""))):
            r.error(name, "I1 缺少工程契约版本")
    for group in ("context", "envelope"):
        if not (repo.contracts / profile[group]["schema"]).exists():
            r.error("integration.yaml", f"I2 {group} Schema不存在")
    sql = (repo.contracts / "storage" / storage["definition"]).read_bytes()
    # 兼容测试沙箱只复制contracts/与packs/；实际工作区核验两语言SQL副本。
    for copy in storage["copies"]:
        if (repo.root / copy).exists() and (repo.root / copy).read_bytes() != sql:
            r.error(copy, "I3 内核SQL与契约不一致")
    total = 0
    for path in sorted((repo.contracts / "apis").glob("*/openapi.yaml")):
        doc = load_yaml(path)
        if doc.get("openapi") != "3.1.0" or not SEMVER.fullmatch(str(doc.get("info", {}).get("version", ""))):
            r.error(str(path), "A1 OpenAPI方言或版本无效")
        components = doc.get("components", {})
        specs = load_yaml(path.parent / "fixtures.yaml")["fixtures"]
        covered = set()
        for spec in specs:
            covered.add(spec["schema"])
            schema = {"components": components, **components["schemas"][spec["schema"]]}
            Draft202012Validator.check_schema(schema)
            validator = Draft202012Validator(schema, format_checker=Draft202012Validator.FORMAT_CHECKER)
            total += check_cases(validator, spec, str(path), r)
            if not any(not case["valid"] for case in spec.get("cases", [])):
                r.error(str(path), f"A2 {spec['schema']}缺少反例")
        if set(components["schemas"]) - covered:
            r.error(str(path), "A2 API Schema缺少样例")
        for route, item in doc["paths"].items():
            for verb, operation in item.items():
                if verb not in {"get", "post", "put", "delete", "patch"}:
                    continue
                if not operation.get("x-timeout-ms") or not isinstance(operation.get("x-idempotent"), bool):
                    r.error(str(path), f"A3 {route}缺少超时或幂等声明")
    r.note(f"工程横切检查：标识、集成配置、SQL副本；API正反例 {total} 个")
    return r
