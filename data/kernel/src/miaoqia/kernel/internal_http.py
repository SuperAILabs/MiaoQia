"""Small bounded internal JSON transport. Peer authentication precedes all HTTP/context parsing.

No plaintext/development bypass; peer delegation is a separate explicit admission policy.
"""

from __future__ import annotations

import http.client
import json
import socket
import ssl
import threading
import time
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

from . import reqctx

from .jsoncodec import MAX_BODY, closed, loads


class Problem(Exception):
    def __init__(self, status: int):
        self.status = status
        super().__init__("internal request rejected")


def _interrupt(connection):
    try:
        connection.shutdown(socket.SHUT_RDWR)
    except (OSError, AttributeError):
        pass


def peer(cert, allowed):
    uris = [v for k, v in cert.get("subjectAltName", ()) if k == "URI"]
    if (
        len(uris) != 1
        or not uris[0].startswith("spiffe://miaoqia/service/")
        or uris[0] not in allowed
    ):
        raise Problem(403)
    return uris[0]


@dataclass(frozen=True)
class TLS:
    cert: str
    key: str
    ca: str

    def context(self, *, server=False):
        if not all((self.cert, self.key, self.ca)):
            raise ValueError("TLS configuration required")
        c = ssl.create_default_context(
            ssl.Purpose.CLIENT_AUTH if server else ssl.Purpose.SERVER_AUTH,
            cafile=self.ca,
        )
        c.minimum_version = ssl.TLSVersion.TLSv1_3
        c.verify_mode = ssl.CERT_REQUIRED
        c.load_cert_chain(self.cert, self.key)
        return c


class Client:
    """HTTPS only; one fresh connection/request, exact server peer, no redirects/retries/cache."""

    def __init__(self, url: str, tls: TLS, allowed_peer: str, timeout=5.0):
        u = urlsplit(url)
        if (
            u.scheme != "https"
            or not u.hostname
            or u.username
            or u.password
            or u.query
            or u.fragment
            or not allowed_peer
        ):
            raise ValueError("authenticated HTTPS endpoint required")
        if not 0 < timeout <= 5:
            raise ValueError("invalid timeout")
        self.host, self.port, self.path = u.hostname, u.port or 443, u.path.rstrip("/")
        self.context, self.allowed, self.timeout = (
            tls.context(),
            {allowed_peer},
            timeout,
        )

    def request(self, method, path, body=None, *, deadline=None):
        budget = min(
            self.timeout, (deadline - time.monotonic()) if deadline else self.timeout
        )
        if budget <= 0:
            raise Problem(503)
        started = time.monotonic()
        conn = http.client.HTTPSConnection(
            self.host, self.port, context=self.context, timeout=budget
        )
        timer = None
        try:
            conn.connect()
            timer = threading.Timer(
                max(0.001, budget - (time.monotonic() - started)),
                lambda: _interrupt(conn.sock) if conn.sock else None,
            )
            timer.daemon = True
            timer.start()
            peer(conn.sock.getpeercert(), self.allowed)
            payload = (
                b""
                if body is None
                else json.dumps(body, separators=(",", ":"), allow_nan=False).encode()
            )
            if len(payload) > MAX_BODY:
                raise ValueError("JSON too large")
            conn.request(
                method,
                self.path + path,
                payload,
                {
                    "Miaoqia-Context": reqctx.current().encode(),
                    "Content-Type": "application/json",
                    "Connection": "close",
                },
            )
            response = conn.getresponse()
            raw = response.read(MAX_BODY + 1)
            if not 200 <= response.status < 300:
                raise Problem(
                    response.status if response.status in (400, 401, 403) else 503
                )
            return loads(raw)
        except Problem:
            raise
        except Exception:
            raise Problem(503) from None
        finally:
            if timer:
                timer.cancel()
            conn.close()


class Identity:
    def __init__(self, client: Client):
        self.client = client

    def resolve(self, *, deadline):
        c = reqctx.current()
        if c.principal.kind != "user" or not c.tenant or not c.cell:
            raise Problem(403)
        v = self.client.request("GET", "/v1/identity/session", deadline=deadline)
        required = {
            "tenant",
            "cell",
            "subject",
            "state",
            "roles",
            "tenant_version",
            "member_version",
        }
        if (
            not isinstance(v, dict)
            or not required <= set(v)
            or set(v) - required - {"unit_roles"}
            or not isinstance(v["roles"], list)
            or any(not isinstance(r, str) for r in v["roles"])
            or any(
                type(v[k]) is not int or v[k] < 1
                for k in ("tenant_version", "member_version")
            )
        ):
            raise Problem(503)
        if any(
            v.get(k) != expected
            for k, expected in (
                ("subject", c.principal.id),
                ("tenant", c.tenant),
                ("cell", c.cell),
                ("state", "active"),
            )
        ):
            raise Problem(403)


class Server(ThreadingHTTPServer):
    daemon_threads = True
    block_on_close = False
    allow_reuse_address = True
    request_queue_size = 32

    def __init__(
        self, address, tls: TLS, delegations, identity, routes, *, timeout=5.0
    ):
        if not delegations or identity is None or not 0 < timeout <= 5:
            raise ValueError("internal admission configuration required")
        # Exact (peer, operation, purpose), user-only. No implicit peer-wide delegation.
        for p, op, purpose in delegations:
            if (
                not p.startswith("spiffe://miaoqia/service/")
                or not op
                or purpose != "interactive"
            ):
                raise ValueError("invalid delegation")
        self.tls, self.delegations, self.identity, self.routes = (
            tls.context(server=True),
            frozenset(delegations),
            identity,
            routes,
        )
        self.timeout_budget = timeout
        self.slots = threading.BoundedSemaphore(32)
        super().__init__(address, Handler)

    def get_request(self):
        raw, address = super().get_request()
        raw.settimeout(self.timeout_budget)
        return raw, address

    def process_request(self, request, address):
        if not self.slots.acquire(blocking=False):
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, address)
        except Exception:
            self.slots.release()
            raise

    def process_request_thread(self, request, address):
        secure = None
        timer = None
        try:
            # Handshakes share the same bounded worker pool as HTTP. A slow handshake
            # cannot serialize every accept or reach the HTTP parser before peer checks.
            secure = self.tls.wrap_socket(request, server_side=True)
            secure.verified_peer = peer(
                secure.getpeercert(), {p for p, _, _ in self.delegations}
            )
            # Budget the entire HTTP exchange, including request line and headers.
            secure.request_deadline = time.monotonic() + self.timeout_budget
            timer = threading.Timer(self.timeout_budget, lambda: _interrupt(secure))
            timer.daemon = True
            timer.start()
            super().process_request_thread(secure, address)
        except Exception:
            self.shutdown_request(secure or request)
        finally:
            if timer is not None:
                timer.cancel()
            self.slots.release()

    def handle_error(self, request, address):
        pass  # Never dump context/credentials/paths/tracebacks from an internal request.


class Handler(BaseHTTPRequestHandler):
    server_version = "Miaoqia"
    sys_version = ""

    def log_message(self, *args):
        pass

    def do_POST(self):
        self._post(self.connection.request_deadline)

    def _post(self, deadline):
        status, result = 503, {"title": "unavailable", "status": 503}
        try:
            if len(str(self.headers)) > 16384 or self.headers.get("Transfer-Encoding"):
                raise ValueError("invalid headers")
            headers = self.headers.get_all("Miaoqia-Context", [])
            if len(headers) != 1:
                raise ValueError("invalid context")
            # Context.decode is reused, with strict duplicate/depth checking before DTO validation.
            h = headers[0]
            if len(h) > 8192:
                raise ValueError("context too large")
            c = reqctx.Context.decode(h)
            route = self.server.routes.get(self.path)
            if route is None:
                raise Problem(404)
            op, fn = route
            if (
                c.principal.kind != "user"
                or not c.tenant
                or not c.cell
                or (self.connection.verified_peer, op, c.purpose)
                not in self.server.delegations
            ):
                raise Problem(403)
            with reqctx.use(c):
                self.server.identity.resolve(deadline=deadline)
                lengths = self.headers.get_all("Content-Length", [])
                if (
                    len(lengths) != 1
                    or not lengths[0].isdigit()
                    or not 0 < int(lengths[0]) <= MAX_BODY
                ):
                    raise ValueError("invalid body length")
                self.connection.settimeout(max(0.001, deadline - time.monotonic()))
                raw = self.rfile.read(int(lengths[0]))
                if len(raw) != int(lengths[0]):
                    raise ValueError("incomplete body")
                result = fn(loads(raw), deadline)
                if time.monotonic() >= deadline:
                    raise Problem(503)
                status = 200
        except Problem as e:
            status = e.status
        except (ValueError, TypeError, RecursionError):
            status = 400
        except Exception:
            status = 503
        if status != 200:
            result = {
                "title": "unavailable" if status == 503 else "request rejected",
                "status": status,
            }
        self.close_connection = True
        raw = json.dumps(result, separators=(",", ":"), allow_nan=False).encode()
        self.send_response(status)
        self.send_header(
            "Content-Type",
            "application/json" if status == 200 else "application/problem+json",
        )
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(raw)
