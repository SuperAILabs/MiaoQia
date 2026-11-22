import hashlib
import os
import time
from datetime import datetime, timezone

import pytest

from miaoqia.acquisition.api.handlers import FetchResponse
from miaoqia.acquisition.infrastructure.archive_spool import ArchiveSpool, Capture, SpoolFull
from miaoqia.acquisition.infrastructure.transport import MAX_BODY


def test_archive_spool_checks_bytes_and_expires_interrupted_writes(tmp_path):
    root = tmp_path / "private"
    spool = ArchiveSpool(root, ttl=60)
    key = hashlib.sha256(b"task").hexdigest()
    response = FetchResponse(200, "text/html", b"notice", "https://example.test/notice")
    capture = Capture(response, 42, datetime.now(timezone.utc))
    spool.put(key, capture)
    path = root / key
    assert path.stat().st_mode & 0o777 == 0o600
    assert spool.get(key) == capture
    spool.discard(key, attempt_id=41)
    assert spool.get(key) == capture
    with path.open("ab") as stream:
        stream.write(b"unexpected bytes")
    assert spool.get(key) is None and not path.exists()

    spool.put(key, capture)
    aged = time.time() - 61
    os.utime(path, (aged, aged))
    partial = root / ".capture-interrupted"
    partial.write_bytes(b"incomplete")
    spool.expire()
    assert not path.exists() and not partial.exists()


def test_archive_spool_rejects_shared_directory(tmp_path):
    path = tmp_path / "shared"
    path.mkdir(mode=0o755)
    path.chmod(0o755)
    with pytest.raises(ValueError, match="private"):
        ArchiveSpool(path)


def test_stream_reservation_and_atomic_publication_share_the_spool(tmp_path):
    root = tmp_path / "private"
    spool = ArchiveSpool(root, capacity=MAX_BODY + 32 * 1024)
    other = ArchiveSpool(root, capacity=spool.capacity)
    key = hashlib.sha256(b"streamed task").hexdigest()
    response = FetchResponse(200, "text/html", b"first last", "https://example.test/notice")
    capture = Capture(response, 42, datetime.now(timezone.utc))
    abandoned = root / ".download-abandoned"
    abandoned.write_bytes(b"partial")

    with spool.receive() as incoming:
        assert not abandoned.exists()
        incoming.write(b"first ")
        assert other.get(key) is None
        with pytest.raises(SpoolFull), other.receive():
            pytest.fail("an in-flight body already reserved the remaining quota")
        incoming.write(b"last")
        assert incoming.body() == response.body
        incoming.publish(key, capture)
        assert other.get(key) == capture

    assert not list(root.glob(".download-*"))
    assert (root / key).stat().st_mode & 0o777 == 0o600
