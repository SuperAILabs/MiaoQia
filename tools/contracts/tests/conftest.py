import shutil
import subprocess
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[3]


def git(cwd: Path, *args: str) -> None:
    subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True)


@pytest.fixture
def sandbox(tmp_path: Path) -> Path:
    """仓库 contracts/、packs/ 与阶段基线的副本，已作为基线提交（HEAD）。"""
    for d in ("contracts", "packs"):
        shutil.copytree(REPO / d, tmp_path / d)
    baseline = Path("docs/05-delivery/06-stage-baseline.md")   # P9 核对待定项引用的阶段基线任务
    (tmp_path / baseline).parent.mkdir(parents=True)
    shutil.copy(REPO / baseline, tmp_path / baseline)
    git(tmp_path, "init", "-q")
    git(tmp_path, "-c", "user.email=t@t", "-c", "user.name=t", "add", "-A")
    git(tmp_path, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "base")
    return tmp_path
