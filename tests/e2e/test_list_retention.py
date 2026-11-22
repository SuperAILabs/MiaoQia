"""Regression controls for read-only S3 discovery expiry verification."""

from datetime import datetime, timedelta, timezone
from hashlib import sha256
import importlib
from io import BytesIO
from pathlib import Path

import boto3
from botocore.exceptions import ClientError
from botocore.stub import Stubber
import pytest

TOOL_DIR = Path(__file__).resolve().parents[2] / "deploy" / "vps" / "data-plane"
CURRENT_TIME = datetime(2026, 11, 20, tzinfo=timezone.utc)
LIST_BODY = b"synthetic list"
RAW_BODY = b"synthetic archive"
LIST_KEY = f"discovery/source/1/{sha256(LIST_BODY).hexdigest()}"
RAW_KEY = f"raw/{sha256(RAW_BODY).hexdigest()}"
STORE = "s3://example-bucket/fixture"
BUCKET = "example-bucket"


@pytest.fixture
def s3(monkeypatch):
    monkeypatch.syspath_prepend(str(TOOL_DIR))
    verifier = importlib.import_module("verify_list_retention")
    client = boto3.client("s3", endpoint_url="http://127.0.0.1:1", region_name="us-east-1",
                          aws_access_key_id="fixture", aws_secret_access_key="fixture")
    with Stubber(client) as stubber:
        yield verifier, verifier.VersionedArchive(client, STORE), stubber
        stubber.assert_no_pending_responses()
    client.close()


def parameters(key, **extra):
    return {"Bucket": BUCKET, "Key": f"fixture/{key}", **extra}


def read(stubber, key, version, body, *, pinned=False):
    args = parameters(key, **({"VersionId": version} if pinned else {}))
    stubber.add_response("get_object", {"Body": BytesIO(body), "ContentLength": len(body),
                                       "VersionId": version}, args)


def list_page(stubber, key, *, versions=(), markers=(), truncated=False, **extra):
    args = {"Bucket": BUCKET, "Prefix": f"fixture/{key}", "MaxKeys": 100}
    if "marker" in extra:
        args.update(extra.pop("marker"))
    stubber.add_response("list_object_versions", {"Versions": list(versions), "DeleteMarkers": list(markers),
                                                 "IsTruncated": truncated, **extra}, args)


def listed(key, version, latest=True):
    return {"Key": f"fixture/{key}", "VersionId": version, "IsLatest": latest}


def versioning(stubber):
    stubber.add_response("get_bucket_versioning", {"Status": "Enabled"}, {"Bucket": BUCKET})


def assert_missing(stubber, key, version=None):
    args = parameters(key, **({"VersionId": version} if version else {}))
    stubber.add_client_error("get_object", "NoSuchVersion" if version else "NoSuchKey",
                             http_status_code=404, expected_params=args)


def control_reads(stubber):
    read(stubber, RAW_KEY, "raw-v1", RAW_BODY)
    list_page(stubber, RAW_KEY, versions=[listed(RAW_KEY, "raw-v1")])
    read(stubber, RAW_KEY, "raw-v1", RAW_BODY, pinned=True)


def baseline(archive):
    expiry = CURRENT_TIME + timedelta(days=30)
    return {"schema_version": 1, "scope_sha256": archive.scope, "bucket_versioning": "Enabled",
            "observed_at": CURRENT_TIME.isoformat(), "expires_at": expiry.isoformat(),
            "verify_after": expiry.isoformat(), "discovery_key": LIST_KEY, "discovery_versions": ["list-v1"],
            "controls": {RAW_KEY: {"version_id": "raw-v1", "bytes": len(RAW_BODY),
                                   "sha256": sha256(RAW_BODY).hexdigest()}}}


def test_observe_reads_exact_versions_across_pages(s3):
    verifier, archive, stubber = s3
    versioning(stubber)
    read(stubber, LIST_KEY, "list-v1", LIST_BODY)
    key_marker = f"fixture/{LIST_KEY}"
    list_page(stubber, LIST_KEY, versions=[listed(LIST_KEY, "list-v1")], truncated=True,
              NextKeyMarker=key_marker, NextVersionIdMarker="list-v1")
    list_page(stubber, LIST_KEY, versions=[listed(LIST_KEY + "-neighbor", "neighbor-v1")],
              marker={"KeyMarker": key_marker, "VersionIdMarker": "list-v1"})
    read(stubber, LIST_KEY, "list-v1", LIST_BODY, pinned=True)
    control_reads(stubber)

    expires = CURRENT_TIME + timedelta(days=30)
    assert verifier.observe(archive, LIST_KEY, [RAW_KEY], expires, expires, CURRENT_TIME) == baseline(archive)


def test_verify_requires_absence_of_current_and_pinned_bytes_and_keeps_control(s3):
    verifier, archive, stubber = s3
    versioning(stubber)
    list_page(stubber, LIST_KEY, versions=[listed(LIST_KEY + "-neighbor", "neighbor-v1")])
    assert_missing(stubber, LIST_KEY)
    assert_missing(stubber, LIST_KEY, "list-v1")
    control_reads(stubber)
    receipt = verifier.verify(archive, baseline(archive), "a" * 64, CURRENT_TIME + timedelta(days=30))
    assert receipt["previous_versions_missing"] == receipt["long_lived_controls"] == 1
    assert receipt["status"] == "verified"


@pytest.mark.parametrize("remaining", ["version", "marker"])
def test_retained_versions_or_markers_on_later_page_fail(s3, remaining):
    verifier, archive, stubber = s3
    versioning(stubber)
    key_marker = f"fixture/{LIST_KEY}"
    list_page(stubber, LIST_KEY, truncated=True, NextKeyMarker=key_marker, NextVersionIdMarker="cursor")
    retained = listed(LIST_KEY, "still-present")
    list_page(stubber, LIST_KEY, versions=[retained] if remaining == "version" else [],
              markers=[retained] if remaining == "marker" else [],
              marker={"KeyMarker": key_marker, "VersionIdMarker": "cursor"})
    with pytest.raises(verifier.VerificationError, match="discovery_versions_or_markers_retained"):
        verifier.verify(archive, baseline(archive), "a" * 64, CURRENT_TIME + timedelta(days=30))


def test_current_404_does_not_prove_pinned_object_absence(s3):
    verifier, archive, stubber = s3
    versioning(stubber)
    list_page(stubber, LIST_KEY)
    assert_missing(stubber, LIST_KEY)
    read(stubber, LIST_KEY, "list-v1", LIST_BODY, pinned=True)
    with pytest.raises(verifier.VerificationError, match="discovery_version_still_readable"):
        verifier.verify(archive, baseline(archive), "a" * 64, CURRENT_TIME + timedelta(days=30))


def test_verify_timing_scope_and_independent_digest_are_required(s3, tmp_path):
    verifier, archive, _ = s3
    record = baseline(archive)
    with pytest.raises(verifier.VerificationError, match="verification_too_early"):
        verifier.verify(archive, record, "a" * 64, CURRENT_TIME)
    other_scope = verifier.VersionedArchive(archive.client, "s3://example-bucket/other")
    with pytest.raises(verifier.VerificationError, match="storage_scope_mismatch"):
        verifier.verify(other_scope, record, "a" * 64, CURRENT_TIME + timedelta(days=30))
    path = tmp_path / "baseline.json"
    raw = verifier.encode_manifest(record)
    path.write_bytes(raw)
    assert verifier.load_baseline(path, sha256(raw).hexdigest()) == record
    with pytest.raises(verifier.VerificationError, match="baseline_digest_mismatch"):
        verifier.load_baseline(path, "a" * 64)


@pytest.mark.parametrize(("code", "status"), [("AccessDenied", 403), ("NoSuchBucket", 404)])
def test_provider_failure_does_not_count_as_object_expiry(s3, code, status):
    _, archive, stubber = s3
    stubber.add_client_error("get_object", code, http_status_code=status,
                             expected_params=parameters(LIST_KEY, VersionId="list-v1"))
    with pytest.raises(ClientError):
        archive.read(LIST_KEY, "list-v1")
