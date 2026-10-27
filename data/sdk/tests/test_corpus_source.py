"""checker/replay 共享来源边界，不能用旗标伪造公开来源。"""
import copy
import hashlib
import json
import shutil
from pathlib import Path

import pytest
import yaml

from miaoqia.sdk.corpus import _load_document
from miaoqia.sdk.corpus_source import corpus_source, fixture_manifest

CORPUS = Path(__file__).resolve().parents[3] / 'packs/information_domains/organizations/corpus'


@pytest.fixture
def sample(tmp_path):
    shutil.copy(CORPUS / 'r2-fixture-registry-v1.json', tmp_path)
    path = tmp_path / 'fixture-r2-org-a'
    shutil.copytree(CORPUS / path.name, path)
    return path


def read(sample):
    return yaml.safe_load((sample / 'meta.yaml').read_text()), (sample / 'raw.html').read_bytes()


def test_fixture_is_url_free_and_checker_uses_same_validation(sample):
    meta, raw = read(sample)
    source = corpus_source(sample, meta, raw, for_replay=True)
    assert source.source_url is None and source.source_key == 'fixture:r2-org:fixture-r2-org-a'
    assert source.observed_at.tzinfo and 'source_url' not in _load_document(sample, meta).data
    assert 'fetched_at' not in meta


@pytest.mark.parametrize('patch', [
    {'artifact_origin': 'official'}, {'artifact_origin': 'public_http'}, {'fixture_only': False},
    {'fixture_only': 'true'}, {'source_id': 'official-qzygjy'}, {'source_key': 'fixture:r2-org:fixture-r2-org-b'},
    {'source_url': 'https://official.example/1'}, {'source_url': None}, {'fetched_at': '2026-01-01'},
    {'observed_at': '2026-01-01'}, {'rights_tag': 'public-official-review'},
    {'fixture_manifest': '../other.json'}, {'fixture_manifest_sha256': '0' * 64}, {'arbitrary': True}, {'raw_sha256': '0' * 64},
])
def test_metadata_forgery_rejected_by_both_modes(sample, patch):
    meta, raw = read(sample)
    for mode in (False, True):
        with pytest.raises(ValueError):
            corpus_source(sample, {**meta, **patch}, raw, for_replay=mode)
    with pytest.raises(ValueError):
        _load_document(sample, {**meta, **patch})


def test_raw_change_and_missing_visible_marker_rejected(sample):
    meta, raw = read(sample)
    with pytest.raises(ValueError, match='哈希'):
        corpus_source(sample, meta, raw + b'changed')
    # 即使本地 manifest 的哈希一并重建，原文仍必须声明合成性质。
    raw = raw.replace('合成测试夹具；非真实来源；不得用于生产核验'.encode(), b'ordinary')
    digest = hashlib.sha256(raw).hexdigest()
    manifest_path = sample.parent / meta['fixture_manifest']
    manifest = json.loads(manifest_path.read_text())
    manifest['samples'][sample.name]['raw_sha256'] = digest
    manifest_path.write_text(json.dumps(manifest))
    with pytest.raises(ValueError, match='可见合成标记'):
        corpus_source(sample, {**meta, 'raw_sha256': digest, 'fixture_manifest_sha256': hashlib.sha256(manifest_path.read_bytes()).hexdigest()}, raw)


def test_manifest_closed_and_unbound_sample_rejected(sample):
    meta, raw = read(sample)
    path = sample.parent / meta['fixture_manifest']
    original = path.read_text()
    manifest = json.loads(original)
    manifest['unexpected'] = True
    path.write_text(json.dumps(manifest))
    with pytest.raises(ValueError, match='不闭合'):
        fixture_manifest(path)
    path.write_text(original.replace('"version": "0.1.0"', '"version":"0.1.0", "version":"0.1.0"'))
    with pytest.raises(ValueError, match='重复'):
        fixture_manifest(path)
    path.write_text(original)
    renamed = sample.with_name('fixture-r2-org-forged')
    sample.rename(renamed)
    with pytest.raises(ValueError, match='manifest 不一致'):
        corpus_source(renamed, meta, raw)


def test_legacy_http_preserves_real_key_and_timezone():
    meta = {'source_id': 'official-qzygjy', 'source_url': 'https://example.org/notice',
            'rights_tag': 'public-official-review', 'fetched_at': '2026-10-26T01:00:00'}
    source = corpus_source(Path('ordinary'), meta, b'<p>ordinary</p>', for_replay=True)
    assert source.source_key == meta['source_url'] == source.source_url
    assert source.observed_at.utcoffset().total_seconds() == 28800
    assert corpus_source(Path('ordinary'), {}, b'html').source_url is None
    with pytest.raises(ValueError):
        corpus_source(Path('ordinary'), {}, b'html', for_replay=True)


def test_known_legacy_judgment_kind_keeps_existing_http_corpus():
    source = CORPUS.parents[1] / 'procurement/corpus/official-failed-acceptance-judgment-20261014'
    meta = yaml.safe_load((source / 'meta.yaml').read_text())
    raw = (source / 'raw.html').read_bytes()
    result = corpus_source(source, meta, raw, for_replay=True)
    assert meta['source_kind'] == 'court_original_judgment' and 'artifact_origin' not in meta
    assert result.artifact_origin == 'public_http' and result.source_key == meta['source_url']


def test_crcc18_provided_notice_records_platform_entrypoint_and_digest():
    sample = CORPUS.parents[1] / 'procurement/corpus/crcc18-g228-concrete-202610'
    meta = yaml.safe_load((sample / 'meta.yaml').read_text())
    raw = (sample / 'raw.html').read_bytes()
    result = corpus_source(sample, meta, raw, for_replay=True)
    assert result.source_url == 'https://www.crccep.cn'
    assert meta['rights_tag'] == 'public-tender'
    assert meta.get('source_id') == 'crcc18-g228-concrete-202610'


def test_generic_envelope_does_not_encode_organization_namespace_or_tags(tmp_path):
    from miaoqia.sdk.corpus_source import SYNTHETIC_MARKER
    sample = tmp_path / 'toy-sample'; sample.mkdir()
    raw = f'<p>{SYNTHETIC_MARKER}</p><p>unrelated fixture payload</p>'.encode()
    (sample / 'raw.html').write_bytes(raw)
    binding = {'source_id': 'toy-source', 'source_key': 'fixture:toy:sample', 'rights_tag': 'toy-exact-tag',
               'raw_sha256': hashlib.sha256(raw).hexdigest()}
    path = tmp_path / 'toy-manifest.json'
    path.write_text(json.dumps({'version': '2.1.0', 'namespace': 'fixture:toy', 'fixture_only': True,
                               'samples': {sample.name: binding}, 'data': {'owner_data': []}}))
    meta = {'artifact_origin': 'synthetic_fixture', **binding, 'fixture_only': True,
            'fixture_manifest': path.name, 'fixture_manifest_sha256': hashlib.sha256(path.read_bytes()).hexdigest(),
            'observed_at': '2026-10-26T01:00:00Z', 'schema': 'toy.schema.json'}
    source = corpus_source(sample, meta, raw, for_replay=True)
    assert source.source_id == 'toy-source' and source.rights_tag == 'toy-exact-tag' and source.source_url is None


def test_synthetic_content_cannot_be_relabelled_public_http_after_flags_removed(sample):
    meta, raw = read(sample)
    public = {key: value for key, value in meta.items()
              if key not in {'fixture_only', 'fixture_manifest', 'fixture_manifest_sha256', 'observed_at'}}
    public.update(artifact_origin='public_http', source_id='ordinary-public-source',
                  source_url='https://example.org/document', fetched_at='2026-10-26T01:00:00Z')
    for mode in (False, True):
        with pytest.raises(ValueError, match='夹具不能伪装公开来源'):
            corpus_source(sample, public, raw, for_replay=mode)
    with pytest.raises(ValueError, match='夹具不能伪装公开来源'):
        _load_document(sample, public)


@pytest.mark.parametrize('body', [
    '%PDF-1.7\n<p>合成测试夹具；非真实来源；不得用于生产核验</p>',
    '<p>&#21512;成测试夹具；非真实来源；不得用于生产核验</p>',
    '<p>合成<span>测试夹具；非真实来源；不得用于生产核验</span></p>',
    '<p>声明：合成测试夹具；非真实来源；不得用于生产核验（仅供测试）</p>',
])
def test_normalized_visible_declaration_rejects_relabelled_http(sample, body):
    raw = body.encode('utf-8')
    (sample / 'raw.html').write_bytes(raw)
    meta = {'artifact_origin': 'public_http', 'source_id': 'ordinary-public-source',
            'source_url': 'https://example.org/document', 'fetched_at': '2026-10-26T01:00:00Z',
            'rights_tag': 'review-only', 'raw_sha256': hashlib.sha256(raw).hexdigest()}
    for mode in (False, True):
        with pytest.raises(ValueError, match='夹具不能伪装公开来源'):
            corpus_source(sample, meta, raw, for_replay=mode)
    with pytest.raises(ValueError, match='夹具不能伪装公开来源'):
        _load_document(sample, meta)


@pytest.mark.parametrize('body', [
    '<!-- 合成测试夹具；非真实来源；不得用于生产核验 --><p>ordinary</p>',
    '<script>合成测试夹具；非真实来源；不得用于生产核验</script><p>ordinary</p>',
    '<style>合成测试夹具；非真实来源；不得用于生产核验</style><p>ordinary</p>',
    '<noscript>合成测试夹具；非真实来源；不得用于生产核验</noscript><p>ordinary</p>',
])
def test_hidden_declaration_is_not_visible_synthetic_content(sample, body):
    raw = body.encode('utf-8'); (sample / 'raw.html').write_bytes(raw)
    meta = {'source_id': 'ordinary', 'source_url': 'https://example.org/ordinary',
            'rights_tag': 'review-only', 'fetched_at': '2026-10-26T01:00:00Z'}
    assert corpus_source(sample, meta, raw, for_replay=True).artifact_origin == 'public_http'
    assert 'ordinary' in next(_load_document(sample, meta).units()).text


@pytest.mark.parametrize('raw', [b'%PDF-1.7\nlegacy PDF', '普通旧编码页面'.encode('gb18030')])
def test_non_html_or_non_utf8_source_metadata_keeps_legacy_path(raw):
    meta = {'source_id': 'ordinary', 'source_url': 'https://example.org/ordinary',
            'rights_tag': 'review-only', 'fetched_at': '2026-10-26T01:00:00Z'}
    assert corpus_source(Path('ordinary'), meta, raw, for_replay=True).artifact_origin == 'public_http'
