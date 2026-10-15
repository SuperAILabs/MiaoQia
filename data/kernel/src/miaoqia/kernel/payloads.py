"""运行时载荷使用生成的离线AsyncAPI制品，启动时编译并拒绝未登记的处理类型。"""
import json
from functools import lru_cache
from importlib.resources import files
from jsonschema import Draft202012Validator

@lru_cache
def schemas():
    from miaoqia import contracts
    return json.loads(files(contracts).joinpath('event_payloads.json').read_text())['schemas']

def validator(name, overrides):
    if name in schemas() and name in overrides:
        raise ValueError('canonical payload schema cannot be overridden')
    schema = overrides.get(name, schemas().get(name))
    if schema is None:
        raise ValueError(f'no payload schema for {name}')
    Draft202012Validator.check_schema(schema)
    return Draft202012Validator(schema, format_checker=Draft202012Validator.FORMAT_CHECKER)
