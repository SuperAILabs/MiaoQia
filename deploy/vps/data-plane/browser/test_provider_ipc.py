"""Local Playwright 1.60.0 stdio feasibility tests for the acquisition renderer."""

import asyncio
from contextlib import AsyncExitStack, asynccontextmanager, contextmanager, suppress
from importlib.metadata import PackageNotFoundError, version
from http.server import BaseHTTPRequestHandler
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile
from threading import Thread, get_ident
from time import monotonic
from types import SimpleNamespace

import pytest

from miaoqia.acquisition.api.handlers import FetchRequest
from miaoqia.acquisition.infrastructure.browser_capture import BrowserCaptureError
from miaoqia.runtime_acquisition.browser_renderer import ConfinedPage, PlaywrightBrowserRenderer

from publisher import BODY, PAGE, SCRIPTS, SELECTED_HTML, PublisherServer


PLAYWRIGHT_VERSION = "1.60.0"
LOOPBACK = "127.0.0.1"
CLEANUP_TIMEOUT = 5


@pytest.fixture(scope="module")
def playwright_runtime():
    try:
        installed = version("playwright")
    except PackageNotFoundError:
        pytest.fail(f"local Playwright {PLAYWRIGHT_VERSION} Python package is unavailable")
    assert installed == PLAYWRIGHT_VERSION, f"expected Playwright {PLAYWRIGHT_VERSION}, found {installed}"

    import playwright
    from lifecycle.controller_ipc import SocketTransport, playwright_session

    driver = Path(playwright.__file__).resolve().parent / "driver"
    cli = driver / "package" / "cli.js"
    assert os.access(driver / "node", os.X_OK) and cli.is_file(), "bundled Playwright driver is unavailable"
    assert json.loads(cli.with_name("package.json").read_text())["version"] == PLAYWRIGHT_VERSION
    browsers = Path(os.environ.get("PLAYWRIGHT_BROWSERS_PATH") or (
        Path.home() / ("Library/Caches/ms-playwright" if os.uname().sysname == "Darwin" else ".cache/ms-playwright")
    )).resolve()
    revision = next(browser["revision"] for browser in json.loads(cli.with_name("browsers.json").read_text())["browsers"]
                    if browser["name"] == "chromium")
    assert (browsers / f"chromium-{revision}").is_dir(), f"local Chromium revision {revision} is unavailable in {browsers}"

    class RecordingTransport(SocketTransport):
        def __init__(self, loop, reader, writer):
            super().__init__(loop, reader, writer)
            self.commands = []
            self.replies = {}
            self.injected_cut = False

        async def run(self):
            try:
                await super().run()
            except (AssertionError, ConnectionError):
                if not self.injected_cut:
                    raise

        def deserialize_message(self, payload):
            message = super().deserialize_message(payload)
            if "id" in message:
                self.replies[message["id"]] = "error" not in message
            return message

        def send(self, message):
            self.commands.append(message)
            super().send(message)

    return SimpleNamespace(command=(str(driver / "node"), str(cli), "run-driver"), browsers=browsers,
                           transport=RecordingTransport, session=playwright_session)


@contextmanager
def local_origin(case, permits):
    requests = []

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_GET(self):
            requests.append((self.path, self.headers.get("Upgrade"), tuple(permits)))
            script = SCRIPTS["websocket"].replace("wss://", "ws://") if case == "websocket" else ""
            if self.path == "/view":
                status, body, media = 200, (PAGE % script).encode(), "text/html"
            elif self.path == "/data":
                status, body, media = 200, BODY, "application/octet-stream"
            else:
                status, body, media = 403, b"", "text/plain"
            self.send_response(status)
            self.send_header("Content-Type", media)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    with PublisherServer((LOOPBACK, 0), Handler) as server:
        listener = Thread(target=server.serve_forever, kwargs={"poll_interval": 0.05}, daemon=True)
        listener.start()
        try:
            yield f"http://{LOOPBACK}:{server.server_port}", requests
        finally:
            server.shutdown()
            listener.join(CLEANUP_TIMEOUT)
            assert not listener.is_alive()


async def relay(reader, writer):
    try:
        while chunk := await reader.read(32768):
            writer.write(chunk)
            await writer.drain()
    except ConnectionError:
        pass
    finally:
        writer.close()


def process_exists(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    return True


def driver_environment(runtime, root):
    return {"HOME": str(root), "TMPDIR": str(root), "PATH": "/usr/bin:/bin", "LANG": "C.UTF-8",
            "PW_LANG_NAME": "python", "PLAYWRIGHT_BROWSERS_PATH": str(runtime.browsers)}


@asynccontextmanager
async def private_link(root):
    path = root / "driver.sock"
    async with AsyncExitStack() as stack:
        stack.callback(path.unlink, missing_ok=True)
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as listener:
            listener.setblocking(False)
            listener.bind(str(path))
            path.chmod(0o600)
            listener.listen(1)
            assert root.stat().st_mode & 0o777 == 0o700 and path.stat().st_mode & 0o777 == 0o600
            reader, writer = await asyncio.open_unix_connection(path)
            stack.callback(writer.close)
            accepted, _ = await asyncio.get_running_loop().sock_accept(listener)
        stack.callback(accepted.close)
        peer_reader, peer = await asyncio.open_unix_connection(sock=accepted)
        stack.callback(peer.close)
        yield reader, writer, peer_reader, peer


@asynccontextmanager
async def run_driver(runtime, root, clean, observed):
    loop = asyncio.get_running_loop()
    async with (private_link(root) as (reader, writer, peer_reader, peer),
                AsyncExitStack() as stack):
        stderr = stack.enter_context((root / "driver.stderr").open("w+b"))
        spawn = asyncio.create_task(asyncio.create_subprocess_exec(
            *runtime.command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=stderr,
            cwd=root, env=clean, start_new_session=True,
        ))
        try:
            process = await asyncio.shield(spawn)
        except asyncio.CancelledError:
            process = await spawn
            with suppress(ProcessLookupError):
                process.kill()
            await asyncio.wait_for(process.communicate(), CLEANUP_TIMEOUT)
            raise

        observed["driver_pid"] = process.pid
        upstream = asyncio.create_task(relay(peer_reader, process.stdin))
        downstream = asyncio.create_task(relay(process.stdout, peer))
        try:
            transport = runtime.transport(loop, reader, writer)
            observed["transport"] = transport
            async with runtime.session(transport) as playwright:
                yield playwright, peer
        finally:
            started = monotonic()
            writer.close()
            try:
                async with asyncio.timeout(CLEANUP_TIMEOUT):
                    outcomes = await asyncio.gather(upstream, downstream, return_exceptions=True)
                    await process.wait()
                    while observed.get("chromium_pid") and process_exists(observed["chromium_pid"]):
                        await asyncio.sleep(0.025)
            finally:
                if process.returncode is None:
                    with suppress(ProcessLookupError):
                        os.killpg(process.pid, signal.SIGKILL)
                    await asyncio.wait_for(process.communicate(), CLEANUP_TIMEOUT)
                if (chromium_group := observed.get("chromium_group")) and process_exists(observed["chromium_pid"]):
                    with suppress(ProcessLookupError):
                        os.killpg(chromium_group, signal.SIGKILL)
            for outcome in outcomes:
                if isinstance(outcome, BaseException):
                    raise outcome
            stderr.seek(0)
            assert process.returncode == 0, stderr.read().decode(errors="replace")
            assert not process_exists(process.pid)
            assert not list(root.glob("playwright_chromiumdev_profile-*"))
            observed["cleanup_seconds"] = monotonic() - started


@pytest.mark.parametrize("case", ("pair", "provider_veto", "websocket", "disconnect"))
def test_renderer_over_provider_ipc(playwright_runtime, case):
    permits, routes, network = [], [], []
    observed = {}
    controller = get_ident()
    with (local_origin(case, permits) as (origin, requests),
          tempfile.TemporaryDirectory(prefix="mq-pw-", dir="/tmp") as directory,
          socket.socket(socket.AF_INET, socket.SOCK_STREAM) as proxy_sink):
        root = Path(directory)
        proxy_sink.bind((LOOPBACK, 0))
        view, body = origin + "/view", origin + "/data"
        plan = SimpleNamespace(navigation_url=view, body_request=FetchRequest("GET", body),
                               body_selector="#mount > article[data-notice-id]")
        clean = driver_environment(playwright_runtime, root)

        @asynccontextmanager
        async def provider(requested):
            assert requested is plan
            async with run_driver(playwright_runtime, root, clean, observed) as (playwright, peer):
                browser = await playwright.chromium.launch(channel="chromium", chromium_sandbox=True, env=clean, args=[
                    "--disable-quic", f"--proxy-server=http://{LOOPBACK}:{proxy_sink.getsockname()[1]}",
                    f"--proxy-bypass-list=<-loopback>;{LOOPBACK}",
                    f"--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE {LOOPBACK}",
                ])
                browser_session = await browser.new_browser_cdp_session()
                processes = (await browser_session.send("SystemInfo.getProcessInfo"))["processInfo"]
                chromium_pid, = (info["id"] for info in processes if info["type"] == "browser")
                assert len({os.getpid(), observed["driver_pid"], chromium_pid}) == 3
                assert process_exists(chromium_pid) and process_exists(observed["driver_pid"])
                assert os.getpgid(chromium_pid) == chromium_pid
                observed.update(chromium_pid=chromium_pid, chromium_group=chromium_pid)
                await browser_session.detach()
                context = await browser.new_context(service_workers="block", accept_downloads=False)
                page = await context.new_page()

                async def provider_route(route):
                    assert get_ident() == controller
                    routes.append(route.request.url)
                    if case == "provider_veto":
                        await route.abort("blockedbyclient")
                    else:
                        await route.continue_()

                await context.route("**/*", provider_route)
                audit = await context.new_cdp_session(page)
                for event in ("Network.requestWillBeSent", "Network.responseReceived"):
                    audit.on(event, lambda payload, event=event: network.append((event, payload)))
                await audit.send("Network.enable")
                loop = asyncio.get_running_loop()

                async def disconnect():
                    observed["disconnected_at"] = monotonic()
                    observed["transport"].injected_cut = True
                    peer.transport.abort()
                    await observed["transport"].wait_until_stopped()

                observed["cut"] = lambda: asyncio.run_coroutine_threadsafe(disconnect(), loop).result(CLEANUP_TIMEOUT)
                yield ConfinedPage(context, page, frozenset({LOOPBACK}))

        @contextmanager
        def permit(request):
            owner = get_ident()
            assert owner != controller
            assert not permits or permits[-1][0] == "exit"
            permits.append(("enter", request.url))

            def complete(status, retry_after):
                assert owner == get_ident()
                permits.append(("complete", status, retry_after))

            try:
                if case == "disconnect" and request.url == body:
                    observed["cut"]()
                yield complete
            finally:
                assert owner == get_ident()
                permits.append(("exit", request.url))

        renderer = PlaywrightBrowserRenderer(provider)
        if case == "pair":
            capture, selection = renderer.render(plan, permit, timeout=15)
        else:
            with pytest.raises(BrowserCaptureError) as rejected:
                renderer.render(plan, permit, timeout=15)

        assert not (root / "driver.sock").exists()
        assert not process_exists(observed["chromium_pid"])
        transport = observed["transport"]
        commands = [message["params"]["method"] for message in transport.commands
                    if message["method"] == "send" and "method" in message["params"]]
        hits = [(path, upgrade) for path, upgrade, _ in requests]
        navigation_permit = [("enter", view), ("complete", 200, None), ("exit", view)]
        if case == "pair":
            assert routes == [view, body] and hits == [("/view", None), ("/data", None)]
            assert permits == navigation_permit + [("enter", body), ("complete", 200, None), ("exit", body)]
            assert requests[0][2] == (("enter", view),)
            assert requests[1][2] == (*navigation_permit, ("enter", body))
            assert commands.count("Fetch.continueRequest") == commands.count("Fetch.continueResponse") == 2
            assert commands.count("Fetch.getResponseBody") == 1
            sent = [item for event, item in network if event == "Network.requestWillBeSent"]
            received = [item for event, item in network if event == "Network.responseReceived"]
            assert [(item["request"]["url"], item["request"]["method"]) for item in sent] == [(view, "GET"), (body, "GET")]
            assert [item["requestId"] for item in sent] == [item["requestId"] for item in received]
            assert sent[0]["requestId"] == selection.navigation_id != sent[1]["requestId"] == capture.request_id
            assert all(item["frameId"] == capture.frame_id == selection.frame_id
                       and item["loaderId"] == capture.loader_id == selection.loader_id for item in sent + received)
            assert (capture.remote_ip, capture.remote_port) == (LOOPBACK, int(origin.rsplit(":", 1)[1]))
            assert capture.response.body == BODY and selection.html == SELECTED_HTML
        elif case == "provider_veto":
            assert str(rejected.value) == "invalid browser response pause"
            assert routes == [view] and not hits
            assert permits in ([], [("enter", view), ("exit", view)])
            abort, = (message for message in transport.commands if message["method"] == "abort")
            assert abort["params"]["errorCode"] == "blockedbyclient"
            assert "Fetch.continueResponse" not in commands
        elif case == "websocket":
            assert "WebSocket" in str(rejected.value)
            close, = (message for message in transport.commands if message["method"] == "closePage")
            assert close["params"]["code"] == 1008 and transport.replies.get(close["id"]) is True
            assert not any(message["method"] == "connect" and message["guid"] == close["guid"]
                           for message in transport.commands)
            assert hits == [("/view", None)] and permits == navigation_permit
        else:
            assert routes in ([view], [view, body]) and hits == [("/view", None)]
            assert permits == navigation_permit + [("enter", body), ("exit", body)]
            assert commands.count("Fetch.continueRequest") == commands.count("Fetch.continueResponse") == 1
            assert str(rejected.value) == "fixture driver socket closed"
            assert monotonic() - observed["disconnected_at"] < CLEANUP_TIMEOUT
        assert observed["cleanup_seconds"] < CLEANUP_TIMEOUT
    assert not root.exists()


@pytest.mark.parametrize("failure", ("setup", "partial_header", "partial_payload"))
def test_driver_teardown_on_fixture_failure(playwright_runtime, failure):
    observed = {}
    with tempfile.TemporaryDirectory(prefix="mq-pw-", dir="/tmp") as directory:
        root = Path(directory)

        async def exercise():
            async with run_driver(playwright_runtime, root, driver_environment(playwright_runtime, root),
                                  observed) as (_, peer):
                if failure == "setup":
                    raise RuntimeError("fixture setup failed after driver initialization")
                partial_frame = b"\x08\x00" if failure == "partial_header" else b"\x08\x00\x00\x00"
                peer.write(partial_frame)
                await peer.drain()
                peer.close()
                await asyncio.Future()

        if failure == "setup":
            expected = pytest.raises(RuntimeError, match="fixture setup failed")
        else:
            expected = pytest.raises(AssertionError, match=f"inside a frame {failure.removeprefix('partial_')}")
        with expected:
            asyncio.run(asyncio.wait_for(exercise(), 2 * CLEANUP_TIMEOUT))
        assert not (root / "driver.sock").exists()
        assert not process_exists(observed["driver_pid"])
