"""data 部署单元入口：mq-data migrate | mq-data --role <角色>（角色见 deploy/data/roles.yaml）。"""
from miaoqia.kernel import runtime


def modules() -> list[runtime.Module]:
    from miaoqia.evidence import module as evidence_module
    from miaoqia.domain_intelligence import module as domain_intelligence_module
    from miaoqia.entity_graph import module as entity_graph_module
    return [evidence_module(), domain_intelligence_module(), entity_graph_module()]


def main() -> None:
    runtime.main("data", modules())
