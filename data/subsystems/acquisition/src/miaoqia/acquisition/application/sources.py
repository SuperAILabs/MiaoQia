"""Source declarations: which inventory sources have a handler and access contract (pack.yaml provides.sources).

A declaration is accepted only for an inventory entry whose readiness is capability_verified, so collection never
starts for a source whose list/detail capability has not been verified (D1b, docs/05-delivery/11-d1b-anhui-pilot-sources.md).
"""
from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

import yaml

from miaoqia.sdk.entrypoints import ENTRY_PATTERN, load_pack_module

from ..api.handlers import AccessContract, SourceHandler

ACCESS_KEYS = {"min_interval_seconds", "throttle_backoff_seconds", "max_attempts", "timeout_seconds"}
SCHEDULE_KEYS = {"every_minutes", "lookback_days", "max_pages"}


@dataclass(frozen=True)
class Schedule:
    """Incremental collection: every every_minutes, over the last lookback_days (inclusive of today), up to
    max_pages list pages per channel and window. A round may use the whole period."""

    every_minutes: int
    lookback_days: int
    max_pages: int

    def __post_init__(self):
        if not (5 <= self.every_minutes <= 24 * 60 and 0 <= self.lookback_days <= 31 and 1 <= self.max_pages <= 50):
            raise ValueError("invalid schedule bounds")
HANDLER_ATTRS = ("source_id", "channels", "windowed", "list_request", "parse_list", "detail_fetches", "check_detail")


@dataclass(frozen=True)
class DeclaredSource:
    source_id: str
    handler_ref: str
    handler: SourceHandler
    access: AccessContract
    schedule: Schedule | None = None

    def access_json(self) -> dict:
        a = self.access
        return {"min_interval_seconds": a.min_interval_seconds, "throttle_backoff_seconds": a.throttle_backoff_seconds,
                "max_attempts": a.max_attempts, "timeout_seconds": a.timeout_seconds}


def load(packs_dir: str | Path) -> dict[str, DeclaredSource]:
    out: dict[str, DeclaredSource] = {}
    for manifest in sorted(Path(packs_dir).glob("information_domains/*/pack.yaml")):
        pack_dir = manifest.parent
        ref = yaml.safe_load(manifest.read_text(encoding="utf-8"))["pack"].get("provides", {}).get("sources")
        if ref is None:
            continue
        inventory = {e["source_id"]: e for e in
                     yaml.safe_load((pack_dir / "source-inventory.yaml").read_text(encoding="utf-8"))["sources"]}
        for item in yaml.safe_load((pack_dir / ref).read_text(encoding="utf-8"))["sources"]:
            declared = _declared(pack_dir, item, inventory)
            if declared.source_id in out:
                raise ValueError(f"source declared twice: {declared.source_id}")
            out[declared.source_id] = declared
    return out


def _declared(pack_dir: Path, item: dict, inventory: dict) -> DeclaredSource:
    if not {"source_id", "handler", "access"} <= set(item) <= {"source_id", "handler", "access", "schedule"}:
        raise ValueError(f"{pack_dir.name}: source declaration fields must be source_id, handler, access and optional schedule")
    schedule = item.get("schedule")
    if schedule is not None and (not isinstance(schedule, dict) or set(schedule) != SCHEDULE_KEYS):
        raise ValueError(f"{pack_dir.name}: {item['source_id']} schedule keys must be {sorted(SCHEDULE_KEYS)}")
    sid, ref, access = item["source_id"], item["handler"], item["access"]
    entry = inventory.get(sid)
    if entry is None:
        raise ValueError(f"{pack_dir.name}: {sid} is not in the source inventory")
    if entry.get("readiness") != "capability_verified":
        raise ValueError(f"{pack_dir.name}: {sid} readiness is {entry.get('readiness')!r}, not capability_verified")
    if not isinstance(ref, str) or not ENTRY_PATTERN.fullmatch(ref):
        raise ValueError(f"{pack_dir.name}: invalid handler entry {ref!r}")
    if not isinstance(access, dict) or not set(access) <= ACCESS_KEYS:
        raise ValueError(f"{pack_dir.name}: {sid} access keys must be within {sorted(ACCESS_KEYS)}")
    module, cls = ref.split(":")
    handler = getattr(load_pack_module(pack_dir, module), cls)()
    for attr in HANDLER_ATTRS:
        if not hasattr(handler, attr):
            raise ValueError(f"{pack_dir.name}: handler {ref} lacks {attr}")
    if handler.source_id != sid or not handler.channels:
        raise ValueError(f"{pack_dir.name}: handler {ref} does not serve {sid}")
    return DeclaredSource(sid, ref, handler, AccessContract(**access), Schedule(**schedule) if schedule else None)
