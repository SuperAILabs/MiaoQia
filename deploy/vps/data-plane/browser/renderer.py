"""Drive the installed Chromium through the acquisition PageProvider seam."""

import asyncio
from contextlib import asynccontextmanager, contextmanager
from hashlib import sha256
from importlib.metadata import version
import json
import os
from pathlib import Path
import socket
import sys
from traceback import extract_tb

from playwright.async_api import Error as PlaywrightError, async_playwright

from miaoqia.acquisition.api.browser_projection import build_projection, read_projection
from miaoqia.acquisition.api.handlers import BrowserPagePlan, FetchRequest
from miaoqia.acquisition.infrastructure.browser_capture import BrowserCaptureError
from miaoqia.runtime_acquisition.browser_renderer import ConfinedPage, PlaywrightBrowserRenderer

from chromium_home import chromium_environment
from publisher import BODY, CASES, HOST, OTHER_HOST, SELECTED_HTML, TLS_PAIR_CASES, emit

TLS_ERRORS = {-200: "ERR_CERT_COMMON_NAME_INVALID", -202: "ERR_CERT_AUTHORITY_INVALID"}


def observed_tls_error(netlog):
    # The CDP page gate may close before Network.loadingFailed is delivered.
    # Read only the verifier/handshake result from the temporary browser log.
    if not netlog.exists():
        return None
    log = json.loads(netlog.read_text())
    types = log["constants"]["logEventTypes"]
    tls_types = {types[name] for name in ("CERT_VERIFIER_JOB", "SSL_CONNECT")}
    errors = {entry.get("params", {}).get("net_error") for entry in log["events"] if entry["type"] in tls_types}
    matched = errors & TLS_ERRORS.keys()
    return TLS_ERRORS[matched.pop()] if len(matched) == 1 else None


def verify_chromium_command_line(profile):
    expected = os.fsencode(f"--user-data-dir={profile}")
    forbidden = (b"--no-sandbox", b"--ignore-certificate-errors", b"--allow-insecure-localhost")
    for cmdline in Path("/proc").glob("[0-9]*/cmdline"):
        try:
            argv = cmdline.read_bytes().split(b"\0")
        except (FileNotFoundError, ProcessLookupError, PermissionError):
            continue
        if expected in argv and not any(argument.startswith(b"--type=") for argument in argv):
            assert not any(argument.startswith(forbidden) for argument in argv)
            return int(cmdline.parent.name)
    raise AssertionError("running Chromium profile was not found")


def probe_kernel():
    pinned_ip = os.environ["PUBLISHER_IP"]
    denied = {}
    for name, address, port in [
        ("other_port", pinned_ip, 8443),
        ("other_peer", os.environ["OTHER_IP"], 443),
        ("business_peer", os.environ["CANARY_IP"], 443),
        ("bridge_first_address", os.environ["BRIDGE_FIRST_IP"], 443),
    ]:
        try:
            with socket.create_connection((address, port), timeout=0.5):
                denied[name] = False
        except OSError:
            denied[name] = True
    # UDP send can succeed locally even if the firewall drops it. The host
    # checks the independent rule counters and the publisher's UDP listener.
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as probe:
        for port in (443, 53):
            try:
                probe.sendto(b"synthetic", (pinned_ip, port))
            except OSError:
                pass
    try:
        with socket.socket(socket.AF_INET6, socket.SOCK_STREAM) as probe:
            probe.settimeout(0.5)
            probe.connect(("::1", 443))
            denied["ipv6_loopback"] = False
    except OSError:
        denied["ipv6_loopback"] = True
    emit(kind="socket_probes", **denied)


def check(case, ca, *, lifecycle=None):
    host = OTHER_HOST if case == "wrong_name" else HOST
    view = f"https://{host}/{case}/view"
    data = f"https://{host}/{case}/data"
    plan = BrowserPagePlan(view, FetchRequest("GET", data), "#mount > article[data-notice-id]")
    pinned_ip = os.environ["PUBLISHER_IP"]
    stages, requests, responses = [], [], []
    runtime = {}
    root = Path("/tmp")
    profiles = set(root.glob("playwright_chromiumdev_profile-*"))

    @asynccontextmanager
    async def provider(requested):
        assert requested == plan
        with chromium_environment(ca, trust=case != "untrusted_ca") as clean:
            netlog = Path(clean["HOME"]) / "tls.json"
            args = [
                "--no-proxy-server", "--disable-quic",
                f"--host-resolver-rules=MAP {HOST} {pinned_ip}, MAP {OTHER_HOST} {pinned_ip}, MAP * ~NOTFOUND",
            ]
            if case in {"untrusted_ca", "wrong_name"}:
                args.append(f"--log-net-log={netlog}")
            async with async_playwright() as playwright:
                browser = await playwright.chromium.launch(
                    channel="chromium", headless=True, chromium_sandbox=True, env=clean, args=args,
                )
                terminate = None
                try:
                    profile, = set(root.glob("playwright_chromiumdev_profile-*")) - profiles
                    assert profile.is_dir()
                    runtime["fresh_profile"] = True
                    if case == "pair":
                        chromium_pid = verify_chromium_command_line(profile)
                        runtime["safe_command_line_verified"] = True
                        sandbox = await browser.new_page()
                        await sandbox.goto("chrome://sandbox", wait_until="domcontentloaded", timeout=2500)
                        await sandbox.get_by_text("You are adequately sandboxed.", exact=True).wait_for(timeout=1500)
                        status = await sandbox.locator("body").inner_text()
                        runtime.update(
                            version=browser.version,
                            namespace_sandbox="Layer 1 Sandbox\tNamespace" in status and all(
                                f"{name}\tYes" in status for name in ("PID namespaces", "Network namespaces")
                            ),
                            seccomp_bpf_sandbox="Seccomp-BPF sandbox\tYes" in status,
                        )
                        await sandbox.close()
                    context = await browser.new_context(service_workers="block", accept_downloads=False)
                    page = await context.new_page()
                    if case == "provider_veto":
                        async def veto(route):
                            runtime["provider_veto_seen"] = True
                            await route.abort("blockedbyclient")
                        await context.route(view, veto)
                    audit = await context.new_cdp_session(page)
                    audit.on("Network.requestWillBeSent", lambda event: requests.append(event))
                    audit.on("Network.responseReceived", lambda event: responses.append(event))
                    await audit.send("Network.enable")
                    if case == "control_loss":
                        navigating = asyncio.Event()
                        page.on("domcontentloaded", lambda _: navigating.set())
                        async def disconnect():
                            await navigating.wait()
                            await browser.close()
                        terminate = asyncio.create_task(disconnect())
                    yield ConfinedPage(context, page, frozenset({pinned_ip}))
                    if lifecycle is not None:
                        await lifecycle(browser, context, page, profile, chromium_pid)
                    if case == "worker":
                        worker = await page.evaluate("globalThis.fixtureWorkerResult")
                        assert worker in {"blocked", "registered", "unavailable"}
                        runtime["worker_registration"] = worker
                finally:
                    await asyncio.wait_for(browser.close(), timeout=3)
                    if netlog.exists():
                        runtime["tls_error"] = observed_tls_error(netlog)
                    if terminate is not None:
                        terminate.cancel()
                        await asyncio.gather(terminate, return_exceptions=True)

    @contextmanager
    def permit(request):
        endpoint = {view: "view", data: "data"}[request.url]
        assert not stages or stages[-1] == ("exit", "view")
        stages.append(("enter", endpoint))
        try:
            yield lambda status, retry: stages.append(("complete", endpoint, status, retry is None))
        finally:
            stages.append(("exit", endpoint))

    metadata = {"case": case}
    try:
        capture, selection = PlaywrightBrowserRenderer(provider).render(plan, permit, timeout=16 if lifecycle else 5)
    except (BrowserCaptureError, PlaywrightError) as error:
        metadata.update(returned_pair=False, deadline="deadline exceeded" in str(error))
    else:
        doc, = [item for item in requests if item["request"]["url"] == view]
        body, = [item for item in requests if item["request"]["url"] == data]
        assert doc["type"] == "Document" and body["type"] in {"Fetch", "XHR"}
        assert doc["request"]["method"] == body["request"]["method"] == "GET"
        assert selection.navigation_id == doc["requestId"] != body["requestId"] == capture.request_id
        assert selection.frame_id == capture.frame_id == doc["frameId"] == body["frameId"]
        assert selection.loader_id == capture.loader_id == doc["loaderId"] == body["loaderId"]
        assert capture.response.body == BODY and selection.html == SELECTED_HTML
        projection = build_projection(selection.html)
        assert read_projection(projection)["blocks"] == [
            {"index": 0, "kind": "paragraph", "text": BODY.decode("cp1252").removesuffix("\r\n")},
        ]
        assert (capture.remote_ip, capture.remote_port) == (pinned_ip, 443)
        protocols = []
        for item in (doc, body):
            response, = [event["response"] for event in responses if event["requestId"] == item["requestId"]]
            assert (response["remoteIPAddress"], response["remotePort"]) == (pinned_ip, 443)
            assert response["securityState"] == "secure"
            protocols.append(response["securityDetails"]["protocol"])
        assert stages == [
            (step, endpoint, 200, True) if step == "complete" else (step, endpoint)
            for endpoint in ("view", "data") for step in ("enter", "complete", "exit")
        ]
        metadata.update(
            returned_pair=True, original_bytes=len(capture.response.body),
            original_sha256=sha256(capture.response.body).hexdigest(),
            projection_sha256=sha256(projection).hexdigest(), projection_bytes=len(projection),
            same_navigation=True, both_cdp_peers_match_pin=True, tls=protocols,
        )
    runtime["temporary_profiles_removed"] = set(root.glob("playwright_chromiumdev_profile-*")) == profiles
    emit(kind="render", **metadata, permit_completions=sum(step[0] == "complete" for step in stages), **runtime)


if __name__ == "__main__":
    try:
        ca_pem = sys.stdin.buffer.read(8192)
        assert version("playwright") == "1.60.0" and ca_pem.startswith(b"-----BEGIN CERTIFICATE-----")
        probe_kernel()
        for scene in TLS_PAIR_CASES if sys.argv[1:] == ["--tls-pair"] else CASES:
            try:
                check(scene, ca_pem)
            except Exception as error:
                frames = extract_tb(error.__traceback__)
                emit(kind="fixture_failure", case=scene, error_type=type(error).__name__,
                     stack=[{"module": Path(frame.filename).name, "line": frame.lineno, "function": frame.name}
                            for frame in frames[-6:]])
                raise SystemExit(1) from None
    except Exception as error:
        emit(kind="fixture_failure", error_type=type(error).__name__)
        raise SystemExit(1) from None
