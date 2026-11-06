"""工程横切检查；API样例复用Schema/事件的正反例执行器。"""
import re
from .repo import load_yaml
from .report import Report
from .fixtures import check_cases

UNVERSIONED = "0.0.0"   # OpenAPI/AsyncAPI 规范要求 info.version；开发期接口不设版本号，统一取此值
from jsonschema import Draft202012Validator


def check_integration(repo):
    r = Report()
    ids = load_yaml(repo.contracts / "identifiers.yaml")
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
    for name, doc in [("identifiers.yaml", ids), ("integration.yaml", profile), ("storage/kernel.yaml", storage)]:
        if "version" in doc:
            r.error(name, "I1 工程契约在开发期不设版本号")
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
        if doc.get("openapi") != "3.1.0":
            r.error(str(path), "A1 OpenAPI方言无效")
        if str(doc.get("info", {}).get("version")) != UNVERSIONED:
            r.error(str(path), f"A1 开发期接口不设版本号，info.version 须为 {UNVERSIONED}")
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
    for path in sorted((repo.contracts / "events").glob("*/*.asyncapi.yaml")):
        if str(load_yaml(path).get("info", {}).get("version")) != UNVERSIONED:
            r.error(str(path), f"E0 开发期事件文档不设版本号，info.version 须为 {UNVERSIONED}；事件类型主版本由 .v<n> 表达")
    r.note(f"工程横切检查：标识、集成配置、SQL副本；API正反例 {total} 个")
    return r
