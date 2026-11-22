"""One HTTP hop with bounded body reads. Redirects are admitted by the fetch runtime."""
from __future__ import annotations

import http.client
import socket
import ssl
import time
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Callable
from dataclasses import replace
from io import BytesIO
from threading import Timer

from ..api.handlers import FetchRequest, FetchResponse

USER_AGENT = "Mozilla/5.0 (compatible; MiaoQiaCollector/0.1)"
MAX_BODY = 20 * 1024 * 1024
REDIRECTS = (301, 302, 303, 307, 308)


class TransportError(RuntimeError):
    """Network failure or response budget violation."""

    def __init__(self, message: str, *, retryable: bool = True):
        super().__init__(message)
        self.retryable = retryable


def request_origin(url: str) -> tuple[str, str, int]:
    """Return (scheme, normalized hostname, port) for an HTTP target."""
    try:
        target = urllib.parse.urlsplit(url)
        host = target.hostname
        if (target.scheme not in ("http", "https") or not host or target.username is not None
                or target.fragment or any(ord(char) <= 32 or char == "\x7f" for char in url)):
            raise ValueError("invalid target")
        host = host.rstrip(".").encode("idna").decode("ascii")
        if not host or not all(char.isalnum() or char in ".-:" for char in host):
            raise ValueError("invalid hostname")
        port = target.port if target.port is not None else (443 if target.scheme == "https" else 80)
        if not port:
            raise ValueError("invalid port")
        return target.scheme, host, port
    except (ValueError, UnicodeError) as exc:
        raise TransportError("invalid HTTP target", retryable=False) from exc


def site_identity(url: str) -> str:
    """Budget identity shared by HTTP/HTTPS and ports on the same normalized hostname."""
    return request_origin(url)[1]


def redirect_request(request: FetchRequest, response: FetchResponse) -> FetchRequest:
    if not response.redirect_to:
        raise TransportError("redirect missing location", retryable=False)
    target = urllib.parse.urljoin(request.url, response.redirect_to)
    origin, new_origin = request_origin(request.url), request_origin(target)
    same_site_upgrade = origin == ("http", new_origin[1], 80) and new_origin[::2] == ("https", 443)
    if origin != new_origin and not same_site_upgrade:
        raise TransportError("redirect outside request origin", retryable=False)
    if request.method == "POST" and response.status in (301, 302, 303):
        return FetchRequest("GET", target, headers=tuple(
            (name, value) for name, value in request.headers if name.lower() not in ("content-length", "content-type")
        ))
    return FetchRequest(request.method, target, request.form, request.json_body, request.headers)


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def _shutdown(sock: socket.socket) -> None:
    try:
        sock.shutdown(socket.SHUT_RDWR)
    except OSError:
        pass  # The peer or HTTPResponse already closed the socket.


class _RequestTimeout(urllib.request.HTTPSHandler):
    """Enforce a wall-clock deadline once a socket is connected, including headers and chunk framing."""

    def __init__(self, deadline: float, context: ssl.SSLContext):
        super().__init__(context=context)
        self.deadline = deadline
        self.timer: Timer | None = None

    def _connection(self, connection_type):
        budget = self

        class Connection(connection_type):
            def connect(self):
                super().connect()
                remaining = budget.deadline - time.monotonic()
                if remaining <= 0:
                    self.close()
                    raise TimeoutError("request deadline exceeded")
                budget.timer = Timer(remaining, _shutdown, args=(self.sock,))
                budget.timer.daemon = True
                budget.timer.start()

        return Connection

    def https_open(self, req):
        connection = self._connection(http.client.HTTPSConnection)
        try:
            return self.do_open(connection, req, context=self._context)
        except urllib.error.URLError as error:
            if (self.timer is not None or not isinstance(error.reason, ssl.SSLError)
                    or getattr(error.reason, "reason", None) != "BAD_ECPOINT"):
                raise
            remaining = self.deadline - time.monotonic()
            if remaining <= 0:
                raise
            compatible = ssl.create_default_context()
            compatible.set_ecdh_curve("prime256v1")
            req.timeout = remaining
            return self.do_open(connection, req, context=compatible)


class _TimedHTTP(urllib.request.HTTPHandler):
    def __init__(self, budget: _RequestTimeout):
        super().__init__()
        self.budget = budget

    def http_open(self, req):
        return self.do_open(self.budget._connection(http.client.HTTPConnection), req)


class UrllibTransport:
    def __init__(self, *, max_body: int = MAX_BODY):
        if max_body <= 0:
            raise ValueError("max_body must be positive")
        self.max_body = max_body
        self.context = ssl.create_default_context()

    def send(self, request: FetchRequest, timeout: float) -> FetchResponse:
        with BytesIO() as body:
            response = self.send_capture(request, timeout, body.write)
            return replace(response, body=body.getvalue())

    def send_capture(self, request: FetchRequest, timeout: float, write: Callable[[bytes], int]) -> FetchResponse:
        """Stream a successful hop's original bytes to write; return its response metadata."""
        request_origin(request.url)
        deadline = time.monotonic() + timeout
        headers = {"User-Agent": USER_AGENT}
        headers.update((name, value) for name, value in request.headers if name.lower() != "host")
        body = None
        if request.form:
            body = urllib.parse.urlencode(request.form).encode("utf-8")
            headers.setdefault("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
        elif request.json_body is not None:
            body = request.json_body.encode("utf-8")
            headers.setdefault("Content-Type", "application/json;charset=UTF-8")
        req = urllib.request.Request(request.url, data=body, headers=headers, method=request.method)
        budget = _RequestTimeout(deadline, self.context)
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), _NoRedirect(), _TimedHTTP(budget), budget)
        try:
            try:
                response = opener.open(req, timeout=max(0.001, deadline - time.monotonic()))
            except urllib.error.HTTPError as error:
                response = error
            with response:
                retry_after = response.headers.get("Retry-After")
                redirect_to = response.headers.get("Location")
                if response.status == 200:
                    self._read(response, deadline, write)
                if time.monotonic() >= deadline:
                    raise TimeoutError("request deadline exceeded")
                return FetchResponse(response.status, _media_type(response.headers.get("Content-Type")),
                                     b"", response.geturl(), retry_after, redirect_to)
        except (urllib.error.URLError, TimeoutError, OSError, http.client.HTTPException) as exc:
            raise TransportError(type(exc).__name__) from exc
        finally:
            if budget.timer is not None:
                budget.timer.cancel()

    def _read(self, response, deadline: float, write: Callable[[bytes], int]) -> None:
        remaining_bytes = self.max_body
        size = response.length
        if size is not None and size > remaining_bytes:
            raise TransportError("response exceeds size cap", retryable=False)
        while response.fp is not None:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("response deadline exceeded")
            chunk = response.read1(min(64 * 1024, remaining_bytes + 1))
            if not chunk:
                break
            remaining_bytes -= len(chunk)
            if remaining_bytes < 0:
                raise TransportError("response exceeds size cap", retryable=False)
            if write(chunk) != len(chunk):
                raise OSError("incomplete response write")
        if size is not None and self.max_body - remaining_bytes != size:
            raise TransportError("truncated response")


def _media_type(content_type: str | None) -> str:
    return (content_type or "application/octet-stream").split(";", 1)[0].strip().lower()
