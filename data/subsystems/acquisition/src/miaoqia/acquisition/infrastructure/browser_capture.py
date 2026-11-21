"""Passive same-response original capture for identity-encoded Chromium resources."""
from __future__ import annotations

import asyncio
import base64
import math
from collections.abc import AsyncIterator, Awaitable, Callable
from dataclasses import dataclass
from ipaddress import ip_address
from typing import Protocol

from ..api.handlers import FetchResponse
from .transport import MAX_BODY, _media_type, request_origin


class BrowserCaptureError(RuntimeError):
    """The observation cannot supply a verified original."""


class CDPSession(Protocol):
    async def send(self, method: str, params: dict) -> dict: ...


@dataclass(frozen=True)
class BrowserCapture:
    response: FetchResponse
    method: str
    request_id: str
    frame_id: str
    loader_id: str
    remote_ip: str
    remote_port: int


def _headers(headers: dict) -> dict[str, str]:
    normalized = {}
    for name, value in headers.items():
        name = name.lower()
        if name in normalized:
            raise BrowserCaptureError("duplicate CDP header")
        normalized[name] = value
    return normalized


def _declared_length(headers: dict[str, str], max_body: int) -> int | None:
    if headers.get("content-encoding", "identity").strip(" \t").lower() != "identity":
        raise BrowserCaptureError("non-identity content encoding")
    length = headers.get("content-length")
    transfer = headers.get("transfer-encoding")
    if "content-range" in headers or (
        transfer is not None and (transfer.strip(" \t").lower() != "chunked" or length is not None)
    ):
        raise BrowserCaptureError("unsupported response framing")
    if length is None:
        return None
    length = length.strip(" \t")
    if not length.isascii() or not length.isdecimal():
        raise BrowserCaptureError("invalid Content-Length")
    digits = length.lstrip("0") or "0"
    if len(digits) > len(str(max_body)) or int(digits) > max_body:
        raise BrowserCaptureError("response exceeds size cap")
    return int(digits)


def _append(body: bytearray, encoded: str, budget: int) -> None:
    remaining = budget - len(body)
    if not isinstance(encoded, str):
        raise BrowserCaptureError("missing CDP binary data")
    if len(encoded) > 4 * ((remaining + 2) // 3):
        raise BrowserCaptureError("response exceeds size cap")
    try:
        chunk = base64.b64decode(encoded, validate=True)
    except ValueError as exc:
        raise BrowserCaptureError("invalid CDP binary data") from exc
    if len(chunk) > remaining:
        raise BrowserCaptureError("response exceeds size cap")
    body.extend(chunk)


def _check_authority(headers: dict, origin: tuple[str, str, int]) -> None:
    headers = _headers(headers)
    authorities = [headers[name] for name in (":authority", "host") if name in headers]
    if not authorities:
        raise BrowserCaptureError("missing wire authority")
    for authority in authorities:
        if (not authority or any(char in authority for char in "/?#@\\,")
                or request_origin(f"{origin[0]}://{authority}") != origin):
            raise BrowserCaptureError("unexpected wire authority")


def _save_extra(extras: dict, request_id: str, event: dict) -> None:
    if request_id in extras:
        extras[request_id] = None
    else:
        extras[request_id] = event


async def capture_browser_response(
    session: CDPSession,
    events: AsyncIterator[tuple[str, dict]],
    *,
    method: str,
    url: str,
    frame_id: str,
    loader_id: str,
    allowed_ips: frozenset[str],
    timeout: float,
    max_body: int = MAX_BODY,
    max_events: int = 4096,
    buffered_body: Callable[[str], Awaitable[bytes | None]] | None = None,
) -> BrowserCapture:
    """Read one original from a trusted, target-scoped Chromium CDP observation.

    The caller enables Network on a dedicated page session before navigation and supplies
    the approved request and main-frame loader for this attempt. The adapter provides ordered,
    lossless events, including both ExtraInfo streams. It bounds message and queue sizes and
    raises on overflow, disconnect or cancellation. Normal iterator exhaustion is an explicit
    end-of-observation barrier after navigation and selection, with pending events drained.
    CDP sends and event reads must be cancellable. The caller owns CDP and browser cleanup.

    A separately confined transport must enforce approved destinations, TLS, source limits and
    access decisions before requests go out. The IP and authority checks here verify evidence
    after receipt. The caller separately binds the returned receipt to the selected notice and
    its inert projection before archival. This collector issues only passive CDP commands.
    A trusted response gate can supply exact bytes buffered under the same network request ID.
    """
    if (type(max_body) is not int or max_body <= 0 or type(max_events) is not int or max_events <= 0
            or not math.isfinite(timeout) or timeout <= 0 or not method or method == "HEAD"
            or not frame_id or not loader_id or not allowed_ips):
        raise ValueError("capture requires positive budgets and an approved request, loader and endpoint")
    origin = request_origin(url)
    endpoints = {ip_address(address) for address in allowed_ips}
    loop = asyncio.get_running_loop()
    deadline = loop.time() + timeout
    request_id = None
    seen_requests: set[str] = set()
    disqualified: set[str] = set()
    request_extra: dict[str, dict | None] = {}
    response_extra: dict[str, dict | None] = {}
    body = bytearray()
    response_headers = None
    declared_length = None
    remote_ip = None
    buffer_size = 0
    gate_buffered = False
    unstreamed_bytes = 0
    saw_stream_data = False
    finished = False
    event_count = 0
    try:
        async with asyncio.timeout_at(deadline):
            async for name, event in events:
                event_count += 1
                if event_count > max_events:
                    raise BrowserCaptureError("capture exceeds event cap")
                if loop.time() >= deadline:
                    raise TimeoutError
                event_id = event.get("requestId")
                if not event_id:
                    continue
                if name == "Network.requestWillBeSentExtraInfo":
                    _save_extra(request_extra, event_id, event)
                elif name == "Network.responseReceivedExtraInfo":
                    _save_extra(response_extra, event_id, event)
                elif name in {"Network.loadingFailed", "Network.requestServedFromCache"}:
                    disqualified.add(event_id)
                    if event_id == request_id:
                        raise BrowserCaptureError("request failed or used cache")
                elif name == "Network.requestWillBeSent":
                    if event_id in seen_requests or "redirectResponse" in event:
                        disqualified.add(event_id)
                    seen_requests.add(event_id)
                    request = event["request"]
                    matches = (request["url"], request["method"], event.get("frameId"), event.get("loaderId"))
                    if matches == (url, method, frame_id, loader_id):
                        if request_id is not None:
                            raise BrowserCaptureError("ambiguous approved request")
                        request_id = event_id
                    if event_id == request_id and event_id in disqualified:
                        raise BrowserCaptureError("redirected or disqualified request")
                elif event_id != request_id:
                    continue
                elif name == "Network.responseReceived":
                    response = event["response"]
                    if (response_headers is not None or finished
                            or (response["url"], event.get("frameId"), event.get("loaderId")) != (url, frame_id, loader_id)
                            or response["status"] != 200
                            or any(response.get(flag) for flag in ("fromDiskCache", "fromServiceWorker", "fromPrefetchCache"))):
                        raise BrowserCaptureError("ineligible response")
                    address = ip_address(response["remoteIPAddress"].removeprefix("[").removesuffix("]"))
                    if address not in endpoints or response.get("remotePort") != origin[2]:
                        raise BrowserCaptureError("unexpected connection endpoint")
                    remote_ip = str(address)
                    response_headers = _headers(response["headers"])
                    declared_length = _declared_length(response_headers, max_body)
                    captured = await buffered_body(request_id) if buffered_body is not None else None
                    if captured is None:
                        stream = await session.send("Network.streamResourceContent", {"requestId": request_id})
                        _append(body, stream["bufferedData"], max_body if declared_length is None else declared_length)
                    else:
                        if not isinstance(captured, bytes) or len(captured) != declared_length:
                            raise BrowserCaptureError("buffered browser response length mismatch")
                        body.extend(captured)
                        gate_buffered = True
                    buffer_size = len(body)
                elif name == "Network.dataReceived":
                    length = event["dataLength"]
                    if finished or response_headers is None or type(length) is not int or length < 0:
                        raise BrowserCaptureError("invalid CDP data event")
                    if gate_buffered and "data" in event:
                        raise BrowserCaptureError("ambiguous browser body capture")
                    if "data" in event:
                        saw_stream_data = True
                        previous_size = len(body)
                        _append(body, event["data"], max_body if declared_length is None else declared_length)
                        if len(body) - previous_size != length:
                            raise BrowserCaptureError("CDP chunk length mismatch")
                    elif length:
                        if saw_stream_data:
                            raise BrowserCaptureError("missing CDP stream chunk")
                        unstreamed_bytes += length
                        if unstreamed_bytes > buffer_size:
                            raise BrowserCaptureError("CDP buffer length mismatch")
                elif name == "Network.loadingFinished":
                    if finished or response_headers is None:
                        raise BrowserCaptureError("unexpected CDP completion")
                    finished = True

            if not finished or response_headers is None or request_id is None or remote_ip is None:
                raise BrowserCaptureError("no completed approved response")
            if unstreamed_bytes != buffer_size:
                raise BrowserCaptureError("CDP buffer length mismatch")
            wire_request = request_extra.get(request_id)
            wire_response = response_extra.get(request_id)
            if wire_request is None or wire_response is None or wire_response.get("statusCode") != 200:
                raise BrowserCaptureError("missing or ineligible wire metadata")
            _check_authority(wire_request["headers"], origin)
            wire_length = _declared_length(_headers(wire_response["headers"]), max_body)
            if wire_length != declared_length or (declared_length is not None and len(body) != declared_length):
                raise BrowserCaptureError("response length mismatch")
            frame = (await session.send("Page.getFrameTree", {}))["frameTree"]["frame"]
            if (frame.get("id"), frame.get("loaderId")) != (frame_id, loader_id):
                raise BrowserCaptureError("main frame or loader changed")
            if loop.time() >= deadline:
                raise TimeoutError
            response = FetchResponse(200, _media_type(response_headers.get("content-type")), bytes(body), url)
            return BrowserCapture(response, method, request_id, frame_id, loader_id, remote_ip, origin[2])
    except BrowserCaptureError:
        raise
    except TimeoutError as exc:
        raise BrowserCaptureError("capture deadline exceeded") from exc
    except Exception as exc:
        raise BrowserCaptureError("invalid or interrupted CDP observation") from exc
