"""客户智能公共定义中尚无顶层使用者的类型（多对象集合与期间）：直接按 $defs 校验正反例。"""
import copy

import pytest
from jsonschema import Draft202012Validator

from mqc.repo import Repo

from conftest import REPO

ID = "https://schemas.miaoqia.com/intelligence/defs.schema.json"
ORG = "org_01JA8ZXXXXXXXXXXXXXXXXXXX1"
HASH = "sha256:" + "0" * 64
PERIOD = {"start": "2026-07-01T00:00:00+08:00", "end": "2026-10-01T00:00:00+08:00",
          "granularity": "quarter", "time_basis": "published_at"}
SNAP = {"as_of": "2026-10-01T00:00:00+08:00", "source_version": "17", "member_count": 1,
        "members": [{"type": "organization", "id": ORG}], "members_hash": HASH}


def validator(name):
    repo = Repo(REPO)
    return Draft202012Validator({"$ref": f"{ID}#/$defs/{name}"}, registry=repo.registry,
                                format_checker=Draft202012Validator.FORMAT_CHECKER)


def patched(base, **kw):
    d = copy.deepcopy(base)
    for k, v in kw.items():
        if v is None:
            d.pop(k)
        else:
            d[k] = v
    return d


@pytest.mark.parametrize("name,doc,ok", [
    ("subject_set", {"kind": "tenant_list", "list_kind": "tracked", "resolved": SNAP}, True),
    ("subject_set", {"kind": "tenant_list", "resolved": SNAP}, False),                      # 缺清单种类
    ("subject_set", {"kind": "group", "ref": {"type": "organization", "id": ORG}, "resolved": SNAP}, True),
    ("subject_set", {"kind": "group", "resolved": SNAP}, False),                             # 集团缺根组织
    ("membership_snapshot", patched(SNAP, members=None, members_object="sets/abc.json", member_count=1200), True),
    ("membership_snapshot", patched(SNAP, members=None), False),                            # 成员无处可恢复
    ("membership_snapshot", patched(SNAP, members_object="sets/abc.json"), False),           # 两种方式只能取一
    ("membership_snapshot", patched(SNAP, as_of=None), False),                              # 缺取值时点
    ("period", PERIOD, True),
    ("period", patched(PERIOD, time_basis=None), False),                                    # 缺时间口径
    ("comparison", {"kind": "previous_period", "baseline": patched(PERIOD, start="2026-04-01T00:00:00+08:00",
                                                                     end="2026-07-01T00:00:00+08:00")}, True),
    ("comparison", {"kind": "previous_period"}, False),                                     # 缺基准期
])
def test_defs(name, doc, ok):
    assert validator(name).is_valid(doc) is ok
