"""Response and origin checks shared by the government procurement HTTP handlers."""
from __future__ import annotations

import re
from datetime import date
from urllib.parse import SplitResult, urljoin, urlsplit

from miaoqia.acquisition.api.handlers import FetchResponse, SourceBlocked
from miaoqia.sdk.source_html import HtmlElement, parse_document

from . import html_text

_DATE = re.compile(r"(?<!\d)(20\d{2})[-/.年]\s*(\d{1,2})[-/.月]\s*(\d{1,2})(?!\d)")
_CAPTCHA_TITLES = {"访问验证", "安全验证", "人机验证"}
_LOGIN_TITLES = {"登录", "用户登录", "请先登录"}


def official_url(origin: str, base: str, href: str) -> SplitResult | None:
    expected = urlsplit(origin)
    try:
        parsed = urlsplit(urljoin(base, href))
        port = parsed.port
    except ValueError:
        return None
    if (parsed.scheme != expected.scheme or parsed.hostname != expected.hostname
            or port not in (None, 443 if expected.scheme == "https" else 80)
            or parsed.username is not None or parsed.password is not None):
        return None
    return parsed


def require_official(origin: str, response: FetchResponse) -> SplitResult:
    if response.status in (401, 403):
        raise SourceBlocked(f"http_{response.status}")
    if response.status != 200:
        raise ValueError(f"unexpected HTTP status {response.status}")
    parsed = official_url(origin, origin, response.url)
    if parsed is None:
        raise ValueError("response did not remain on the official origin")
    reported = urlsplit(response.url)
    if reported.scheme != parsed.scheme or not reported.netloc:
        raise ValueError("response did not remain on the official origin")
    return parsed


def checked_html(source: str) -> HtmlElement:
    return checked_document(parse_document(source))


def checked_document(root: HtmlElement) -> HtmlElement:
    for node in root.xpath(".//script|.//style|.//noscript"):
        node.drop_tree()
    headings = {visible_text(node) for node in root.xpath("//title|//h1|//h2")}
    text = visible_text(root)
    if headings & _CAPTCHA_TITLES or ("为保证您的正常访问" in text and "日志ID" in text):
        raise SourceBlocked("captcha_challenge")
    if headings & _LOGIN_TITLES or root.xpath(".//form//input[@type='password']"):
        raise SourceBlocked("login_required")
    return root


def response_html(response: FetchResponse) -> HtmlElement:
    return checked_html(html_text(response))


def visible_text(node: HtmlElement) -> str:
    return " ".join(" ".join(node.itertext()).split())


def publication_date(text: str) -> date | None:
    match = _DATE.search(text)
    if match:
        try:
            return date(int(match[1]), int(match[2]), int(match[3]))
        except ValueError:
            return None
    return None
