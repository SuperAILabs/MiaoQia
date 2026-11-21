"""data 部署单元入口及 data-api 的最小可达内部 listener。"""
import os
import signal
import sys
import threading

from miaoqia.kernel import runtime
from miaoqia.kernel.internal_http import Client, Identity, Server, TLS
from miaoqia.kernel.internal_http import delegations as parse_delegations


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


# Seconds; well below the listener's per-request budget (internal_http.Server timeout, at most 5).
POOL_TIMEOUT = 1.0


def _tls():
    return TLS(*(os.environ.get("MIAOQIA_INTERNAL_TLS_" + k, "") for k in ("CERT", "KEY", "CA")))


def read_routes(dom, evd, store, rights, identity, *, bind_public_organizations=None):
    from miaoqia import evidence
    from miaoqia.domain_intelligence import queries
    from miaoqia.domain_intelligence.application.read_transport import ReadAuthorization, admitted_display, routes

    provenance = evidence.source_requirements(evd)
    facts = queries(dom, provenance=provenance)
    resolver = evidence.resolver(evd, store, authorize=admitted_display)

    def evidence_groups(ref, deadline):
        closure = provenance.evidence(ref, deadline=deadline)
        return [source.dto() for source in provenance.resolve(closure, deadline=deadline)]

    return routes(facts, resolver, admit=ReadAuthorization(rights, identity).prepare,
                  evidence_groups=evidence_groups, notice_chunks=evidence.notice_chunks(evd, store),
                  bind_public_organizations=bind_public_organizations)


def serve():
    """Run the data-api listener; missing remote dependencies fail closed."""
    from miaoqia.acquisition.api.remote import RightsClient
    from miaoqia.kernel.objectstore import open_object_store
    from .organization_read import configured_public_organizations

    dom_dsn = os.environ.get(runtime.env_key("domain-intelligence", "DB_DSN"))
    evd_dsn = os.environ.get(runtime.env_key("evidence", "DB_DSN"))
    root = os.environ.get("MIAOQIA_EVIDENCE_OBJECT_STORE_DIR")
    rights_url = os.environ.get("MIAOQIA_ACQUISITION_RIGHTS_URL") or os.environ.get("MIAOQIA_ACQUISITION_URL")
    listen = os.environ.get("MIAOQIA_DATA_LISTEN")
    if not dom_dsn or not evd_dsn or not root or not rights_url or not listen:
        raise SystemExit("data-api listener configuration unavailable")
    from miaoqia.kernel import db
    # Pool checkout must fit inside one request budget: db.tx refuses a request
    # whose remaining deadline is shorter than the checkout bound.
    dom, evd = db.Database(dom_dsn, pool_timeout=POOL_TIMEOUT), db.Database(evd_dsn, pool_timeout=POOL_TIMEOUT)
    try:
        identity = Identity(Client(os.environ["MIAOQIA_IDENTITY_URL"], _tls(), os.environ["MIAOQIA_IDENTITY_PEER"]))
        rights = RightsClient(Client(rights_url, _tls(), os.environ["MIAOQIA_ACQUISITION_PEER"]))
        delegations = _delegations(os.environ.get("MIAOQIA_DATA_DELEGATIONS", ""))
        host, port = listen.rsplit(":", 1)
        store = open_object_store(root)
        with configured_public_organizations(os.environ, dom, evd, store, evd_dsn, pool_timeout=POOL_TIMEOUT) as bind:
            server = Server((host, int(port)), _tls(), delegations, identity,
                            read_routes(dom, evd, store, rights, identity, bind_public_organizations=bind))
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
    from miaoqia.domain_intelligence.application.read_transport import RIGHTS_OPERATIONS

    return parse_delegations(json.loads(raw), RIGHTS_OPERATIONS)


def main() -> None:
    if os.environ.get("MIAOQIA_DATA_API_LISTENER") == "1" and sys.argv[1:] in (["--role", "data-api"], ["--role=data-api"]):
        serve()
    else:
        runtime.main("data", modules())
