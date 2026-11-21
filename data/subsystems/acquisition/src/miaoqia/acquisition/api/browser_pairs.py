"""Browser observation contract shared by the acquisition and evidence owners."""
from __future__ import annotations

import hashlib
import json
from functools import cache
from urllib.parse import urlsplit

from jsonschema.exceptions import ValidationError

from miaoqia.kernel import payloads
from miaoqia.kernel.objectstore import ObjectStore

from .browser_projection import read_projection

BROWSER_COMPLETED = "acquisition.fetch.completed.v4"


def _sha256(value: bytes) -> str:
    return f"sha256:{hashlib.sha256(value).hexdigest()}"


def describe_projection(projection: bytes) -> dict:
    read_projection(projection)
    digest = _sha256(projection)
    return {"artifact_ref": f"projection/{digest[7:]}", "content_hash": digest, "size_bytes": len(projection),
            "media_type": "application/json", "version": 1}


def seal_browser_event(fields: dict) -> dict:
    """Seal closed, publisher-scrubbed event fields before emitting the event."""
    if "binding_digest" in fields:
        raise ValueError("cannot re-seal a browser event")
    event = {**fields, "binding_digest": _digest(fields)}
    return validate_browser_event(event)


@cache
def _validator():
    return payloads.validator(BROWSER_COMPLETED, {})


def _digest(fields: dict) -> str:
    canonical = json.dumps(fields, sort_keys=True, separators=(",", ":"), ensure_ascii=False, allow_nan=False)
    return _sha256(canonical.encode("utf-8"))


def _origin(address: str, *, allow_query: bool) -> tuple[str, int]:
    target = urlsplit(address)
    if (target.scheme != "https" or not target.hostname or target.username is not None or target.fragment
            or target.port == 0 or "#" in address or (not allow_query and "?" in address)
            or any(ord(char) < 33 or ord(char) == 127 or char == "\\" for char in address)):
        raise ValueError("invalid browser locator")
    return target.hostname, 443 if target.port is None else target.port


def validate_browser_event(event: dict) -> dict:
    """Verify metadata and binding before any archive lookup."""
    try:
        _validator().validate(event)
    except ValidationError as exc:
        raise ValueError("invalid browser completion event") from exc
    projection = event["projection"]
    binding = event["binding"]
    integer_fields = (event["observation_version"], event["size_bytes"], projection["size_bytes"],
                      projection["version"], binding["version"], binding["run_id"],
                      binding["attempt_id"], binding["list_attempt_id"])
    if (event["artifact_ref"] != f"raw/{event['content_hash'][7:]}"
            or projection["artifact_ref"] != f"projection/{projection['content_hash'][7:]}"
            or any(type(value) is not int for value in integer_fields)
            or binding["attempt_id"] == binding["list_attempt_id"]
            or event["notice_key"] == event["source_url"]):
        raise ValueError("invalid browser observation binding")
    if _origin(event["source_url"], allow_query=True) != _origin(binding["request_url"], allow_query=False):
        raise ValueError("browser body request outside the notice origin")
    if _digest({name: value for name, value in event.items() if name != "binding_digest"}) != event["binding_digest"]:
        raise ValueError("browser observation binding digest mismatch")
    return event


def _read_verified(store: ObjectStore, descriptor: dict) -> bytes:
    data = store.get(descriptor["artifact_ref"], max_bytes=descriptor["size_bytes"])
    if len(data) != descriptor["size_bytes"] or _sha256(data) != descriptor["content_hash"]:
        raise ValueError("browser artifact failed its size or hash check")
    return data


def read_browser_pair(store: ObjectStore, event: dict) -> tuple[bytes, dict]:
    """Read a verified pair after the caller has applied the operation's authority."""
    validate_browser_event(event)
    original = _read_verified(store, event)
    projection = _read_verified(store, event["projection"])
    return original, read_projection(projection)
