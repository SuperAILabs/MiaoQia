"""Deliver the archived browser event and replay its evidence in the private fixture."""

from ipaddress import IPv4Network
import json
import re
import secrets
import subprocess
from urllib.parse import urlsplit

from composition_ipc import entries
from probe import emit
from supervisor import command
from verify import require


KAFKA = "docker.io/apache/kafka@sha256:bce21a1a56707320ce239d743143bf4359ba45da27d31a5236f2169001eec751"
BROKER_PROBE = """
import socket
import sys
import time

from confluent_kafka import KafkaException
from confluent_kafka.admin import AdminClient

broker = sys.argv[1]
host, port = broker.split(":")
deadline = time.monotonic() + 70
client = None
while time.monotonic() < deadline:
    if client is None:
        try:
            with socket.create_connection((host, int(port)), timeout=1):
                client = AdminClient({"bootstrap.servers": broker, "log_level": 0})
        except OSError:
            time.sleep(0.25)
            continue
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        break
    try:
        metadata = client.list_topics(timeout=min(5, remaining))
        if [(entry.host, entry.port) for entry in metadata.brokers.values()] == [(host, int(port))]:
            print("ready")
            sys.exit(0)
    except KafkaException:
        pass
    time.sleep(0.25)
print("tcp_unavailable" if client is None else "metadata_unavailable")
sys.exit(1)
"""
AWS_KEYS = (
    "AWS_ENDPOINT_URL_S3", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_DEFAULT_REGION",
    "AWS_EC2_METADATA_DISABLED", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE",
)
DATABASES = {"EVIDENCE": "evd", "DOMAIN_INTELLIGENCE": "dom", "ENTITY_GRAPH": "ent"}
PENDING_STAGES = ("published_outbox", "exact_broker_record", "live_commit", "empty_dlqs", "archive_pair", "pending_only")
STAGES = {
    "init": ("data_consumer_migration", "relay_grants", "admission_paused", "kafka_topics"),
    "pending": PENDING_STAGES,
    "replay": (*PENDING_STAGES, "consumer_stopped", "explicit_replay", "linked_snapshot", "idempotent_replay"),
}
PENDING_COUNTS = {"kafka_partition": 0, "kafka_offset": 0, "health_kafka_offset": 1, "kafka_count": 2,
                  "evidence_live_committed_offset": 2, "outbox_count": 2, "browser_count": 1,
                  "inbox_count": 1, "pending_count": 1, "normalized_count": 0, "dlq_count": 0}
REPLAY_COUNTS = {**PENDING_COUNTS, "pending_count": 0, "normalized_count": 1,
                 "replay_processed": 1, "repeat_replay_processed": 0}
ERROR_STAGES = {
    "arguments", "broker_endpoint", "postgres_configuration", "postgres_endpoint", "postgres_identities",
    "postgres_database_scope", "data_migrate", "relay_grants", "fixture_manifest", "fixture_documents_absent",
    "fixture_document_scope", "fixture_admission", "kafka_topics", "outbox_browser_scope", "outbox_health_scope",
    "published_outbox_count", "published_outbox", "acquisition_observation_count", "acquisition_observation",
    "topic_partitions", "acquisition_watermarks", "dlq_empty", "kafka_record", "kafka_exact_bytes",
    "evidence_live_offset", "evidence_consumer_stopped", "archive_pair_bytes",
    "evidence_live_inbox", "no_inferred_extractions", "exact_pending_event", "no_pending_normalized_output",
    "replay_command", "replay_processed_count", "pending_drained", "one_snapshot", "snapshot_browser_linkage",
    "snapshot_outbox", "normalized_document_binding", "normalized_document_bytes", "archive_configuration",
    "stable_replay", "pending_timeout", "backend_or_configuration",
}
WAIT_STAGES = ("published_outbox", "kafka_record", "evidence_live_offset", "evidence_live_inbox")
OUTBOX_TYPE_FIELDS = {"sampled", "browser", "health", "other"}


def read_environment(path):
    values = {}
    for line in path.read_text(encoding="ascii").splitlines():
        key, separator, value = line.partition("=")
        require(separator and re.fullmatch(r"[A-Z][A-Z0-9_]*", key) and key not in values,
                "invalid fixture environment file")
        values[key] = value
    return values


def start_kafka(podman, name, label, network, role_common, image, probe_name):
    record, = json.loads(command([*podman, "network", "inspect", network]))
    require(record["internal"] and not record["dns_enabled"] and not record["ipv6_enabled"],
            "fixture broker network isolation")
    address = str(IPv4Network(record["subnets"][0]["subnet"])[53])
    broker = f"{address}:9092"
    settings = {
        "KAFKA_HEAP_OPTS": "-Xms128m -Xmx384m",
        "KAFKA_NODE_ID": "1",
        "KAFKA_PROCESS_ROLES": "broker,controller",
        "KAFKA_LISTENERS": "INTERNAL://:9092,CONTROLLER://:9093",
        "KAFKA_ADVERTISED_LISTENERS": f"INTERNAL://{broker}",
        "KAFKA_LISTENER_SECURITY_PROTOCOL_MAP": "INTERNAL:PLAINTEXT,CONTROLLER:PLAINTEXT",
        "KAFKA_INTER_BROKER_LISTENER_NAME": "INTERNAL",
        "KAFKA_CONTROLLER_LISTENER_NAMES": "CONTROLLER",
        "KAFKA_CONTROLLER_QUORUM_VOTERS": f"1@{address}:9093",
        "KAFKA_OFFSETS_TOPIC_NUM_PARTITIONS": "3",
        "KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR": "1",
        "KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR": "1",
        "KAFKA_TRANSACTION_STATE_LOG_MIN_ISR": "1",
        "KAFKA_AUTO_CREATE_TOPICS_ENABLE": "false",
        "KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS": "0",
        "KAFKA_LOG_DIRS": "/tmp/mq-kafka-logs",
    }
    command([*podman, "pull", "--quiet", "--platform", "linux/amd64", KAFKA], timeout=175)
    command([*podman, "run", "-d", "--pull=never", "--platform", "linux/amd64", "--http-proxy=false",
             "--name", name, "--network", network, "--ip", address, "--label", label,
             "--image-volume=ignore", "--memory=1024m", "--cpus=1", "--log-driver=k8s-file",
             *(part for key, value in settings.items() for part in ("--env", f"{key}={value}")), KAFKA])
    require(not command([*podman, "port", "--all"]).strip(), "fixture published a host port")
    exit_code = 0
    category = "readiness_receipt"
    try:
        output = command([*role_common, "--rm", "--name", probe_name, image, "python", "-c", BROKER_PROBE, broker],
                         timeout=75)
        if output.strip() == "ready":
            return broker
    except subprocess.CalledProcessError as error:
        exit_code = error.returncode
        candidate = (error.stdout or "").strip()
        category = candidate if candidate in {"tcp_unavailable", "metadata_unavailable"} else "readiness_process"
    except subprocess.TimeoutExpired:
        exit_code, category = None, "readiness_timeout"
    emit("ingest_broker_error", category=category, exit_code=exit_code, broker_state=role_state(podman, name))
    raise RuntimeError("fixture broker readiness failed")


def receipt(output):
    require(isinstance(output, str) and len(output) <= 4096, "invalid evidence receipt size")
    record, = entries(output)
    return record


def role_state(podman, name):
    try:
        status, exit_code, oom = command([
            *podman, "inspect", "--format", "{{.State.Status}} {{.State.ExitCode}} {{.State.OOMKilled}}", name,
        ], timeout=3).split()
        require(status in {"configured", "created", "initialized", "running", "paused", "stopping", "stopped", "exited"}
                and oom in {"true", "false"}, "invalid role state")
        return {"status": status, "exit_code": int(exit_code), "oom_killed": oom == "true"}
    except (ValueError, RuntimeError, OSError, subprocess.SubprocessError):
        return {"status": "unavailable"}


def failure_details(record, stage):
    details = {}
    source_line = record.get("source_line")
    if stage == "backend_or_configuration" and record.get("source_file") == "role_evidence.py":
        if type(source_line) is int and 1 <= source_line <= 4096:
            details.update(source_file="role_evidence.py", source_line=source_line)
    waiting_for = record.get("waiting_for")
    if stage == "pending_timeout" and waiting_for in WAIT_STAGES:
        details["waiting_for"] = waiting_for
    counts = record.get("outbox_types")
    if (stage in ("published_outbox_count", "outbox_health_scope") and isinstance(counts, dict)
            and counts.keys() == OUTBOX_TYPE_FIELDS
            and all(type(value) is int and 0 <= value <= 3 for value in counts.values())
            and counts["sampled"] == counts["browser"] + counts["health"] + counts["other"]):
        details["outbox_types"] = counts
    return details


def report_failure(podman, names, mode, stage, exit_code, stdout_parsed, **details):
    states = {role: role_state(podman, names[role]) for role in ("kafka", "outbox-relay", "data-consumer")}
    emit("ingest_evidence_error", mode=mode, stage=stage, exit_code=exit_code,
         stdout_parsed=stdout_parsed, role_states=states, **details)


def validated_receipt(record, mode, expected_hashes):
    require(record.get("kind") == f"role_evidence_{mode}", "unexpected evidence receipt kind")
    require(record.get("stages") == list(STAGES[mode]), "unexpected evidence receipt stages")
    hashes = {*expected_hashes, "event_id_sha256", "health_envelope_sha256"} if mode != "init" else set()
    counts = {}
    if mode == "pending":
        counts = PENDING_COUNTS
    if mode == "replay":
        hashes.update(("normalized_sha256", "document_sha256"))
        counts = REPLAY_COUNTS
    result = {"stages": record["stages"]}
    for key in hashes:
        value = record.get(key)
        require(isinstance(value, str) and re.fullmatch(r"[0-9a-f]{64}", value), "invalid evidence digest")
        require(key not in expected_hashes or value == expected_hashes[key], "evidence digest differs from archive")
        result[key] = value
    for key, expected in counts.items():
        value = record.get(key)
        require(type(value) is int and value == expected, "unexpected evidence receipt count")
        result[key] = value
    return result


def verify_stage(podman, names, common, image, mode, env_file, mount, expected_hashes):
    args = [*common, "--rm", "--name", names[f"evidence-{mode}"], "--env-file", str(env_file), *mount,
            image, "python", "-m", "lifecycle.role_evidence", mode]
    record = None
    try:
        output = command(args, timeout=75 if mode == "pending" else 120)
        record = receipt(output)
        return validated_receipt(record, mode, expected_hashes)
    except (ValueError, RuntimeError):
        report_failure(podman, names, mode, "verifier_receipt", 0, record is not None)
    except subprocess.SubprocessError as error:
        details = {}
        try:
            record = receipt(error.stdout)
        except (ValueError, RuntimeError):
            pass
        if isinstance(error, subprocess.CalledProcessError):
            stage, exit_code = "verifier_process", error.returncode
            if record and record.get("kind") == "role_evidence_error" and record.get("mode") == mode:
                candidate = record.get("stage")
                if isinstance(candidate, str) and candidate in ERROR_STAGES:
                    stage = candidate
                    details = failure_details(record, stage)
        else:
            stage, exit_code = "verifier_timeout", None
        report_failure(podman, names, mode, stage, exit_code, record is not None, **details)
    raise RuntimeError("evidence verification failed")


def stop_role(podman, name):
    command([*podman, "kill", "--signal", "TERM", name], timeout=4)
    require(int(command([*podman, "wait", name], timeout=20)) == 0, "ingest role did not stop cleanly")


def run_ingest(root, podman, prefix, label, network, image, role_common, state, verified):
    from composition_role import write_environment

    plan = verified["plan_id"]
    require(verified["kind"] == "role_archive_verified" and verified["outbox_count"] == 1,
            "ingest requires verified acquisition outbox")
    collector = read_environment(root / "collector.env")
    archive_init = read_environment(root / "archive-init.env")
    expected_hashes = {"envelope_sha256": verified["outbox_envelope_sha256"],
                       "original_sha256": verified["original_sha256"],
                       "projection_sha256": verified["projection_sha256"]}
    names = {role: f"{prefix}-{role}" for role in (
        "kafka", "kafka-ready", "outbox-relay", "data-consumer", "evidence-init", "evidence-pending", "evidence-replay",
    )}
    emit("ingest_resources", containers=list(names.values()), network=network, label=label)
    broker = start_kafka(podman, names["kafka"], label, network, role_common, image, names["kafka-ready"])
    emit("ingest_broker", manifest=KAFKA.split("@", 1)[1], host_ports_published=0)

    acquisition_key = "MIAOQIA_ACQUISITION_DB_DSN"
    endpoint = urlsplit(collector[acquisition_key])
    require(endpoint.path == "/acq" and endpoint.hostname and endpoint.port, "fixture acquisition endpoint")
    def fixture_dsn(user, database):
        return f"postgresql://{user}:{secrets.token_hex(24)}@{endpoint.hostname}:{endpoint.port}/{database}"

    relay_dsn = fixture_dsn("outbox_relay", "{db}")
    shared = {key: collector[key] for key in (*AWS_KEYS, "MIAOQIA_DEPLOY_DIR", "MIAOQIA_PACKS_DIR")}
    shared["MIAOQIA_KAFKA_BROKERS"] = broker
    relay = {"MIAOQIA_RELAY_DSN_TEMPLATE": relay_dsn, "MIAOQIA_RELAY_SOURCES": "acquisition",
             "MIAOQIA_DEPLOY_DIR": shared["MIAOQIA_DEPLOY_DIR"], "MIAOQIA_KAFKA_BROKERS": broker}
    downstream_app = {f"MIAOQIA_{subsystem}_DB_DSN": fixture_dsn(f"{database}_app", database)
                      for subsystem, database in DATABASES.items()}
    downstream_owner = {f"MIAOQIA_{subsystem}_DB_OWNER_DSN": fixture_dsn(f"{database}_owner", database)
                        for subsystem, database in DATABASES.items()}
    consumer = {**shared, **downstream_app,
                "MIAOQIA_EVIDENCE_OBJECT_STORE_DIR": collector["MIAOQIA_ACQUISITION_OBJECT_STORE_DIR"]}
    verifier = {**consumer, acquisition_key: collector[acquisition_key],
                "MQ_FIXTURE_PUBLISHED_ON": collector["MQ_FIXTURE_PUBLISHED_ON"]}
    init_env, relay_env, consumer_env, verify_env = (root / f"ingest-{mode}.env" for mode in (
        "init", "relay", "consumer", "verify",
    ))
    write_environment(init_env, {**archive_init, **verifier, **downstream_owner,
                                 "MQ_FIXTURE_RELAY_DSN": relay_dsn.replace("{db}", "acq")})
    write_environment(relay_env, relay)
    write_environment(consumer_env, consumer)
    write_environment(verify_env, verifier)
    common = [*role_common, "--security-opt=label=disable"]
    writable = ["--volume", f"{state}:/fixture-state:rw"]
    readonly = ["--volume", f"{state}:/fixture-state:ro"]
    initialized = verify_stage(podman, names, common, image, "init", init_env, writable, expected_hashes)
    emit("ingest_init", plan_id=plan, **initialized)
    for role, env_file, mount, args in (
        ("outbox-relay", relay_env, [], ("platform", "-role", "outbox-relay")),
        ("data-consumer", consumer_env, readonly, ("mq-data", "--role", "data-consumer")),
    ):
        command([*common, "-d", "--name", names[role], "--env-file", str(env_file), *mount, image, *args])
    pending = verify_stage(podman, names, common, image, "pending", verify_env, readonly, expected_hashes)
    emit("ingest_pending", plan_id=plan, **pending)
    for role in ("data-consumer", "outbox-relay"):
        stop_role(podman, names[role])
    emit("ingest_stop", consumer_exit=0, relay_exit=0, signal="SIGTERM")
    expected_hashes.update({key: pending[key] for key in ("event_id_sha256", "health_envelope_sha256")})
    replay = verify_stage(podman, names, common, image, "replay", verify_env, writable, expected_hashes)
    emit("ingest_replay", plan_id=plan, **replay)
