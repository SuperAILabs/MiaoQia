#!/usr/bin/env python3
"""Run the C11 operator measurements once and emit one JSON result."""

from __future__ import annotations

import argparse
from contextlib import contextmanager
from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import re
import signal
import stat
import time


METRICS = {
    "source_interruption": "seconds",
    "queue_backlog": "plans",
    "error_rate": "ratio",
    "dead_letter": "offsets",
    "outbox_backlog": "rows",
    "disk_watermark": "ratio",
}
IO_SECONDS = 4
DB_SECONDS = 8
ALIAS = r"[a-z][a-z0-9_-]{0,63}"
SECRET_REF = r"C11_[A-Z0-9_]{1,59}"
DIRECTORY_FLAGS = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC

HELP = """Run with the repository's data Python environment:
  data/.venv/bin/python tools/dev/c11_alert_probe.py --config /etc/miaoqia/c11.json

Example JSON shape (illustrative thresholds and scope):
  {
    "profile": "shared", "window_seconds": 60,
    "thresholds": {"source_interruption": 600, "queue_backlog": 20,
      "error_rate": 0.1, "dead_letter": 0, "outbox_backlog": 100, "disk_watermark": 0.85},
    "acquisition": "acq", "sources": {"publisher_a": "explicit_source_id"},
    "databases": {"acq": {"database": "acq", "role": "c11_acq", "dsn_env": "C11_ACQ_DSN"},
                  "facts": {"database": "dom", "role": "c11_dom", "dsn_env": "C11_DOM_DSN"}},
    "kafka": {"bootstrap": "kafka.example.net:9093", "ca": "/etc/miaoqia/kafka-ca.pem",
      "mechanism": "SCRAM-SHA-512", "username_env": "C11_KAFKA_USER", "password_env": "C11_KAFKA_PASSWORD",
      "topics": {"evidence_dlq": {"topic": "dlq.evidence", "partitions": [0, 1]}}},
    "volume": {"alias": "data", "mount": "/srv/miaoqia-data", "fsid": 12345}
  }

Inject each secret reference in the process environment. Each PostgreSQL DSN must
explicitly supply host, port, dbname, user and password and, for shared mode,
sslrootcert with an absolute CA path. PostgreSQL authenticates with SCRAM.
Shared mode uses PostgreSQL verify-full and Kafka SASL_SSL. Supply dedicated
SELECT-only roles and a Kafka principal with Describe access only. Deployment
configuration and egress policy approve the databases, the mount, and all
broker addresses (including advertised peers).
For disposable local fixtures, use profile "fixture", 127.0.0.1 endpoints, and
omit Kafka ca/mechanism/username_env/password_env and PostgreSQL sslrootcert.
The mount must be a canonical local filesystem mount point. A directory on
the parent filesystem or a same-device bind mount cannot be sampled. Obtain
the filesystem ID with
  python3 -c 'import os,sys; print(os.statvfs(sys.argv[1]).f_fsid)' /srv/miaoqia-data

One threshold per metric, applied independently to every configured scope
(an alert means value > threshold). The interruption age uses acquisition's
last successful collection; blocked or never-observed sources are unavailable.
Queue counts unsettled incremental and backfill plans whose not-before time
has passed, including in-flight work. Error rate divides recorded attempts
with an outcome other than ok by all recorded attempts in [start, end).
That window opens after the first Kafka sampling sweep and lasts exactly
window_seconds. Keep the probe and database clocks synchronized. The Kafka
offset sweeps sample around those boundaries. Each DLQ needs its complete
explicit partition list. Offsets measure log growth in offset units. Outbox
counts unpublished rows per configured database.
Volume usage uses capacity available to an unprivileged process.

Exit 0 healthy, 1 alert, 2 unavailable or invalid configuration. Unavailable
measurements have null value and alert; other measured alerts remain visible.
"""


class Unavailable(Exception):
    """Only fixed reason codes may reach output."""


class InvalidConfig(Exception):
    """Configuration errors use an allowlisted section name."""


class DatabaseTimeout(Unavailable):
    pass


class Parser(argparse.ArgumentParser):
    def error(self, _message):
        emit({"status": "unavailable", "reason": "arguments", "measurements": []})
        raise SystemExit(2)


def emit(result):
    print(json.dumps(result, allow_nan=False, separators=(",", ":")))


def object_pairs(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise InvalidConfig("duplicate_key")
        result[key] = value
    return result


def fields(obj, names, section):
    if not isinstance(obj, dict) or set(obj) != set(names.split()):
        raise InvalidConfig(section)


def matches(value, pattern):
    return isinstance(value, str) and re.fullmatch(pattern, value) is not None


def integer(value, low, high):
    return type(value) is int and low <= value <= high


def scopes(obj, limit, section):
    if not isinstance(obj, dict) or not 1 <= len(obj) <= limit or not all(matches(k, ALIAS) for k in obj):
        raise InvalidConfig(section)
    return obj.items()


def absolute(value):
    return (isinstance(value, str) and 0 < len(value) <= 1024 and value.startswith("/")
            and value == os.path.normpath(value) and not value.startswith("//")
            and not any(ord(char) < 32 for char in value))


def env_secret(ref, used, section):
    if not matches(ref, SECRET_REF) or ref in used:
        raise InvalidConfig(section)
    used.add(ref)
    value = os.environ.get(ref, "")
    if not 0 < len(value) <= 8192:
        raise InvalidConfig(section)
    return value


def database_args(config, fixture, used):
    from psycopg import pq
    from psycopg.conninfo import conninfo_to_dict

    # libpq otherwise fills omitted options from the environment.
    if any(option.envvar and option.envvar.decode("ascii") in os.environ for option in pq.Conninfo.parse(b"")):
        raise InvalidConfig("postgres_environment")
    endpoints = set()
    logins = set()
    args = {}
    for alias, handle in scopes(config, 8, "databases"):
        fields(handle, "database role dsn_env", "databases")
        if not all(matches(handle[key], r"[a-z_][a-z0-9_]{0,62}") for key in ("database", "role")):
            raise InvalidConfig("databases")
        dsn = env_secret(handle["dsn_env"], used, "postgres_secret")
        try:
            info = conninfo_to_dict(dsn)
        except Exception:
            raise InvalidConfig("postgres_dsn") from None
        keys = "host port dbname user password" if fixture else "host port dbname user password sslrootcert"
        fields(info, keys, "postgres_dsn")
        if (not matches(info["host"], r"[a-zA-Z0-9][a-zA-Z0-9.-]{0,252}")
                or (fixture and info["host"] != "127.0.0.1")
                or not matches(info["port"], r"[0-9]{1,5}") or not 1 <= int(info["port"]) <= 65535
                or info["dbname"] != handle["database"] or info["user"] != handle["role"]
                or not info["password"] or (not fixture and not absolute(info["sslrootcert"]))):
            raise InvalidConfig("postgres_dsn")
        server = info["host"].lower().rstrip("."), int(info["port"])
        endpoint, login = (*server, info["dbname"]), (*server, info["user"])
        if endpoint in endpoints or login in logins:
            raise InvalidConfig("databases")
        endpoints.add(endpoint)
        logins.add(login)
        args[alias] = dict(
            info, sslmode="disable" if fixture else "verify-full", sslcertmode="disable", gssencmode="disable",
            require_auth="scram-sha-256", passfile=os.devnull, application_name="c11-alert-probe",
            connect_timeout=IO_SECONDS,
            options="-c search_path=pg_catalog -c default_transaction_read_only=on "
                    "-c statement_timeout=3000ms -c lock_timeout=1000ms -c idle_in_transaction_session_timeout=4000ms",
        )
    return args


def kafka_args(config, fixture, used):
    from miaoqia.kernel.kafka import connection_config

    keys = "bootstrap topics" if fixture else "bootstrap topics ca mechanism username_env password_env"
    fields(config, keys, "kafka")
    bootstrap = config["bootstrap"]
    if not matches(bootstrap, r"[a-zA-Z0-9][a-zA-Z0-9.-]{0,252}:[0-9]{1,5}"):
        raise InvalidConfig("kafka")
    host, port = bootstrap.rsplit(":", 1)
    if not 1 <= int(port) <= 65535 or (fixture and host != "127.0.0.1"):
        raise InvalidConfig("kafka")
    topics = set()
    partition_count = 0
    for _, entry in scopes(config["topics"], 16, "topics"):
        fields(entry, "topic partitions", "topics")
        topic, partitions = entry["topic"], entry["partitions"]
        if not matches(topic, r"dlq\.[a-zA-Z0-9_-][a-zA-Z0-9_.-]{0,244}") or topic in topics:
            raise InvalidConfig("topics")
        if (not isinstance(partitions, list) or not 1 <= len(partitions) <= 128
                or not all(integer(part, 0, 2147483647) for part in partitions)
                or len(set(partitions)) != len(partitions)):
            raise InvalidConfig("topics")
        topics.add(topic)
        partition_count += len(partitions)
    if partition_count > 128:
        raise InvalidConfig("topics")

    security = {}
    if not fixture:
        if not absolute(config["ca"]) or config["mechanism"] not in ("SCRAM-SHA-256", "SCRAM-SHA-512"):
            raise InvalidConfig("kafka")
        security = {
            "MIAOQIA_KAFKA_TLS_CA": config["ca"],
            "MIAOQIA_KAFKA_SASL_MECHANISM": config["mechanism"],
            "MIAOQIA_KAFKA_SASL_USERNAME": env_secret(config["username_env"], used, "kafka_secret"),
            "MIAOQIA_KAFKA_SASL_PASSWORD": env_secret(config["password_env"], used, "kafka_secret"),
        }
    return connection_config(bootstrap, environ=security) | {
        "client.id": "c11-alert-probe",
        "allow.auto.create.topics": False,
        "socket.timeout.ms": IO_SECONDS * 1000,
        "socket.connection.setup.timeout.ms": IO_SECONDS * 1000,
        "log_level": 0,
    }


def load_config(path):
    fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK | os.O_CLOEXEC)
    with os.fdopen(fd, "rb") as stream:
        if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
            raise InvalidConfig("config_file")
        encoded = stream.read(32769)
    if len(encoded) > 32768:
        raise InvalidConfig("config_file")
    config = json.loads(encoded, object_pairs_hook=object_pairs)
    fields(config, "profile window_seconds thresholds acquisition sources databases kafka volume", "config")
    if config["profile"] not in ("shared", "fixture") or not integer(config["window_seconds"], 1, 300):
        raise InvalidConfig("profile_or_window")
    threshold = config["thresholds"]
    fields(threshold, " ".join(METRICS), "thresholds")
    for metric, unit in METRICS.items():
        value = threshold[metric]
        valid = (type(value) in (float, int) and 0 <= value <= 1) if unit == "ratio" else integer(
            value, 0, 2**53 - 1)
        if not valid:
            raise InvalidConfig("thresholds")
    sources = [source for _, source in scopes(config["sources"], 64, "sources")]
    if not all(matches(source, r"[a-z][a-z0-9_-]{0,95}") for source in sources) or len(set(sources)) != len(sources):
        raise InvalidConfig("sources")
    volume = config["volume"]
    fields(volume, "alias mount fsid", "volume")
    if (not matches(volume["alias"], ALIAS) or not absolute(volume["mount"])
            or not integer(volume["fsid"], 1, 2**64 - 1)):
        raise InvalidConfig("volume")

    used = set()
    fixture = config["profile"] == "fixture"
    db_handles = database_args(config["databases"], fixture, used)
    if not matches(config["acquisition"], ALIAS) or config["acquisition"] not in db_handles:
        raise InvalidConfig("acquisition")
    return config, db_handles, kafka_args(config["kafka"], fixture, used)


ROLE_SQL = """
SELECT current_database() = %s AND current_user = %s AND session_user = current_user
       AND current_setting('transaction_read_only') = 'on'
       AND r.rolcanlogin AND NOT (r.rolsuper OR r.rolbypassrls OR r.rolreplication OR r.rolcreaterole OR r.rolcreatedb)
       AND r.oid <> d.datdba AND NOT has_database_privilege(d.oid, 'CREATE')
       AND NOT has_schema_privilege('public', 'CREATE')
       AND NOT EXISTS (SELECT FROM pg_catalog.pg_auth_members WHERE member = r.oid)
FROM pg_catalog.pg_roles r JOIN pg_catalog.pg_database d ON d.datname = current_database()
WHERE r.rolname = current_user
"""
ACQUISITION_SQL = """
SELECT s.source_id, s.state, h.checked_at, h.last_success_at, q.pending, a.total, a.errors,
       transaction_timestamp()
FROM public.acq_sources s
LEFT JOIN public.acq_source_health h USING (source_id)
CROSS JOIN LATERAL (
    SELECT count(*) AS pending FROM public.acq_collection_plans p
    WHERE p.source_id = s.source_id AND p.mode IN ('incremental', 'backfill')
      AND p.settlement_reason IS NULL AND p.settled_at IS NULL
      AND p.not_before <= transaction_timestamp()
) q
CROSS JOIN LATERAL (
    SELECT count(*) AS total, count(*) FILTER (WHERE outcome <> 'ok') AS errors
    FROM public.acq_fetch_attempts
    WHERE source_id = s.source_id AND attempted_at >= %s AND attempted_at < %s
) a
WHERE s.source_id = ANY(%s)
"""
OUTBOX_SQL = "SELECT count(*) FROM public.kernel_outbox WHERE published_at IS NULL"
TABLES_SQL = """
SELECT NOT EXISTS (
    SELECT FROM unnest(%s::text[]) AS target(name)
    WHERE has_table_privilege(name, 'INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER')
       OR has_any_column_privilege(name, 'INSERT, UPDATE, REFERENCES')
) AND NOT EXISTS (
    SELECT FROM unnest(%s::text[], %s::text[]) AS target(name, column_name)
    WHERE has_column_privilege(name, column_name, 'SELECT')
)
"""
ACQUISITION_TABLES = ("acq_sources", "acq_source_health", "acq_collection_plans", "acq_fetch_attempts")
ACQUISITION_PRIVATE = (
    ("acq_sources", "access"), ("acq_sources", "state_reason"), ("acq_source_health", "last_run"),
    ("acq_fetch_attempts", "url"), ("acq_fetch_attempts", "error"), ("acq_fetch_attempts", "fingerprint"),
)


@contextmanager
def read_database(args, tables, private):
    import psycopg

    def expired(_signal, _frame):
        raise DatabaseTimeout("postgres_timeout")

    # The server-side limits do not bound a failed network read or name lookup.
    # The operations CLI runs on the main thread and owns the process timer.
    conn = None
    previous = signal.signal(signal.SIGALRM, expired)
    signal.setitimer(signal.ITIMER_REAL, DB_SECONDS)
    try:
        conn = psycopg.connect(**args)
        conn.read_only = True
        conn.isolation_level = psycopg.IsolationLevel.REPEATABLE_READ
        result = conn.execute(ROLE_SQL, (args["dbname"], args["user"])).fetchone()
        if result != (True,):
            raise Unavailable("postgres_role")
        protected_tables, columns = zip(*private, strict=True)
        result = conn.execute(TABLES_SQL, (["public." + table for table in tables],
                                          ["public." + table for table in protected_tables],
                                          list(columns))).fetchone()
        if result != (True,):
            raise Unavailable("postgres_privileges")
        yield conn
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)
        if conn is not None:
            conn.close()


def measure(thresholds, metric, scope, value=None, reason=None):
    threshold = thresholds[metric]
    status = "unavailable" if reason else "alert" if value > threshold else "healthy"
    result = {"metric": metric, "scope": scope, "unit": METRICS[metric], "threshold": threshold,
              "value": value, "alert": None if reason else status == "alert", "status": status}
    if reason:
        result["reason"] = reason
    return result


def acquisition_rows(args, sources, start, end):
    before = datetime.now(timezone.utc)
    with read_database(args, ACQUISITION_TABLES, ACQUISITION_PRIVATE) as conn:
        rows = conn.execute(ACQUISITION_SQL, (start, end, list(sources))).fetchall()
    after = datetime.now(timezone.utc)
    tolerance = timedelta(milliseconds=250)
    if rows and not before - tolerance <= rows[0][-1] <= after + tolerance:
        raise Unavailable("postgres_observation_clock")
    return {row[0]: row[1:] for row in rows}


def acquisition_measurements(thresholds, sources, rows, failure):
    for alias, source in sources.items():
        if source not in rows:
            for metric in ("source_interruption", "queue_backlog", "error_rate"):
                yield measure(thresholds, metric, alias, reason=failure or "source_missing")
            continue
        state, checked, success, pending, total, errors, observed_at = rows[source]
        if state == "blocked":
            yield measure(thresholds, "source_interruption", alias, reason="source_access_blocked")
        elif not checked or not success:
            yield measure(thresholds, "source_interruption", alias, reason="source_success_missing")
        elif not success <= checked <= observed_at:
            yield measure(thresholds, "source_interruption", alias, reason="source_timestamp_invalid")
        else:
            yield measure(thresholds, "source_interruption", alias, (observed_at - success).total_seconds())
        yield measure(thresholds, "queue_backlog", alias, pending)
        yield measure(thresholds, "error_rate", alias, errors / total if total else None,
                      reason=None if total else "no_attempts")


def kafka_boundaries(client, topic, timeout):
    from confluent_kafka import TopicCollection

    descriptions = client.describe_topics(TopicCollection([topic]), request_timeout=timeout)
    result = descriptions[topic].result(timeout)
    identity = result.topic_id
    if (identity is None or (identity.get_most_significant_bits() == 0 and identity.get_least_significant_bits() == 0)
            or result.name != topic or result.is_internal):
        raise Unavailable("dlq_identity_unavailable")
    return str(identity), {part.id for part in result.partitions}


def kafka_snapshot(client, topics):
    from confluent_kafka import IsolationLevel, TopicPartition
    from confluent_kafka.admin import OffsetSpec

    deadline = time.monotonic() + IO_SECONDS

    def remaining():
        budget = deadline - time.monotonic()
        if budget <= 0:
            raise Unavailable("kafka_timeout")
        return budget

    result = {}
    for alias, entry in topics.items():
        try:
            topic = entry["topic"]
            partitions = set(entry["partitions"])
            identity, actual = kafka_boundaries(client, topic, remaining())
            if partitions != actual:
                raise Unavailable("dlq_partition_scope")
            parts = [TopicPartition(topic, part) for part in sorted(partitions)]
            low = client.list_offsets(dict.fromkeys(parts, OffsetSpec.earliest()),
                                      isolation_level=IsolationLevel.READ_UNCOMMITTED, request_timeout=remaining())
            high = client.list_offsets(dict.fromkeys(parts, OffsetSpec.latest()),
                                       isolation_level=IsolationLevel.READ_UNCOMMITTED, request_timeout=remaining())
            offsets = []
            for part in parts:
                first, last = low[part].result(remaining()), high[part].result(remaining())
                if first.offset < 0 or last.offset < first.offset or last.leader_epoch is None or last.leader_epoch < 0:
                    raise Unavailable("dlq_offset_unavailable")
                offsets.append((first.offset, last.offset, last.leader_epoch))
            if (identity, partitions) != kafka_boundaries(client, topic, remaining()):
                raise Unavailable("dlq_identity_changed")
            result[alias] = identity, offsets
        except Unavailable as exc:
            result[alias] = str(exc)
        except Exception:
            result[alias] = "kafka_read_failed"
    return result


def kafka_measurements(thresholds, topics, first, last, clock_valid):
    for alias in topics:
        before, after = first[alias], last[alias]
        if not clock_valid:
            yield measure(thresholds, "dead_letter", alias, reason="observation_clock")
        elif isinstance(before, str) or isinstance(after, str):
            yield measure(thresholds, "dead_letter", alias, reason=before if isinstance(before, str) else after)
        elif before[0] != after[0]:
            yield measure(thresholds, "dead_letter", alias, reason="dlq_identity_changed")
        else:
            value = 0
            for (low1, high1, epoch1), (low2, high2, epoch2) in zip(before[1], after[1], strict=True):
                if epoch1 != epoch2 or low2 < low1 or high2 < high1 or low2 > high1:
                    yield measure(thresholds, "dead_letter", alias, reason="dlq_offset_discontinuity")
                    break
                value += high2 - high1
            else:
                yield measure(thresholds, "dead_letter", alias, value)


def volume_usage(volume):
    path = Path(volume["mount"])
    fd = os.open("/", DIRECTORY_FLAGS)
    try:
        parent_device = None
        for name in path.parts[1:]:
            parent_device = os.fstat(fd).st_dev
            child = os.open(name, DIRECTORY_FLAGS, dir_fd=fd)
            os.close(fd)
            fd = child
        if parent_device == os.fstat(fd).st_dev:
            raise Unavailable("volume_not_mount")
        stats = os.fstatvfs(fd)
        if stats.f_fsid != volume["fsid"]:
            raise Unavailable("volume_identity_changed")
        if stats.f_frsize <= 0 or not 0 <= stats.f_bavail <= stats.f_bfree <= stats.f_blocks or stats.f_blocks <= 0:
            raise Unavailable("volume_capacity_invalid")
        used = stats.f_blocks - stats.f_bfree
        available = used + stats.f_bavail
        if available <= 0:
            raise Unavailable("volume_capacity_invalid")
        return used / available
    finally:
        os.close(fd)


def run(config, handles, kafka_config):
    from confluent_kafka.admin import AdminClient

    thresholds = config["thresholds"]
    topics = config["kafka"]["topics"]
    try:
        client = AdminClient(kafka_config)
    except Exception:
        client = None
    failed_client = dict.fromkeys(topics, "kafka_client_failed")
    first = kafka_snapshot(client, topics) if client is not None else failed_client
    elapsed_start = time.monotonic()
    start = datetime.now(timezone.utc)
    window_seconds = config["window_seconds"]
    end = start + timedelta(seconds=window_seconds)
    time.sleep(window_seconds)
    last = kafka_snapshot(client, topics) if client is not None else failed_client
    observed_end = datetime.now(timezone.utc)
    elapsed = time.monotonic() - elapsed_start
    clock_valid = observed_end >= end and abs((observed_end - start).total_seconds() - elapsed) <= 0.5

    rows, failure = {}, None
    try:
        if not clock_valid:
            raise Unavailable("observation_clock")
        rows = acquisition_rows(handles[config["acquisition"]], config["sources"].values(), start, end)
    except Unavailable as exc:
        failure = str(exc)
    except Exception:
        failure = "postgres_acquisition_read_failed"
    measurements = list(acquisition_measurements(thresholds, config["sources"], rows, failure))
    measurements.extend(kafka_measurements(thresholds, topics, first, last, clock_valid))

    for alias, args in handles.items():
        try:
            with read_database(args, ("kernel_outbox",), (("kernel_outbox", "envelope"),)) as conn:
                count = conn.execute(OUTBOX_SQL).fetchone()[0]
            measurements.append(measure(thresholds, "outbox_backlog", alias, count))
        except Unavailable as exc:
            measurements.append(measure(thresholds, "outbox_backlog", alias, reason=str(exc)))
        except Exception:
            measurements.append(measure(thresholds, "outbox_backlog", alias, reason="postgres_outbox_read_failed"))

    volume = config["volume"]
    try:
        sample = measure(thresholds, "disk_watermark", volume["alias"], volume_usage(volume))
    except Unavailable as exc:
        sample = measure(thresholds, "disk_watermark", volume["alias"], reason=str(exc))
    except OSError:
        sample = measure(thresholds, "disk_watermark", volume["alias"], reason="volume_unavailable")
    measurements.append(sample)

    statuses = {row["status"] for row in measurements}
    status = "unavailable" if "unavailable" in statuses else "alert" if "alert" in statuses else "healthy"
    return {"status": status, "window": {"start": start.isoformat(), "end": end.isoformat(),
                                         "kafka_completed": observed_end.isoformat()},
            "measurements": measurements}


def main():
    parser = Parser(description=__doc__, epilog=HELP, formatter_class=argparse.RawDescriptionHelpFormatter,
                    allow_abbrev=False)
    parser.add_argument("--config", required=True, metavar="FILE")
    args = parser.parse_args()
    try:
        config, handles, kafka_config = load_config(args.config)
        result = run(config, handles, kafka_config)
    except InvalidConfig as exc:
        result = {"status": "unavailable", "reason": str(exc), "measurements": []}
    except (OSError, UnicodeError, ValueError, RecursionError):
        result = {"status": "unavailable", "reason": "config_or_probe_io", "measurements": []}
    except Exception:
        result = {"status": "unavailable", "reason": "probe_failed", "measurements": []}
    emit(result)
    return {"healthy": 0, "alert": 1, "unavailable": 2}[result["status"]]


if __name__ == "__main__":
    raise SystemExit(main())
