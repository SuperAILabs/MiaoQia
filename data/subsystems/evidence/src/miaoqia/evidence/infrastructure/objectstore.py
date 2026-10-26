"""对象存储端口与文件系统实现。

对象一经写入不可变：同一键再次写入相同内容是幂等的，写入不同内容被拒绝（不覆盖已发布的原件或规范化文档）。
文件系统实现用于回放贯通与单机部署（键即相对路径）；生产使用受测 S3 公共子集的实现（技术栈第66行），接口相同。
"""
from __future__ import annotations

import os
import re
import tempfile
from pathlib import Path
from typing import Protocol

KEY = re.compile(r"(raw|normalized|enrichment)/[A-Za-z0-9_.-]{1,200}")


class ObjectConflict(RuntimeError):
    """同一对象键已存在不同内容。"""


class ObjectStore(Protocol):
    def put(self, key: str, data: bytes) -> None: ...

    def get(self, key: str) -> bytes: ...


class FilesystemObjectStore:
    def __init__(self, root: str | Path):
        self.root = Path(root).resolve()
        self.root.mkdir(parents=True, exist_ok=True)

    def _path(self, key: str) -> Path:
        if not KEY.fullmatch(key):
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

    def get(self, key: str) -> bytes:
        return self._path(key).read_bytes()
