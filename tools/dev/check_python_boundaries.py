"""R4 信息领域包静态导入；R3 SDK、kernel和生成契约不能反向静态依赖领域包。

只检查静态源码，不证明动态加载、网络或数据库访问隔离。
"""
from __future__ import annotations

import ast
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

# R4“被扩展子系统”：按包种类取 docs/03-architecture/04-extensibility.md 第 1 节表中的 Python 子系统。
EXTENDED_SUBSYSTEMS = {
    "information_domain": ("acquisition", "evidence", "domain-intelligence", "entity-graph"),
}


def check_source(source: str, package: str, apis: set[str], *, sdk=False, pack_roots=frozenset()) -> list[str]:
    errors = []
    for node in ast.walk(ast.parse(source)):
        if isinstance(node, ast.Import):
            names = [alias.name for alias in node.names]
        elif isinstance(node, ast.ImportFrom):
            if node.level:
                if node.level > len(package.split(".")):
                    errors.append(f"{node.lineno}: 相对导入越过本包")
                continue  # 本包内相对导入；其目标文件同样接受检查。
            names = [node.module or ""]
            if node.module == "miaoqia" or any(api.rsplit(".", 1)[0] == node.module for api in apis):
                names = [f"{node.module}.{alias.name}" for alias in node.names]
        else:
            continue
        for name in names:
            if sdk:
                bad = name.startswith("miaoqia_pack_") or name.startswith("miaoqia.miaoqia_pack_") or any(name == root or name.startswith(root + ".") for root in pack_roots)
            else:
                allowed = {package.split(".")[0], "miaoqia.sdk", *apis}
                bad = (name.split(".")[0] not in sys.stdlib_module_names
                       and not any(name == prefix or name.startswith(prefix + ".") for prefix in allowed))
            if bad:
                errors.append(f"{node.lineno}: {'R3' if sdk else 'R4'} 禁止导入 {name}")
    return errors


def check(root=ROOT) -> list[str]:
    import yaml
    registry = yaml.safe_load((root / "deploy/subsystems.yaml").read_text())["subsystems"]
    all_apis = {"miaoqia." + name.replace("-", "_") + ".api"
                for name, spec in registry.items() if spec["language"] == "python"}
    errors = []
    manifests = sorted((root / "packs/information_domains").glob("*/pack.yaml"))
    pack_roots = {path.relative_to(manifest.parent / "src").parts[0].removesuffix(".py")
                  for manifest in manifests for path in (manifest.parent / "src").rglob("*.py")}
    collisions = pack_roots & ({"miaoqia"} | sys.stdlib_module_names)
    for name in sorted(collisions):
        errors.append(f"R4 领域包根命名空间不得覆盖平台或标准库：{name}")
    for manifest in manifests:
        kind = yaml.safe_load(manifest.read_text())["pack"].get("kind")
        extended = EXTENDED_SUBSYSTEMS.get(kind)
        if extended is None or any(registry.get(name, {}).get("language") != "python" for name in extended):
            errors.append(f"{manifest.relative_to(root)}: R4 未登记包种类 {kind!r} 的被扩展 Python 子系统")
            continue
        apis = {"miaoqia." + name.replace("-", "_") + ".api" for name in extended}
        for path in sorted((manifest.parent / "src").rglob("*.py")):
            parts = path.relative_to(manifest.parent / "src").with_suffix("").parts
            package = ".".join(parts[:-1])
            for error in check_source(path.read_text(), package, apis):
                errors.append(f"{path.relative_to(root)}:{error}")
    for shared in ("sdk", "kernel", "contracts"):
        for path in (root / f"data/{shared}/src").rglob("*.py"):
            for error in check_source(path.read_text(), f"miaoqia.{shared}", all_apis, sdk=True, pack_roots=pack_roots):
                errors.append(f"{path.relative_to(root)}:{error}")
    return errors


if __name__ == "__main__":
    errors = check()
    for error in errors:
        print(error, file=sys.stderr)
    print(f"Python R3/R4 静态边界：{len(errors)} 个违规")
    raise SystemExit(bool(errors))
