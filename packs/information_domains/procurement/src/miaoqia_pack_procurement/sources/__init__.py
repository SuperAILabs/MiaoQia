"""Shared helpers for the procurement pack's publisher-specific handlers.

Source declarations and access contracts are in sources.yaml. Verified
endpoints and coverage are recorded in docs/05-delivery/12-public-source-probe.md.
"""
from __future__ import annotations

import json
from datetime import date, datetime, timedelta, timezone

from miaoqia.acquisition.api.handlers import DetailFetch, DetailRef, FetchResponse

CHINA = timezone(timedelta(hours=8))


def json_body(response: FetchResponse) -> dict:
    try:
        data = json.loads(response.body.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError("response is not JSON") from exc
    if not isinstance(data, dict):
        raise ValueError("response is not a JSON object")
    return data


def html_text(response: FetchResponse) -> str:
    if response.media_type not in ("text/html", "text/plain"):
        raise ValueError(f"unexpected media type {response.media_type}")
    try:
        return response.body.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise ValueError("response is not UTF-8") from exc


def china_date(epoch_ms: int) -> date:
    return datetime.fromtimestamp(epoch_ms / 1000, CHINA).date()


class DiscoveryOnly:
    """Keep publisher list identities until its body retrieval is verified."""

    @staticmethod
    def to_html(raw: bytes) -> str:
        raise ValueError("this listing has no independently fetchable announcement body")

    def detail_fetches(self, channel: str, ref: DetailRef) -> tuple[DetailFetch, ...]:
        return ()

    def check_detail(self, response: FetchResponse) -> None:
        self.to_html(response.body)
