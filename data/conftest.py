"""Shared real-TLS fixtures for Python transport and subsystem contract tests."""

from pathlib import Path
import subprocess
import threading

import pytest

from miaoqia.kernel.internal_http import TLS


@pytest.fixture
def tls_factory(tmp_path):
    def run(*args):
        subprocess.run(
            ["openssl", *args],
            cwd=tmp_path,
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )

    run(
        "req",
        "-x509",
        "-newkey",
        "rsa:2048",
        "-nodes",
        "-keyout",
        "ca.key",
        "-out",
        "ca.crt",
        "-days",
        "2",
        "-subj",
        "/CN=Tests",
        "-addext",
        "basicConstraints=critical,CA:TRUE",
        "-addext",
        "keyUsage=critical,keyCertSign,cRLSign",
    )

    def make(name, uri=None, *, extra_uri=None, days="1"):
        run(
            "req",
            "-newkey",
            "rsa:2048",
            "-nodes",
            "-keyout",
            name + ".key",
            "-out",
            name + ".csr",
            "-subj",
            "/CN=localhost",
        )
        sans = ["DNS:localhost", "IP:127.0.0.1"]
        if uri:
            sans.append("URI:" + uri)
        if extra_uri:
            sans.append("URI:" + extra_uri)
        ext = tmp_path / (name + ".ext")
        ext.write_text(
            "subjectAltName="
            + ",".join(sans)
            + "\nextendedKeyUsage=serverAuth,clientAuth\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nsubjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid,issuer\n"
        )
        run(
            "x509",
            "-req",
            "-in",
            name + ".csr",
            "-CA",
            "ca.crt",
            "-CAkey",
            "ca.key",
            "-CAcreateserial",
            "-out",
            name + ".crt",
            "-days",
            "0" if days == "-1" else days,
            "-extfile",
            name + ".ext",
        )
        return TLS(
            str(tmp_path / (name + ".crt")),
            str(tmp_path / (name + ".key")),
            str(tmp_path / "ca.crt"),
        )

    return make


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
