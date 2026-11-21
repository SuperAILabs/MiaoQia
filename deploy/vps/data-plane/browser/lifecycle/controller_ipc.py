"""Run the synthetic Acquisition renderer over the host's authenticated Unix stream.

Run from the fixture directory with python -m lifecycle.controller_ipc {pair|hold}
<publisher_ipv4>. Stage this module alongside publisher.py and the Acquisition
packages. The host owns the socket, peer authentication, and driver lifecycle.
The driver owns Chromium's executable, profile, and synthetic CA trust.
"""

import asyncio
from contextlib import asynccontextmanager, contextmanager, suppress
from hashlib import sha256
from importlib.metadata import version
from ipaddress import IPv4Address
import sys
from threading import Event, Lock
from traceback import extract_tb

from playwright._impl._connection import Connection
from playwright._impl._errors import TargetClosedError
from playwright._impl._object_factory import create_remote_object
from playwright._impl._transport import Transport
from playwright.async_api import Error as PlaywrightError, Playwright

from miaoqia.acquisition.api.browser_projection import build_projection, read_projection
from miaoqia.acquisition.api.handlers import BrowserPagePlan, FetchRequest
from miaoqia.acquisition.infrastructure.browser_capture import BrowserCaptureError
from miaoqia.runtime_acquisition.browser_renderer import ConfinedPage, PlaywrightBrowserRenderer

from publisher import BODY, HOST, SELECTED_HTML, emit


PLAYWRIGHT_VERSION = "1.60.0"
SOCKET_PATH = "/fixture-control/driver.sock"
FRAME_LIMIT = 1024 * 1024
RENDER_TIMEOUT = 15
CLOSE_TIMEOUT = 3
VIEW = FetchRequest("GET", f"https://{HOST}/pair/view")
BODY_REQUEST = FetchRequest("GET", f"https://{HOST}/pair/data")
PLAN = BrowserPagePlan(VIEW.url, BODY_REQUEST, "#mount > article[data-notice-id]")


class SocketTransport(Transport):
    def __init__(self, loop, reader, writer):
        super().__init__(loop)
        self.reader, self.writer = reader, writer
        self.stopped = asyncio.Event()

    async def connect(self):
        pass

    def request_stop(self):
        self.writer.close()

    async def wait_until_stopped(self):
        await self.stopped.wait()

    async def run(self):
        try:
            while True:
                try:
                    header = await self.reader.readexactly(4)
                except asyncio.IncompleteReadError as error:
                    if error.partial:
                        raise AssertionError("driver socket closed inside a frame header") from None
                    return
                length = int.from_bytes(header, "little")
                if not 0 < length <= FRAME_LIMIT:
                    raise AssertionError("unexpected fixture stdio frame size")
                try:
                    payload = await self.reader.readexactly(length)
                except asyncio.IncompleteReadError:
                    raise AssertionError("driver socket closed inside a frame payload") from None
                self.on_message(self.deserialize_message(payload))
        finally:
            self.stopped.set()

    def send(self, message):
        payload = self.serialize_message(message)
        if not 0 < len(payload) <= FRAME_LIMIT:
            raise AssertionError("unexpected fixture stdio frame size")
        self.writer.write(len(payload).to_bytes(4, "little") + payload)


def close_connection(connection):
    def retrieve_closed(future):
        if not future.cancelled():
            assert isinstance(future.exception(), TargetClosedError)

    for callback in connection._callbacks.values():
        if not callback.no_reply and not callback.future.done():
            callback.future.add_done_callback(retrieve_closed)
    connection.cleanup("fixture driver socket closed")


@asynccontextmanager
async def playwright_session(transport):
    loop = asyncio.get_running_loop()
    connection = Connection(None, create_remote_object, transport, loop)
    owner = asyncio.current_task()
    active = disconnected = False

    async def drive():
        nonlocal disconnected
        try:
            await connection.run()
        finally:
            close_connection(connection)
            if active:
                disconnected = True
                owner.cancel()

    protocol = asyncio.create_task(drive())
    try:
        done, _ = await asyncio.wait((connection.playwright_future, protocol), return_when=asyncio.FIRST_COMPLETED)
        if protocol in done:
            protocol.result()
            raise BrowserCaptureError("fixture driver socket closed during initialization")
        active = True
        yield Playwright(connection.playwright_future.result())
    except asyncio.CancelledError:
        if disconnected:
            raise BrowserCaptureError("fixture driver socket closed") from None
        raise
    finally:
        active = False
        transport.request_stop()
        async with asyncio.timeout(CLOSE_TIMEOUT):
            await protocol


@asynccontextmanager
async def connect_driver():
    reader, writer = await asyncio.open_unix_connection(SOCKET_PATH)
    try:
        writer.write(b"MQPW1\n")
        await writer.drain()
        if await reader.readexactly(1) != b"\x01":
            raise BrowserCaptureError("fixture driver did not acknowledge the session")
        async with playwright_session(SocketTransport(asyncio.get_running_loop(), reader, writer)) as playwright:
            yield playwright
    finally:
        writer.close()
        with suppress(ConnectionError):
            async with asyncio.timeout(CLOSE_TIMEOUT):
                await writer.wait_closed()


def require(condition, message):
    if not condition:
        raise BrowserCaptureError(message)


def verify_hop(request, response, expected, resource_types, peer):
    received = response["response"]
    identity = tuple(request[key] for key in ("requestId", "frameId", "loaderId"))
    require(all(identity) and identity == tuple(response[key] for key in ("requestId", "frameId", "loaderId"))
            and request["type"] == response["type"] and request["type"] in resource_types
            and request["request"]["method"] == expected.method
            and request["request"]["url"] == received["url"] == expected.url
            and "redirectResponse" not in request and received["status"] == 200,
            "fixture CDP request and response did not match")
    protocol = received["securityDetails"]["protocol"]
    require((received["remoteIPAddress"], received["remotePort"], received["securityState"]) == (peer, 443, "secure")
            and protocol in {"TLS 1.2", "TLS 1.3"}, "fixture TLS peer did not match")
    return identity, protocol


def run_fixture(mode, peer):
    require(mode in {"pair", "hold"}, "invalid fixture mode")
    peer = str(IPv4Address(peer))
    require(version("playwright") == PLAYWRIGHT_VERSION, "unexpected Playwright version")
    shutdown = Event()
    audit_lock = Lock()
    requests, responses, stages = [], [], []
    metadata = {}
    view_permit = [("enter", "view"), ("complete", "view"), ("exit", "view")]

    def record(events, event):
        with audit_lock:
            if len(events) < 3:
                events.append(event)

    @asynccontextmanager
    async def provider(requested):
        require(requested == PLAN, "unexpected fixture browser plan")
        async with connect_driver() as playwright:
            browser = await playwright.chromium.launch(
                channel="chromium", headless=True, chromium_sandbox=True,
                args=["--no-proxy-server", "--disable-quic", f"--host-resolver-rules=MAP {HOST} {peer}, MAP * ~NOTFOUND"],
            )
            metadata.update(browser_version=browser.version, chromium_sandbox_requested=True)
            try:
                context = await browser.new_context(service_workers="block", accept_downloads=False,
                                                    ignore_https_errors=False)
                page = await context.new_page()
                audit = await context.new_cdp_session(page)
                audit.on("Network.requestWillBeSent", lambda event: record(requests, event))
                audit.on("Network.responseReceived", lambda event: record(responses, event))
                await audit.send("Network.enable")
                yield ConfinedPage(context, page, frozenset({peer}))
            except BaseException:
                with audit_lock:
                    shutdown.set()
                with suppress(PlaywrightError, TimeoutError, ConnectionError):
                    await asyncio.wait_for(browser.close(), timeout=CLOSE_TIMEOUT)
                raise
            else:
                await asyncio.wait_for(browser.close(), timeout=CLOSE_TIMEOUT)

    @contextmanager
    def permit(request):
        if request == VIEW:
            require(not stages, "unexpected navigation permit order")
            endpoint = "view"
        else:
            require(request == BODY_REQUEST and stages == view_permit, "unexpected body permit order")
            endpoint = "data"
            with audit_lock:
                require(bool(requests) and len(responses) == 1, "missing completed navigation audit")
                verify_hop(requests[0], responses[0], VIEW, {"Document"}, peer)
        stages.append(("enter", endpoint))

        def complete(status, retry_after):
            require(stages[-1] == ("enter", endpoint) and (status, retry_after) == (200, None),
                    "unexpected fixture permit completion")
            stages.append(("complete", endpoint))

        try:
            if mode == "hold" and request == BODY_REQUEST:
                with audit_lock:
                    require(not shutdown.is_set(), "fixture browser is closing")
                    emit(kind="body_paused")
                shutdown.wait(RENDER_TIMEOUT)
                raise BrowserCaptureError("fixture body permit stayed closed")
            yield complete
        finally:
            stages.append(("exit", endpoint))

    capture, selection = PlaywrightBrowserRenderer(provider).render(PLAN, permit, timeout=RENDER_TIMEOUT)
    require(mode == "pair" and stages == view_permit + [("enter", "data"), ("complete", "data"), ("exit", "data")]
            and len(requests) == len(responses) == 2, "incomplete fixture pair")
    navigation, navigation_tls = verify_hop(requests[0], responses[0], VIEW, {"Document"}, peer)
    body, body_tls = verify_hop(requests[1], responses[1], BODY_REQUEST, {"Fetch", "XHR"}, peer)
    require(navigation == (selection.navigation_id, selection.frame_id, selection.loader_id)
            and body == (capture.request_id, capture.frame_id, capture.loader_id)
            and navigation[0] != body[0] and navigation[1:] == body[1:]
            and (capture.remote_ip, capture.remote_port) == (peer, 443), "fixture pair navigation did not match")
    require(capture.response.body == BODY and selection.html == SELECTED_HTML, "fixture pair content did not match")
    projection = build_projection(selection.html)
    require(read_projection(projection)["blocks"] == [
        {"index": 0, "kind": "paragraph", "text": BODY.decode("cp1252").removesuffix("\r\n")},
    ], "fixture projection did not match")
    return dict(kind="pair", original_sha256=sha256(capture.response.body).hexdigest(),
                original_bytes=len(capture.response.body), projection_sha256=sha256(projection).hexdigest(),
                projection_bytes=len(projection), permit_completions=2, permit_stages=stages,
                same_navigation=True, both_cdp_peers_match_pin=True, tls=[navigation_tls, body_tls], **metadata)


def main():
    try:
        if len(sys.argv) != 3:
            raise ValueError("invalid fixture arguments")
        emit(**run_fixture(*sys.argv[1:]))
    except Exception as error:
        frames = [frame for frame in extract_tb(error.__traceback__)
                  if frame.filename == __file__ and frame.name != "require"]
        location = frames[-1]
        emit(kind="controller_error", error_type=type(error).__name__, stage=location.name, line=location.lineno)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
