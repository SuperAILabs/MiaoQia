"""Real TLS listener/client + PostgreSQL rights authority; identity session contract served over TLS."""

from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler
import base64
import http.client
import json
from pathlib import Path
import ssl
import socket
import subprocess
import time
from urllib.parse import urlsplit

import pytest

from miaoqia.acquisition.api.remote import RightsClient
from miaoqia.acquisition.application.http import routes
from miaoqia.acquisition.application.rights import Decisions
from miaoqia.kernel import reqctx
from miaoqia.kernel.internal_http import Client, Identity, Problem, Server, TLS, loads
from test_rights import CTX, SOURCE, TENDER, seed, request

DATA = "spiffe://miaoqia/service/data-api"
ACQ = "spiffe://miaoqia/service/acq-admin"
IDENTITY = "spiffe://miaoqia/service/platform-api"


class NoopAdmission:
    def resolve(self, **kw):
        pass


class IdentityContract(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        assert self.path == "/internal/v1/identity/session"
        c = reqctx.Context.decode(self.headers["Miaoqia-Context"])
        state = self.server.sessions
        state["seen"].append(c)
        if state.get("sleep"):
            time.sleep(state["sleep"])
        status = (
            503
            if state.get("down")
            else (
                403
                if state.get("removed") or c.tenant != CTX.tenant or c.cell != CTX.cell
                else 200
            )
        )
        v = {
            "subject": c.principal.id,
            "tenant": c.tenant,
            "cell": c.cell,
            "state": "suspended" if state.get("suspended") else "active",
            "roles": [],
            "unit_roles": [],
            "tenant_version": 1,
            "member_version": 1,
        }
        raw = json.dumps(v).encode()
        self.send_response(status)
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


@pytest.fixture
def remote(registry, tls_factory, running_server):
    read, write, _ = registry
    seed(write)
    data_tls = tls_factory("data", DATA)
    acq_tls = tls_factory("acq", ACQ)
    identity_tls = tls_factory("identity", IDENTITY)
    ident = Server(
        ("localhost", 0),
        identity_tls,
        {(ACQ, "identity.session", "interactive")},
        NoopAdmission(),
        {},
    )
    ident.RequestHandlerClass = IdentityContract
    ident.sessions = {"seen": []}
    identurl = running_server(ident)
    identity = Identity(Client(identurl + "/internal", acq_tls, IDENTITY))
    srv = Server(
        ("localhost", 0),
        acq_tls,
        {
            (DATA, op, "interactive")
            for op in ("rights.resolve", "rights.confirm", "rights.status")
        },
        identity,
        routes(Decisions(read)),
    )
    url = running_server(srv)
    return (
        RightsClient(Client(url + "/internal/v1", data_tls, ACQ)),
        srv,
        ident,
        data_tls,
        url,
        read,
        write,
    )


def call(client, method="resolve", body=None, context=CTX):
    with reqctx.use(context):
        return (
            client.status()
            if method == "status"
            else getattr(client, method)(request() if body is None else body)
        )


def test_network_positive_revocation_no_inventory_and_status(remote):
    client, srv, ident, tls, url, read, write = remote
    a = call(client)
    assert a["allowed"]
    assert call(client, "confirm", {**request(), "receipt": a["receipt"]})["allowed"]
    assert call(client, "status", {}) == {
        "domain_service": "not_live",
        "capability": "frozen_source_rights_only",
    }
    assert ident.sessions["seen"][-1] == CTX
    with reqctx.use(CTX):
        write.command(
            {"action": "revoke", "entry_id": "building-tender"}, "op", "network revoke"
        )
    assert not call(client, "confirm", {**request(), "receipt": a["receipt"]})[
        "allowed"
    ]
    assert reqctx.get() is None


def test_network_identity_admission_before_no_match_and_confirm(remote, monkeypatch):
    client, srv, ident, tls, url, read, write = remote
    calls = []
    original = read.snapshot

    def snapshot():
        calls.append(1)
        return original()

    monkeypatch.setattr(read, "snapshot", snapshot)
    bad = request("unknown")
    a = call(client)
    for ctx in (
        reqctx.Context(CTX.principal, "other-tenant", CTX.cell, CTX.purpose),
        reqctx.Context(CTX.principal, CTX.tenant, "cell-02", CTX.purpose),
        reqctx.Context(CTX.principal, CTX.tenant, CTX.cell, "research"),
        reqctx.Context(
            reqctx.Principal("svc:data", "service"), CTX.tenant, CTX.cell, "interactive"
        ),
    ):
        before = len(calls)
        for method, body in (
            ("resolve", bad),
            ("confirm", {**bad, "receipt": a["receipt"]}),
            ("status", {}),
        ):
            with pytest.raises(Problem) as e:
                call(client, method, body, ctx)
            assert e.value.status == 403
        assert len(calls) == before
    for state in ("removed", "suspended"):
        ident.sessions[state] = True
        before = len(calls)
        with pytest.raises(Problem) as e:
            call(client, body=bad)
        assert e.value.status == 403 and len(calls) == before
        ident.sessions.pop(state)
    ident.sessions["down"] = True
    before = len(calls)
    with pytest.raises(Problem) as e:
        call(client, body=bad)
    assert e.value.status == 503 and len(calls) == before


def raw(url, tls, body, header, extra=None):
    u = urlsplit(url)
    c = http.client.HTTPSConnection(
        u.hostname, u.port, context=tls.context(), timeout=3
    )
    try:
        c.request(
            "POST",
            "/internal/v1/source-rights/resolve",
            body,
            {
                "Content-Type": "application/json",
                "Miaoqia-Context": header,
                **(extra or {}),
            },
        )
        r = c.getresponse()
        return r.status, r.read()
    finally:
        c.close()


def test_closed_context_json_bounds_and_safe_failures(remote, monkeypatch):
    client, srv, ident, tls, url, read, write = remote
    for context in (
        '{"principal":{"id":"user-a","kind":"user"},"tenant":"tnt_01JA8ZXXXXXXXXXXXXXXXXXXX1","cell":"cell-01","purpose":"interactive","roles":["admin"]}',
        '{"principal":{"id":"user-a","kind":"user"},"purpose":"interactive","purpose":"research"}',
    ):
        h = base64.urlsafe_b64encode(context.encode()).decode().rstrip("=")
        assert raw(url, tls, json.dumps(request()), h)[0] == 400
    for body in (
        '{"operation":"fact_read","operation":"evidence_resolve","inputs":[]}',
        json.dumps({**request(), "unexpected": "value"}),
        "[" * 14 + "0" + "]" * 14,
        "x" * 65537,
    ):
        assert raw(url, tls, body, CTX.encode())[0] == 400
    # Forwarded identity cannot replace absent authenticated Context.
    assert raw(url, tls, "{}", "", {"X-Forwarded-User": "user-a"})[0] == 400

    def outage():
        raise RuntimeError("/secret/local/path postgres://sensitive")

    monkeypatch.setattr(read, "snapshot", outage)
    code, body = raw(url, tls, json.dumps(request()), CTX.encode())
    assert code == 503 and b"secret" not in body and b"postgres" not in body


def test_real_tls_negative_peers(remote, tls_factory, tmp_path):
    client, srv, ident, tls, url, read, write = remote
    bad = [
        tls_factory("unlisted", "spiffe://miaoqia/service/unlisted"),
        tls_factory("wrong-uri", "https://wrong/peer"),
        tls_factory("no-uri"),
        tls_factory("multiple", DATA, extra_uri=ACQ),
        tls_factory("expired", DATA, days="-1"),
    ]
    for t in bad:
        with pytest.raises(Problem):
            call(RightsClient(Client(url + "/internal/v1", t, ACQ)))
    # No client certificate; actual TLS handshake cannot enter handler.
    u = urlsplit(url)
    c = http.client.HTTPSConnection(
        u.hostname, u.port, context=ssl.create_default_context(cafile=tls.ca), timeout=2
    )
    with pytest.raises((ssl.SSLError, OSError, http.client.HTTPException)):
        c.request("POST", "/internal/v1/source-rights/resolve", "{}")
        c.getresponse()
    c.close()
    # A certificate signed by a different CA is rejected by server, even with the right URI.
    key = str(tmp_path / "rogue.key")
    crt = str(tmp_path / "rogue.crt")
    subprocess.run(
        [
            "openssl",
            "req",
            "-x509",
            "-newkey",
            "rsa:2048",
            "-nodes",
            "-keyout",
            key,
            "-out",
            crt,
            "-days",
            "1",
            "-subj",
            "/CN=rogue",
            "-addext",
            "subjectAltName=URI:" + DATA,
        ],
        check=True,
        capture_output=True,
    )
    with pytest.raises(Problem):
        call(RightsClient(Client(url + "/internal/v1", TLS(crt, key, tls.ca), ACQ)))
    with pytest.raises(Problem):
        call(RightsClient(Client(url + "/internal/v1", tls, IDENTITY)))
    assert call(client)["allowed"]


def test_concurrency_receipt_no_transfer_and_deadline(remote):
    client, srv, ident, tls, url, read, write = remote

    def read_as(i):
        c = reqctx.Context(
            reqctx.Principal(f"user-{i}", "user"), CTX.tenant, CTX.cell, CTX.purpose
        )
        return call(client, context=c), c

    with ThreadPoolExecutor(6) as pool:
        results = list(pool.map(read_as, range(12)))
    assert all(v["allowed"] for v, c in results)
    a, c = results[0]
    assert not call(
        client, "confirm", {**request(), "receipt": a["receipt"]}, results[1][1]
    )["allowed"]
    assert reqctx.get() is None
    ident.sessions["sleep"] = 0.2
    short = RightsClient(Client(url + "/internal/v1", tls, ACQ, timeout=0.03))
    with pytest.raises(Problem) as e:
        call(short)
    assert e.value.status == 503


def test_missing_configuration_and_no_redirect(remote):
    client, srv, ident, tls, url, read, write = remote
    with pytest.raises(ValueError):
        TLS("", "", "").context()
    with pytest.raises(ValueError):
        Server(("localhost", 0), tls, set(), None, {})
    with pytest.raises(ValueError):
        Client("http://localhost", tls, ACQ)

    class Redirect(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_GET(self):
            self.send_response(302)
            self.send_header("Location", "https://untrusted.invalid")
            self.end_headers()

    ident.RequestHandlerClass = Redirect
    with pytest.raises(Problem) as e:
        call(client)
    assert e.value.status == 503


def test_real_acq_admin_console_assembly_and_reject_operator_role(remote, registry):
    import os
    import socket

    client, srv, ident, tls, url, read, write = remote
    dsns = registry[2]
    with socket.socket() as reservation:
        reservation.bind(("localhost", 0))
        port = reservation.getsockname()[1]
    directory = Path(tls.cert).parent
    env = {
        **os.environ,
        "MIAOQIA_ACQUISITION_RIGHTS_ENABLED": "1",
        "MIAOQIA_DEPLOY_DIR": str(Path(__file__).resolve().parents[4] / "deploy"),
        "MIAOQIA_ACQUISITION_RIGHTS_LISTEN": f"localhost:{port}",
        "MIAOQIA_ACQUISITION_RIGHTS_READER_DSN": dsns["app"],
        "MIAOQIA_INTERNAL_TLS_CERT": str(directory / "acq.crt"),
        "MIAOQIA_INTERNAL_TLS_KEY": str(directory / "acq.key"),
        "MIAOQIA_INTERNAL_TLS_CA": tls.ca,
        "MIAOQIA_IDENTITY_URL": f"https://localhost:{ident.server_port}/internal",
        "MIAOQIA_IDENTITY_PEER": IDENTITY,
        "MIAOQIA_ACQUISITION_RIGHTS_DELEGATIONS": json.dumps(
            [
                {"peer": DATA, "operation": op, "purpose": "interactive"}
                for op in ("rights.resolve", "rights.confirm", "rights.status")
            ]
        ),
    }
    child = subprocess.Popen(
        ["mq-acquisition", "--role", "acq-admin"],
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    try:
        endpoint = RightsClient(
            Client(f"https://localhost:{port}/internal/v1", tls, ACQ)
        )
        end = time.monotonic() + 4
        while True:
            try:
                assert call(endpoint)["allowed"]
                break
            except Problem:
                assert child.poll() is None
                if time.monotonic() >= end:
                    raise
                time.sleep(0.05)
    finally:
        child.terminate()
        out, err = child.communicate(timeout=5)
    assert child.returncode == 0 and not out and not err
    denied = subprocess.run(
        ["mq-acquisition", "--role", "acq-admin"],
        env={**env, "MIAOQIA_ACQUISITION_RIGHTS_READER_DSN": dsns["operator"]},
        capture_output=True,
        text=True,
        timeout=5,
    )
    assert denied.returncode == 1
    assert denied.stderr.strip() == "acquisition rights listener unavailable"
    assert dsns["operator"] not in denied.stderr


def test_client_rejects_malformed_decision_over_network(remote):
    client, srv, ident, tls, url, read, write = remote
    srv.routes["/internal/v1/source-rights/resolve"] = (
        "rights.resolve",
        lambda body, deadline: {"allowed": "yes"},
    )
    with pytest.raises(Problem) as e:
        call(client)
    assert e.value.status == 503


def test_stalled_untrusted_handshake_does_not_block_valid_peer(remote):
    import socket

    client, srv, ident, tls, url, read, write = remote
    stalled = socket.create_connection(("localhost", srv.server_port), timeout=1)
    try:
        start = time.monotonic()
        assert call(client)["allowed"]
        assert time.monotonic() - start < 2
    finally:
        stalled.close()


def test_http_budget_includes_slow_request_headers(tls_factory, running_server):
    client_tls = tls_factory("header-client", DATA)
    server_tls = tls_factory("header-server", ACQ)
    calls = []

    def route(body, deadline):
        calls.append(body)
        return {"ok": True}

    server = Server(
        ("localhost", 0), server_tls,
        {(DATA, "rights.resolve", "interactive")}, NoopAdmission(),
        {"/probe": ("rights.resolve", route)}, timeout=0.2,
    )
    url = running_server(server)
    with reqctx.use(CTX):
        assert Client(url, client_tls, ACQ).request("POST", "/probe", {}) == {"ok": True}
    assert calls == [{}]
    with client_tls.context().wrap_socket(
        socket.create_connection(("localhost", server.server_port)),
        server_hostname="localhost",
    ) as connection:
        connection.settimeout(2)
        start = time.monotonic()
        connection.sendall((
            "POST /probe HTTP/1.1\r\nHost: localhost\r\nMiaoqia-Context: "
            + CTX.encode() + "\r\nContent-Length: 2\r\nX-Slow: "
        ).encode())
        response = b""
        try:
            for _ in range(10):
                time.sleep(0.08)
                connection.sendall(b"a")
            connection.sendall(b"\r\n\r\n{}")
            response = connection.recv(4096)
        except OSError:
            pass  # Deadline closes the authenticated connection mid-header.
        assert b"200" not in response
        assert time.monotonic() - start < 1.5
    assert calls == [{}]
