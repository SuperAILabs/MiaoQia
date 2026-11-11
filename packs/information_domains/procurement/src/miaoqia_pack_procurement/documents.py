"""Document adapters: the announcement HTML inside a live source's original response (evidence live ingest)."""
from __future__ import annotations

import json
from html import escape


def anhui_ccgp_html(raw: bytes) -> str:
    """安徽政府采购网详情接口：正文 HTML 在 result.data.content，标题另在 result.data.title。"""
    body = json.loads(raw.decode("utf-8"))
    data = (body.get("result") or {}).get("data") or {}
    content, title = data.get("content"), data.get("title") or ""
    if body.get("success") is not True or not isinstance(content, str) or not content.strip():
        raise ValueError("detail response has no content")
    # The title is outside the content HTML; keep it as the first block like the site page shows it.
    return f"<html><body><h1>{escape(title)}</h1>{content}</body></html>"
