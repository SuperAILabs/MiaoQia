"""Trusted fixture-only Chromium transport probes and independent TLS health check."""

import asyncio
import base64
import os
import socket
import ssl
import sys

from publisher import HOST, emit


PROBE_URL = f"https://{HOST}/lifecycle/probe"


async def receive(expected):
    if await asyncio.to_thread(sys.stdin.buffer.readline, 64) != expected + b"\n":
        raise RuntimeError("invalid supervisor command")


async def probe_page(browser):
    context = await browser.new_context(service_workers="block", accept_downloads=False)
    page = await context.new_page()

    async def restrict(route):
        request = route.request
        if (request.method == "GET" and request.url == PROBE_URL and request.resource_type == "document"
                and request.frame == page.main_frame and request.redirected_from is None):
            await route.continue_()
        else:
            await route.abort()

    await context.route("**/*", restrict)
    async def reject_socket(route):
        await route.close(code=1008)

    await context.route_web_socket("**/*", reject_socket)
    return context, page


async def timeout_probe(page):
    from playwright.async_api import TimeoutError as NavigationTimeout

    try:
        await page.goto(PROBE_URL, timeout=900)
    except NavigationTimeout:
        return True
    return False


async def observe(browser, context, page, profile, chromium_pid):
    from renderer import verify_chromium_command_line

    warm, warm_page = await probe_page(browser)
    fresh, fresh_page = await probe_page(browser)
    try:
        initial = await warm_page.goto(PROBE_URL, timeout=2000)
        assert initial.status == 200 and await initial.body() == b""
        assert warm_page.url == PROBE_URL
        await page.evaluate("globalThis.lifecycleFixtureMarker = true")

        async def browser_alive():
            return (browser.is_connected() and verify_chromium_command_line(profile) == chromium_pid
                    and page.context == context and await page.evaluate("globalThis.lifecycleFixtureMarker") is True)

        emit(kind="warm", browser_alive=await browser_alive())
        await receive(b"old")
        old_timed_out = await timeout_probe(warm_page)
        await warm.close()
        emit(kind="old", timed_out=old_timed_out, browser_alive=await browser_alive())
        await receive(b"new")
        new_timed_out = await timeout_probe(fresh_page)
        emit(kind="new", timed_out=new_timed_out, browser_alive=await browser_alive())
        await receive(b"finish")
    finally:
        await fresh.close()


def health(ca):
    tls = ssl.create_default_context(cadata=ca.decode("ascii"))
    with socket.create_connection((os.environ["PUBLISHER_IP"], 443), timeout=2) as connection:
        with tls.wrap_socket(connection, server_hostname=HOST) as secured:
            secured.sendall(f"GET /lifecycle/health HTTP/1.1\r\nHost: {HOST}\r\nConnection: close\r\n\r\n".encode("ascii"))
            with secured.makefile("rb") as response:
                assert response.readline(256) == b"HTTP/1.1 200 OK\r\n"


if __name__ == "__main__":
    try:
        ca = base64.b64decode(sys.stdin.buffer.readline(8192).removesuffix(b"\n"), validate=True)
        if sys.argv[1:] == ["health"]:
            health(ca)
        elif sys.argv[1:] == ["browser"]:
            from renderer import check

            check("pair", ca, lifecycle=observe)
        else:
            raise ValueError("unknown fixture role")
    except Exception as error:
        from pathlib import Path
        from traceback import extract_tb

        emit(kind="worker_failure", error=type(error).__name__, stack=[
            [Path(frame.filename).name, frame.lineno, frame.name] for frame in extract_tb(error.__traceback__)[-5:]
        ])
        raise SystemExit(1) from None
