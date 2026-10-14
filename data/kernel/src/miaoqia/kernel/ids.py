"""类型化 ULID 标识（前缀登记在 contracts/identifiers.yaml）。"""
from ulid import ULID


def new(prefix: str) -> str:
    return f"{prefix}_{ULID()}"
