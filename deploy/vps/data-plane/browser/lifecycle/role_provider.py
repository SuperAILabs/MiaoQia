"""Operator renderer binding for the two synthetic Acquisition role plans."""

import asyncio
from contextlib import asynccontextmanager, suppress
from importlib.metadata import version
from ipaddress import IPv4Address
import os

from playwright.async_api import Error as PlaywrightError

from miaoqia.acquisition.api.handlers import FetchRequest
from miaoqia.runtime_acquisition.browser_renderer import ConfinedPage, PlaywrightBrowserRenderer

from publisher import HOST, ROLE_DETAIL, ROLE_LIST, ROLE_SELECTOR

from .controller_ipc import CLOSE_TIMEOUT, PLAYWRIGHT_VERSION, connect_driver, require


ROLE_PLANS = tuple(
    (f"https://{HOST}/{route}/view", FetchRequest("GET", f"https://{HOST}/{route}/data"), ROLE_SELECTOR)
    for route in (ROLE_LIST, ROLE_DETAIL)
)


def make_renderer() -> PlaywrightBrowserRenderer:
    peer = str(IPv4Address(os.environ["MQ_FIXTURE_PUBLISHER_IP"]))
    require(version("playwright") == PLAYWRIGHT_VERSION, "unexpected Playwright version")

    @asynccontextmanager
    async def provider(plan):
        require((plan.navigation_url, plan.body_request, plan.body_selector) in ROLE_PLANS,
                "unexpected fixture browser plan")
        async with connect_driver() as playwright:
            browser = await playwright.chromium.launch(
                channel="chromium", headless=True, chromium_sandbox=True,
                args=["--no-proxy-server", "--disable-quic", f"--host-resolver-rules=MAP {HOST} {peer}, MAP * ~NOTFOUND"],
            )
            try:
                context = await browser.new_context(service_workers="block", accept_downloads=False,
                                                    ignore_https_errors=False)
                page = await context.new_page()
                yield ConfinedPage(context, page, frozenset({peer}))
            except BaseException:
                with suppress(PlaywrightError, TimeoutError, ConnectionError):
                    await asyncio.wait_for(browser.close(), timeout=CLOSE_TIMEOUT)
                raise
            else:
                await asyncio.wait_for(browser.close(), timeout=CLOSE_TIMEOUT)

    return PlaywrightBrowserRenderer(provider)
