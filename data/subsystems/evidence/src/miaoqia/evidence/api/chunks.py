"""The owner-visible bounds and spans for normalized search chunks."""
from dataclasses import dataclass

from miaoqia.sdk.document import Span

MAX_NOTICE_CHUNKS = 12
NOTICE_CHUNK_LENGTH = 768


@dataclass(frozen=True)
class NoticeChunks:
    snapshot: str
    spans: tuple[Span, ...]
