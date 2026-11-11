"""Source handlers for the Anhui pilot (SPI: miaoqia.acquisition.api.handlers.SourceHandler).

Endpoints, parameters and limits come from the D1b acceptance record
(docs/05-delivery/11-d1b-anhui-pilot-sources.md); declarations and access contracts are in sources.yaml.
"""
from __future__ import annotations

import json
from datetime import date, datetime, timedelta, timezone

from miaoqia.acquisition.api.handlers import FetchResponse

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
