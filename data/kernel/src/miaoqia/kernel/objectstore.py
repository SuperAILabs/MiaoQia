"""对象存储端口，供 acquisition 与 evidence 共用文件系统或 S3 兼容存储。

对象一经写入不可变：同一键再次写入相同内容是幂等的，写入不同内容被拒绝（不覆盖已发布的原件或规范化文档）。
文件系统实现用于回放贯通与单机部署（键即相对路径）；生产使用受测 S3 公共子集的实现（技术栈第66行），接口相同。
"""
from __future__ import annotations

import os
import re
import tempfile
import time
from base64 import b64encode
from contextlib import closing
from hashlib import md5
from pathlib import Path
from typing import Protocol
from urllib.parse import urlsplit

# Keys follow data-architecture 3.2: raw/<sha256>, projection/<sha256>, normalized/<pipeline>/<version>/<snapshot>.json,
# enrichment/<kind>/<name>/<version>/<sha256>.json. A segment never starts with "." (no "."/".." and no clash with the
# ".tmp-" files of the filesystem store).
SEGMENT = r"[A-Za-z0-9_@-][A-Za-z0-9_.@-]{0,199}"
KEY = re.compile(rf"(raw|normalized|enrichment|discovery)(/{SEGMENT}){{1,5}}|projection/[a-f0-9]{{64}}")
MAX_KEY = 512


def segment(value: str) -> str:
    """Return value when it is a valid single key segment (used to build versioned keys)."""
    if not re.fullmatch(SEGMENT, value):
        raise ValueError(f"非法对象键片段：{value!r}")
    return value


def versioned(identifier: str) -> str:
    """Turn a versioned identifier ``<name>@<version>`` into the key path ``<name>/<version>``."""
    name, sep, version = identifier.partition("@")
    if not sep:
        raise ValueError(f"缺少版本的标识：{identifier!r}")
    return f"{segment(name)}/{segment(version)}"


class ObjectConflict(RuntimeError):
    """同一对象键已存在不同内容。"""


class ObjectStore(Protocol):
    def put(self, key: str, data: bytes) -> None: ...

    def get(self, key: str, *, max_bytes: int | None = None, deadline: float | None = None) -> bytes: ...

    def delete_discovery(self, key: str) -> None: ...


class _ReadOnlyStore:
    def __init__(self, store: ObjectStore):
        self._store = store

    def get(self, key: str, *, max_bytes: int | None = None, deadline: float | None = None) -> bytes:
        return self._store.get(key, max_bytes=max_bytes, deadline=deadline)

    def put(self, key: str, data: bytes) -> None:
        raise PermissionError("object store is read only")

    def delete_discovery(self, key: str) -> None:
        raise PermissionError("object store is read only")


def parse_s3_location(location: str) -> tuple[str, str]:
    uri = urlsplit(location)
    if (uri.scheme != "s3" or not re.fullmatch(r"[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]", uri.netloc)
            or uri.query or uri.fragment):
        raise ValueError("invalid S3 object store URI")
    prefix = uri.path.removeprefix("/").removesuffix("/")
    if len(prefix) > MAX_KEY:
        raise ValueError("S3 prefix too long")
    if prefix:
        for part in prefix.split("/"):
            segment(part)
    return uri.netloc, prefix


def s3_client(*, profile: str | None = None):
    import boto3
    from botocore.config import Config

    config = Config(connect_timeout=2, read_timeout=2, retries={"total_max_attempts": 1},
                    request_checksum_calculation="when_required")
    if profile is None:
        return boto3.client("s3", config=config)

    from botocore.session import Session

    if not profile.strip():
        raise ValueError("expected named S3 profile")
    session = Session(profile=profile)
    settings = session.get_scoped_config()
    # Pin connection and signing identity together. In particular, do not fall back
    # to primary credentials, endpoint or region from the process environment.
    endpoint = settings.get("endpoint_url")
    region = settings.get("region")
    address = urlsplit(endpoint or "")
    if (address.scheme not in {"http", "https"} or not address.hostname or address.username is not None
            or address.query or address.fragment or not region):
        raise ValueError("S3 profile requires an explicit endpoint and region")
    if not settings.get("aws_access_key_id") or not settings.get("aws_secret_access_key"):
        raise ValueError("S3 profile requires explicit credentials")
    session.set_credentials(settings["aws_access_key_id"], settings["aws_secret_access_key"],
                            settings.get("aws_session_token"))
    return session.create_client("s3", endpoint_url=endpoint, region_name=region, config=config)


def open_object_store(location: str | Path, *, read_only: bool = False, s3_profile: str | None = None) -> ObjectStore:
    store: ObjectStore
    if isinstance(location, str) and "://" in location:
        bucket, prefix = parse_s3_location(location)
        store = S3ObjectStore(bucket, prefix, client=s3_client(profile=s3_profile))
    else:
        if s3_profile is not None:
            raise ValueError("S3 profile requires an S3 location")
        store = FilesystemObjectStore(location, create=not read_only)
    return _ReadOnlyStore(store) if read_only else store


def _validate_key(key: str) -> None:
    if len(key) > MAX_KEY or not KEY.fullmatch(key):
        raise ValueError(f"非法对象键：{key!r}")


def _read_budget(max_bytes: int | None, deadline: float | None) -> None:
    if max_bytes is not None and (type(max_bytes) is not int or max_bytes < 0):
        raise ValueError("invalid object size cap")
    if deadline is not None and time.monotonic() >= deadline:
        raise TimeoutError("object read deadline expired")


def _read_bytes(stream, size: int, *, max_bytes: int | None, deadline: float | None) -> bytes:
    _read_budget(max_bytes, deadline)
    if max_bytes is not None and size > max_bytes:
        raise ValueError("object exceeds bounded read")
    data = bytearray()
    while True:
        _read_budget(max_bytes, deadline)
        chunk = stream.read(min(64 * 1024, max_bytes - len(data) + 1) if max_bytes is not None else 64 * 1024)
        _read_budget(max_bytes, deadline)
        if not chunk:
            return bytes(data)
        data.extend(chunk)
        if max_bytes is not None and len(data) > max_bytes:
            raise ValueError("object exceeds bounded read")


class FilesystemObjectStore:
    def __init__(self, root: str | Path, *, create: bool = True):
        self.root = Path(root).resolve()
        if create:
            self.root.mkdir(parents=True, exist_ok=True)
        elif not self.root.is_dir():
            raise FileNotFoundError("object store root does not exist")

    def _path(self, key: str) -> Path:
        _validate_key(key)
        path = (self.root / key).resolve()
        if not path.is_relative_to(self.root):
            raise ValueError(f"非法对象键：{key!r}")
        return path

    def put(self, key: str, data: bytes) -> None:
        path = self._path(key)
        path.parent.mkdir(parents=True, exist_ok=True)
        fd, tmp = tempfile.mkstemp(dir=path.parent, prefix=".tmp-")
        try:
            with os.fdopen(fd, "wb") as f:
                f.write(data)
                f.flush()
                os.fsync(f.fileno())
            try:
                os.link(tmp, path)          # 不覆盖：键已存在时失败
            except FileExistsError:
                if path.read_bytes() != data:
                    raise ObjectConflict(key) from None
        finally:
            os.unlink(tmp)

    def get(self, key: str, *, max_bytes: int | None = None, deadline: float | None = None) -> bytes:
        path = self._path(key)
        _read_budget(max_bytes, deadline)
        with path.open("rb") as stream:
            return _read_bytes(stream, os.fstat(stream.fileno()).st_size, max_bytes=max_bytes, deadline=deadline)

    def delete_discovery(self, key: str) -> None:
        if not key.startswith("discovery/"):
            raise ValueError("only discovery captures may expire")
        self._path(key).unlink(missing_ok=True)


class S3ObjectStore:
    def __init__(self, bucket: str, prefix: str = "", *, client=None):
        self._client = s3_client() if client is None else client
        self._bucket = bucket
        self._prefix = prefix

    def _key(self, key: str) -> str:
        _validate_key(key)
        return f"{self._prefix}/{key}" if self._prefix else key

    def put(self, key: str, data: bytes) -> None:
        from botocore.exceptions import BotoCoreError, ClientError

        object_key = self._key(key)
        checksum = b64encode(md5(data, usedforsecurity=False).digest()).decode("ascii")
        for attempt in range(2):
            try:
                self._client.put_object(Bucket=self._bucket, Key=object_key, Body=data,
                                        IfNoneMatch="*", ContentMD5=checksum)
                return
            except ClientError as exc:
                status = exc.response.get("ResponseMetadata", {}).get("HTTPStatusCode")
                if status == 409 and attempt == 0:
                    continue
                if status != 412:
                    raise OSError("S3 object write failed") from exc
            except BotoCoreError as exc:
                raise OSError("S3 object write failed") from exc
            try:
                if self.get(key, max_bytes=len(data)) == data:
                    return
            except ValueError:
                pass
            raise ObjectConflict(key)

    def get(self, key: str, *, max_bytes: int | None = None, deadline: float | None = None) -> bytes:
        from botocore.exceptions import BotoCoreError, ClientError

        object_key = self._key(key)
        _read_budget(max_bytes, deadline)
        try:
            response = self._client.get_object(Bucket=self._bucket, Key=object_key)
            with closing(response["Body"]) as stream:
                return _read_bytes(stream, response["ContentLength"], max_bytes=max_bytes, deadline=deadline)
        except ClientError as exc:
            if exc.response.get("Error", {}).get("Code") in {"NoSuchKey", "404"}:
                raise FileNotFoundError(key) from exc
            raise OSError("S3 object read failed") from exc
        except BotoCoreError as exc:
            raise OSError("S3 object read failed") from exc

    def delete_discovery(self, key: str) -> None:
        from botocore.exceptions import BotoCoreError, ClientError

        if not key.startswith("discovery/"):
            raise ValueError("only discovery captures may expire")
        try:
            self._client.delete_object(Bucket=self._bucket, Key=self._key(key))
        except (BotoCoreError, ClientError) as exc:
            raise OSError("S3 discovery capture expiry failed") from exc
