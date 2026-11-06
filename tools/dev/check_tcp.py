#!/usr/bin/env python3
"""Fail fast when a shared TCP service endpoint is not reachable."""

from __future__ import annotations

import argparse
import socket
import sys


def parse_endpoint(value: str) -> tuple[str, int]:
    value = value.strip()
    if value.startswith("["):
        host, separator, port = value[1:].partition("]:")
        if not separator:
            raise ValueError(f"invalid endpoint: {value}")
    else:
        host, separator, port = value.rpartition(":")
        if not separator or not host:
            raise ValueError(f"invalid endpoint: {value}")
    if not host or not port.isdigit():
        raise ValueError(f"invalid endpoint: {value}")
    return host, int(port)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--label", default="service")
    parser.add_argument("--endpoints", required=True)
    args = parser.parse_args()

    errors: list[str] = []
    for raw in args.endpoints.split(","):
        try:
            host, port = parse_endpoint(raw)
            with socket.create_connection((host, port), timeout=2):
                return 0
        except (OSError, ValueError) as exc:
            errors.append(f"{raw.strip()}: {exc}")

    print(f"{args.label} 未就绪：{'; '.join(errors)}", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
