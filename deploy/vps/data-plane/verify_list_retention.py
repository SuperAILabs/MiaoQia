"""Read-only observation and verification of versioned discovery-object expiry."""

import argparse
from contextlib import closing
from datetime import datetime, timezone
from hashlib import sha256
import json
from pathlib import Path
import sys
import time

from botocore.exceptions import ClientError

from backup_manifest import SHA256, VerificationError, encode_manifest, open_regular
from miaoqia.kernel.objectstore import KEY, MAX_KEY, parse_s3_location, s3_client

BASELINE_BYTES = 1024 * 1024
MAX_PAGES = 64
MAX_OBJECT_BYTES = 64 * 1024**2
MISSING_OBJECT = {"404", "NoSuchKey", "NotFound"}


def require(condition, reason):
    if not condition:
        raise VerificationError(reason)


def instant(value):
    stamp = datetime.fromisoformat(value)
    require(stamp.utcoffset() is not None, "timezone_required")
    return stamp.astimezone(timezone.utc)


def version_id(value):
    return isinstance(value, str) and 0 < len(value) <= 1024 and not any(ord(char) < 32 for char in value)


class VersionedArchive:
    def __init__(self, client, store):
        self.client = client
        self.bucket, self.prefix = parse_s3_location(store)
        self.deadline = time.monotonic() + 120
        self.scope = sha256(encode_manifest([client.meta.endpoint_url, self.bucket, self.prefix])).hexdigest()

    def key(self, logical_key):
        require(isinstance(logical_key, str) and len(logical_key) <= MAX_KEY and KEY.fullmatch(logical_key),
                "invalid_object_key")
        return f"{self.prefix}/{logical_key}" if self.prefix else logical_key

    def check_deadline(self):
        require(time.monotonic() < self.deadline, "deadline_exceeded")

    def versioning(self):
        self.check_deadline()
        status = self.client.get_bucket_versioning(Bucket=self.bucket).get("Status")
        require(status in {"Enabled", "Suspended"}, "versioned_bucket_required")
        return status

    def inventory(self, logical_key):
        key = self.key(logical_key)
        self.check_deadline()
        pages = self.client.get_paginator("list_object_versions").paginate(
            Bucket=self.bucket, Prefix=key, PaginationConfig={"PageSize": 100},
        )
        versions = {}
        markers = set()
        for index, page in enumerate(pages, 1):
            self.check_deadline()
            truncated = page.get("IsTruncated")
            require(type(truncated) is bool, "unconfirmed_version_listing")
            for item in page.get("Versions", []):
                if item["Key"] == key:
                    version = item["VersionId"]
                    require(version_id(version) and version not in versions and version not in markers
                            and type(item.get("IsLatest")) is bool, "invalid_version_listing")
                    versions[version] = item["IsLatest"]
            for item in page.get("DeleteMarkers", []):
                if item["Key"] == key:
                    version = item["VersionId"]
                    require(version_id(version) and version not in markers and version not in versions,
                            "invalid_version_listing")
                    markers.add(version)
            if not truncated:
                return versions, markers
            require(index < MAX_PAGES, "version_listing_limit")
        raise VerificationError("incomplete_version_listing")

    def read(self, logical_key, version=None):
        self.check_deadline()
        args = {"Bucket": self.bucket, "Key": self.key(logical_key)}
        if version is not None:
            args["VersionId"] = version
        try:
            response = self.client.get_object(**args)
        except ClientError as exc:
            code = exc.response.get("Error", {}).get("Code")
            codes = MISSING_OBJECT | {"NoSuchVersion"} if version is not None else MISSING_OBJECT
            if exc.response.get("ResponseMetadata", {}).get("HTTPStatusCode") == 404 and code in codes:
                return None
            raise
        with closing(response["Body"]) as stream:
            actual_version = response.get("VersionId", "null")
            size = response["ContentLength"]
            require(version_id(actual_version) and (version is None or actual_version == version),
                    "object_version_mismatch")
            require(type(size) is int and 0 <= size <= MAX_OBJECT_BYTES, "object_read_limit")
            checksum = sha256()
            count = 0
            while True:
                self.check_deadline()
                chunk = stream.read(64 * 1024)
                if not chunk:
                    break
                count += len(chunk)
                require(count <= size, "object_length_mismatch")
                checksum.update(chunk)
            self.check_deadline()
            require(count == size, "object_length_mismatch")
        return {"version_id": actual_version, "bytes": count, "sha256": checksum.hexdigest()}


def control(archive, key):
    current = archive.read(key)
    require(current is not None, "control_missing")
    versions, _ = archive.inventory(key)
    require(versions.get(current["version_id"]) is True, "control_version_not_listed")
    require(archive.read(key, current["version_id"]) == current, "control_version_changed")
    return current


def validate_keys(discovery, controls):
    segments = discovery.split("/")
    require(len(segments) == 4 and segments[0] == "discovery" and segments[2].isascii()
            and segments[2].isdigit() and SHA256.fullmatch(segments[3]), "invalid_discovery_key")
    require(controls and len(controls) <= 16 and all(not key.startswith("discovery/") for key in controls),
            "invalid_control_keys")


def observe(archive, discovery, controls, expires, verify_after, now):
    validate_keys(discovery, controls)
    require(now < expires <= verify_after, "invalid_retention_times")
    require(len(controls) == len(set(controls)), "invalid_control_keys")
    mode = archive.versioning()
    current = archive.read(discovery)
    require(current is not None, "discovery_missing_before_expiry")
    checksum = discovery.rsplit("/", 1)[1]
    require(current["sha256"] == checksum, "discovery_hash_mismatch")
    versions, _ = archive.inventory(discovery)
    require(versions.get(current["version_id"]) is True, "discovery_version_not_listed")
    for version in versions:
        pinned = archive.read(discovery, version)
        require(pinned is not None and pinned["sha256"] == checksum, "discovery_version_mismatch")
    return {"schema_version": 1, "scope_sha256": archive.scope, "bucket_versioning": mode,
            "observed_at": now.isoformat(), "expires_at": expires.isoformat(), "verify_after": verify_after.isoformat(),
            "discovery_key": discovery, "discovery_versions": sorted(versions),
            "controls": {key: control(archive, key) for key in controls}}


def load_baseline(path, expected):
    require(SHA256.fullmatch(expected), "invalid_baseline_digest")
    with open_regular(path) as stream:
        raw = stream.read(BASELINE_BYTES + 1)
    require(len(raw) <= BASELINE_BYTES and sha256(raw).hexdigest() == expected, "baseline_digest_mismatch")
    baseline = json.loads(raw)
    require(isinstance(baseline, dict) and type(baseline.get("schema_version")) is int
            and baseline["schema_version"] == 1, "invalid_baseline")
    return baseline


def verify(archive, baseline, baseline_digest, now):
    observed, expires, after = (instant(baseline[key]) for key in ("observed_at", "expires_at", "verify_after"))
    require(observed < expires <= after, "invalid_retention_times")
    require(now >= after, "verification_too_early")
    require(archive.scope == baseline["scope_sha256"], "storage_scope_mismatch")
    require(archive.versioning() == baseline["bucket_versioning"], "bucket_versioning_changed")
    discovery = baseline["discovery_key"]
    controls = baseline["controls"]
    previously_seen = baseline["discovery_versions"]
    validate_keys(discovery, controls)
    require(isinstance(controls, dict) and isinstance(previously_seen, list) and previously_seen
            and all(version_id(item) for item in previously_seen), "invalid_baseline")
    versions, markers = archive.inventory(discovery)
    require(not versions and not markers, "discovery_versions_or_markers_retained")
    require(archive.read(discovery) is None, "discovery_current_still_readable")
    for version in previously_seen:
        require(archive.read(discovery, version) is None, "discovery_version_still_readable")
    for key, proof in controls.items():
        require(control(archive, key) == proof, "long_lived_control_changed")
    return {"status": "verified", "baseline_sha256": baseline_digest, "verified_at": now.isoformat(),
            "previous_versions_missing": len(previously_seen), "long_lived_controls": len(controls)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="phase", required=True)
    start = subparsers.add_parser("observe")
    start.add_argument("--store", required=True)
    start.add_argument("--discovery-key", required=True)
    start.add_argument("--control-key", action="append", required=True)
    start.add_argument("--expires-at", required=True)
    start.add_argument("--verify-after", required=True)
    finish = subparsers.add_parser("verify")
    finish.add_argument("--store", required=True)
    finish.add_argument("--baseline", type=Path, required=True)
    finish.add_argument("--expected-sha256", required=True)
    args = parser.parse_args()
    try:
        baseline = load_baseline(args.baseline, args.expected_sha256) if args.phase == "verify" else None
        now = datetime.now(timezone.utc)
        with closing(s3_client()) as client:
            archive = VersionedArchive(client, args.store)
            if args.phase == "observe":
                receipt = observe(archive, args.discovery_key, args.control_key, instant(args.expires_at),
                                  instant(args.verify_after), now)
            else:
                receipt = verify(archive, baseline, args.expected_sha256, now)
        sys.stdout.buffer.write(encode_manifest(receipt))
    except VerificationError as exc:
        print(json.dumps({"status": "failed", "reason": str(exc)}), file=sys.stderr)
        return 1
    except Exception:
        print('{"status":"failed","reason":"retention_evidence_unavailable"}', file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
