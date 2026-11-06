"""Test-owned loopback forwarder for exercising the real mTLS server.

This helper is deliberately confined to tests: it binds 127.0.0.1, forwards
the exact request bytes over a client certificate, and returns upstream status
and body unchanged. It contains no canned success path or plaintext product
listener.
"""
from __future__ import annotations

import http.client
import ssl
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import threading


def forwarder(upstream_host, upstream_port, tls_context, *, path_prefix=""):
    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            length = int(self.headers.get("Content-Length", "-1"))
            if length < 0 or length > 65536:
                self.send_error(400)
                return
            body = self.rfile.read(length)
            conn = http.client.HTTPSConnection(upstream_host, upstream_port, context=tls_context, timeout=5)
            try:
                conn.request("POST", path_prefix + self.path, body,
                             {"Content-Type": self.headers.get("Content-Type", "application/json"),
                              "Miaoqia-Context": self.headers.get("Miaoqia-Context", ""),
                              "Connection": "close"})
                response = conn.getresponse()
                payload = response.read(65537)
                self.send_response(response.status)
                self.send_header("Content-Length", str(len(payload)))
                self.send_header("Content-Type", response.getheader("Content-Type", "application/json"))
                self.end_headers()
                self.wfile.write(payload)
            finally:
                conn.close()

        def log_message(self, *_):
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, thread
