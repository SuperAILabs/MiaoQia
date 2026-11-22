"""HTTP transport probes use only a loopback synthetic publisher."""
from __future__ import annotations

import ssl
import time
from urllib.error import URLError
from urllib.request import Request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from miaoqia.acquisition.api.handlers import FetchRequest
from miaoqia.acquisition.infrastructure.archive_spool import ArchiveSpool
from miaoqia.acquisition.infrastructure.transport import TransportError, UrllibTransport, redirect_request, site_identity
from miaoqia.acquisition.infrastructure.transport import _RequestTimeout


@pytest.mark.parametrize("reason", ["BAD_ECPOINT", "CERTIFICATE_VERIFY_FAILED", "UNSAFE_LEGACY_RENEGOTIATION_DISABLED"])
def test_tls_curve_retry_only_handles_bad_ecpoint_with_default_certificate_checks(monkeypatch, reason):
    original = ssl.create_default_context()
    handler = _RequestTimeout(time.monotonic() + 3, original)
    contexts = []
    failure = ssl.SSLError(1, reason)
    failure.reason = reason

    def open_probe(connection, request, *, context):
        contexts.append(context)
        if len(contexts) == 1:
            raise URLError(failure)
        assert context is not original and context.verify_mode == ssl.CERT_REQUIRED and context.check_hostname
        assert 0 < request.timeout <= 3
        return context

    monkeypatch.setattr(handler, "do_open", open_probe)
    request = Request("https://example.invalid")
    if reason == "BAD_ECPOINT":
        assert handler.https_open(request) is contexts[-1]
        assert len(contexts) == 2
    else:
        with pytest.raises(URLError):
            handler.https_open(request)
        assert len(contexts) == 1


@pytest.fixture
def publisher(running_server):
    received = []

    class Publisher(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_GET(self):
            received.append(self.path)
            if self.path in ("/local", "/other"):
                self.send_response(302)
                target = "/body" if self.path == "/local" else f"http://localhost:{self.server.server_port}/body"
                self.send_header("Location", target)
                self.end_headers()
                return
            if self.path == "/throttle":
                self.send_response(429)
                self.send_header("Retry-After", "120")
                self.end_headers()
                return
            if self.path == "/slow-headers":
                try:
                    self.wfile.write(b"HTTP/1.1 200 OK\r\n")
                    for _ in range(8):
                        time.sleep(0.05)
                        self.wfile.write(b"X-Slow: 1\r\n")
                    self.wfile.write(b"\r\n")
                except OSError:
                    pass
                return
            self.send_response(200)
            self.send_header("Content-Length", "8")
            self.end_headers()
            if self.path == "/body":
                self.wfile.write(b"original")
            elif self.path == "/truncated":
                self.wfile.write(b"short")
            else:
                try:
                    for _ in range(8):
                        self.wfile.write(b"Z")
                        time.sleep(0.05)
                except OSError:
                    pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Publisher)
    running_server(server)
    return f"http://127.0.0.1:{server.server_port}", received


def test_redirect_hops_are_explicit_and_restricted_to_origin(publisher):
    url, received = publisher
    transport = UrllibTransport()
    request = FetchRequest("GET", f"{url}/local")
    response = transport.send(request, 2)
    assert (response.status, response.redirect_to, received) == (302, "/body", ["/local"])
    assert transport.send(redirect_request(request, response), 2).body == b"original"
    request = FetchRequest("GET", f"{url}/other", headers=(("Authorization", "synthetic"),))
    response = transport.send(request, 2)
    with pytest.raises(TransportError, match="outside request origin"):
        redirect_request(request, response)
    assert received == ["/local", "/body", "/other"]


def test_site_identity_canonicalizes_hostname_without_scheme_or_port():
    assert site_identity("http://ExAmPlE.test.:8080/list") == site_identity("https://example.test/body") == "example.test"
    with pytest.raises(TransportError, match="invalid HTTP target"):
        site_identity("https://user@example.test/")


@pytest.mark.parametrize("path", ["/slow", "/slow-headers"])
def test_absolute_transport_timeout_for_trickling_response(publisher, path):
    url, _ = publisher
    start = time.monotonic()
    with pytest.raises(TransportError):
        UrllibTransport().send(FetchRequest("GET", url + path), 0.13)
    assert time.monotonic() - start < 0.35


def test_bounded_body_and_truncated_response(publisher):
    url, _ = publisher
    with pytest.raises(TransportError, match="size cap") as caught:
        UrllibTransport(max_body=7).send(FetchRequest("GET", f"{url}/body"), 2)
    assert not caught.value.retryable
    with pytest.raises(TransportError, match="truncated response"):
        UrllibTransport().send(FetchRequest("GET", f"{url}/truncated"), 2)
    response = UrllibTransport().send(FetchRequest("GET", f"{url}/throttle"), 2)
    assert (response.status, response.retry_after) == (429, "120")


def test_http_streams_to_spool_as_bytes_arrive_and_discards_a_timed_out_download(publisher, tmp_path):
    url, _ = publisher
    transport = UrllibTransport()
    spool = ArchiveSpool(tmp_path / "private")
    with spool.receive() as incoming:
        with pytest.raises(TransportError):
            transport.send_capture(FetchRequest("GET", f"{url}/slow"), 0.18, incoming.write)
        assert 0 < incoming.size < 8
        assert incoming.body() == b"Z" * incoming.size
    assert not list(spool.root.glob(".download-*"))

    with spool.receive() as incoming:
        response = transport.send_capture(FetchRequest("GET", f"{url}/body"), 2, incoming.write)
        assert response.body == b""
        assert incoming.body() == b"original"
