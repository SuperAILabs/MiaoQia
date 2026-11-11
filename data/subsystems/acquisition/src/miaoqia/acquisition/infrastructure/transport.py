"""HTTP transport for the fetch runtime (standard library only).

Returns every HTTP status as a response so the runtime can classify throttling and errors; only network failures
raise. Redirects are followed by urllib; response bodies are capped.
"""
from __future__ import annotations

import ssl
import urllib.error
import urllib.parse
import urllib.request

from ..api.handlers import FetchRequest, FetchResponse

USER_AGENT = "Mozilla/5.0 (compatible; MiaoQiaCollector/0.1)"
MAX_BODY = 20 * 1024 * 1024


class TransportError(RuntimeError):
    """Network-level failure (DNS, connect, TLS, timeout, truncated body)."""


class UrllibTransport:
    def __init__(self, *, max_body: int = MAX_BODY):
        self.max_body = max_body
        self.context = ssl.create_default_context()

    def send(self, request: FetchRequest, timeout: float) -> FetchResponse:
        headers = {"User-Agent": USER_AGENT, **dict(request.headers)}
        body = None
        if request.form:
            body = urllib.parse.urlencode(request.form).encode("utf-8")
            headers.setdefault("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
        elif request.json_body is not None:
            body = request.json_body.encode("utf-8")
            headers.setdefault("Content-Type", "application/json;charset=UTF-8")
        req = urllib.request.Request(request.url, data=body, headers=headers, method=request.method)
        try:
            with urllib.request.urlopen(req, timeout=timeout, context=self.context) as r:
                return FetchResponse(r.status, _media_type(r.headers.get("Content-Type")), self._read(r), r.geturl())
        except urllib.error.HTTPError as e:
            data = self._read(e) if e.fp is not None else b""
            return FetchResponse(e.code, _media_type(e.headers.get("Content-Type") if e.headers else None), data, request.url)
        except (urllib.error.URLError, TimeoutError, OSError) as e:
            raise TransportError(type(e).__name__) from e

    def _read(self, stream) -> bytes:
        data = stream.read(self.max_body + 1)
        if len(data) > self.max_body:
            raise TransportError("response exceeds size cap")
        return data


def _media_type(content_type: str | None) -> str:
    return (content_type or "application/octet-stream").split(";", 1)[0].strip().lower()
