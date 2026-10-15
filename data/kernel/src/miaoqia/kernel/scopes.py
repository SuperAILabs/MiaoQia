"""实例域与角色选择的纯校验；启动、迁移和部署CI共用，不访问凭证或存储。"""
from __future__ import annotations

import re

VALID_SCOPES = {"public", "tenant", "platform"}
CELL_PATTERN = re.compile(r"[a-z0-9][a-z0-9-]{0,62}")


def instance_scope(subsystem: str, spec: dict, allowed: list[str]) -> str:
    if not isinstance(allowed, list) or not allowed or any(not isinstance(v, str) or v not in VALID_SCOPES for v in allowed) or len(allowed) != len(set(allowed)):
        raise ValueError(f"{subsystem}: 非法 data_scopes 登记")
    declared = spec.get("data_scopes", {})
    if not isinstance(declared, dict):
        raise ValueError("角色 data_scopes 必须是映射")
    scope = declared.get(subsystem) if subsystem in declared else (allowed[0] if len(allowed) == 1 else None)
    if not isinstance(scope, str) or scope not in allowed:
        raise ValueError(f"{subsystem}: 缺少或非法 data_scope {scope!r}")
    cell = spec.get("cell", "")
    if not isinstance(cell, str) or (cell and not CELL_PATTERN.fullmatch(cell)):
        raise ValueError(f"{subsystem}: 非法 cell")
    if scope == "public" and cell:
        raise ValueError(f"{subsystem}: 公共实例不能绑定租户 cell")
    if scope == "tenant" and not cell:
        raise ValueError(f"{subsystem}: 租户实例缺少有效 cell")
    return scope


def role_scopes(spec: dict, registry: dict, *, unit: str | None = None, available=None) -> dict[str, str]:
    if not isinstance(spec, dict):
        raise ValueError("角色必须是映射")
    names = spec.get("subsystems")
    if not isinstance(names, list) or not names or any(not isinstance(name, str) for name in names) or len(names) != len(set(names)):
        raise ValueError("角色subsystems必须是非空且不重复的名称列表")
    declared = spec.get("data_scopes", {})
    if not isinstance(declared, dict) or set(declared) - set(names):
        raise ValueError("角色data_scopes只能声明已加载的子系统")
    scopes = {}
    for name in names:
        if name not in registry or (available is not None and name not in available):
            raise ValueError(f"未登记或未组装子系统：{name}")
        entry = registry[name]
        if unit is not None and entry.get("unit") != unit:
            raise ValueError(f"{name} 不属于 {unit} 单元")
        scopes[name] = instance_scope(name, spec, entry.get("data_scopes"))
    return scopes
