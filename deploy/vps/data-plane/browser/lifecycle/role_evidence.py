"""Verify one disposable browser observation across the relay and Evidence roles."""

from contextlib import contextmanager
from datetime import date, datetime
from ipaddress import IPv4Address
import json
import os
import subprocess
import sys
import time

from confluent_kafka import Consumer, KafkaError, KafkaException, TopicPartition
from confluent_kafka.admin import AdminClient, NewTopic
import psycopg
from psycopg import sql
from psycopg.conninfo import conninfo_to_dict
from psycopg.rows import dict_row
import yaml

from miaoqia.acquisition.api.browser_pairs import read_browser_pair
from miaoqia.acquisition.api.browser_projection import build_projection
from miaoqia.acquisition.application.fetch import HEALTH_CHANGED
from miaoqia.evidence.api import SNAPSHOT_CREATED, TOPIC as EVIDENCE_TOPIC
from miaoqia.evidence.domain.snapshot import normalized_hash
from miaoqia.evidence.extensions.packs import load_documents
from miaoqia.kernel import runtime
from miaoqia.kernel.objectstore import open_object_store, versioned

from .role_archive import (
    ADMIN_DSN, APP_DSN, BODY, BROWSER_COMPLETED, CHANNEL, DETAIL_VIEW, KEY, OWNER_DSN,
    PACKS, SOURCE, STORE, FixtureError, create_role, digest, grant_app_defaults, require, selected_html,
)


ACQUISITION_TOPIC = "public.acquisition"
LIVE_GROUP = "evidence.live"
DLQS = ("dlq.evidence", "dlq.domain-intelligence")
TOPICS = (ACQUISITION_TOPIC, EVIDENCE_TOPIC, *DLQS)
DATABASES = (("evidence", "evd"), ("domain-intelligence", "dom"), ("entity-graph", "ent"))
EVIDENCE_DSN = runtime.env_key("evidence", "DB_DSN")
EVIDENCE_STORE = runtime.env_key("evidence", "OBJECT_STORE_DIR")
RELAY_DSN = "MQ_FIXTURE_RELAY_DSN"
PARAGRAPH = BODY.decode("cp1252").removesuffix("\r\n")
PENDING_WAIT_SECONDS = 30
COORDINATOR_STARTING = frozenset({
    KafkaError._TIMED_OUT, KafkaError._WAIT_COORD, KafkaError.COORDINATOR_LOAD_IN_PROGRESS,
    KafkaError.COORDINATOR_NOT_AVAILABLE, KafkaError.NOT_COORDINATOR,
})


class PendingNotReady(FixtureError):
    pass


class PendingTimeout(FixtureError):
    def __init__(self, waiting_for):
        super().__init__("pending_timeout")
        self.waiting_for = waiting_for


class OutboxShapeError(FixtureError):
    def __init__(self, stage, rows):
        super().__init__(stage)
        types = [row["envelope"].get("type") for row in rows]
        self.counts = {"sampled": len(types), "v4": types.count(BROWSER_COMPLETED),
                       "health": types.count(HEALTH_CHANGED)}
        self.counts["other"] = len(types) - self.counts["v4"] - self.counts["health"]


def fixture_broker():
    broker = os.environ.get("MIAOQIA_KAFKA_BROKERS", "")
    host, _, port = broker.partition(":")
    require(port == "9092", "broker_endpoint")
    try:
        address = IPv4Address(host)
    except ValueError:
        raise FixtureError("broker_endpoint") from None
    require(address.is_private and not (address.is_unspecified or address.is_multicast), "broker_endpoint")
    return {"bootstrap.servers": broker, "allow.auto.create.topics": False, "log_level": 0}


@contextmanager
def read_database(env_key):
    with psycopg.connect(os.environ[env_key], connect_timeout=5, row_factory=dict_row,
                         options="-c statement_timeout=5000") as connection:
        connection.execute("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY")
        yield connection


def database_config():
    keys = [ADMIN_DSN, OWNER_DSN, APP_DSN, RELAY_DSN]
    for subsystem, _ in DATABASES:
        keys.extend(runtime.env_key(subsystem, suffix) for suffix in ("DB_OWNER_DSN", "DB_DSN"))
    config = {key: conninfo_to_dict(os.environ[key]) for key in keys}
    endpoint = (config[ADMIN_DSN]["host"], config[ADMIN_DSN].get("port", "5432"))
    require(IPv4Address(endpoint[0]).is_private, "postgres_endpoint")
    require(all({"host", "dbname", "user", "password"} <= value.keys()
                and all(value[name] for name in ("user", "password"))
                and (value["host"], value.get("port", "5432")) == endpoint
                and not {"hostaddr", "service"} & value.keys() for value in config.values()),
            "postgres_endpoint")
    require(len({options["user"] for options in config.values()}) == len(config)
            and config[ADMIN_DSN]["dbname"] == "postgres"
            and all(config[key]["dbname"] == "acq" for key in (OWNER_DSN, APP_DSN, RELAY_DSN))
            and config[RELAY_DSN]["user"] == "outbox_relay", "postgres_identities")
    for subsystem, database in DATABASES:
        require(all(config[runtime.env_key(subsystem, suffix)]["dbname"] == database
                    for suffix in ("DB_OWNER_DSN", "DB_DSN")), "postgres_database_scope")
    return config


def provision_databases(config):
    with psycopg.connect(os.environ[ADMIN_DSN], autocommit=True, connect_timeout=5) as admin:
        for subsystem, name in DATABASES:
            owner_key = runtime.env_key(subsystem, "DB_OWNER_DSN")
            app_key = runtime.env_key(subsystem, "DB_DSN")
            owner, app = config[owner_key], config[app_key]
            create_role(admin, owner)
            create_role(admin, app)
            database = sql.Identifier(name)
            app_role = sql.Identifier(app["user"])
            admin.execute(sql.SQL("CREATE DATABASE {} OWNER {}").format(database, sql.Identifier(owner["user"])))
            admin.execute(sql.SQL("REVOKE CONNECT ON DATABASE {} FROM PUBLIC").format(database))
            admin.execute(sql.SQL("GRANT CONNECT ON DATABASE {} TO {}").format(database, app_role))
            with psycopg.connect(os.environ[owner_key], connect_timeout=5) as connection:
                grant_app_defaults(connection, app["user"])
    run_command(["mq-data", "migrate", "--role", "data-consumer"], "data_migrate")
    with psycopg.connect(os.environ[ADMIN_DSN], connect_timeout=5) as admin:
        create_role(admin, config[RELAY_DSN])
    with psycopg.connect(os.environ[OWNER_DSN], connect_timeout=5) as acquisition:
        acquisition.execute("REVOKE CONNECT ON DATABASE acq FROM PUBLIC")
        acquisition.execute(sql.SQL("GRANT CONNECT ON DATABASE acq TO {}, outbox_relay").format(
            sql.Identifier(config[APP_DSN]["user"]),
        ))
        acquisition.execute("GRANT USAGE ON SCHEMA public TO outbox_relay")
        acquisition.execute("GRANT SELECT, UPDATE (published_at) ON kernel_outbox TO outbox_relay")
    with read_database(RELAY_DSN) as relay:
        grants = relay.execute("""
            SELECT current_user AS role, current_database() AS db,
                   has_table_privilege('kernel_outbox', 'SELECT') AS read_outbox,
                   has_column_privilege('kernel_outbox', 'published_at', 'UPDATE') AS mark_published,
                   has_column_privilege('kernel_outbox', 'envelope', 'UPDATE') AS change_envelope,
                   has_table_privilege('acq_raw_artifacts', 'SELECT') AS read_artifacts,
                   has_table_privilege('kernel_outbox', 'INSERT,DELETE,TRUNCATE') AS mutate_outbox,
                   has_database_privilege('evd', 'CONNECT') OR has_database_privilege('dom', 'CONNECT')
                       OR has_database_privilege('ent', 'CONNECT') AS connect_downstream
        """).fetchone()
        require(grants == {"role": "outbox_relay", "db": "acq", "read_outbox": True, "mark_published": True,
                           "change_envelope": False, "read_artifacts": False, "mutate_outbox": False,
                           "connect_downstream": False}, "relay_grants")


def run_command(argv, stage):
    try:
        result = subprocess.run(argv, capture_output=True, check=False, timeout=45)
    except (OSError, subprocess.TimeoutExpired):
        raise FixtureError(stage) from None
    require(result.returncode == 0, stage)
    return result.stdout


def documents(live_parse, *, previous):
    pack = PACKS / "information_domains" / SOURCE
    manifest = pack / "pack.yaml"
    document = pack / "documents.yaml"
    config = yaml.safe_load(manifest.read_text(encoding="utf-8"))
    provides = config["pack"]["provides"]
    require(provides == ({"sources": "sources.yaml"} if previous is None
                         else {"sources": "sources.yaml", "documents": "documents.yaml"}), "fixture_manifest")
    if previous is None:
        require(not document.exists() and not load_documents(PACKS), "fixture_documents_absent")
    else:
        verify_admission(previous)
    new = {"documents": [{"source_id": SOURCE, "media_type": "text/plain", "browser_projection": True,
                          "live_parse": live_parse}]}
    staged = document.with_suffix(".tmp")
    staged.write_text(yaml.safe_dump(new), encoding="utf-8")
    staged.replace(document)
    if previous is None:
        provides["documents"] = document.name
        manifest.write_text(yaml.safe_dump(config), encoding="utf-8")
    verify_admission(live_parse)


def verify_admission(expected):
    adapters = load_documents(PACKS)
    require(set(adapters) == {SOURCE}, "fixture_document_scope")
    adapter = adapters[SOURCE]
    require(adapter.live_parse is expected and adapter.browser_projection and adapter.media_type == "text/plain",
            "fixture_admission")


def init(broker):
    try:
        config = database_config()
    except (KeyError, ValueError, psycopg.Error):
        raise FixtureError("postgres_configuration") from None
    provision_databases(config)
    documents(False, previous=None)
    try:
        admin = AdminClient(broker)
        futures = admin.create_topics([NewTopic(topic, num_partitions=1, replication_factor=1) for topic in TOPICS],
                                      operation_timeout=15, request_timeout=20)
        for future in futures.values():
            future.result(timeout=20)
    except Exception:
        raise FixtureError("kafka_topics") from None
    return {"stages": ["data_consumer_migration", "relay_grants", "admission_paused", "kafka_topics"]}


def capture(day):
    with read_database(APP_DSN) as acquisition:
        rows = acquisition.execute("""
            SELECT seq, event_id, topic, partition_key, envelope, envelope::text AS wire, published_at
            FROM kernel_outbox ORDER BY seq LIMIT 3
        """).fetchall()
        observed_types = [row["envelope"].get("type") for row in rows]
        expected_types = [BROWSER_COMPLETED, HEALTH_CHANGED]
        if observed_types != expected_types[:len(observed_types)]:
            raise OutboxShapeError("published_outbox_count", rows)
        if len(rows) != 2 or any(row["published_at"] is None for row in rows):
            raise PendingNotReady("published_outbox")
        captured, health = rows
        envelope = captured["envelope"]
        event = envelope["data"]
        require(captured["event_id"] == envelope["id"] and envelope["type"] == BROWSER_COMPLETED
                and captured["topic"] == ACQUISITION_TOPIC and captured["partition_key"] == f"{SOURCE}|{KEY}"
                and envelope["subject"] == event["artifact_ref"] and event["source_id"] == SOURCE
                and event["notice_key"] == KEY and event["source_url"] == DETAIL_VIEW
                and event["published_on"] == day.isoformat() and event["observation_version"] == 1
                and event["media_type"] == "text/plain", "outbox_v4_scope")
        health_event = health["envelope"]
        if (health["event_id"] != health_event["id"] or health["topic"] != ACQUISITION_TOPIC
                or health["partition_key"] != SOURCE or health_event["subject"] != SOURCE
                or health_event["data"] != {"source_id": SOURCE, "before": "unknown", "after": "healthy",
                                            "reason_code": "ok"}):
            raise OutboxShapeError("outbox_health_scope", rows)
        observations = acquisition.execute("""
            SELECT o.version, o.notice_key, o.attempt_id, o.source_url, o.published_on, o.channel,
                   r.payload, r.run_id, r.list_attempt_id, r.task_key,
                   a.source_key, a.artifact_ref, a.content_hash, a.size_bytes, a.media_type
            FROM acq_source_observations o
            JOIN acq_browser_receipts r ON r.attempt_id = o.browser_attempt_id AND r.artifact_id = o.artifact_id
                AND r.source_id = o.source_id AND r.notice_key = o.notice_key
            JOIN acq_raw_artifacts a ON a.id = o.artifact_id AND a.source_id = o.source_id
            WHERE o.source_id = %s LIMIT 2
        """, (SOURCE,)).fetchall()
    require(len(observations) == 1, "acquisition_observation_count")
    observation = observations[0]
    require(observation["payload"] == event and observation["version"] == event["observation_version"]
            and observation["source_key"] == f"browser:{KEY}" and observation["notice_key"] == KEY
            and observation["source_url"] == DETAIL_VIEW and observation["published_on"] == day
            and observation["channel"] == CHANNEL
            and all(observation[key] == event[key] for key in ("artifact_ref", "content_hash", "size_bytes", "media_type"))
            and all(observation[key] == event["binding"][key]
                    for key in ("attempt_id", "run_id", "list_attempt_id", "task_key")), "acquisition_observation")
    return captured, health


def broker_records(broker, rows, *, stopped):
    client = Consumer({**broker, "group.id": LIVE_GROUP, "enable.auto.commit": False,
                       "enable.auto.offset.store": False, "fetch.message.max.bytes": 65536})
    try:
        topic = TopicPartition(ACQUISITION_TOPIC, 0)
        metadata = client.list_topics(timeout=5).topics
        require(all(name in metadata and metadata[name].error is None and set(metadata[name].partitions) == {0}
                    and metadata[name].partitions[0].error is None for name in TOPICS), "topic_partitions")
        watermarks = client.get_watermark_offsets(topic, timeout=5)
        if watermarks in ((0, 0), (0, 1)):
            raise PendingNotReady("kafka_record")
        require(watermarks == (0, len(rows)), "acquisition_watermarks")
        require(all(client.get_watermark_offsets(TopicPartition(name, 0), timeout=5) == (0, 0)
                    for name in DLQS), "dlq_empty")
        client.assign([TopicPartition(ACQUISITION_TOPIC, 0, 0)])
        deadline = time.monotonic() + 8
        for offset, row in enumerate(rows):
            record = None
            while record is None and time.monotonic() < deadline:
                record = client.poll(0.25)
            require(record is not None and record.error() is None and record.topic() == ACQUISITION_TOPIC
                    and record.partition() == 0 and record.offset() == offset, "kafka_record")
            require(record.value() == row["wire"].encode("utf-8")
                    and record.key() == row["partition_key"].encode("utf-8")
                    and record.headers() == [("ce_id", row["event_id"].encode("utf-8")),
                                             ("content-type", b"application/cloudevents+json")], "kafka_exact_bytes")
        try:
            committed, = client.committed([topic], timeout=5)
        except KafkaException as error:
            if not stopped and error.args[0].code() in COORDINATOR_STARTING:
                raise PendingNotReady("evidence_live_offset") from None
            raise
        if committed.error is None and committed.offset < len(rows):
            raise PendingNotReady("evidence_live_offset")
        require(committed.error is None and committed.offset == len(rows), "evidence_live_offset")
        if stopped:
            admin = AdminClient(broker)
            groups = admin.describe_consumer_groups([LIVE_GROUP], request_timeout=5)
            description = groups[LIVE_GROUP].result(timeout=6)
            require(not description.members and description.state.name == "EMPTY", "evidence_consumer_stopped")
        v4, health = rows
        return {"kafka_partition": 0, "kafka_offset": 0, "health_kafka_offset": 1, "kafka_count": len(rows),
                "evidence_live_committed_offset": committed.offset,
                "envelope_sha256": digest(v4["wire"].encode("utf-8"))[7:],
                "health_envelope_sha256": digest(health["wire"].encode("utf-8"))[7:]}
    finally:
        client.close()


def archive_pair(captured):
    event = captured["envelope"]["data"]
    store = open_object_store(STORE, read_only=True)
    raw, projection = read_browser_pair(store, event)
    expected = build_projection(selected_html(PARAGRAPH))
    require(raw == BODY and event["projection"]["content_hash"] == digest(expected)
            and [block["text"] for block in projection["blocks"]] == [PARAGRAPH], "archive_pair_bytes")
    return raw, projection


def evidence_state(connection, captured):
    inbox = connection.execute("SELECT event_id FROM kernel_inbox WHERE consumer = %s LIMIT 2", (LIVE_GROUP,)).fetchall()
    if not inbox:
        raise PendingNotReady("evidence_live_inbox")
    require(inbox == [{"event_id": captured["event_id"]}], "evidence_live_inbox")
    empty = connection.execute("SELECT NOT EXISTS (SELECT FROM evd_extraction_revisions) AS ok").fetchone()
    require(empty["ok"], "no_inferred_extractions")
    return connection.execute("SELECT event_id, source_id, envelope FROM evd_pending_live_events LIMIT 2").fetchall()


def pending(captured):
    verify_admission(False)
    with read_database(EVIDENCE_DSN) as connection:
        queued = evidence_state(connection, captured)
        require(queued == [{"event_id": captured["event_id"], "source_id": SOURCE,
                            "envelope": captured["envelope"]}], "exact_pending_event")
        empty = connection.execute("""
            SELECT NOT EXISTS (SELECT FROM evd_snapshots)
               AND NOT EXISTS (SELECT FROM evd_browser_observations)
               AND NOT EXISTS (SELECT FROM evd_live_observations)
               AND NOT EXISTS (SELECT FROM kernel_outbox) AS ok
        """).fetchone()
        require(empty["ok"], "no_pending_normalized_output")


def replay_one(expected):
    output = run_command(["mq-evidence-replay", "--pending-source", SOURCE, "--limit", "1", "--packs", str(PACKS)],
                         "replay_command")
    require(json.loads(output) == {"source_id": SOURCE, "processed": expected}, "replay_processed_count")


def snapshot(captured, raw, projection):
    event = captured["envelope"]["data"]
    with read_database(EVIDENCE_DSN) as connection:
        require(evidence_state(connection, captured) == [], "pending_drained")
        counts = connection.execute("""
            SELECT (SELECT count(*) FROM evd_snapshots) AS snapshots,
                   (SELECT count(*) FROM evd_browser_observations) AS bindings,
                   (SELECT count(*) FROM evd_live_observations) AS cursors,
                   (SELECT count(*) FROM kernel_outbox) AS events
        """).fetchone()
        require(counts == {"snapshots": 1, "bindings": 1, "cursors": 1, "events": 1}, "one_snapshot")
        row = connection.execute("""
            SELECT s.*, b.event, b.is_anchor, b.observation_version, o.version AS live_version,
                   x.envelope AS snapshot_event, x.topic, x.partition_key, x.published_at
            FROM evd_snapshots s
            JOIN evd_browser_observations b ON b.snapshot_id = s.id AND b.source_id = s.source_id
            JOIN evd_live_observations o ON o.source_id = b.source_id AND o.notice_key = b.notice_key
                AND o.stream_key = s.source_key
            JOIN kernel_outbox x ON x.envelope->>'subject' = s.id
            WHERE b.source_id = %s AND b.notice_key = %s
        """, (SOURCE, KEY)).fetchone()
    require(row is not None and row["is_anchor"] and row["event"] == event
            and row["live_version"] == row["observation_version"] == event["observation_version"]
            and row["source_key"] == f"notice:{KEY}" and row["seq"] == 1
            and row["raw_sha256"] == digest(raw) == event["content_hash"]
            and row["artifact_ref"] == event["artifact_ref"] and row["media_type"] == event["media_type"]
            and row["rights_tags"] == event["rights_tags"]
            and row["observed_at"] == datetime.fromisoformat(event["fetched_at"]), "snapshot_v4_linkage")
    notice = row["snapshot_event"]
    require(row["topic"] == EVIDENCE_TOPIC and row["partition_key"] == f"{SOURCE}|notice:{KEY}"
            and row["published_at"] is None and notice["type"] == SNAPSHOT_CREATED
            and notice["data"] == {"snapshot_ref": row["id"], "source_id": SOURCE, "artifact_ref": row["artifact_ref"],
                                   "rights_tags": row["rights_tags"], "content_hash": row["raw_sha256"]},
            "snapshot_outbox")
    expected = {**projection, "snapshot_ref": row["id"]}
    require(row["pipeline_version"] == expected["pipeline_version"]
            and row["document_ref"] == f"normalized/{versioned(row['pipeline_version'])}/{row['id']}.json"
            and row["normalized_hash"] == normalized_hash(expected), "normalized_document_binding")
    store = open_object_store(os.environ[EVIDENCE_STORE], read_only=True)
    document = store.get(row["document_ref"], max_bytes=4096)
    expected_bytes = json.dumps(expected, ensure_ascii=False, sort_keys=True).encode("utf-8")
    require(document == expected_bytes, "normalized_document_bytes")
    return row, digest(document)


def pending_readback(mode, broker, day):
    stopped = mode == "replay"
    deadline = time.monotonic() + PENDING_WAIT_SECONDS
    while True:
        try:
            rows = capture(day)
            broker_receipt = broker_records(broker, rows, stopped=stopped)
            pending(rows[0])
            return rows, broker_receipt
        except PendingNotReady as error:
            remaining = deadline - time.monotonic()
            if stopped:
                raise
            if remaining <= 0:
                raise PendingTimeout(str(error)) from None
            time.sleep(min(remaining, 0.25))


def verify(mode, broker):
    published_on = os.environ["MQ_FIXTURE_PUBLISHED_ON"]
    day = date.fromisoformat(published_on)
    require(day.isoformat() == published_on and os.environ[EVIDENCE_STORE] == STORE, "archive_configuration")
    rows, broker_receipt = pending_readback(mode, broker, day)
    captured = rows[0]
    raw, projection = archive_pair(captured)
    event = captured["envelope"]["data"]
    result = {"stages": ["published_outbox", "exact_broker_record", "live_commit", "empty_dlqs", "archive_pair",
                         "pending_only"], **broker_receipt, "outbox_count": len(rows), "v4_count": 1, "inbox_count": 1,
              "pending_count": 1, "normalized_count": 0, "dlq_count": 0,
              "event_id_sha256": digest(captured["event_id"].encode("utf-8"))[7:], "original_sha256": digest(raw)[7:],
              "projection_sha256": event["projection"]["content_hash"][7:]}
    if mode == "replay":
        documents(True, previous=False)
        replay_one(1)
        first, document_hash = snapshot(captured, raw, projection)
        replay_one(0)
        require(capture(day) == rows and broker_records(broker, rows, stopped=True) == broker_receipt
                and archive_pair(captured) == (raw, projection)
                and snapshot(captured, raw, projection) == (first, document_hash), "stable_replay")
        result["stages"].extend(["consumer_stopped", "explicit_replay", "linked_snapshot", "idempotent_replay"])
        result.update(pending_count=0, normalized_count=1, replay_processed=1, repeat_replay_processed=0,
                      normalized_sha256=first["normalized_hash"][7:], document_sha256=document_hash[7:])
    return result


def failure_receipt(mode, error):
    record = {"kind": "role_evidence_error", "mode": mode, "stage": "backend_or_configuration"}
    if isinstance(error, FixtureError):
        record["stage"] = str(error)
        if isinstance(error, PendingTimeout):
            record["waiting_for"] = error.waiting_for
        if isinstance(error, OutboxShapeError):
            record["outbox_types"] = error.counts
    else:
        frame = error.__traceback__
        while frame is not None:
            if frame.tb_frame.f_code.co_filename == __file__:
                record.update(source_file="role_evidence.py", source_line=frame.tb_lineno)
            frame = frame.tb_next
    return record


def main():
    mode = sys.argv[1] if len(sys.argv) == 2 and sys.argv[1] in {"init", "pending", "replay"} else None
    try:
        require(mode is not None, "arguments")
        broker = fixture_broker()
        result = init(broker) if mode == "init" else verify(mode, broker)
    except Exception as error:
        print(json.dumps(failure_receipt(mode, error)), flush=True)
        return 1
    print(json.dumps({"kind": f"role_evidence_{mode}", **result}), flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
