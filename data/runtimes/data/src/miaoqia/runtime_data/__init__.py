"""data 部署单元入口及 data-api 的最小可达内部 listener。"""
import os
import signal
import sys
import threading

from miaoqia.kernel import runtime
from miaoqia.kernel.internal_http import Client, Identity, Server, TLS


def modules() -> list[runtime.Module]:
    from miaoqia import evidence
    from miaoqia.evidence import module as evidence_module
    from miaoqia.domain_intelligence import module as domain_intelligence_module
    from miaoqia.entity_graph import module as entity_graph_module

    def extractions():
        # 数据单元内的本地绑定：domain-intelligence 以 evidence 的数据库账号只读解析修订。
        key = runtime.env_key("evidence", "DB_DSN")
        if not os.environ.get(key):
            raise SystemExit(f"未配置 {key}（domain-intelligence 读取解析修订）")
        return evidence.extractions(os.environ[key])

    return [evidence_module(), domain_intelligence_module(extractions), entity_graph_module()]


def _tls():
    return TLS(*(os.environ.get("MIAOQIA_INTERNAL_TLS_" + k, "") for k in ("CERT", "KEY", "CA")))


def _groups(facts, provenance, body, deadline):
    """Resolve one fixed fact handle into owner-verified source groups."""
    if not isinstance(body, dict) or not isinstance(body.get("version"), int):
        return []
    closure = facts.requirements(body.get("fact_id"), body["version"])
    return [source.dto() for source in provenance.resolve(closure)]


def _evidence_groups(provenance, body, deadline):
    if not isinstance(body, dict) or not isinstance(body.get("evidence_ref"), str):
        return []
    return [source.dto() for source in provenance.resolve(provenance.evidence(body["evidence_ref"]))]


def serve():
    """Run the data-api listener; missing remote dependencies fail closed."""
    from miaoqia import evidence
    from miaoqia.acquisition.api.remote import RightsClient
    from miaoqia.domain_intelligence import queries
    from miaoqia.domain_intelligence.application.read_transport import ReadAuthorization, routes
    from miaoqia.evidence.infrastructure.objectstore import FilesystemObjectStore

    dom_dsn = os.environ.get(runtime.env_key("domain-intelligence", "DB_DSN"))
    evd_dsn = os.environ.get(runtime.env_key("evidence", "DB_DSN"))
    root = os.environ.get("MIAOQIA_EVIDENCE_OBJECT_STORE_DIR")
    rights_url = os.environ.get("MIAOQIA_ACQUISITION_RIGHTS_URL") or os.environ.get("MIAOQIA_ACQUISITION_URL")
    listen = os.environ.get("MIAOQIA_DATA_LISTEN")
    if not dom_dsn or not evd_dsn or not root or not rights_url or not listen:
        raise SystemExit("data-api listener configuration unavailable")
    from miaoqia.kernel import db
    dom, evd = db.Database(dom_dsn), db.Database(evd_dsn)
    try:
        identity = Identity(Client(os.environ["MIAOQIA_IDENTITY_URL"], _tls(), os.environ["MIAOQIA_IDENTITY_PEER"]))
        rights = RightsClient(Client(rights_url, _tls(), os.environ["MIAOQIA_ACQUISITION_PEER"]))
        provenance = evidence.source_requirements(evd)
        # Route admission is the authoritative d0 gate; query callbacks still
        # require a callback and therefore only accept already-admitted groups.
        allowed = lambda groups, purpose: True
        facts = queries(dom, authorize=allowed, provenance=provenance)
        resolver = evidence.resolver(evd, FilesystemObjectStore(root), authorize=allowed)
        auth = ReadAuthorization(rights, identity)
        collector = {
            "data.get_fact": lambda b, d: _groups(facts, provenance, b, d),
            "data.fact_history": lambda b, d: [],
            "data.query_facts": lambda b, d: [],
            "data.bid_result_units": lambda b, d: _groups(facts, provenance, b, d),
            "data.resolve_evidence": lambda b, d: _evidence_groups(provenance, b, d),
        }
        delegations = _delegations(os.environ.get("MIAOQIA_DATA_DELEGATIONS", ""))
        host, port = listen.rsplit(":", 1)
        server = Server((host, int(port)), _tls(), delegations, identity,
                        routes(facts, resolver, admit=auth.prepare,
                               before_send=lambda op, value, hook: hook,
                               collectors=collector))
        stop = threading.Event()
        signal.signal(signal.SIGTERM, lambda *_: stop.set())
        signal.signal(signal.SIGINT, lambda *_: stop.set())
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        try:
            stop.wait()
        finally:
            server.shutdown(); server.server_close(); worker.join(5)
    finally:
        dom.close(); evd.close()


def _delegations(raw):
    import json
    value = json.loads(raw)
    if not isinstance(value, list) or not value:
        raise ValueError("explicit delegation list required")
    result = []
    for item in value:
        if not isinstance(item, dict) or set(item) != {"peer", "operation", "purpose"}:
            raise ValueError("invalid delegation")
        result.append((item["peer"], item["operation"], item["purpose"]))
    return result


def main() -> None:
    if os.environ.get("MIAOQIA_DATA_API_LISTENER") == "1" and sys.argv[1:] in (["--role", "data-api"], ["--role=data-api"]):
        serve()
    else:
        runtime.main("data", modules())
