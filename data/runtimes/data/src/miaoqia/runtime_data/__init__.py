"""data 部署单元入口：mq-data migrate | mq-data --role <角色>（角色见 deploy/data/roles.yaml）。"""
import os

from miaoqia.kernel import runtime


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
    runtime.main("data", modules())
