"""部署域与扩展包依赖的负向检查，复用正式检查入口。"""
import importlib.util
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[3]


def load(name):
    spec = importlib.util.spec_from_file_location(name, ROOT / f"tools/dev/{name}.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


boundaries = load("check_python_boundaries")
deployment = load("gen_local")


@pytest.mark.parametrize("source", ["from miaoqia.kernel import db", "from miaoqia import kernel",
    "from miaoqia.evidence.application import probe", "import miaoqia.contracts",
    "import miaoqia_pack_other", "import sqlalchemy"])
def test_pack_cannot_bypass_sdk_and_api(source):
    assert boundaries.check_source(source, "miaoqia_pack_example", {"miaoqia.evidence.api"})


def test_pack_can_use_own_modules_sdk_api_and_standard_library():
    assert not boundaries.check_source("import re\nfrom miaoqia.sdk import Document\nfrom miaoqia.evidence.api import NAME\nfrom . import validation", "miaoqia_pack_example", {"miaoqia.evidence.api"})
    assert boundaries.check_source("from miaoqia_pack_example import validation", "miaoqia.sdk", set(), sdk=True)


@pytest.mark.parametrize("scope,cell", [(None, "cell-01"), ("platform", "cell-01"), ("tenant", ""), ("tenant", "bad.cell")])
def test_cross_domain_roles_must_select_scope_and_cell(tmp_path, scope, cell):
    import yaml
    role = {"subsystems": ["search"], "data_scopes": {} if scope is None else {"search": scope}, "cell": cell}
    path = tmp_path / "roles.yaml"
    path.write_text(yaml.safe_dump({"roles": {"test": role}}))
    assert deployment.check_roles({"platform": path})


def test_registered_roles_and_python_imports_are_consistent():
    assert not boundaries.check()
    assert not deployment.check_roles()


def test_public_instance_cannot_share_a_tenant_cell(tmp_path):
    path = tmp_path / "roles.yaml"
    path.write_text("roles:\n  test:\n    subsystems: [search]\n    data_scopes: {search: public}\n    cell: cell-01\n")
    assert deployment.check_roles({"platform": path})


def test_pack_registration_checks_the_declared_function(sandbox):
    from mqc.packs import check_information_pack
    from mqc.repo import Repo
    import yaml
    directory = sandbox / "packs/information_domains/procurement"
    manifest = directory / "pack.yaml"
    data = yaml.safe_load(manifest.read_text())
    data["pack"]["provides"]["semantic_validator"] = "miaoqia_pack_procurement.validation:missing"
    manifest.write_text(yaml.safe_dump(data))
    report = check_information_pack(Repo(sandbox), directory)
    assert any("校验函数不存在" in item for item in report.errors)


def test_ci_and_runtime_use_the_same_instance_contract():
    import json
    for case in json.loads((ROOT / 'tests/contract/instance-scopes.json').read_text()):
        if case['valid']:
            assert deployment._scope_module.role_scopes(case['spec'], case['registry'], unit=case['unit'])
        else:
            with pytest.raises(ValueError):
                deployment._scope_module.role_scopes(case['spec'], case['registry'], unit=case['unit'])


@pytest.mark.parametrize('source', ['from miaoqia import miaoqia_pack_example', 'from custom_domain import validation'])
def test_shared_code_cannot_reverse_depend_on_pack_roots(source):
    assert boundaries.check_source(source, 'miaoqia.sdk', set(), sdk=True, pack_roots={'custom_domain'})


@pytest.mark.parametrize('entry', [None, '', 'bad/path:validate'])
def test_invalid_validator_registration_cannot_be_treated_as_absent(sandbox, entry):
    from mqc.packs import check_information_pack
    from mqc.repo import Repo
    import yaml
    directory = sandbox / 'packs/information_domains/credit'
    manifest = directory / 'pack.yaml'
    data = yaml.safe_load(manifest.read_text())
    data['pack']['provides']['semantic_validator'] = entry
    manifest.write_text(yaml.safe_dump(data))
    report = check_information_pack(Repo(sandbox), directory)
    assert any('入口格式无效' in item for item in report.errors)


def test_pack_kind_without_registered_extension_targets_fails(tmp_path):
    import yaml
    (tmp_path / 'deploy').mkdir()
    (tmp_path / 'deploy/subsystems.yaml').write_text(yaml.safe_dump({'subsystems': {
        name: {'language': 'python'} for name in ('acquisition', 'evidence', 'domain-intelligence', 'entity-graph')}}))
    pack = tmp_path / 'packs/information_domains/example'
    (pack / 'src').mkdir(parents=True)
    (pack / 'pack.yaml').write_text(yaml.safe_dump({'pack': {'kind': 'unknown_kind'}}))
    assert any('未登记包种类' in error for error in boundaries.check(tmp_path))
    (pack / 'pack.yaml').write_text(yaml.safe_dump({'pack': {'kind': 'information_domain'}}))
    assert not boundaries.check(tmp_path)
