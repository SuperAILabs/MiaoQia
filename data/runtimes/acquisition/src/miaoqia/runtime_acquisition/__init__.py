"""acquisition 部署单元入口：mq-acquisition migrate | mq-acquisition --role <角色>（角色见 deploy/acquisition/roles.yaml）。"""

from miaoqia.kernel import runtime


def modules() -> list[runtime.Module]:
    from miaoqia.acquisition import module as acquisition_module
    from .bindings import operator_callable

    def browser():
        return operator_callable("MIAOQIA_ACQUISITION_BROWSER_RENDERER")()

    return [acquisition_module(browser_factory=browser)]


def main() -> None:
    import os
    import sys

    if (
        sys.argv[1:] in (["--role", "acq-admin"], ["--role=acq-admin"])
        and os.environ.get("MIAOQIA_ACQUISITION_RIGHTS_ENABLED") == "1"
    ):
        from .rights import serve

        try:
            runtime.prepare_role("acquisition", "acq-admin", modules())
            serve()
        except Exception:
            raise SystemExit("acquisition rights listener unavailable") from None
    else:
        runtime.main("acquisition", modules())
