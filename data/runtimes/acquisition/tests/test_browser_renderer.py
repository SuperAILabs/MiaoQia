import asyncio
import threading
from contextlib import contextmanager
from contextvars import ContextVar
from dataclasses import replace
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock

import pytest

from miaoqia.acquisition.api.handlers import BrowserDetailPlan, FetchRequest, SourceBlocked
from miaoqia.acquisition.infrastructure.browser_capture import BrowserCaptureError
from miaoqia.runtime_acquisition.browser_renderer import ConfinedPage, PlaywrightBrowserRenderer, _Gate, _Hop

VIEW = "https://publisher.test/view"
BODY = "https://publisher.test/body"
PLAN = BrowserDetailPlan(VIEW, FetchRequest("GET", BODY), "article")
FRAME = {"id": "main", "url": "about:blank"}
IDENTITY = ContextVar("request_identity")


def paused(name, url, resource, *, status=None, **fields):
    event = {
        "requestId": f"fetch-{name}", "networkId": name, "frameId": FRAME["id"], "resourceType": resource,
        "request": {"url": url, "method": "GET", "headers": {}},
    }
    if status is not None:
        event.update(responseStatusCode=status, responseHeaders=[])
    event.update(fields)
    return event


def document(**fields):
    return paused("document", VIEW, "Document", **fields)


def body(**fields):
    return paused("body", BODY, "Fetch", **fields)


class Emitter:
    def __init__(self):
        self.listeners = {}

    def on(self, name, callback):
        self.listeners[name] = callback

    def emit(self, name, params=None):
        self.listeners[name](params)


class Session(Emitter):
    def __init__(self):
        super().__init__()
        self.sent = []
        self.changed = asyncio.Event()

    async def send(self, method, params):
        self.sent.append((method, params))
        self.changed.set()
        return {"frameTree": {"frame": FRAME}} if method == "Page.getFrameTree" else {}

    async def wait_for(self, method, request_id):
        async with asyncio.timeout(1):
            while (method, {"requestId": request_id}) not in self.sent:
                self.changed.clear()
                await self.changed.wait()

    def pause(self, event):
        self.emit("Fetch.requestPaused", event)

    def finish(self, name):
        self.emit("Network.loadingFinished", {"requestId": name})


class Page(Emitter):
    url = "about:blank"
    frames = (object(),)
    main_frame = frames[0]


class Context(Emitter):
    def __init__(self, page, session):
        super().__init__()
        self.pages = [page]
        self.session = session

    async def new_cdp_session(self, page):
        assert self.pages == [page]
        return self.session

    async def route(self, pattern, handler):
        assert pattern == "**/*"
        self.http = handler

    async def route_web_socket(self, pattern, handler):
        assert pattern == "**/*"
        self.websocket = handler


async def setup(permit):
    session, page = Session(), Page()
    context = Context(page, session)
    gate = _Gate(PLAN, permit)
    await gate.install(ConfinedPage(context, page, frozenset({"192.0.2.2"})))
    gate.navigating = True
    return gate, session, context, page


@pytest.mark.parametrize("url,resource", [(VIEW, "document"), (BODY, "fetch"), (BODY, "xhr")])
def test_context_valid_request_defers_to_existing_provider_routes(url, resource):
    async def verify():
        gate, _, context, page = await setup(None)
        request = SimpleNamespace(url=url, resource_type=resource, method="GET", redirected_from=None,
                                  frame=page.main_frame)
        route = SimpleNamespace(request=request, fallback=AsyncMock(), abort=AsyncMock())
        await context.http(route)
        route.fallback.assert_awaited_once_with()
        route.abort.assert_not_awaited()
        gate.check()

    asyncio.run(verify())


@pytest.mark.parametrize("change", [
    {"frame": object()}, {"resource_type": "fetch"}, {"method": "POST"},
    {"redirected_from": object()}, {"url": VIEW + "?other=1"},
])
def test_context_denies_unapproved_request_before_fallback(change):
    async def verify():
        gate, _, context, page = await setup(None)
        request = SimpleNamespace(**({
            "url": VIEW, "resource_type": "document", "method": "GET", "redirected_from": None,
            "frame": page.main_frame,
        } | change))
        route = SimpleNamespace(request=request, fallback=AsyncMock(), abort=AsyncMock())
        await context.http(route)
        route.abort.assert_awaited_once_with("blockedbyclient")
        route.fallback.assert_not_awaited()
        with pytest.raises(BrowserCaptureError, match="unapproved browser context request"):
            gate.check()

    asyncio.run(verify())


def test_context_closes_websocket_without_connecting_and_requires_routes():
    async def verify():
        gate, _, context, page = await setup(None)
        route = SimpleNamespace(close=AsyncMock(), connect_to_server=Mock())
        await context.websocket(route)
        route.close.assert_awaited_once_with(code=1008)
        route.connect_to_server.assert_not_called()
        with pytest.raises(BrowserCaptureError, match="unapproved browser context WebSocket"):
            gate.check()

        for websocket_route in (None, AsyncMock(side_effect=RuntimeError("registration error"))):
            session = Session()
            candidate = Context(page, session)
            candidate.route_web_socket = websocket_route
            with pytest.raises(BrowserCaptureError, match="context routing could not be installed"):
                await _Gate(PLAN, None).install(ConfinedPage(candidate, page, frozenset({"192.0.2.2"})))
            assert not session.sent

    asyncio.run(verify())


@pytest.mark.parametrize("response, failure", [
    ({"body": "gAFpgA==", "base64Encoded": True}, None),
    ({"body": "gAFpgA==", "base64Encoded": False}, "not binary"),
    ({"body": "gAE=", "base64Encoded": True}, "length mismatch"),
], ids=["exact-binary", "decoded-string", "truncated"])
def test_gate_buffers_small_binary_response_before_continuing(response, failure):
    async def verify():
        gate = _Gate(PLAN, None)
        hop = _Hop(PLAN.body_request, "fetch-body")
        gate.hops["body"] = hop
        commands = []

        async def send(method, params):
            assert params == {"requestId": "fetch-body"}
            commands.append(method)
            return response

        gate._send = send
        headers = [{"name": "Content-Length", "value": "4"}]
        if failure is not None:
            with pytest.raises(BrowserCaptureError, match=failure):
                await gate._continue_response(hop, headers)
            assert not hop.response_continued and commands == ["Fetch.getResponseBody"]
        else:
            await gate._continue_response(hop, headers)
            assert await gate.buffered_body("body") == b"\x80\x01i\x80"
            assert commands == ["Fetch.getResponseBody", "Fetch.continueResponse"]

    asyncio.run(verify())


def test_body_waits_for_navigation_completion_and_permit_exit():
    async def verify():
        history = []
        loop_thread = threading.get_ident()
        release_document = threading.Event()
        document_completed = asyncio.Event()

        @contextmanager
        def permit(request):
            owner = threading.get_ident()
            assert owner != loop_thread and IDENTITY.get() == "worker authority"
            history.append(("enter", request.url))

            def complete(status, retry_after):
                assert threading.get_ident() == owner and IDENTITY.get() == "worker authority"
                assert (status, retry_after) == (200, None)
                history.append(("complete", request.url))

            try:
                yield complete
            finally:
                if request.url == VIEW:
                    loop.call_soon_threadsafe(document_completed.set)
                    assert release_document.wait(1)
                history.append(("exit", request.url))

        loop = asyncio.get_running_loop()
        IDENTITY.set("worker authority")
        gate, session, _, _ = await setup(permit)
        try:
            async with gate.armed():
                session.pause(document())
                await session.wait_for("Fetch.continueRequest", "fetch-document")
                session.pause(document(status=200))
                session.pause(body())
                await session.wait_for("Fetch.continueResponse", "fetch-document")
                assert history == [("enter", VIEW)]
                session.finish("document")
                await asyncio.wait_for(document_completed.wait(), 1)
                assert history == [("enter", VIEW), ("complete", VIEW)]
                release_document.set()
                await session.wait_for("Fetch.continueRequest", "fetch-body")
                session.pause(body(status=200))
                await session.wait_for("Fetch.continueResponse", "fetch-body")
                assert history[-1] == ("enter", BODY)
                session.finish("body")
                await asyncio.wait_for(gate.settled(), 1)
        finally:
            release_document.set()
            await gate.stop()
        assert history == [(phase, url) for url in (VIEW, BODY) for phase in ("enter", "complete", "exit")]

    asyncio.run(verify())


@pytest.mark.parametrize("event", [
    paused("outsider", "https://other.test/body", "Fetch"),
    paused("same-origin", BODY + "?wrong=1", "Fetch"),
    paused("child", BODY, "Fetch", frameId="subframe"),
    paused("script", BODY, "Script"),
    body(networkId=None),
    body(redirectedRequestId="previous-hop"),
    body(),
    document(request={"url": VIEW, "method": "POST", "headers": {}}),
    document(request={"url": VIEW, "method": "GET", "headers": {"Authorization": "private"}}),
])
def test_unapproved_requests_stay_paused(event):
    async def verify():
        def permit(_):
            pytest.fail("unapproved request must not enter the permit")

        gate, session, _, _ = await setup(permit)
        async with gate.armed():
            session.pause(event)
            with pytest.raises(BrowserCaptureError, match="browser request"):
                gate.check()
        await gate.stop()
        assert not any(method.startswith("Fetch.continue") for method, _ in session.sent)

    asyncio.run(verify())


def test_approved_document_cannot_continue_before_observer_starts_navigation():
    async def verify():
        gate, session, _, _ = await setup(None)
        gate.navigating = False
        async with gate.armed():
            session.pause(document())
            with pytest.raises(BrowserCaptureError, match="unapproved browser request"):
                gate.check()
        await gate.stop()
        assert not any(method.startswith("Fetch.continue") for method, _ in session.sent)

    asyncio.run(verify())


@pytest.mark.parametrize(("status", "retry_after"), [(302, None), (403, None), (429, "15"), (200, "3")])
def test_rejected_response_is_never_resumed_but_receipt_is_recorded(status, retry_after):
    async def verify():
        receipts = []

        @contextmanager
        def permit(request):
            assert request.url == VIEW
            yield lambda *result: receipts.append(result)

        gate, session, _, _ = await setup(permit)
        async with gate.armed():
            session.pause(document())
            await session.wait_for("Fetch.continueRequest", "fetch-document")
            event = document(status=status)
            if retry_after:
                event["responseHeaders"] = [{"name": "Retry-After", "value": retry_after}]
            session.pause(event)
            with pytest.raises(BrowserCaptureError, match="not admissible"):
                gate.check()
        await gate.stop()
        assert receipts == [(status, retry_after)]
        assert not any(method == "Fetch.continueResponse" for method, _ in session.sent)

    asyncio.run(verify())


def test_cancel_during_pacing_cannot_resume_request_and_releases_permit():
    async def verify():
        exited = []
        waiting = asyncio.Event()
        allow_enter = threading.Event()
        loop = asyncio.get_running_loop()

        @contextmanager
        def permit(_):
            loop.call_soon_threadsafe(waiting.set)
            assert allow_enter.wait(1)
            try:
                yield lambda *_: pytest.fail("cancelled request must not complete")
            finally:
                exited.append(True)

        gate, session, _, _ = await setup(permit)
        async with gate.armed():
            session.pause(document())
            await asyncio.wait_for(waiting.wait(), 1)
        with pytest.raises(BrowserCaptureError, match="gate is closed"):
            gate.check()
        allow_enter.set()
        await gate.stop()
        assert exited == [True]
        assert not any(method == "Fetch.continueRequest" for method, _ in session.sent)

    asyncio.run(verify())


def test_denied_permit_never_resumes_wire_request():
    async def verify():
        @contextmanager
        def permit(_):
            raise SourceBlocked("revoked")
            yield

        gate, session, _, _ = await setup(permit)
        async with gate.armed():
            session.pause(document())
            assert isinstance(await asyncio.wait_for(gate.failure, 1), SourceBlocked)
        with pytest.raises(SourceBlocked, match="revoked"):
            await gate.stop()
        assert not any(method == "Fetch.continueRequest" for method, _ in session.sent)

    asyncio.run(verify())


@pytest.mark.parametrize(("surface", "event"), [("context", "page"), ("page", "frameattached"),
                                                 ("page", "download"), ("page", "crash")])
def test_unexpected_browser_activity_fails_gate(surface, event):
    async def verify():
        gate, _, context, page = await setup(None)
        async with gate.armed():
            (context if surface == "context" else page).emit(event)
            with pytest.raises(BrowserCaptureError, match="unexpected browser"):
                gate.check()
        await gate.stop()

    asyncio.run(verify())


@pytest.mark.parametrize("plan", [
    replace(PLAN, body_selector=None),
    replace(PLAN, body_request=FetchRequest("GET", VIEW)),
    replace(PLAN, body_request=FetchRequest("GET", "https://other.test/body")),
])
def test_invalid_plan_never_opens_provider(plan):
    def provider(_):
        pytest.fail("invalid plan must not open a page")

    with pytest.raises(BrowserCaptureError, match="same-origin GET plan"):
        PlaywrightBrowserRenderer(provider).render(plan, None, timeout=1)
