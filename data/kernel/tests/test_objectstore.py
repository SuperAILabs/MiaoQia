from contextlib import closing
from io import BytesIO
from time import monotonic
from urllib.parse import parse_qs, urlsplit

import boto3
import pytest
from botocore.response import StreamingBody
from botocore.stub import ANY, Stubber

from miaoqia.kernel.objectstore import ObjectConflict, S3ObjectStore, open_object_store, s3_client


@pytest.fixture
def s3():
    client = boto3.client("s3", region_name="us-east-1", aws_access_key_id="testing", aws_secret_access_key="testing")
    with Stubber(client) as stub:
        yield S3ObjectStore("fixture-bucket", "public", client=client), stub
        stub.assert_no_pending_responses()


def request(key="raw/example"):
    return {"Bucket": "fixture-bucket", "Key": f"public/{key}"}


def write(data):
    return {**request(), "Body": data, "IfNoneMatch": "*", "ContentMD5": ANY}


def body(data):
    return {"ContentLength": len(data), "Body": StreamingBody(BytesIO(data), len(data))}


def test_read_only_store_does_not_create_or_modify_local_archive(tmp_path):
    root = tmp_path / "archive"
    with pytest.raises(FileNotFoundError):
        open_object_store(root, read_only=True)
    assert not root.exists()
    writer = open_object_store(root)
    writer.put("raw/example", b"original")
    writer.put("discovery/source/1/digest", b"listing")
    reader = open_object_store(root, read_only=True)
    assert reader.get("raw/example", max_bytes=8) == b"original"
    with pytest.raises(PermissionError):
        reader.put("raw/another", b"new")
    with pytest.raises(PermissionError):
        reader.delete_discovery("discovery/source/1/digest")
    assert writer.get("discovery/source/1/digest") == b"listing"
    assert not (root / "raw/another").exists()


def test_named_s3_profile_pins_endpoint_region_and_credentials(tmp_path, monkeypatch):
    config = tmp_path / "config"
    credentials = tmp_path / "credentials"
    config.write_text("[profile archive-copy]\nendpoint_url = https://backup.example\nregion = eu-central-1\n")
    credentials.write_text("[archive-copy]\naws_access_key_id = copy-key\naws_secret_access_key = copy-secret\n"
                           "aws_session_token = copy-token\n")
    monkeypatch.setenv("AWS_CONFIG_FILE", str(config))
    monkeypatch.setenv("AWS_SHARED_CREDENTIALS_FILE", str(credentials))
    monkeypatch.setenv("AWS_ENDPOINT_URL_S3", "https://primary.example")
    monkeypatch.setenv("AWS_DEFAULT_REGION", "us-east-1")
    monkeypatch.setenv("AWS_ACCESS_KEY_ID", "primary-key")
    monkeypatch.setenv("AWS_SECRET_ACCESS_KEY", "primary-secret")
    monkeypatch.setenv("AWS_SESSION_TOKEN", "primary-token")
    with closing(s3_client(profile="archive-copy")) as client:
        url = urlsplit(client.generate_presigned_url("get_object", Params={"Bucket": "archive", "Key": "raw/key"}))
    assert url.hostname == "backup.example"
    signature = parse_qs(url.query)
    signer = signature["X-Amz-Credential"][0].split("/")
    assert signer[0] == "copy-key" and signer[2] == "eu-central-1"
    assert signature["X-Amz-Security-Token"] == ["copy-token"]

    credentials.unlink()
    with pytest.raises(ValueError, match="explicit credentials"):
        s3_client(profile="archive-copy")
    config.write_text("[profile archive-copy]\nregion = eu-central-1\n")
    with pytest.raises(ValueError, match="explicit endpoint"):
        s3_client(profile="archive-copy")
    with pytest.raises(ValueError, match="S3 location"):
        open_object_store(tmp_path / "typo", read_only=True, s3_profile="archive-copy")
    assert not (tmp_path / "typo").exists()


def test_s3_retries_conditional_write_conflict(s3):
    store, stub = s3
    stub.add_client_error("put_object", http_status_code=409, expected_params=write(b"original"))
    stub.add_response("put_object", {}, write(b"original"))
    store.put("raw/example", b"original")


@pytest.mark.parametrize("existing", [b"different", b"longer than original"])
def test_s3_rejects_immutable_overwrite(s3, existing):
    store, stub = s3
    stub.add_client_error("put_object", http_status_code=412, expected_params=write(b"original"))
    stub.add_response("get_object", body(existing), request())
    with pytest.raises(ObjectConflict):
        store.put("raw/example", b"original")


def test_s3_reuses_identical_object(s3):
    store, stub = s3
    stub.add_client_error("put_object", http_status_code=412, expected_params=write(b"original"))
    stub.add_response("get_object", body(b"original"), request())
    store.put("raw/example", b"original")


def test_s3_expiry_cannot_delete_a_permanent_object(s3):
    store, stub = s3
    key = "discovery/source/1/digest"
    stub.add_response("delete_object", {}, request(key))
    store.delete_discovery(key)
    with pytest.raises(ValueError, match="only discovery"):
        store.delete_discovery("raw/example")


def test_s3_preserves_bounded_read_and_missing_keys(s3):
    store, stub = s3
    stub.add_response("get_object", body(b"original"), request())
    assert store.get("raw/example", max_bytes=8) == b"original"
    stub.add_response("get_object", body(b"original"), request())
    with pytest.raises(ValueError, match="bounded read"):
        store.get("raw/example", max_bytes=7)
    stub.add_client_error("get_object", service_error_code="NoSuchKey", http_status_code=404, expected_params=request())
    with pytest.raises(FileNotFoundError):
        store.get("raw/example")
    with pytest.raises(TimeoutError):
        store.get("raw/example", deadline=monotonic() - 1)
    with pytest.raises(ValueError):
        store.get("raw/../example")


@pytest.mark.parametrize("location", ["http://example.com", "s3://abc/../private", "s3://abc/?secret=value"])
def test_invalid_store_uri_is_rejected(location):
    with pytest.raises(ValueError):
        open_object_store(location)
