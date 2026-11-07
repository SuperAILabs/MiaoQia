"""mTLS data-plane topology shared by the process-level E2E gates.

Builds an isolated test CA, one SPIFFE certificate per process role, the
platform internal mTLS settings, the data-api listener and the acq-admin
source-rights listener with an explicit rights batch. Certificates are test
material only; they are generated per run and never used for deployments.
"""
from __future__ import annotations

import json
import subprocess
import urllib.parse
from pathlib import Path

import psycopg

from miaoqia.kernel import reqctx
from miaoqia.kernel.internal_http import Client, Problem, TLS
from miaoqia.kernel.testcerts import TestCA

SPIFFE = "spiffe://miaoqia/service/"
PLATFORM_API, PLATFORM_CONSUMER = SPIFFE + "platform-api", SPIFFE + "platform-consumer"
DATA_API, ACQ_ADMIN, HARNESS = SPIFFE + "data-api", SPIFFE + "acq-admin", SPIFFE + "e2e-harness"
DATA_LISTEN, RIGHTS_LISTEN, PLATFORM_INTERNAL = "127.0.0.1:8094", "127.0.0.1:8095", "127.0.0.1:8091"
DATA_URL = f"https://{DATA_LISTEN}"
DATA_OPERATIONS = ("data.get_fact", "data.fact_history", "data.query_facts", "data.bid_result_units", "data.resolve_evidence")
RIGHTS_OPERATIONS = ("rights.resolve", "rights.confirm", "rights.status")
# Service reads are tenant-scoped and name the exact service principal
# (docs/03-architecture/subsystems/source-rights-runtime.md).
SERVICE_READS = (
    ("monitoring", "svc:detection", ("data.bid_result_units",)),
    ("simulation", "svc:learning", ("data.query_facts", "data.bid_result_units")),
)
READER, OPERATOR = "acq_rights_reader", "acq_rights_operator"


def _env_tls(prefix: str, tls: TLS) -> dict[str, str]:
    return {prefix + "_CERT": tls.cert, prefix + "_KEY": tls.key, prefix + "_CA": tls.ca}


class Topology:
    def __init__(self, work: Path):
        ca = TestCA(work / "certs")
        self.certs = {name: ca.peer(name, SPIFFE + name)
                      for name in ("platform-api", "platform-consumer", "platform-scheduler",
                                   "platform-outbox-relay", "data-api", "acq-admin", "e2e-harness")}

    def platform(self, role: str) -> dict[str, str]:
        """Platform role env: internal mTLS plus mTLS data-api URLs."""
        return {
            **_env_tls("MIAOQIA_INTERNAL_TLS", self.certs["platform-" + role]),
            # Only data-api and acq-admin call the identity session interface.
            "MIAOQIA_INTERNAL_ALLOWED_PEERS": ",".join((DATA_API, ACQ_ADMIN)),
            "MIAOQIA_INTERNAL_ADDR": PLATFORM_INTERNAL,  # only the api role listens
            "MIAOQIA_DETECTION_DATA_API_URL": DATA_URL,
            "MIAOQIA_GATEWAY_DATA_API_URL": DATA_URL,
            "MIAOQIA_LEARNING_DATA_API_URL": DATA_URL,
        }

    def _identity(self) -> dict[str, str]:
        return {"MIAOQIA_IDENTITY_URL": f"https://{PLATFORM_INTERNAL}/internal", "MIAOQIA_IDENTITY_PEER": PLATFORM_API}

    def data_api(self) -> dict[str, str]:
        delegations = [{"peer": peer, "operation": op, "purpose": "interactive"}
                       for peer in (PLATFORM_API, HARNESS) for op in DATA_OPERATIONS]
        delegations += [{"peer": PLATFORM_CONSUMER, "operation": op, "purpose": purpose, "principal": principal}
                        for purpose, principal, ops in SERVICE_READS for op in ops]
        return {
            **_env_tls("MIAOQIA_INTERNAL_TLS", self.certs["data-api"]), **self._identity(),
            "MIAOQIA_DATA_API_LISTENER": "1", "MIAOQIA_DATA_LISTEN": DATA_LISTEN,
            "MIAOQIA_DATA_DELEGATIONS": json.dumps(delegations),
            "MIAOQIA_ACQUISITION_RIGHTS_URL": f"https://{RIGHTS_LISTEN}/internal/v1",
            "MIAOQIA_ACQUISITION_PEER": ACQ_ADMIN,
        }

    def acq_admin(self, pg: str) -> dict[str, str]:
        delegations = [{"peer": DATA_API, "operation": op, "purpose": "interactive"} for op in RIGHTS_OPERATIONS]
        delegations += [{"peer": DATA_API, "operation": op, "purpose": purpose, "principal": principal}
                        for purpose, principal, _ in SERVICE_READS for op in RIGHTS_OPERATIONS]
        return {
            **_env_tls("MIAOQIA_INTERNAL_TLS", self.certs["acq-admin"]), **self._identity(),
            "MIAOQIA_ACQUISITION_RIGHTS_ENABLED": "1", "MIAOQIA_ACQUISITION_RIGHTS_LISTEN": RIGHTS_LISTEN,
            "MIAOQIA_ACQUISITION_RIGHTS_READER_DSN": f"postgres://{READER}:{READER}@{pg}/acq",
            "MIAOQIA_ACQUISITION_RIGHTS_DELEGATIONS": json.dumps(delegations),
        }

    def operator_env(self, pg: str) -> dict[str, str]:
        return {"MIAOQIA_ACQUISITION_RIGHTS_OPERATOR_DSN": f"postgres://{OPERATOR}:{OPERATOR}@{pg}/acq"}

    def harness(self) -> Client:
        """The harness reads data-api as an interactive user through its own peer certificate."""
        return Client(DATA_URL, self.certs["e2e-harness"], DATA_API)


def hostport(dsn: str) -> str:
    p = urllib.parse.urlparse(dsn)
    return f"{p.hostname or 'localhost'}:{p.port or 5432}"


def provision_rights(env: dict[str, str], root: Path, topology: Topology) -> None:
    """Create the dedicated rights roles, migrate acq and grant reader/operator."""
    pg = hostport(env["MIAOQIA_TEST_PG"])
    with psycopg.connect(env["MIAOQIA_TEST_PG"], autocommit=True) as c:
        for role in (READER, OPERATOR):
            if c.execute("SELECT 1 FROM pg_roles WHERE rolname = %s", (role,)).fetchone() is None:
                c.execute(f"CREATE ROLE {role} LOGIN PASSWORD '{role}' NOBYPASSRLS")
            c.execute(f"GRANT CONNECT ON DATABASE acq TO {role}")
    subprocess.run(["uv", "run", "-q", "--locked", "--all-packages", "mq-acquisition", "migrate"],
                   cwd=root / "data", env=env, check=True)
    subprocess.run(["uv", "run", "-q", "--locked", "--all-packages", "mq-source-rights", "grant-roles",
                    "--reader", READER, "--operator", OPERATOR], cwd=root / "data",
                   env=dict(env, MIAOQIA_ACQUISITION_DB_OWNER_DSN=f"postgres://acq_owner:acq_owner@{pg}/acq"), check=True)


def apply_rights(env: dict[str, str], root: Path, topology: Topology, commands: list[dict], path: Path, reason: str) -> None:
    path.write_text(json.dumps(commands, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    result = subprocess.run(["uv", "run", "-q", "--locked", "--all-packages", "mq-source-rights", "apply",
                             "--file", str(path), "--actor", "e2e-harness", "--reason", reason],
                            cwd=root / "data", env=dict(env, **topology.operator_env(hostport(env["MIAOQIA_TEST_PG"]))),
                            text=True, capture_output=True)
    if result.returncode != 0:
        raise RuntimeError(
            "来源权利批次未能应用（同一 entry_id/version 已登记为不同内容时整批回滚，本机 acq 库需另行处理）："
            + (result.stderr or result.stdout)[-2000:])


def read(client: Client, path: str, body: dict, context: reqctx.Context) -> tuple[int, object]:
    with reqctx.use(context):
        try:
            return 200, client.request("POST", path, body)
        except Problem as e:
            return e.status, None
