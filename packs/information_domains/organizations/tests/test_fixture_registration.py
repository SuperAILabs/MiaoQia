import copy
import json
from pathlib import Path

import pytest

from miaoqia.sdk.corpus import _load_document, check
from miaoqia.sdk import ExtractionContext
from miaoqia.sdk.validation import validate_extraction
from miaoqia_pack_organizations.fixtures import FixtureRegistrationExtractor, load_registry_manifest, MANIFEST
from miaoqia_pack_organizations.validation import validate
import yaml

ROOT = Path(__file__).resolve().parents[4]
CORPUS = MANIFEST.parent


def test_four_automatic_fixture_records_and_existing_manual_corpus(monkeypatch):
    monkeypatch.chdir(ROOT)
    assert check(CORPUS, 'miaoqia_pack_organizations.fixtures:FixtureRegistrationExtractor', 'domains/organization/registration.schema.json', 1) == 0
    for schema in ['registration', 'change', 'abnormal']:
        assert check(CORPUS, None, f'domains/organization/{schema}.schema.json', None, reviewed=True) == 0
    manifest = load_registry_manifest()
    extractor = FixtureRegistrationExtractor()
    for record in manifest['data']['records']:
        sample = CORPUS / record['sample']
        doc = _load_document(sample, yaml.safe_load((sample / 'meta.yaml').read_text()))
        assert not extractor.applies(doc, {'source_id': 'official-qzygjy'})
        assert extractor.applies(doc, {'source_id': 'fixture-r2-org'})
        ctx = ExtractionContext(doc, 'org.fixture-registration@0.1.0')
        facts = extractor.extract(doc, ctx).facts
        assert not validate_extraction(doc, facts, ctx.evidence, semantic_validator=validate)
        assert 'org_id' not in facts[0]['subject'] and 'uscc' not in facts[0]
        assert facts[0]['record_scope'] == 'fixture'
        bad = copy.deepcopy(facts); del bad[0]['record_scope']
        assert any('record_scope' in e for e in validate(doc, bad, ctx.evidence))
        bad = copy.deepcopy(facts); bad[0]['uscc'] = 'invented'
        assert any('真实登记属性' in e for e in validate(doc, bad, ctx.evidence))


def test_registry_manifest_duplicate_keys_ids_and_unknown_data_fail(tmp_path):
    manifest = load_registry_manifest()
    for field in ('fixture_subject_key', 'org_id', 'sample'):
        bad = copy.deepcopy(manifest)
        bad['data']['records'][1][field] = bad['data']['records'][0][field]
        path = tmp_path / 'manifest.json'; path.write_text(json.dumps(bad))
        with pytest.raises(ValueError): load_registry_manifest(path)
    bad = copy.deepcopy(manifest); bad['data']['records'][0]['uscc'] = 'invented'
    path.write_text(json.dumps(bad))
    with pytest.raises(ValueError, match='不闭合'): load_registry_manifest(path)


def test_generated_optional_scope_does_not_mark_existing_real_records():
    from miaoqia.contracts import models
    from pydantic import ValidationError
    base = yaml.safe_load((ROOT / 'contracts/tests/organization.registration.yaml').read_text())['base']
    model = models.OrgRegistration.model_validate(base)
    assert model.record_scope is None and 'record_scope' not in model.model_dump(exclude_none=True)
    with pytest.raises(ValidationError):
        models.OrgRegistration.model_validate({**base, 'record_scope': 'production'})
