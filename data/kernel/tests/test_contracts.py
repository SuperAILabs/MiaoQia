"""与Go读取同一工程契约完整实例，验证Schema与运行时一致。"""
import json
from pathlib import Path
import pytest
from miaoqia.kernel import outbox, reqctx

ROOT = Path(__file__).resolve().parents[3] / 'contracts/tests'

def cases(name):
    spec = json.loads((ROOT / f'integration.{name}.json').read_text())
    return [{'name': 'base', 'valid': True, 'instance': spec['base']}, *spec['cases']]

@pytest.mark.parametrize('case', cases('event-envelope'), ids=lambda c:c['name'])
def test_envelope_schema_matches_runtime(case):
    if case['valid']:
        outbox.validate(case['instance'])
    else:
        with pytest.raises(outbox.InvalidEnvelope): outbox.validate(case['instance'])

@pytest.mark.parametrize('case', cases('request-context'), ids=lambda c:c['name'])
def test_context_schema_matches_runtime(case):
    def validate():
        return reqctx.Context.from_dict(case['instance'])
    if case['valid']:
        c = validate()
        assert reqctx.Context.decode(c.encode()) == c
    else:
        with pytest.raises((ValueError, TypeError, KeyError)): validate()

@pytest.mark.parametrize('topic,valid', [('public.domain',True),('platform.audit',False),('cell-01.detection',False),
    ('evil.domain',False),('public',False),('public.domain.extra',False),('cell-02.detection',False)])
def test_topic_routes(topic, valid):
    env = cases('event-envelope')[0]['instance']
    if valid: outbox.check_topic(topic,env)
    else:
        with pytest.raises(outbox.InvalidEnvelope): outbox.check_topic(topic,env)
