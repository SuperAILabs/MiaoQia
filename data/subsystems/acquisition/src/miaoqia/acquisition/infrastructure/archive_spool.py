"""Host-local, quota-bound recovery for complete acquisition responses."""
from __future__ import annotations

import fcntl
import hashlib
import json
import os
import re
import tempfile
import time
from collections.abc import Callable, Iterator
from contextlib import contextmanager
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from typing import BinaryIO

from ..api.handlers import FetchResponse
from ..api.browser_projection import MAX_PROJECTION_BYTES
from .transport import MAX_BODY

_KEY = re.compile(r"[0-9a-f]{64}")
_METADATA_LIMIT = 16 * 1024
_DEFAULT_CAPACITY = 512 * 1024 * 1024
_DEFAULT_TTL = 24 * 60 * 60
_RESERVATION = MAX_BODY + _METADATA_LIMIT


class SpoolFull(OSError):
    """The host's configured capacity would be exceeded."""


class SpoolWriteError(RuntimeError):
    """A local write failed while the transport was receiving bytes."""


class InvalidCaptureMetadata(ValueError):
    """The response metadata exceeds the spool format's bounds."""


@dataclass(frozen=True)
class Capture:
    response: FetchResponse
    attempt_id: int
    received_at: datetime
    projection: bytes | None = None


def _header(response: FetchResponse, attempt: int, received: datetime, size: int, digest: str,
            projection: bytes | None = None) -> bytes:
    header = {
        "attempt_id": attempt, "media_type": response.media_type,
        "url": response.url, "received_at": received.isoformat(),
        "size": size, "sha256": digest,
    }
    if projection is not None:
        header.update(projection_size=len(projection), projection_sha256=hashlib.sha256(projection).hexdigest())
    encoded = json.dumps(header, separators=(",", ":")).encode()
    if len(encoded) >= _METADATA_LIMIT:
        raise InvalidCaptureMetadata("archive spool metadata exceeds limit")
    return encoded


class IncomingCapture:
    """A private download with a reserved quota and a hash accumulated during writes."""

    def __init__(self, spool: ArchiveSpool, path: Path, stream: BinaryIO):
        self.spool = spool
        self.path = path
        self.stream = stream
        self.size = 0
        self.digest = hashlib.sha256()
        self.stream.seek(_METADATA_LIMIT)

    def write(self, chunk: bytes) -> int:
        if self.size + len(chunk) > MAX_BODY:
            raise SpoolWriteError("stream exceeded its spool reservation")
        try:
            written = self.stream.write(chunk)
        except OSError as exc:
            raise SpoolWriteError("archive spool write failed") from exc
        if written != len(chunk):
            raise SpoolWriteError("incomplete archive spool write")
        self.digest.update(chunk)
        self.size += written
        return written

    def body(self) -> bytes:
        try:
            self.stream.seek(_METADATA_LIMIT)
            body = self.stream.read(self.size)
        except OSError as exc:
            raise SpoolWriteError("archive spool read failed") from exc
        if len(body) != self.size or hashlib.sha256(body).digest() != self.digest.digest():
            raise SpoolWriteError("archive spool bytes failed verification")
        return body

    def publish(self, key: str, capture: Capture) -> None:
        if capture.projection is not None:
            raise ValueError("streamed HTTP captures cannot carry a browser projection")
        header = _header(capture.response, capture.attempt_id, capture.received_at, self.size, self.digest.hexdigest())
        target = self.spool._path(key)
        self.stream.seek(0)
        self.stream.write(header.ljust(_METADATA_LIMIT - 1, b" ") + b"\n")
        self.stream.flush()
        os.fsync(self.stream.fileno())
        with self.spool._locked():
            if target.exists():
                raise FileExistsError("archive spool capture already exists")
            os.replace(self.path, target)
            self.spool._sync_directory()


class ArchiveSpool:
    def __init__(self, root: str | Path, *, capacity: int = _DEFAULT_CAPACITY, ttl: int = _DEFAULT_TTL):
        if type(capacity) is not int or capacity <= _METADATA_LIMIT or type(ttl) is not int or ttl < 1:
            raise ValueError("invalid archive spool bounds")
        self.root = Path(root)
        self.root.mkdir(parents=True, mode=0o700, exist_ok=True)
        metadata = self.root.lstat()
        if self.root.is_symlink() or not self.root.is_dir() or metadata.st_uid != os.geteuid() or metadata.st_mode & 0o077:
            raise ValueError("archive spool requires a directory private to the collector")
        self.capacity = capacity
        self.ttl = ttl

    @classmethod
    def configured(cls, config: Callable[[str], str | None]) -> ArchiveSpool | None:
        path = config("ARCHIVE_SPOOL_DIR")
        if not path:
            return None
        return cls(path, capacity=int(config("ARCHIVE_SPOOL_MAX_BYTES") or _DEFAULT_CAPACITY),
                   ttl=int(config("ARCHIVE_SPOOL_TTL_SECONDS") or _DEFAULT_TTL))

    @contextmanager
    def _locked(self) -> Iterator[None]:
        fd = os.open(self.root / ".lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "rb") as stream:
            fcntl.flock(stream, fcntl.LOCK_EX)
            yield

    def _path(self, key: str) -> Path:
        if not _KEY.fullmatch(key):
            raise ValueError("invalid archive spool key")
        return self.root / key

    def _sync_directory(self) -> None:
        descriptor = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)

    def _remove(self, path: Path) -> None:
        path.unlink()
        self._sync_directory()

    def _size_after_expiry(self, now: float) -> int:
        total = 0
        removed = False
        for path in self.root.iterdir():
            if path.name.startswith(".capture-"):
                # A writer holds the lock for the lifetime of the temporary file.
                path.unlink()
                removed = True
            elif path.name.startswith(".download-"):
                with path.open("rb") as stream:
                    try:
                        fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    except BlockingIOError:
                        total += _RESERVATION
                    else:
                        path.unlink()
                        removed = True
            elif _KEY.fullmatch(path.name):
                metadata = path.stat()
                if metadata.st_mtime + self.ttl <= now:
                    path.unlink()
                    removed = True
                else:
                    total += metadata.st_size
        if removed:
            self._sync_directory()
        return total

    def expire(self) -> None:
        with self._locked():
            self._size_after_expiry(time.time())

    def ensure_room(self) -> None:
        """Check the worst permitted response before making a new HTTP request."""
        with self._locked():
            if self._size_after_expiry(time.time()) + _RESERVATION > self.capacity:
                raise SpoolFull("archive spool capacity reached")

    @contextmanager
    def receive(self) -> Iterator[IncomingCapture]:
        """Reserve capacity across processes, without holding a global lock during network I/O."""
        with self._locked():
            if self._size_after_expiry(time.time()) + _RESERVATION > self.capacity:
                raise SpoolFull("archive spool capacity reached")
            fd, name = tempfile.mkstemp(dir=self.root, prefix=".download-")
            try:
                fcntl.flock(fd, fcntl.LOCK_EX)
            except OSError:
                os.close(fd)
                Path(name).unlink()
                raise
        path = Path(name)
        with os.fdopen(fd, "w+b") as stream:
            try:
                yield IncomingCapture(self, path, stream)
            finally:
                with self._locked():
                    path.unlink(missing_ok=True)

    def put(self, key: str, capture: Capture) -> None:
        response = capture.response
        projection = capture.projection
        if response.status != 200 or len(response.body) > MAX_BODY:
            raise ValueError("only bounded complete responses may be staged")
        if projection is not None and (not isinstance(projection, bytes) or not 1 <= len(projection) <= MAX_PROJECTION_BYTES):
            raise ValueError("only bounded complete projections may be staged")
        path = self._path(key)
        header = _header(response, capture.attempt_id, capture.received_at, len(response.body),
                         hashlib.sha256(response.body).hexdigest(), projection) + b"\n"
        with self._locked():
            used = self._size_after_expiry(time.time())
            if path.exists():
                raise FileExistsError("archive spool capture already exists")
            if used + len(header) + len(response.body) + len(projection or b"") > self.capacity:
                raise SpoolFull("archive spool capacity reached")
            fd, temporary = tempfile.mkstemp(dir=self.root, prefix=".capture-")
            try:
                with os.fdopen(fd, "wb") as stream:
                    stream.write(header)
                    body = memoryview(response.body)
                    for position in range(0, len(body), 64 * 1024):
                        stream.write(body[position:position + 64 * 1024])
                    if projection is not None:
                        stream.write(projection)
                    stream.flush()
                    os.fsync(stream.fileno())
                os.replace(temporary, path)
                self._sync_directory()
            finally:
                Path(temporary).unlink(missing_ok=True)

    def get(self, key: str) -> Capture | None:
        path = self._path(key)
        with self._locked():
            try:
                with path.open("rb") as stream:
                    if os.fstat(stream.fileno()).st_mtime + self.ttl > time.time():
                        result = self._read(stream)
                        if result is not None:
                            return result
                self._remove(path)
            except FileNotFoundError:
                pass
        return None

    @staticmethod
    def _read(stream) -> Capture | None:
        try:
            header = json.loads(stream.readline(_METADATA_LIMIT))
            size = header["size"]
            received = datetime.fromisoformat(header["received_at"])
            if type(size) is not int or not 0 <= size <= MAX_BODY or received.tzinfo is None:
                return None
            body = stream.read(size)
            projection = None
            if "projection_size" in header:
                projection_size = header["projection_size"]
                if type(projection_size) is not int or not 1 <= projection_size <= MAX_PROJECTION_BYTES:
                    return None
                projection = stream.read(projection_size)
                if (len(projection) != projection_size
                        or hashlib.sha256(projection).hexdigest() != header["projection_sha256"]):
                    return None
            if stream.read(1):
                return None
            attempt, media, url = header["attempt_id"], header["media_type"], header["url"]
            if (len(body) == size and hashlib.sha256(body).hexdigest() == header["sha256"]
                    and type(attempt) is int and attempt > 0 and isinstance(media, str) and isinstance(url, str)):
                return Capture(FetchResponse(200, media, body, url), attempt, received, projection)
        except (KeyError, TypeError, ValueError, RecursionError):
            pass
        return None

    def discard(self, key: str, attempt_id: int) -> None:
        path = self._path(key)
        with self._locked():
            try:
                with path.open("rb") as stream:
                    header = json.loads(stream.readline(_METADATA_LIMIT))
                if not isinstance(header, dict) or header.get("attempt_id") == attempt_id:
                    self._remove(path)
            except FileNotFoundError:
                pass
            except (ValueError, RecursionError):
                self._remove(path)
