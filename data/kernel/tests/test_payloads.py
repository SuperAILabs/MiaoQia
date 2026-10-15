import json
from importlib.resources import files
import pytest
from miaoqia import contracts
from miaoqia.kernel import payloads, consumer

CASES=json.loads(files(contracts).joinpath('event_payloads.json').read_text())['cases']
assert CASES
@pytest.mark.parametrize('case',CASES,ids=lambda c:c['event']+'/'+c['name'])
def test_payload_contracts(case):
    assert payloads.validator(case['event'],{}).is_valid(case['value']) == case['valid']

def test_handler_without_schema_fails_before_connect():
    c=consumer.Consumer('test','test','unused',[],{'test.unregistered.event.v1':lambda *_:None},None)
    with pytest.raises(ValueError,match='no payload schema'): c.run()

def test_canonical_schema_cannot_be_disabled():
    with pytest.raises(ValueError,match='cannot be overridden'):
        payloads.validator('operations.probe.requested.v1',{'operations.probe.requested.v1':{}})
