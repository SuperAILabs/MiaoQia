"""Load source declarations and enforce live admission separately from offline handler availability."""
from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from pathlib import Path
from typing import Literal

import yaml
from sqlalchemy import Connection, text

from miaoqia.sdk.entrypoints import ENTRY_PATTERN, load_pack_module

from ..api.handlers import AccessContract, BrowserSourceHandler, SourceHandler

ACCESS_KEYS = {"min_interval_seconds", "throttle_backoff_seconds", "max_attempts", "timeout_seconds", "list_retention_days",
               "attachment_origins"}
SCHEDULE_KEYS = {"every_minutes", "lookback_days", "max_pages"}
OPTIONAL_SCHEDULE_KEYS = {"initial_backfill_days", "backfill_max_pages"}
SourceTransport = Literal["http", "browser"]
TRANSPORTS = ("http", "browser")
HANDLER_FIELDS = ("source_id", "channels", "windowed")
LIST_METHODS = ("list_request", "parse_list")
HTTP_DETAIL_METHODS = ("detail_fetches", "check_detail")
BROWSER_DETAIL_METHODS = ("browser_detail", "validate_browser_notice")
BROWSER_LIST_METHODS = ("browser_list", "parse_browser_list")
BROWSER_UNSUPPORTED_METHODS = (*HTTP_DETAIL_METHODS, "resolve_detail", "attachment_fetches",
                               "group_request", "expand_group", "to_selected_html")


@dataclass(frozen=True)
class Schedule:
    """Incremental collection: every every_minutes, over the last lookback_days (inclusive of today), up to
    max_pages list pages per channel and window. A round may use the whole period."""

    every_minutes: int
    lookback_days: int
    max_pages: int
    initial_backfill_days: int = 30
    backfill_max_pages: int = 50

    def __post_init__(self):
        if not (5 <= self.every_minutes <= 24 * 60 and 0 <= self.lookback_days <= 31 and 1 <= self.max_pages <= 50):
            raise ValueError("invalid schedule bounds")
        if not (type(self.initial_backfill_days) is int and 0 <= self.initial_backfill_days <= 365
                and type(self.backfill_max_pages) is int and 1 <= self.backfill_max_pages <= 50):
            raise ValueError("invalid initial backfill bounds")


@dataclass(frozen=True)
class DeclaredSource:
    source_id: str
    handler_ref: str
    handler: SourceHandler | BrowserSourceHandler
    access: AccessContract
    schedule: Schedule | None = None
    transport: SourceTransport = "http"

    def access_json(self) -> dict:
        a = self.access
        return {"min_interval_seconds": a.min_interval_seconds, "throttle_backoff_seconds": a.throttle_backoff_seconds,
                "max_attempts": a.max_attempts, "timeout_seconds": a.timeout_seconds,
                "list_retention_days": a.list_retention_days,
                **({"attachment_origins": list(a.attachment_origins)} if a.attachment_origins else {})}


@dataclass(frozen=True)
class CatalogSource:
    source_id: str
    readiness: str
    access_mode: str
    declaration: DeclaredSource | None
    admission: str
    readiness_gap: str | None = None


def register(conn: Connection, source: DeclaredSource) -> str:
    """Upsert the declaration and retain the current operational state."""
    return conn.execute(text("""
        INSERT INTO acq_sources (source_id, handler, access) VALUES (:s, :h, CAST(:a AS jsonb))
        ON CONFLICT (source_id) DO UPDATE SET handler = EXCLUDED.handler, access = EXCLUDED.access,
            updated_at = CASE WHEN acq_sources.handler IS DISTINCT FROM EXCLUDED.handler
                                OR acq_sources.access IS DISTINCT FROM EXCLUDED.access
                              THEN now() ELSE acq_sources.updated_at END
        RETURNING state
    """), {"s": source.source_id, "h": source.handler_ref, "a": json.dumps(source.access_json())}).scalar_one()


def config_fingerprint(source: DeclaredSource) -> str:
    browser = source.transport == "browser"
    contract = [source.handler_ref, source.handler.channels, source.handler.windowed, source.access_json()]
    contract.append({"transport": "browser"} if browser else source.access.headers)
    version = _handler_version(source.source_id, source.handler, required=browser)
    if version is not None:
        contract.append(version)
    if source.schedule is not None and callable(getattr(source.handler, "expand_group", None)):
        contract.append({"grouped_backfill_max_pages": source.schedule.backfill_max_pages})
    canonical = json.dumps(contract, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(canonical.encode()).hexdigest()


def _handler_version(sid: str, handler: SourceHandler | BrowserSourceHandler, *, required: bool) -> int | None:
    version = getattr(handler, "contract_version", None)
    if version is None and not required:
        return None
    if type(version) is not int or version < 1:
        raise ValueError(f"{sid}: handler contract_version must be a positive integer")
    return version


def catalog(packs_dir: str | Path) -> dict[str, CatalogSource]:
    out: dict[str, CatalogSource] = {}
    for manifest in sorted(Path(packs_dir).glob("information_domains/*/pack.yaml")):
        pack_dir = manifest.parent
        ref = yaml.safe_load(manifest.read_text(encoding="utf-8"))["pack"].get("provides", {}).get("sources")
        if ref is None:
            continue
        inventory = _entries(pack_dir / "source-inventory.yaml")
        declarations = _entries(pack_dir / ref)
        if missing := declarations.keys() - inventory.keys():
            raise ValueError(f"{pack_dir.name}: sources missing from inventory: {sorted(missing)}")
        for source_id, entry in inventory.items():
            if source_id in out:
                raise ValueError(f"source listed twice: {source_id}")
            item = declarations.get(source_id)
            out[source_id] = _catalog_source(pack_dir, entry, item)
    return out


def load(packs_dir: str | Path, *, transport: SourceTransport | None = None) -> dict[str, DeclaredSource]:
    """Load active sources from the shared catalog. Execution roles select their transport."""
    if transport is not None and transport not in TRANSPORTS:
        raise ValueError(f"unknown source transport {transport!r}")
    return {sid: s.declaration for sid, s in catalog(packs_dir).items()
            if s.admission == "active" and s.declaration is not None
            and (transport is None or s.declaration.transport == transport)}


def for_validation(packs_dir: str | Path, source_id: str) -> tuple[CatalogSource, DeclaredSource]:
    entry = catalog(packs_dir).get(source_id)
    if entry is None:
        raise ValueError(f"unknown source {source_id!r}; use --list-sources")
    if entry.access_mode != "public" or entry.admission == "authorization_required":
        raise ValueError(f"source {source_id!r}: authorization_required")
    if entry.declaration is None or entry.admission not in {"active", "staged"}:
        raise ValueError(f"source {source_id!r}: {entry.admission}")
    if entry.declaration.transport == "browser":
        raise ValueError(f"source {source_id!r}: browser acquisition role is required")
    return entry, entry.declaration


def _entries(path: Path) -> dict:
    entries = {}
    for item in yaml.safe_load(path.read_text(encoding="utf-8"))["sources"]:
        source_id = item["source_id"]
        if source_id in entries:
            raise ValueError(f"{path}: source listed twice: {source_id}")
        entries[source_id] = item
    return entries


def _catalog_source(pack_dir: Path, entry: dict, item: dict | None) -> CatalogSource:
    sid, readiness = entry["source_id"], entry["readiness"]
    access_mode = entry.get("reference_access_mode", "public")
    gap = entry.get("readiness_gap")
    if item is None:
        admission = "authorization_required" if access_mode == "authenticated" else "handler_pending"
        return CatalogSource(sid, readiness, access_mode, None, admission, gap)
    admission = item.get("admission", "active")
    if admission not in ("active", "staged"):
        raise ValueError(f"{pack_dir.name}: invalid admission for {sid}")
    if admission == "active":
        if readiness != "capability_verified":
            raise ValueError(f"{pack_dir.name}: {sid} readiness is {readiness!r}, not capability_verified")
        if access_mode != "public":
            raise ValueError(f"{pack_dir.name}: {sid} requires an authorized source transport")
    if access_mode == "authenticated":
        admission = "authorization_required"
    declared = _declared(pack_dir, item)
    if admission == "active" and callable(getattr(declared.handler, "expand_group", None)):
        for method in ("group_request", "to_selected_html"):
            if not callable(getattr(declared.handler, method, None)):
                raise ValueError(f"{pack_dir.name}: {sid} grouped source requires {method}")
    return CatalogSource(sid, readiness, access_mode, declared, admission, gap)


def _declared(pack_dir: Path, item: dict) -> DeclaredSource:
    required = {"source_id", "handler", "access"}
    if not required <= set(item) <= required | {"schedule", "admission", "transport"}:
        raise ValueError(f"{pack_dir.name}: source declaration requires source_id, handler and access")
    schedule = item.get("schedule")
    if schedule is not None and (not isinstance(schedule, dict)
                                 or not SCHEDULE_KEYS <= set(schedule) <= SCHEDULE_KEYS | OPTIONAL_SCHEDULE_KEYS):
        raise ValueError(f"{pack_dir.name}: {item['source_id']} invalid schedule keys")
    sid, ref, access = item["source_id"], item["handler"], item["access"]
    transport = item.get("transport", "http")
    if transport not in TRANSPORTS:
        raise ValueError(f"{pack_dir.name}: {sid} unknown source transport {transport!r}")
    if not isinstance(ref, str) or not ENTRY_PATTERN.fullmatch(ref):
        raise ValueError(f"{pack_dir.name}: invalid handler entry {ref!r}")
    if not isinstance(access, dict) or not set(access) <= ACCESS_KEYS:
        raise ValueError(f"{pack_dir.name}: {sid} access keys must be within {sorted(ACCESS_KEYS)}")
    if transport == "browser" and access.get("attachment_origins"):
        raise ValueError(f"{pack_dir.name}: {sid} browser sources cannot declare attachment origins")
    module, cls = ref.split(":")
    handler = getattr(load_pack_module(pack_dir, module), cls)()
    fields = (*HANDLER_FIELDS, *LIST_METHODS, *HTTP_DETAIL_METHODS) if transport == "http" else HANDLER_FIELDS
    for attr in fields:
        if not hasattr(handler, attr):
            raise ValueError(f"{pack_dir.name}: handler {ref} lacks {attr}")
    if handler.source_id != sid or not handler.channels:
        raise ValueError(f"{pack_dir.name}: handler {ref} does not serve {sid}")
    if transport == "browser":
        if (type(handler.windowed) is not bool or not isinstance(handler.channels, tuple)
                or any(not isinstance(channel, str) or not channel.strip() for channel in handler.channels)
                or len(set(handler.channels)) != len(handler.channels)):
            raise ValueError(f"{pack_dir.name}: browser handler {ref} has invalid channels or windowed")
        _handler_version(sid, handler, required=True)
        rendered_list = any(hasattr(handler, method) for method in BROWSER_LIST_METHODS)
        list_methods = BROWSER_LIST_METHODS if rendered_list else LIST_METHODS
        if rendered_list and any(hasattr(handler, method) for method in (*LIST_METHODS, "list_plan", "check_list_preflight")):
            raise ValueError(f"{pack_dir.name}: browser handler {ref} must declare exactly one listing contract")
        for method in (*list_methods, *BROWSER_DETAIL_METHODS):
            if not callable(getattr(handler, method, None)):
                raise ValueError(f"{pack_dir.name}: browser handler {ref} requires callable {method}")
    forbidden = (*BROWSER_DETAIL_METHODS, *BROWSER_LIST_METHODS) if transport == "http" else BROWSER_UNSUPPORTED_METHODS
    for method in forbidden:
        if hasattr(handler, method):
            raise ValueError(f"{pack_dir.name}: {transport} handler {ref} cannot implement {method}")
    return DeclaredSource(sid, ref, handler, AccessContract(**access), Schedule(**schedule) if schedule else None,
                          transport=transport)
