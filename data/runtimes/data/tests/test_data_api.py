from __future__ import annotations

import json
import threading
import urllib.request
from datetime import datetime, timezone

from miaoqia.domain_intelligence.api import Basis, FactRead, Page
from miaoqia.evidence.api import ResolvedEvidence
from miaoqia.kernel import reqctx
from miaoqia.runtime_data.data_api import DataAPIServer


class Facts:
    def get_fact(self, fact_id, basis, *, version=None):
        return FactRead(basis, {"id": fact_id, "version": version or 1})

    def get_fact_history(self, fact_id, basis, *, after=0, limit=50):
        return Page(basis, ({"id": fact_id, "version": after + 1},), None)

    def query_facts(self, info_domain, object_type, basis, *, after="", limit=50):
        return Page(basis, ({"info_domain": info_domain, "object_type": object_type},), None)

    def bid_result_units(self, fact_id, basis, *, version=None):
        return Page(basis, ({"key": fact_id, "version": version or 1},), None)


class Evidence:
    def resolve(self, evidence_ref):
        return ResolvedEvidence(id=evidence_ref, display_level="excerpt", snippet="原文")


def test_data_api_requires_context_and_dispatches_reads(monkeypatch):
    monkeypatch.setenv("MIAOQIA_ENV", "development")
    server = DataAPIServer(("127.0.0.1", 0), Facts(), Evidence())
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        url = f"http://127.0.0.1:{server.server_port}/internal/v1/data/bid-result-units"
        body = json.dumps({"fact_id": "ntc_abc", "basis": {"view": "current"}, "version": 1}).encode()
        request = urllib.request.Request(url, data=body, method="POST", headers={"Content-Type": "application/json"})
        try:
            urllib.request.urlopen(request)
        except urllib.error.HTTPError as error:
            assert error.code == 401
        else:
            raise AssertionError("missing context accepted")

        context = reqctx.Context(principal=reqctx.Principal("svc:detection"), tenant="tnt_a", cell="cell-01", purpose="signal")
        request.add_header("Miaoqia-Context", context.encode())
        with urllib.request.urlopen(request) as response:
            payload = json.load(response)
        assert payload["items"] == [{"key": "ntc_abc", "version": 1}]
        assert payload["basis"] == {"view": "current", "recorded_cutoff": None}
    finally:
        server.shutdown()
        server.server_close()
        thread.join(2)


def test_data_api_healthz():
    server = DataAPIServer(("127.0.0.1", 0), Facts(), Evidence())
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{server.server_port}/healthz") as response:
            assert response.status == 200
            assert json.load(response) == {"status": "ok"}
    finally:
        server.shutdown()
        server.server_close()
        thread.join(2)


def test_data_api_serializes_known_at_cutoff_and_evidence():
    server = DataAPIServer(("127.0.0.1", 0), Facts(), Evidence())
    context = reqctx.Context(principal=reqctx.Principal("svc:gateway"), tenant="tnt_a", cell="cell-01", purpose="object_page")
    cutoff = datetime(2026, 10, 28, tzinfo=timezone.utc).isoformat().replace("+00:00", "Z")
    result = server.dispatch("/internal/v1/data/get-fact", {"fact_id": "ntc_abc", "basis": {"view": "known_at", "recorded_cutoff": cutoff}})
    assert result["basis"]["recorded_cutoff"] == cutoff
    with reqctx.use(context):
        evidence = server.dispatch("/internal/v1/data/resolve-evidence", {"evidence_ref": "ev_abc"})
    assert evidence["display_level"] == "excerpt"
    server.server_close()


def test_production_listener_rejects_plain_socket(monkeypatch):
    monkeypatch.setenv("MIAOQIA_ENV", "production")

    class PlainHandler:
        connection = object()

    assert DataAPIServer.peer_allowed(PlainHandler()) is False
