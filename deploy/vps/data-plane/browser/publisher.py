"""Synthetic HTTPS origin and TCP/UDP canaries. Output is metadata only."""

from datetime import date
from hashlib import sha256
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
import socket
import ssl
import sys
from threading import Thread

HOST = "publisher.fixture.test"
OTHER_HOST = "other.fixture.test"
ROLE_LIST = "role-list"
ROLE_DETAIL = "role-detail"
ROLE_SELECTOR = "#mount > article[data-notice-id]"
KEY = "90071992547409931"
CASES = (
    "pair", "redirect", "path", "method", "query", "duplicate", "foreign", "gzip",
    "iframe", "popup", "worker", "websocket", "prefetch", "untrusted_ca", "wrong_name", "control_loss",
    "forbidden_status", "throttled", "popup_view", "provider_veto",
)
TLS_PAIR_CASES = ("pair", "untrusted_ca", "wrong_name")
SCRIPTS = {
    "path": 'await fetch("extra");',
    "method": 'await fetch("data", {method: "POST", body: "synthetic"});',
    "query": 'await fetch("data?extra=1");',
    "duplicate": 'await fetch("data");',
    "foreign": f'await fetch("https://{OTHER_HOST}/extra");',
    "iframe": 'document.body.appendChild(Object.assign(document.createElement("iframe"), {src: "extra"}));',
    "popup": 'window.open("data");',
    "popup_view": 'window.open("view");',
    "worker": '''globalThis.fixtureWorkerResult = navigator.serviceWorker
      ? (await navigator.serviceWorker.register("extra") ? "registered" : "blocked") : "unavailable";''',
    "websocket": 'new WebSocket("wss://" + location.host + location.pathname.replace("view", "extra"));',
    "prefetch": 'document.head.appendChild(Object.assign(document.createElement("link"), {rel: "prefetch", href: "extra"}));',
    "control_loss": 'await new Promise(resolve => setTimeout(resolve, 1500));',
}
PAGE_TEMPLATE = """<!doctype html><meta charset="utf-8"><link rel="icon" href="data:,">
<main id="mount"></main><script>
(async () => {
  %s
  const response = await fetch("data");
  const raw = new TextDecoder("%s").decode(await response.arrayBuffer());
  const article = document.createElement("article");
  article.dataset.noticeId = "%s";
  const paragraph = document.createElement("p");
  paragraph.textContent = %s;
  article.appendChild(paragraph);
  document.getElementById("mount").appendChild(article);
})().catch(() => {});
</script>"""
DETAIL_TEXT = "raw.slice(0, -2)"
PAGE = PAGE_TEMPLATE % ("%s", "windows-1252", "fixture-1", DETAIL_TEXT)
BODY = b"R\xe9sum\xe9 \x96 fixture\r\n"
SELECTED_HTML = '<article data-notice-id="fixture-1"><p>Résumé – fixture</p></article>'


def role_routes(published_on):
    if date.fromisoformat(published_on).isoformat() != published_on:
        raise ValueError("fixture publication date must be YYYY-MM-DD")
    list_body = json.dumps({"key": KEY, "page": 1, "published_on": published_on},
                           separators=(",", ":")).encode("utf-8")
    return {
        ROLE_LIST: {
            "view": ((PAGE_TEMPLATE % ("", "utf-8", KEY, "raw")).encode("utf-8"), "text/html; charset=utf-8"),
            "data": (list_body, "application/json; charset=utf-8"),
        },
        ROLE_DETAIL: {
            "view": ((PAGE_TEMPLATE % ("", "windows-1252", KEY, DETAIL_TEXT)).encode("utf-8"),
                     "text/html; charset=utf-8"),
            "data": (BODY, "text/plain; charset=windows-1252"),
        },
    }


def emit(**fields):
    sys.stdout.write(json.dumps(fields, sort_keys=True) + "\n")
    sys.stdout.flush()


class PublisherServer(ThreadingHTTPServer):
    routes = {}

    def handle_error(self, _request, _address):
        error = sys.exc_info()[1]
        disconnected = isinstance(error, (BrokenPipeError, ConnectionResetError, ssl.SSLEOFError))
        emit(kind="client_disconnect" if disconnected else "listener_error")


class LifecycleServer(PublisherServer):
    def __init__(self, address, handler, tls, *, routes=None):
        self.tls = tls
        if routes is not None:
            self.routes = routes
        super().__init__(address, handler)

    def get_request(self):
        connection, address = super().get_request()
        emit(kind="tcp_accept", from_browser=address[0] == os.environ["BROWSER_IP"], port=address[1])
        try:
            connection.settimeout(2)
            secured = self.tls.wrap_socket(connection, server_side=True)
            secured.settimeout(25)
            return secured, address
        except OSError:
            connection.close()
            raise


class Publisher(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        self.do_GET()

    def do_GET(self):
        parts = self.path.split("/")
        case, endpoint = parts[1:] if len(parts) == 3 else ("unknown", "")
        routes = self.server.routes.get(case)
        case = case if case in CASES or routes is not None else "unknown"
        permitted = (
            self.command == "GET" and self.headers.get("Host") == HOST
            and "Upgrade" not in self.headers and endpoint in {"view", "data"}
            and case != "unknown"
            and (routes is None or self.requestline.split()[1] == self.path)
        )
        status, content, media = 403, b"", "text/plain"
        headers = {}
        if permitted and routes is not None:
            status = 200
            content, media = routes[endpoint]
        elif permitted and endpoint == "view":
            status, content, media = 200, (PAGE % SCRIPTS.get(case, "")).encode(), "text/html"
            if case == "redirect":
                status, content = 302, b""
                headers["Location"] = "extra"
            if case in {"forbidden_status", "throttled"}:
                status, content = (403 if case == "forbidden_status" else 429), b""
        elif permitted and endpoint == "data":
            status, content = 200, BODY
            if case == "gzip":
                from gzip import compress
                content = compress(content, mtime=0)
                headers["Content-Encoding"] = "gzip"
        emit(
            kind="request", case=case, request=endpoint if permitted else "unexpected",
            status=status, tls=self.connection.version(),
            approved_peer=self.connection.getsockname() == (os.environ["PUBLISHER_IP"], 443),
            expected_client=self.client_address[0] == os.environ["BROWSER_IP"],
            body_sha256=sha256(content).hexdigest() if permitted and endpoint == "data" else None,
        )
        self.send_response(status)
        self.send_header("Content-Type", media)
        self.send_header("Content-Length", str(len(content)))
        for name, value in headers.items():
            self.send_header(name, value)
        self.end_headers()
        self.wfile.write(content)


class LifecyclePublisher(Publisher):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        endpoint = {"/lifecycle/probe": "probe", "/lifecycle/health": "health"}.get(self.path)
        if endpoint is None:
            super().do_GET()
            return
        browser_peer = self.client_address[0] == os.environ["BROWSER_IP"]
        allowed = (self.command == "GET" and self.headers.get("Host") == HOST
                   and browser_peer == (endpoint == "probe") and "Upgrade" not in self.headers)
        emit(kind="lifecycle_http", endpoint=endpoint, from_browser=browser_peer,
             port=self.client_address[1], allowed=allowed, tls=self.connection.version())
        self.send_response(200 if allowed else 403)
        self.send_header("Content-Length", "0")
        self.send_header("Cache-Control", "no-store")
        self.end_headers()


def canary(address, port, *, datagram=False):
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM if datagram else socket.SOCK_STREAM) as listener:
        listener.bind((address, port))
        if not datagram:
            listener.listen()
        name = "udp" if datagram else "tcp"
        emit(kind="canary_ready", protocol=name)
        while True:
            if datagram:
                listener.recvfrom(512)
            else:
                connection, _ = listener.accept()
                connection.close()
            emit(kind="canary_arrival", protocol=name)


def main():
    address = os.environ["PUBLISHER_IP"]
    provider_role = sys.argv[1:] == ["--provider-role"]
    routes = role_routes(os.environ["MQ_FIXTURE_PUBLISHED_ON"]) if provider_role else None
    for port, datagram in [(443, True), (8443, False)]:
        Thread(target=canary, args=(address, port), kwargs={"datagram": datagram}, daemon=True).start()
    ssl_context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ssl_context.minimum_version = ssl.TLSVersion.TLSv1_2
    ssl_context.load_cert_chain("/fixture-pki/leaf.crt", "/fixture-pki/leaf.key")

    def record_sni(_socket, name, _context):
        emit(kind="sni", match=name == HOST, wrong_name_control=name == OTHER_HOST)

    ssl_context.sni_callback = record_sni
    if provider_role or sys.argv[1:] == ["--lifecycle"]:
        with LifecycleServer((address, 443), LifecyclePublisher, ssl_context, routes=routes) as server:
            emit(kind="https_ready")
            server.serve_forever()
        return
    with PublisherServer((address, 443), Publisher) as server:
        server.socket = ssl_context.wrap_socket(server.socket, server_side=True)
        emit(kind="https_ready")
        server.serve_forever()


if __name__ == "__main__":
    if os.environ.get("CANARY_IP"):
        canary(os.environ["CANARY_IP"], 443)
    else:
        main()
