"""acquisition 部署单元入口：mq-acquisition migrate | mq-acquisition --role <角色>（角色见 deploy/acquisition/roles.yaml）。"""
from miaoqia.kernel import runtime


def modules() -> list[runtime.Module]:
    from miaoqia.acquisition import module as acquisition_module
    return [acquisition_module()]


def main() -> None:
    runtime.main("acquisition", modules())
