"""data 部署单元入口：mq-data migrate | mq-data --role <角色>（角色见 deploy/data/roles.yaml）。"""
import argparse
import os

from miaoqia.kernel import db, runtime, tracing


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


def main() -> None:
    parser = argparse.ArgumentParser(prog="mq-data")
    parser.add_argument("command", nargs="?", choices=["migrate"])
    parser.add_argument("--role")
    args = parser.parse_args()
    if args.role == "data-api" and args.command is None:
        run_data_api()
        return
    # Keep migrate and all worker roles on the shared runtime path.
    runtime.main("data", modules())


def run_data_api() -> None:
    """Start the data-api query listener for the public data instance."""
    from miaoqia.evidence import resolver
    from miaoqia.evidence.infrastructure.objectstore import FilesystemObjectStore
    from miaoqia.domain_intelligence import queries
    from .data_api import serve

    evidence_dsn = os.environ["MIAOQIA_EVIDENCE_DB_DSN"]
    domain_dsn = os.environ["MIAOQIA_DOMAIN_INTELLIGENCE_DB_DSN"]
    evidence_db = db.Database(evidence_dsn)
    domain_db = db.Database(domain_dsn)
    rights = _rights_policy()

    def display(tags, purpose):
        if not rights(tags, purpose):
            return "none"
        return "full_text" if purpose == "object_page" else "excerpt"

    facts = queries(domain_db, allowed=rights)
    evidence_reader = resolver(evidence_db, FilesystemObjectStore(
        os.environ.get("MIAOQIA_EVIDENCE_OBJECT_STORE_DIR", "/var/lib/miaoqia/evidence")), display=display)
    tracing.setup("data", "data-api")
    try:
        serve(os.environ.get("MIAOQIA_DATA_API_ADDR", ":8094"), facts, evidence_reader,
              evidence_object=facts.object_id_for_evidence)
    finally:
        evidence_db.close()
        domain_db.close()
        tracing.shutdown()


def _rights_policy():
    configured = {item.strip() for item in os.environ.get("MIAOQIA_DATA_PUBLIC_RIGHTS", "").split(",") if item.strip()}

    def allowed(tags, _purpose):
        if configured:
            return any(tag in configured or "*" in configured for tag in tags)
        if os.environ.get("MIAOQIA_ENV", "") == "development":
            return any(tag == "public-official-review" or tag.startswith("corpus:") for tag in tags)
        return False

    return allowed
