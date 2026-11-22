"""Observe one approved navigation on a caller-confined Chromium page."""
from __future__ import annotations

import asyncio
import json
import math
from collections import deque
from collections.abc import AsyncIterator, Awaitable, Callable
from dataclasses import dataclass
from typing import Any, Protocol, TypeVar

from .browser_capture import BrowserCapture, BrowserCaptureError, CDPSession, capture_browser_response
from .transport import MAX_BODY, request_origin

_REQUEST = "Network.requestWillBeSent"
_NETWORK_FIELDS = {
    _REQUEST: ("requestId", "frameId", "loaderId"),
    "Network.requestWillBeSentExtraInfo": ("requestId",),
    "Network.responseReceived": ("requestId", "frameId", "loaderId"),
    "Network.responseReceivedExtraInfo": ("requestId", "statusCode"),
    "Network.dataReceived": ("requestId", "dataLength", "data"),
    "Network.loadingFinished": ("requestId",),
    "Network.loadingFailed": ("requestId",),
    "Network.requestServedFromCache": ("requestId",),
}
_FRAME_NAVIGATED = "Page.frameNavigated"
_PAGE_EVENTS = (_FRAME_NAVIGATED, "Page.navigatedWithinDocument", "Page.frameDetached", "Page.windowOpen")
_WIRE_REQUEST = "Network.requestWillBeSentExtraInfo"
_WIRE_RESPONSE = "Network.responseReceivedExtraInfo"
_COMPLETION_EVENTS = frozenset({"Network.loadingFinished", _WIRE_REQUEST, _WIRE_RESPONSE})
_RESPONSE_FIELDS = ("url", "status", "remoteIPAddress", "remotePort", "fromDiskCache",
                    "fromServiceWorker", "fromPrefetchCache")
_BODY_HEADERS = frozenset({"content-type", "content-encoding", "content-length", "content-range", "transfer-encoding"})
_AUTHORITY_HEADERS = frozenset({"host", ":authority"})


class _ObserverSession(CDPSession, Protocol):
    def on(self, event: str, callback: Callable) -> None: ...

    def remove_listener(self, event: str, callback: Callable) -> None: ...

    async def detach(self) -> None: ...


class _Context(Protocol):
    async def new_cdp_session(self, page: Any) -> _ObserverSession: ...


@dataclass(frozen=True)
class BrowserNavigation:
    navigation_id: str
    frame_id: str
    loader_id: str


class _Selection(Protocol):
    @property
    def html(self) -> str: ...

    @property
    def navigation_id(self) -> str: ...

    @property
    def frame_id(self) -> str: ...

    @property
    def loader_id(self) -> str: ...


_SelectionT = TypeVar("_SelectionT", bound=_Selection)


def _fields(source: dict, keys: tuple[str, ...]) -> dict:
    return {key: source[key] for key in keys if key in source}


def _headers(source: dict, keep: frozenset[str]) -> dict:
    return {key: value for key, value in source["headers"].items() if key.lower() in keep}


def _capture_event(name: str, event: dict) -> dict:
    projected = _fields(event, _NETWORK_FIELDS[name])
    if name == _REQUEST:
        projected["request"] = _fields(event["request"], ("url", "method"))
        if "redirectResponse" in event:
            projected["redirectResponse"] = True
    elif name == "Network.responseReceived":
        response = event["response"]
        projected["response"] = _fields(response, _RESPONSE_FIELDS) | {"headers": _headers(response, _BODY_HEADERS)}
    elif name in (_WIRE_REQUEST, _WIRE_RESPONSE):
        keep = _AUTHORITY_HEADERS if name == _WIRE_REQUEST else _BODY_HEADERS
        projected["headers"] = _headers(event, keep)
    return projected


class _Observation:
    def __init__(self, frame_id: str, navigation_url: str, body_url: str, max_events: int, max_event_bytes: int):
        self.frame_id = frame_id
        self.navigation_url = navigation_url
        self.body_url = body_url
        self.max_events = max_events
        self.max_event_bytes = max_event_bytes
        self.event_count = 0
        self.event_bytes = 0
        self.queue: deque[tuple[str, dict]] = deque()
        self.changed = asyncio.Event()
        self.error: BrowserCaptureError | None = None
        self.navigation: BrowserNavigation | None = None
        self.committed = False
        self.started = False
        self.sealed = False
        self.body_id: str | None = None
        self.completion: dict[str, set[str]] = {}

    def fail(self, message: str) -> None:
        if self.error is None:
            self.error = BrowserCaptureError(message)
        self.changed.set()

    def check(self) -> None:
        if self.error is not None:
            raise self.error

    def receive(self, name: str, event: dict) -> None:
        if self.error is not None:
            return
        try:
            self.event_count += 1
            if self.event_count > self.max_events:
                self.fail("browser event count cap exceeded")
                return
            self.event_bytes += len(name) + len(json.dumps(event, separators=(",", ":"), ensure_ascii=True))
            if self.event_bytes > self.max_event_bytes:
                self.fail("browser event byte cap exceeded")
                return
            if name in _NETWORK_FIELDS:
                self._network(name, event)
                if not self.sealed and self.error is None:
                    self.queue.append((name, _capture_event(name, event)))
            else:
                self._page(name, event)
        except (KeyError, TypeError, ValueError, AttributeError, RecursionError):
            self.fail("invalid browser event")
        self.changed.set()

    def _network(self, name: str, event: dict) -> None:
        request_id = event["requestId"]
        if not isinstance(request_id, str) or not request_id:
            raise ValueError("invalid request ID")
        if name == _REQUEST and event.get("frameId") == self.frame_id:
            request = event["request"]
            if event.get("type") == "Document":
                if (not self.started or self.navigation is not None or "redirectResponse" in event
                        or request["url"] != self.navigation_url or request["method"] != "GET"
                        or not isinstance(event.get("loaderId"), str) or not event["loaderId"]):
                    self.fail("unexpected main-frame navigation")
                    return
                self.navigation = BrowserNavigation(request_id, self.frame_id, event["loaderId"])
            navigation = self.navigation
            if (navigation is not None and event.get("loaderId") == navigation.loader_id
                    and request["url"] == self.body_url and request["method"] == "GET"):
                if self.body_id is not None:
                    self.fail("ambiguous approved request")
                    return
                self.body_id = request_id
        if self.sealed and request_id == self.body_id:
            self.fail("late event for sealed body request")
        elif name in _COMPLETION_EVENTS:
            self.completion.setdefault(request_id, set()).add(name)

    def _page(self, name: str, event: dict) -> None:
        if name == "Page.windowOpen":
            self.fail("popup during browser observation")
        elif name == _FRAME_NAVIGATED:
            frame = event["frame"]
            if "parentId" not in frame:
                navigation = self.navigation
                if (navigation is None or self.committed or (frame["id"], frame["loaderId"], frame["url"]) !=
                        (self.frame_id, navigation.loader_id, self.navigation_url)):
                    self.fail("unexpected main-frame commit")
                else:
                    self.committed = True
        elif event["frameId"] == self.frame_id:
            self.fail("main-frame history change or detach")

    def body_complete(self) -> bool:
        return self.body_id is not None and self.completion.get(self.body_id) == _COMPLETION_EVENTS

    async def wait_for(self, predicate: Callable[[], bool]) -> None:
        while True:
            self.check()
            if predicate():
                return
            self.changed.clear()
            await self.changed.wait()

    async def events(self) -> AsyncIterator[tuple[str, dict]]:
        while True:
            await self.wait_for(lambda: bool(self.queue) or self.sealed)
            if not self.queue:
                return
            yield self.queue.popleft()


async def _frame(session: CDPSession) -> dict:
    return (await session.send("Page.getFrameTree", {}))["frameTree"]["frame"]


async def observe_browser_navigation(
    context: _Context,
    page: Any,
    *,
    navigation_url: str,
    body_url: str,
    allowed_ips: frozenset[str],
    navigate: Callable[[], Awaitable[object]],
    select: Callable[[BrowserNavigation], Awaitable[_SelectionT]],
    timeout: float,
    max_body: int = MAX_BODY,
    max_events: int = 4096,
    max_event_bytes: int = 32 * 1024 * 1024,
    max_html_bytes: int = 1024 * 1024,
    buffered_body: Callable[[str], Awaitable[bytes | None]] | None = None,
) -> tuple[BrowserCapture, _SelectionT]:
    """Pair the CDP original and the trusted callback's main-frame selection.

    Supply an exclusive, fresh about:blank page in an anonymous Chromium context. The caller
    must install independent confinement before invoking this function, including TLS, route
    and endpoint policy, source budgets and prevention of extra targets. The caller owns and
    closes the page and context on every outcome. The observer owns its CDP session.

    `navigate` starts one approved main-frame GET. `select` reads the uniquely selected region
    in that same page's main frame and returns HTML with the supplied navigation identity.
    Both callbacks must be cancellable. The caller verifies publisher identity and original to
    selection correspondence before archival. Timeout covers capture, plus up to one second
    reserved for CDP detach. `max_event_bytes` bounds cumulative serialized event input to the
    adapter, after Playwright receives it. Bound transport ingress separately in deployment.
    """
    if (not math.isfinite(timeout) or timeout <= 0 or not allowed_ips
            or any(type(limit) is not int or limit <= 0 for limit in (max_body, max_events, max_event_bytes, max_html_bytes))):
        raise ValueError("browser observation requires positive budgets and approved endpoints")
    request_origin(navigation_url)
    request_origin(body_url)
    loop = asyncio.get_running_loop()
    deadline = loop.time() + timeout
    session = None
    listeners = []
    succeeded = False
    try:
        async with asyncio.timeout_at(deadline):
            session = await context.new_cdp_session(page)
            initial = await _frame(session)
            if initial["url"] != "about:blank" or not initial["id"] or "parentId" in initial:
                raise BrowserCaptureError("browser observer requires a fresh blank main frame")
            observer = _Observation(initial["id"], navigation_url, body_url, max_events, max_event_bytes)
            for event in (*_NETWORK_FIELDS, *_PAGE_EVENTS):
                callback = lambda params, name=event: observer.receive(name, params)
                session.on(event, callback)
                listeners.append((event, callback))
            on_close = lambda *_: observer.fail("browser CDP session closed")
            session.on("close", on_close)
            listeners.append(("close", on_close))
            await session.send("Page.enable", {})
            await session.send("Network.enable", {"maxTotalBufferSize": 2 * max_body,
                                                    "maxResourceBufferSize": max_body, "maxPostDataSize": 0})
            await session.send("Network.setCacheDisabled", {"cacheDisabled": True})
            await session.send("Network.setBypassServiceWorker", {"bypass": True})
            observer.check()

            async def pin() -> BrowserNavigation:
                await observer.wait_for(lambda: observer.navigation is not None)
                assert observer.navigation is not None
                return observer.navigation

            async def collect() -> BrowserCapture:
                navigation = await pin()
                return await capture_browser_response(
                    session, observer.events(), method="GET", url=body_url, frame_id=navigation.frame_id,
                    loader_id=navigation.loader_id, allowed_ips=allowed_ips, timeout=max(0.000001, deadline - loop.time()),
                    max_body=max_body, max_events=max_events, buffered_body=buffered_body,
                )

            async def select_and_seal() -> _SelectionT:
                observer.started = True
                await navigate()
                navigation = await pin()
                await observer.wait_for(lambda: observer.committed)

                async def check_frame() -> None:
                    current = await _frame(session)
                    observer.check()
                    if (current["id"], current["loaderId"], current["url"]) != (
                        navigation.frame_id, navigation.loader_id, navigation_url
                    ):
                        raise BrowserCaptureError("main frame changed during selection")

                await check_frame()
                selection = await select(navigation)
                if (selection.navigation_id, selection.frame_id, selection.loader_id) != (
                    navigation.navigation_id, navigation.frame_id, navigation.loader_id
                ):
                    raise BrowserCaptureError("selection belongs to a different navigation")
                html = selection.html
                if (not isinstance(html, str) or len(html) > max_html_bytes
                        or not html.strip() or len(html.encode("utf-8")) > max_html_bytes):
                    raise BrowserCaptureError("invalid or oversized HTML selection")
                await observer.wait_for(observer.body_complete)
                await check_frame()
                observer.sealed = True
                observer.changed.set()
                return selection

            tasks = (asyncio.create_task(collect()), asyncio.create_task(select_and_seal()))
            try:
                capture, selection = await asyncio.gather(*tasks)
            finally:
                for task in tasks:
                    if not task.done():
                        task.cancel()
                await asyncio.gather(*tasks, return_exceptions=True)
            observer.check()
            if loop.time() >= deadline:
                raise TimeoutError
            succeeded = True
            return capture, selection
    except BrowserCaptureError as exc:
        raise BrowserCaptureError(str(exc)) from None
    except TimeoutError:
        raise BrowserCaptureError("browser observation deadline exceeded") from None
    except Exception:
        raise BrowserCaptureError("browser session or callback failed") from None
    finally:
        if session is not None:
            try:
                try:
                    for event, callback in listeners:
                        session.remove_listener(event, callback)
                finally:
                    async with asyncio.timeout(1):
                        await session.detach()
            except Exception:
                if succeeded:
                    raise BrowserCaptureError("browser observer failed to detach") from None
