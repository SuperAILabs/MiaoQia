import shutil
import subprocess
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[3]


def git(cwd: Path, *args: str) -> None:
    subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True)


@pytest.fixture
def sandbox(tmp_path: Path) -> Path:
    """仓库 contracts/ 与 packs/ 的副本，已作为基线提交（HEAD）。"""
    for d in ("contracts", "packs"):
        shutil.copytree(REPO / d, tmp_path / d)
    git(tmp_path, "init", "-q")
    git(tmp_path, "-c", "user.email=t@t", "-c", "user.name=t", "add", "-A")
    git(tmp_path, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "base")
    return tmp_path
