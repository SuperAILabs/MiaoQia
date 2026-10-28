"""Bounded explicit owner operations; no request-driven backfill."""
import re


def audit(actor, reason):
    if (not isinstance(actor, str) or not re.fullmatch(r"[a-zA-Z0-9_-]{1,96}", actor)
            or not isinstance(reason, str) or not 1 <= len(reason) <= 256
            or any(ord(c) < 32 or ord(c) == 127 for c in reason)):
        raise ValueError("invalid audit metadata")


def batch(values):
    if not isinstance(values, (list, tuple)) or not 1 <= len(values) <= 100:
        raise ValueError("explicit batch of 1–100 fixed handles required")
    if len({str(v) for v in values}) != len(values):
        raise ValueError("duplicate batch handle")
