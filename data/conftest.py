"""Shared real-TLS fixtures for Python transport and subsystem contract tests."""

import threading

import pytest

from miaoqia.kernel.testcerts import TestCA


@pytest.fixture
def tls_factory(tmp_path):
    return TestCA(tmp_path).peer


@pytest.fixture
def running_server():
    servers = []

    def run(server):
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        servers.append((server, thread))
        return f"https://localhost:{server.server_port}"

    yield run
    for server, thread in reversed(servers):
        server.shutdown()
        server.server_close()
        thread.join(5)
