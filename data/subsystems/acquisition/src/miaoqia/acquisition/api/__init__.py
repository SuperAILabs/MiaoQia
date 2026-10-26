"""Acquisition public current-rights port. Trusted callers supply persisted provenance, never user source assertions."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
import re
from typing import Protocol

NAME = "acquisition"
DISPLAYS = ("none", "citation_only", "excerpt", "full_text")
OPERATIONS = ("fact_read", "evidence_resolve")
ID = re.compile(r"[a-z][a-z0-9_-]{0,95}")


def identifier(value):
    if not isinstance(value, str) or not ID.fullmatch(value):
        raise ValueError("invalid identifier")
    return value


def instant(value):
    if not isinstance(value, str):
        raise ValueError("invalid instant")
    t = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if t.tzinfo is None:
        raise ValueError("timezone required")
    return t


@dataclass(frozen=True)
class SourceInput:
    source_id: str
    tags: tuple[str, ...]

    @classmethod
    def parse(cls, value):
        if not isinstance(value, dict) or set(value) != {"source_id", "tags"}:
            raise ValueError("invalid source input")
        identifier(value["source_id"])
        tags = value["tags"]
        if (
            not isinstance(tags, list)
            or not 1 <= len(tags) <= 16
            or any(
                not isinstance(t, str) or not re.fullmatch(r"[a-zA-Z0-9:_-]{1,160}", t)
                for t in tags
            )
            or len(set(tags)) != len(tags)
        ):
            raise ValueError("invalid tags")
        return cls(value["source_id"], tuple(sorted(tags)))

    def dto(self):
        return {"source_id": self.source_id, "tags": list(self.tags)}


def inputs(value):
    if not isinstance(value, list) or not 1 <= len(value) <= 64:
        raise ValueError("invalid inputs")
    return tuple(SourceInput.parse(v) for v in value)


def policy(value):
    keys = {
        "schema_version",
        "entry_id",
        "version",
        "source_id",
        "scopes",
        "purposes",
        "display",
        "effective_at",
        "expires_at",
        "redistribution",
    }
    if (
        not isinstance(value, dict)
        or set(value) != keys
        or value["schema_version"] != 1
        or type(value["schema_version"]) is not int
    ):
        raise ValueError("invalid policy")
    identifier(value["entry_id"])
    identifier(value["source_id"])
    if (
        type(value["version"]) is not int
        or value["version"] < 1
        or value["display"] not in DISPLAYS
        or type(value["redistribution"]) is not bool
    ):
        raise ValueError("invalid policy value")
    scopes = value["scopes"]
    if not isinstance(scopes, list) or not 1 <= len(scopes) <= 64:
        raise ValueError("explicit scopes required")
    for scope in scopes:
        if not isinstance(scope, dict) or set(scope) != {"tenant", "cell"}:
            raise ValueError("invalid scope")
        if not all(
            isinstance(scope[k], str)
            and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,95}", scope[k])
            for k in ("tenant", "cell")
        ):
            raise ValueError("invalid scope identifier")
    if value["purposes"] != ["interactive"] or instant(
        value["effective_at"]
    ) >= instant(value["expires_at"]):
        raise ValueError("invalid policy purpose/time")
    return value


class SourceRights(Protocol):
    def resolve(self, request: dict) -> dict: ...
    def confirm(self, request: dict) -> dict: ...


def decision(value):
    keys = {"allowed", "display", "redistribution", "epoch"}
    if (
        not isinstance(value, dict)
        or not keys <= set(value)
        or set(value) - keys - {"receipt"}
        or type(value["allowed"]) is not bool
        or type(value["redistribution"]) is not bool
        or type(value["epoch"]) is not int
        or value["epoch"] < 0
        or value["display"] not in DISPLAYS
    ):
        raise ValueError("invalid decision")
    receipt = value.get("receipt")
    if "receipt" in value and receipt is None:
        raise ValueError("invalid receipt")
    if receipt is not None:
        if (
            not isinstance(receipt, dict)
            or set(receipt) != {"epoch", "expires", "nonce", "mac"}
            or type(receipt["epoch"]) is not int
            or receipt["epoch"] != value["epoch"]
            or type(receipt["expires"]) is not int
            or receipt["expires"] < 1
            or not isinstance(receipt["nonce"], str)
            or not re.fullmatch("[0-9a-f]{32}", receipt["nonce"])
            or not isinstance(receipt["mac"], str)
            or not re.fullmatch("[0-9a-f]{64}", receipt["mac"])
        ):
            raise ValueError("invalid receipt")
    if not value["allowed"] and (
        value["display"] != "none" or value["redistribution"] or receipt is not None
    ):
        raise ValueError("invalid denial")
    if value["allowed"] and value["display"] == "none":
        raise ValueError("invalid grant")
    return value
