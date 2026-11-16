"""对象存储端口与文件系统实现，供 acquisition（原始制品归档）与 evidence（原件、规范化文档、增强）共用。

对象一经写入不可变：同一键再次写入相同内容是幂等的，写入不同内容被拒绝（不覆盖已发布的原件或规范化文档）。
文件系统实现用于回放贯通与单机部署（键即相对路径）；生产使用受测 S3 公共子集的实现（技术栈第66行），接口相同。
"""
from __future__ import annotations

import os
import re
import tempfile
import time
from pathlib import Path
from typing import Protocol

# Keys follow data-architecture 3.2: raw/<sha256> (content-addressed), normalized/<pipeline>/<version>/<snapshot>.json,
# enrichment/<kind>/<name>/<version>/<sha256>.json. A segment never starts with "." (no "."/".." and no clash with the
# ".tmp-" files of the filesystem store).
SEGMENT = r"[A-Za-z0-9_@-][A-Za-z0-9_.@-]{0,199}"
KEY = re.compile(rf"(raw|normalized|enrichment)(/{SEGMENT}){{1,5}}")
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


class FilesystemObjectStore:
    def __init__(self, root: str | Path):
        self.root = Path(root).resolve()
        self.root.mkdir(parents=True, exist_ok=True)

    def _path(self, key: str) -> Path:
        if len(key) > MAX_KEY or not KEY.fullmatch(key):
            raise ValueError(f"非法对象键：{key!r}")
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
        if max_bytes is not None and (type(max_bytes) is not int or max_bytes < 0):
            raise ValueError("invalid object size cap")
        if deadline is not None and time.monotonic() >= deadline:
            raise TimeoutError("object read deadline expired")
        if max_bytes is not None and path.stat().st_size > max_bytes:
            raise ValueError("object exceeds bounded read")
        with path.open("rb") as stream:
            data = bytearray()
            while True:
                if deadline is not None and time.monotonic() >= deadline:
                    raise TimeoutError("object read deadline expired")
                chunk = stream.read(min(64 * 1024, (max_bytes - len(data) + 1) if max_bytes is not None else 64 * 1024))
                if not chunk:
                    return bytes(data)
                data.extend(chunk)
                if max_bytes is not None and len(data) > max_bytes:
                    raise ValueError("object exceeds bounded read")
