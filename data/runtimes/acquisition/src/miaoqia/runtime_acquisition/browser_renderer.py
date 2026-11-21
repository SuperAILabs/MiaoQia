"""Chromium request gate for independently confined, operator-supplied pages."""
from __future__ import annotations

import asyncio
import math
from collections.abc import Callable, Coroutine
from concurrent.futures import Future
from contextlib import AbstractAsyncContextManager, asynccontextmanager
from contextvars import copy_context
from dataclasses import dataclass, field
from ipaddress import ip_address
from typing import Any

from miaoqia.acquisition.api.browser_projection import MAX_SELECTED_HTML_BYTES
from miaoqia.acquisition.api.handlers import BrowserPagePlan, BrowserSelection, FetchRequest
from miaoqia.acquisition.application.browser_jobs import Permit
from miaoqia.acquisition.infrastructure.browser_capture import BrowserCapture, BrowserCaptureError, _append, _declared_length
from miaoqia.acquisition.infrastructure.browser_session import BrowserNavigation, observe_browser_navigation
from miaoqia.acquisition.infrastructure.transport import MAX_BODY, request_origin

_MAX_EARLY_BODY_BYTES = 64 * 1024


@dataclass(frozen=True)
class ConfinedPage:
    context: Any
    page: Any
    allowed_ips: frozenset[str]


PageProvider = Callable[[BrowserPagePlan], AbstractAsyncContextManager[ConfinedPage]]
Receipt = tuple[int, str | None]


@dataclass
class _Hop:
    request: FetchRequest
    fetch_id: str
    receipt: Receipt | None = None
    continued: bool = False
    response_continued: bool = False
    outcome: Future[Receipt | None] = field(default_factory=Future)
    settled: asyncio.Event = field(default_factory=asyncio.Event)
    buffer_ready: asyncio.Event = field(default_factory=asyncio.Event)
    buffered_body: bytes | None = None

    def finish(self, receipt: Receipt | None) -> None:
        if not self.outcome.done():
            self.outcome.set_result(receipt)


class _Gate:
    def __init__(self, plan: BrowserPagePlan, permit: Permit):
        self.loop = asyncio.get_running_loop()
        self.navigation = FetchRequest("GET", plan.navigation_url)
        self.body = plan.body_request
        self.permit = permit
        self.hops: dict[str, _Hop] = {}
        self.tasks: set[asyncio.Task] = set()
        self.workers: list[asyncio.Future] = []
        self.failure: asyncio.Future[Exception] = self.loop.create_future()
        self.body_seen = asyncio.Event()
        self.frame_id = ""
        self.session: Any = None
        self.closing = False
        self.navigating = False

    def fail(self, message: str) -> None:
        if not self.closing and not self.failure.done():
            self.failure.set_result(BrowserCaptureError(message))

    def check(self) -> None:
        if self.failure.done():
            raise self.failure.result()
        if self.closing:
            raise BrowserCaptureError("browser request gate is closed")

    async def _send(self, method: str, params: dict) -> dict:
        self.check()
        try:
            return await self.session.send(method, params)
        except Exception:
            raise BrowserCaptureError("Chromium request gate command failed") from None

    def _start(self, operation: Coroutine) -> None:
        task = self.loop.create_task(operation)
        self.tasks.add(task)

        def finished(result: asyncio.Task) -> None:
            self.tasks.discard(result)
            if not result.cancelled() and (error := result.exception()) is not None and not self.failure.done():
                self.failure.set_result(error)

        task.add_done_callback(finished)

    def _expected(self, url: str, resource_type: str) -> FetchRequest | None:
        if url == self.navigation.url and resource_type == "document":
            return self.navigation
        if url == self.body.url and resource_type in ("fetch", "xhr"):
            return self.body
        return None

    async def _install_context(self, context: Any, page: Any) -> None:
        async def route_http(route: Any) -> None:
            try:
                request = route.request
                allowed = (self.navigating and not self.closing and not self.failure.done()
                           and request.method == "GET" and request.redirected_from is None
                           and self._expected(request.url, request.resource_type) is not None
                           and request.frame == page.main_frame)
            except Exception:
                # Recent popups and requests without a page frame can fail
                # to resolve the Playwright frame. They have no admission.
                allowed = False
            try:
                if allowed:
                    # Keep any narrower provider routes in effect.
                    await route.fallback()
                else:
                    self.fail("unapproved browser context request")
                    await route.abort("blockedbyclient")
            except Exception:
                self.fail("Chromium context routing command failed")

        async def route_socket(route: Any) -> None:
            self.fail("unapproved browser context WebSocket")
            try:
                await route.close(code=1008)
            except Exception:
                self.fail("Chromium context WebSocket close failed")

        try:
            if not callable(getattr(context, "route_web_socket", None)):
                raise TypeError
            await context.route("**/*", route_http)
            await context.route_web_socket("**/*", route_socket)
        except Exception:
            raise BrowserCaptureError("Chromium context routing could not be installed") from None

    async def install(self, prepared: ConfinedPage) -> None:
        context, page = prepared.context, prepared.page
        if context.pages != [page] or len(page.frames) != 1 or page.url != "about:blank":
            raise BrowserCaptureError("browser provider must supply one fresh main frame")
        await self._install_context(context, page)
        context.on("page", lambda *_: self.fail("unexpected browser target"))
        for name in ("close", "crash", "frameattached", "download", "websocket", "worker"):
            page.on(name, lambda *_, event=name: self.fail(f"unexpected browser {event}"))
        self.session = await context.new_cdp_session(page)
        self.session.on("close", lambda *_: self.fail("request gate disconnected"))
        self.session.on("Fetch.requestPaused", self._paused)
        self.session.on("Network.loadingFinished", lambda event: self._terminal(event, completed=True))
        self.session.on("Network.loadingFailed", lambda event: self._terminal(event, completed=False))
        self.session.on("Network.requestServedFromCache", self._cached)
        frame = (await self._send("Page.getFrameTree", {}))["frameTree"]["frame"]
        self.frame_id = frame["id"]
        if not self.frame_id or frame["url"] != "about:blank" or "parentId" in frame:
            raise BrowserCaptureError("browser gate requires a fresh blank main frame")
        await self._send("Network.enable", {"maxPostDataSize": 0})
        await self._send("Network.setCacheDisabled", {"cacheDisabled": True})
        await self._send("Network.setBypassServiceWorker", {"bypass": True})
        await self._send("Fetch.enable", {"patterns": [
            {"urlPattern": "*", "requestStage": "Request"}, {"urlPattern": "*", "requestStage": "Response"},
        ]})
        self.check()

    def _paused(self, event: dict) -> None:
        if self.closing or self.failure.done():
            return
        try:
            request = event["request"]
            url = request["url"]
            fetch_id, network_id = event["requestId"], event.get("networkId")
            if (not self.navigating or not fetch_id or not network_id or event["frameId"] != self.frame_id
                    or request["method"] != "GET" or request.get("hasPostData") or "postData" in request
                    or "redirectedRequestId" in event or any(
                        header.lower() in {"authorization", "proxy-authorization"} for header in request["headers"]
                    )):
                self.fail("unapproved browser request")
                return
            expected = self._expected(url, event["resourceType"].lower())
            if expected is None:
                self.fail("unapproved browser request")
                return
            if "responseStatusCode" in event or "responseErrorReason" in event:
                self._response(event, network_id, fetch_id, expected)
                return
            if (network_id in self.hops or any(hop.request == expected for hop in self.hops.values())
                    or (expected == self.body and not self.hops)):
                self.fail("repeated or out-of-order browser request")
                return
            hop = _Hop(expected, fetch_id)
            self.hops[network_id] = hop
            if expected == self.body:
                self.body_seen.set()
            self._start(self._admit(hop))
        except (KeyError, TypeError, AttributeError, ValueError):
            self.fail("invalid request gate event")

    def _response(self, event: dict, network_id: str, fetch_id: str, expected: FetchRequest) -> None:
        hop = self.hops.get(network_id)
        status = event.get("responseStatusCode")
        if (hop is None or hop.request != expected or hop.fetch_id != fetch_id or hop.receipt is not None
                or not hop.continued or type(status) is not int or not 100 <= status <= 599
                or "responseErrorReason" in event):
            self.fail("invalid browser response pause")
            return
        retries = [header["value"] for header in event["responseHeaders"] if header["name"].lower() == "retry-after"]
        retry_after = retries[0][:256] if retries else None
        hop.receipt = status, retry_after
        if status != 200 or retry_after is not None:
            self.fail("browser response is not admissible")
            return
        self._start(self._continue_response(hop, event["responseHeaders"]))

    async def _continue_response(self, hop: _Hop, headers: list[dict]) -> None:
        params = {"requestId": hop.fetch_id}
        if hop.request == self.body:
            relevant = {}
            for header in headers:
                name = header["name"].lower()
                if name not in {"content-encoding", "content-length", "content-range", "transfer-encoding"}:
                    continue
                if name in relevant:
                    raise BrowserCaptureError("duplicate framing header at browser response gate")
                relevant[name] = header["value"]
            length = _declared_length(relevant, MAX_BODY)
            if length is not None and length <= _MAX_EARLY_BODY_BYTES:
                raw = await self._send("Fetch.getResponseBody", params)
                if raw.get("base64Encoded") is not True:
                    raise BrowserCaptureError("paused browser response is not binary")
                body = bytearray()
                _append(body, raw.get("body"), length)
                if len(body) != length:
                    raise BrowserCaptureError("paused browser response length mismatch")
                hop.buffered_body = bytes(body)
        hop.buffer_ready.set()
        hop.response_continued = True
        await self._send("Fetch.continueResponse", params)

    async def buffered_body(self, request_id: str) -> bytes | None:
        hop = self.hops.get(request_id)
        if hop is None or hop.request != self.body:
            raise BrowserCaptureError("browser capture did not match the permitted network request")
        await hop.buffer_ready.wait()
        return hop.buffered_body

    def _terminal(self, event: dict, *, completed: bool) -> None:
        if self.closing:
            return
        hop = self.hops.get(event.get("requestId"))
        if hop is None:
            return
        if not completed or not hop.response_continued or hop.receipt is None or hop.outcome.done():
            self.fail("browser request failed or did not complete through the gate")
            hop.finish(self._cancellation_receipt(hop))
            return
        hop.finish(hop.receipt)

    def _cached(self, event: dict) -> None:
        if event.get("requestId") in self.hops:
            self.fail("browser request used cache")

    @staticmethod
    def _cancellation_receipt(hop: _Hop) -> Receipt | None:
        return hop.receipt if hop.receipt is not None and hop.receipt != (200, None) else None

    async def _admit(self, hop: _Hop) -> None:
        if hop.request == self.body:
            navigation = next(item for item in self.hops.values() if item.request == self.navigation)
            await navigation.settled.wait()
        self.check()
        ready: Future[bool] = Future()

        def hold() -> None:
            try:
                with self.permit(hop.request) as complete:
                    ready.set_result(True)
                    outcome = hop.outcome.result()
                    if outcome is not None:
                        complete(*outcome)
            finally:
                if not ready.done():
                    ready.set_result(False)

        worker = self.loop.run_in_executor(None, copy_context().run, hold)
        self.workers.append(worker)
        if not await asyncio.shield(asyncio.wrap_future(ready)) or hop.outcome.done():
            await asyncio.shield(worker)
            raise BrowserCaptureError("browser permit closed before request")
        self.check()
        hop.continued = True
        await self._send("Fetch.continueRequest", {"requestId": hop.fetch_id})
        await asyncio.shield(worker)
        self.check()
        hop.settled.set()

    async def settled(self) -> None:
        await self.body_seen.wait()
        for hop in self.hops.values():
            await hop.settled.wait()
        self.check()

    @asynccontextmanager
    async def armed(self):
        try:
            yield
        finally:
            self.closing = True
            for task in self.tasks:
                task.cancel()

    async def stop(self) -> None:
        for hop in self.hops.values():
            hop.finish(self._cancellation_receipt(hop))
        await asyncio.gather(*self.tasks, return_exceptions=True)
        for result in await asyncio.gather(*self.workers, return_exceptions=True):
            if isinstance(result, BaseException):
                raise result


class PlaywrightBrowserRenderer:
    """Bind a prepared Playwright page to the synchronous acquisition renderer SPI.

    The provider owns process isolation, the fresh anonymous context, TLS, pinned peer
    enforcement and browser-level egress and lifecycle controls. It must promptly
    destroy the context/browser on exit, even if page/session operations fail.
    Context routing and CDP permissions apply within this boundary.

    Permits enter, complete and exit on a worker thread. The synchronous permit must
    have its own bounded wait/DB operations: cancellation closes the browser first,
    then waits for any outstanding permit operation to unwind without resuming a request.
    """

    def __init__(self, page_provider: PageProvider):
        self.page_provider = page_provider

    def render(self, plan: BrowserPagePlan, permit: Permit, *, timeout: float) -> tuple[BrowserCapture, BrowserSelection]:
        if (not math.isfinite(timeout) or timeout <= 0 or not plan.body_selector
                or plan.navigation_url == plan.body_request.url
                or request_origin(plan.navigation_url) != request_origin(plan.body_request.url)):
            raise BrowserCaptureError("renderer requires a selector and a distinct same-origin GET plan")
        return asyncio.run(self._render(plan, permit, timeout))

    async def _render(self, plan: BrowserPagePlan, permit: Permit, timeout: float) -> tuple[BrowserCapture, BrowserSelection]:
        gate = _Gate(plan, permit)
        deadline = gate.loop.time() + timeout
        try:
            async with asyncio.timeout_at(deadline):
                async with self.page_provider(plan) as prepared, gate.armed():
                    if not prepared.allowed_ips:
                        raise BrowserCaptureError("browser provider supplied no approved peer IPs")
                    for address in prepared.allowed_ips:
                        ip_address(address)
                    await gate.install(prepared)
                    page = prepared.page

                    async def navigate() -> None:
                        gate.check()
                        gate.navigating = True
                        await page.goto(plan.navigation_url, wait_until="domcontentloaded")

                    async def select(navigation: BrowserNavigation) -> BrowserSelection:
                        await gate.settled()
                        if len(page.frames) != 1:
                            raise BrowserCaptureError("unexpected browser frame")
                        locator = page.main_frame.locator(plan.body_selector)
                        await locator.wait_for(state="visible")
                        html = await locator.evaluate("""(node, limit) => {
                            const box = node.getBoundingClientRect();
                            const view = node.ownerDocument.defaultView;
                            if (view !== view.top || box.width === 0 || box.height === 0 ||
                                view.getComputedStyle(node).visibility !== "visible") return null;
                            const html = node.outerHTML;
                            return html.length <= limit && new TextEncoder().encode(html).length <= limit
                                ? html : null;
                        }""", MAX_SELECTED_HTML_BYTES)
                        if not isinstance(html, str):
                            raise BrowserCaptureError("browser body selection is invalid or oversized")
                        return BrowserSelection(html, navigation.navigation_id, navigation.frame_id, navigation.loader_id)

                    capture = asyncio.create_task(observe_browser_navigation(
                        prepared.context, page, navigation_url=plan.navigation_url, body_url=plan.body_request.url,
                        allowed_ips=prepared.allowed_ips, navigate=navigate, select=select,
                        timeout=max(0.000001, deadline - gate.loop.time()),
                        max_html_bytes=MAX_SELECTED_HTML_BYTES, buffered_body=gate.buffered_body,
                    ))
                    try:
                        await asyncio.wait((capture, gate.failure), return_when=asyncio.FIRST_COMPLETED)
                        gate.check()
                        return await capture
                    finally:
                        if not capture.done():
                            capture.cancel()
                        await asyncio.gather(capture, return_exceptions=True)
        except TimeoutError:
            raise BrowserCaptureError("browser render deadline exceeded") from None
        finally:
            await gate.stop()
