"""Regression controls for the sealed recovery bundle file layout."""

import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

import pytest


TOOL = Path(__file__).resolve().parents[2] / "deploy/vps/data-plane/backup_manifest.py"
CUTOFF = "2026-11-20T08:00:00+00:00"


def manifest_command(*args):
    return subprocess.run([sys.executable, TOOL, *args], capture_output=True, timeout=10)


@pytest.fixture
def sealed_bundle(tmp_path):
    root = tmp_path / "bundle"
    for directory in ("db", "objects/raw", "versions"):
        (root / directory).mkdir(parents=True)
    for database in ("acq", "evd", "dom", "ent", "sea_public"):
        (root / "db" / f"{database}.dump").write_bytes(b"example dump bytes")
    for name in ("roles.review.sql", "db-metadata.json", "kafka-checkpoint.json", "object-inventory.json"):
        (root / name).write_bytes(b"{}")
    receipt = json.dumps({
        "status": "verified", "schema_version": 1, "cutoff": CUTOFF,
        "sections": {"acq_original": {}}, "discovery_policy": {},
    })
    for name in ("archive-source.json", "archive-copy.json"):
        (root / name).write_text(receipt)
    (root / "objects/raw/fixture").write_bytes(b"original")
    command = ("create", "--root", root, "--cutoff", CUTOFF,
               "--release-sha256", "a" * 64, "--approval", "C11-fixture")
    result = manifest_command(*command)
    assert result.returncode == 0, result.stderr
    manifest = tmp_path / "manifest.json"
    manifest.write_bytes(result.stdout)
    copied = tmp_path / "copied"
    shutil.copytree(root, copied)
    digest = hashlib.sha256(result.stdout).hexdigest()
    return root, copied, ("verify", "--manifest", manifest, "--expected-sha256", digest), command


def test_detached_copy_passes(sealed_bundle):
    _, copied, verify, _ = sealed_bundle
    result = manifest_command(*verify, "--root", copied)
    assert result.returncode == 0, result.stderr
    assert json.loads(result.stdout)["files"] == 12


@pytest.mark.parametrize("change", ["file_link", "directory_link", "root_link", "hardlink", "extra_dir", "fifo"])
def test_recovery_rejects_unsafe_entries(sealed_bundle, tmp_path, change):
    root, copied, verify, _ = sealed_bundle
    obj = copied / "objects/raw/fixture"
    source = root / "objects/raw/fixture"
    target = copied
    if change == "directory_link":
        shutil.rmtree(obj.parent)
        obj.parent.symlink_to(source.parent, target_is_directory=True)
    elif change == "root_link":
        alias = tmp_path / "alias"
        alias.symlink_to(copied, target_is_directory=True)
        target = f"{alias}/"
    elif change == "extra_dir":
        (obj.parent / "extra").mkdir()
    else:
        obj.unlink()
        if change == "file_link":
            obj.symlink_to(source)
        elif change == "hardlink":
            os.link(source, obj)
        else:
            os.mkfifo(obj)
    result = manifest_command(*verify, "--root", target)
    assert result.returncode == 1
    assert not result.stdout
    assert json.loads(result.stderr)["status"] == "failed"


def test_sealing_requires_successful_matching_archive_receipts(sealed_bundle):
    root, _, _, create = sealed_bundle
    wrong_cutoff = (root / "archive-source.json").read_text().replace(CUTOFF, "2026-11-19T08:00:00+00:00")
    for name in ("archive-source.json", "archive-copy.json"):
        (root / name).write_text(wrong_cutoff)
    result = manifest_command(*create)
    assert result.returncode == 1
    assert json.loads(result.stderr)["reason"] == "archive_receipt_invalid"
