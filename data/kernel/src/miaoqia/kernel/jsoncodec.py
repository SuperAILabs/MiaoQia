"""Bounded strict JSON codec shared by request Context and internal DTOs."""

import json

MAX_BODY = 65536


def _pairs(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key")
        result[key] = value
    return result


def loads(body):
    if len(body) > MAX_BODY:
        raise ValueError("JSON too large")
    value = json.loads(
        body,
        object_pairs_hook=_pairs,
        parse_constant=lambda _: (_ for _ in ()).throw(ValueError()),
    )

    def depth(v, n=0):
        if n > 12:
            raise ValueError("JSON too deep")
        for child in (
            v.values() if isinstance(v, dict) else v if isinstance(v, list) else ()
        ):
            depth(child, n + 1)

    depth(value)
    return value


def closed(value, keys):
    if not isinstance(value, dict) or set(value) != set(keys):
        raise ValueError("invalid object")
    return value
