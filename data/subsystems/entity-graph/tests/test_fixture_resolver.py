"""纯函数控制；这些内存证明句柄不是 Pipeline/固定事实的集成证明。"""
from dataclasses import replace
from pathlib import Path
import json

import pytest

from miaoqia.entity_graph import fixture_resolver
from miaoqia.entity_graph.api import FixtureFieldProof, FixtureOrganization, FixtureRegistry

ROOT = Path(__file__).resolve().parents[4]


@pytest.fixture
def registry():
    manifest = json.loads((ROOT / 'packs/information_domains/organizations/corpus/r2-fixture-registry-v1.json').read_text())
    records = []
    for i, r in enumerate(manifest['data']['records'], 1):
        proofs = tuple(FixtureFieldProof('ogr_' + '0' * 25 + str(i), 1, path, 'ev_' + '0' * 25 + str(j))
                       for j, path in enumerate(['/name/value', *[f'/former_names/{n}/value' for n in range(len(r['aliases']))]], 1))
        records.append(FixtureOrganization(r['fixture_subject_key'], r['org_id'], r['name'], tuple(r['aliases']), proofs, 1.0))
    return FixtureRegistry('fixture:r2-org', 'fixture-r2-org-registry', '0.1.0', 'sha256:' + '0' * 64, tuple(records))


def test_exact_names_aliases_ambiguity_unknown_and_all_proofs(registry):
    resolver = fixture_resolver(registry)
    a, b, c, d = registry.records
    for name, expected in [(a.name, a.org_id), (b.aliases[0], b.org_id), ('  ' + a.name + '  ', a.org_id)]:
        result = resolver.resolve(name)
        assert result.status == 'resolved' and result.selected_id == expected
        assert result.fixture_only and result.namespace == 'fixture:r2-org'
        assert result.reference_hash == registry.reference_hash
        assert result.consulted_proofs == tuple(p for r in registry.records for p in r.proofs)
    for name, ids in [(b.name, (b.org_id, d.org_id)), (b.aliases[1], (b.org_id, c.org_id))]:
        result = resolver.resolve(name)
        assert result.status == 'ambiguous' and result.candidates == ids and result.selected_id is None
    for name in ['【夹具】未登记组织有限公司', '青岚采购甲有限公司', a.name.replace('青岚', '青 岚'), a.name + '。', '宣城市宣州区国投置业有限公司', '']:
        result = resolver.resolve(name)
        assert result.status == 'unknown' and not result.candidates and result.selected_id is None
        assert len(result.consulted_proofs) == sum(len(r.proofs) for r in registry.records)


def test_strong_key_conflict_never_falls_back(registry):
    resolver = fixture_resolver(registry)
    a, b, c, d = registry.records
    result = resolver.resolve(b.aliases[0], fixture_subject_key=b.fixture_subject_key)
    assert result.status == 'resolved' and result.selected_id == b.org_id
    result = resolver.resolve(c.name, fixture_subject_key=b.fixture_subject_key)
    assert result.status == 'conflict' and result.selected_id is None and result.candidates == (b.org_id, c.org_id)
    result = resolver.resolve(a.name, fixture_subject_key='fixture:r2-org:absent')
    assert result.status == 'unknown' and result.selected_id is None
    assert resolver.resolve('【夹具】未知', fixture_subject_key=b.fixture_subject_key).status == 'conflict'
    assert resolver.resolve(b.name, fixture_subject_key=b.fixture_subject_key).selected_id == b.org_id


def test_reorder_and_new_name_version_keep_explicit_id(registry):
    first = fixture_resolver(registry).resolve(registry.records[1].aliases[0])
    assert fixture_resolver(replace(registry, records=tuple(reversed(registry.records)))).resolve(registry.records[1].aliases[0]) == first
    renamed = replace(registry.records[0], name='【夹具】青岚采购甲新名称有限公司')
    changed = replace(registry, reference_version='0.2.0', reference_hash='sha256:' + '1' * 64, records=(renamed, *registry.records[1:]))
    result = fixture_resolver(changed).resolve(renamed.name)
    assert result.selected_id == renamed.org_id and result.reference_version == '0.2.0'
    assert fixture_resolver(registry).resolve(renamed.name).status == 'unknown'


@pytest.mark.parametrize('change', ['key', 'id', 'proof', 'confidence', 'marker'])
def test_closed_projection_rejects_invalid_inputs(registry, change):
    a, b, *rest = registry.records
    if change == 'key': b = replace(b, fixture_subject_key=a.fixture_subject_key)
    if change == 'id': b = replace(b, org_id=a.org_id)
    if change == 'proof': b = replace(b, proofs=b.proofs[:-1])
    if change == 'confidence': b = replace(b, confidence=0.89)
    if change == 'marker': b = replace(b, name='真实组织名称')
    with pytest.raises(ValueError): fixture_resolver(replace(registry, records=(a, b, *rest)))
