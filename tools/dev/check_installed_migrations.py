"""验证 Python 子系统的 Alembic 迁移随 wheel 分发：脱离源码目录安装后仍能定位并被 Alembic 读取。

  uv run --project data --locked python tools/dev/check_installed_migrations.py

构建工作区全部 wheel，按 data/uv.lock 的版本装入临时虚拟环境，再在临时目录以隔离模式（-I，不把仓库加入
导入路径）导入各子系统，确认 module().migrations 位于该虚拟环境内且 Alembic 能读取迁移脚本。不连接数据库，
不执行迁移本身。
"""
from __future__ import annotations

import importlib
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
DATA = ROOT / "data"


def subsystem_modules() -> list[str]:
    """data/subsystems 下每个工作区成员对应的顶层模块（目录名 - 换为 _）。"""
    return ["miaoqia." + p.parent.name.replace("-", "_")
            for p in sorted((DATA / "subsystems").glob("*/pyproject.toml"))]


def check_installed(prefix: Path, modules: list[str]) -> list[str]:
    """在已安装环境中运行：迁移目录须在 prefix 内，且含 env.py、模板与 versions/。"""
    from alembic.script import ScriptDirectory

    errors = []
    for name in modules:
        path = Path(importlib.import_module(name).module().migrations).resolve()
        if not path.is_relative_to(prefix):
            errors.append(f"{name}: 迁移目录不在安装环境内：{path}")
            continue
        missing = [f for f in ("env.py", "script.py.mako", "versions") if not (path / f).exists()]
        if missing:
            errors.append(f"{name}: 安装后的迁移目录缺少 {', '.join(missing)}：{path}")
            continue
        heads = ScriptDirectory(str(path)).get_heads()
        print(f"{name}: {path}（heads={heads or '无'}）")
    return errors


def main() -> int:
    with tempfile.TemporaryDirectory() as tmp:
        tmp = Path(tmp)
        wheels, venv, constraints = tmp / "wheels", tmp / "venv", tmp / "constraints.txt"
        subprocess.run(["uv", "build", "--all-packages", "--wheel", "-q", "-o", str(wheels)], cwd=DATA, check=True)
        subprocess.run(["uv", "export", "--locked", "--all-packages", "--no-emit-workspace", "--no-dev",
                        "--no-hashes", "-q", "-o", str(constraints)], cwd=DATA, check=True)
        subprocess.run(["uv", "venv", "-q", "--python", "3.12", str(venv)], check=True)
        python = venv / "bin" / "python"
        # 工作区包按本地 wheel 文件安装，不从索引解析同名包。
        subprocess.run(["uv", "pip", "install", "-q", "--python", str(python), "-c", str(constraints),
                        *map(str, sorted(wheels.glob("*.whl")))], check=True)
        result = subprocess.run([str(python), "-I", str(Path(__file__).resolve()), "--installed", str(venv),
                                 *subsystem_modules()], cwd=tmp)
        return result.returncode


if __name__ == "__main__":
    if sys.argv[1:2] == ["--installed"]:
        errors = check_installed(Path(sys.argv[2]).resolve(), sys.argv[3:])
        for error in errors:
            print(error, file=sys.stderr)
        raise SystemExit(bool(errors))
    raise SystemExit(main())
