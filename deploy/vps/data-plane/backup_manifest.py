"""Bind a frozen public data recovery bundle to an independently recorded digest."""

import argparse
import hashlib
import json
import os
import re
import stat
import sys
from datetime import datetime, timezone


DATABASES = ("acq", "evd", "dom", "ent", "sea_public")
REQUIRED_FILES = {
    *(f"db/{database}.dump" for database in DATABASES),
    "roles.review.sql",
    "db-metadata.json",
    "kafka-checkpoint.json",
    "object-inventory.json",
    "archive-source.json",
    "archive-copy.json",
}
OBJECT_TREES = {"objects", "versions"}
REQUIRED_DIRECTORIES = {"db", *OBJECT_TREES}
FILE_FLAGS = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK
CHUNK_SIZE = 1024**2
SHA256 = re.compile(r"[0-9a-f]{64}")
APPROVAL = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}")


class VerificationError(Exception):
    pass


def open_regular(name, *, dir_fd=None):
    fd = os.open(name, FILE_FLAGS, dir_fd=dir_fd)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
            raise VerificationError("unsafe_file")
        return os.fdopen(fd, "rb")
    except BaseException:
        os.close(fd)
        raise


def file_state(info):
    return (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns)


def hash_file(name, directory_fd):
    checksum = hashlib.sha256()
    length = 0
    with open_regular(name, dir_fd=directory_fd) as stream:
        before = os.fstat(stream.fileno())
        for chunk in iter(lambda: stream.read(CHUNK_SIZE), b""):
            checksum.update(chunk)
            length += len(chunk)
        after = os.stat(name, dir_fd=directory_fd, follow_symlinks=False)
        if length != before.st_size or file_state(before) != file_state(after):
            raise VerificationError("file_changed")
    return {"size_bytes": length, "sha256": checksum.hexdigest()}


def raise_walk_error(error):
    raise error


def inventory(root_fd):
    directories = set()
    files = {}
    for directory, children, names, fd in os.fwalk(".", dir_fd=root_fd, onerror=raise_walk_error,
                                                    follow_symlinks=False):
        relative = directory.removeprefix("./")
        for name in children:
            path = name if relative == "." else f"{relative}/{name}"
            if not stat.S_ISDIR(os.stat(name, dir_fd=fd, follow_symlinks=False).st_mode):
                raise VerificationError("unsafe_directory")
            if path not in REQUIRED_DIRECTORIES and path.split("/", 1)[0] not in OBJECT_TREES:
                raise VerificationError("unexpected_directory")
            directories.add(path)
        for name in names:
            path = name if relative == "." else f"{relative}/{name}"
            if path not in REQUIRED_FILES and path.split("/", 1)[0] not in OBJECT_TREES:
                raise VerificationError("unexpected_file")
            files[path] = hash_file(name, fd)
    if not REQUIRED_DIRECTORIES <= directories or not REQUIRED_FILES <= files.keys():
        raise VerificationError("missing_bundle_entry")
    if any(files[path]["size_bytes"] == 0 for path in REQUIRED_FILES):
        raise VerificationError("empty_required_file")
    return sorted(directories), files


def check_receipts(root_fd, files, cutoff):
    source = files["archive-source.json"]
    if source != files["archive-copy.json"]:
        raise VerificationError("archive_receipts_differ")
    with open_regular("archive-source.json", dir_fd=root_fd) as stream:
        raw = stream.read(source["size_bytes"] + 1)
    if hashlib.sha256(raw).hexdigest() != source["sha256"]:
        raise VerificationError("file_changed")
    receipt = json.loads(raw)
    if (not isinstance(receipt, dict) or type(receipt.get("schema_version")) is not int
            or receipt["schema_version"] != 1 or receipt.get("status") != "verified"
            or receipt.get("cutoff") != cutoff or not isinstance(receipt.get("sections"), dict)
            or not receipt["sections"] or not isinstance(receipt.get("discovery_policy"), dict)):
        raise VerificationError("archive_receipt_invalid")


def make_manifest(root, cutoff, release, approval):
    timestamp = datetime.fromisoformat(cutoff)
    if timestamp.utcoffset() is None or not SHA256.fullmatch(release) or not APPROVAL.fullmatch(approval):
        raise VerificationError("invalid_recovery_metadata")
    cutoff = timestamp.astimezone(timezone.utc).isoformat()
    root_fd = os.open(os.path.normpath(root), os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        directories, files = inventory(root_fd)
        check_receipts(root_fd, files, cutoff)
    finally:
        os.close(root_fd)
    return {
        "schema_version": 1,
        "cutoff": cutoff,
        "release_sha256": release,
        "approval": approval,
        "directories": directories,
        "files": files,
    }


def encode_manifest(manifest):
    return (json.dumps(manifest, sort_keys=True, indent=2) + "\n").encode("ascii")


def verify_manifest(root, manifest_path, expected_sha256):
    if not SHA256.fullmatch(expected_sha256):
        raise VerificationError("invalid_expected_sha256")
    with open_regular(manifest_path) as stream:
        raw = stream.read()
    if hashlib.sha256(raw).hexdigest() != expected_sha256:
        raise VerificationError("manifest_digest_mismatch")
    expected = json.loads(raw)
    if not isinstance(expected, dict) or expected.get("schema_version") != 1:
        raise VerificationError("invalid_manifest")
    metadata = tuple(expected.get(key) for key in ("cutoff", "release_sha256", "approval"))
    if not all(isinstance(value, str) for value in metadata):
        raise VerificationError("invalid_recovery_metadata")
    actual = make_manifest(root, *metadata)
    if encode_manifest(actual) != raw:
        raise VerificationError("bundle_manifest_mismatch")
    return {
        "status": "verified",
        "manifest_sha256": expected_sha256,
        "cutoff": actual["cutoff"],
        "release_sha256": actual["release_sha256"],
        "approval": actual["approval"],
        "files": len(actual["files"]),
        "bytes": sum(file["size_bytes"] for file in actual["files"].values()),
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    create = commands.add_parser("create")
    create.add_argument("--root", required=True)
    create.add_argument("--cutoff", required=True)
    create.add_argument("--release-sha256", required=True)
    create.add_argument("--approval", required=True)
    verify = commands.add_parser("verify")
    verify.add_argument("--root", required=True)
    verify.add_argument("--manifest", required=True)
    verify.add_argument("--expected-sha256", required=True)
    options = parser.parse_args()
    try:
        if options.command == "create":
            manifest = make_manifest(options.root, options.cutoff, options.release_sha256, options.approval)
            sys.stdout.buffer.write(encode_manifest(manifest))
        else:
            receipt = verify_manifest(options.root, options.manifest, options.expected_sha256)
            print(json.dumps(receipt, sort_keys=True))
    except VerificationError as exc:
        print(json.dumps({"status": "failed", "reason": str(exc)}), file=sys.stderr)
        return 1
    except (OSError, ValueError, OverflowError, RecursionError):
        print('{"status":"failed","reason":"bundle_or_manifest_unreadable"}', file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
