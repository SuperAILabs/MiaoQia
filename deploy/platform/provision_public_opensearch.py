"""Install namespace-scoped roles for a dedicated public OpenSearch cluster."""
from __future__ import annotations

import argparse
import http.client
import json
import re
import ssl
import sys
from urllib.parse import urlsplit


NAMESPACE = re.compile(r"[a-z][a-z0-9-]{0,39}")
WRITE_ACTIONS = (
    "indices:admin/refresh*",
    "indices:data/read/get",
    "indices:data/write/bulk[s]",
    "indices:data/write/index",
)
PERMISSIONS = {
    "reader": (
        "indices:admin/aliases/get",
        "indices:data/read/point_in_time/create",
        "indices:data/read/search",
    ),
    "writer": WRITE_ACTIONS,
    "publisher": (
        *WRITE_ACTIONS,
        "indices:admin/aliases/get",
        "indices:admin/aliases",
        "indices:admin/create",
    ),
}


def resources(namespace):
    for use, actions in PERMISSIONS.items():
        role = f"mq_public_{namespace}_{use}"
        yield f"roles/{role}", {
            # OpenSearch dispatches even a single-document PUT through
            # its bulk coordinator, then checks the item against its index.
            "cluster_permissions": ["indices:data/write/bulk"] if use != "reader" else [],
            "index_permissions": [{
                "index_patterns": [f"/mq-public-{namespace}-(current|[0-9a-f]{{32}}(-write)?)/"],
                "allowed_actions": list(actions),
            }],
            "tenant_permissions": [],
        }
    for use in PERMISSIONS:
        role = f"mq_public_{namespace}_{use}"
        yield f"rolesmapping/{role}", {
            "users": [f"mq-public-{namespace}-{use}"],
            "backend_roles": [],
            "hosts": [],
        }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--origin", required=True, help="Direct HTTPS Security API origin")
    parser.add_argument("--namespace", required=True)
    parser.add_argument("--ca", required=True)
    parser.add_argument("--cert", required=True, help="OpenSearch administration certificate file")
    parser.add_argument("--key", required=True)
    args = parser.parse_args()
    try:
        origin = urlsplit(args.origin)
        if (origin.scheme != "https" or not origin.hostname or origin.username is not None
                or origin.path not in ("", "/") or "?" in args.origin or "#" in args.origin
                or origin.port == 0 or not NAMESPACE.fullmatch(args.namespace)):
            raise ValueError
        port = origin.port or 443
    except ValueError:
        parser.error("a direct HTTPS origin and a valid public-search namespace are required")
    try:
        tls = ssl.create_default_context(cafile=args.ca)
        tls.load_cert_chain(args.cert, args.key)
    except OSError:
        print("public_opensearch_tls_unavailable", file=sys.stderr)
        return 1

    conn = http.client.HTTPSConnection(origin.hostname, port, context=tls, timeout=10)
    try:
        for resource, spec in resources(args.namespace):
            conn.request("PUT", f"/_plugins/_security/api/{resource}", json.dumps(spec),
                         {"Content-Type": "application/json"})
            response = conn.getresponse()
            if response.status not in (200, 201) or len(response.read(64 * 1024 + 1)) > 64 * 1024:
                print(f"public_opensearch_provision_rejected {resource} status={response.status}", file=sys.stderr)
                return 1
    except (OSError, http.client.HTTPException):
        print("public_opensearch_provision_unavailable", file=sys.stderr)
        return 1
    finally:
        conn.close()
    print("public_opensearch_roles_and_mappings_applied")
    return 0


if __name__ == "__main__":
    sys.exit(main())
