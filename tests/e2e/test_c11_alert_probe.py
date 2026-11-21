r"""Run from the repository root on macOS with a sanitized environment.

    env -i PATH="/path/to/postgresql/bin:/usr/bin:/bin:/usr/sbin:/sbin" \
      JAVA_HOME=/path/to/jdk C11_EXERCISE_KAFKA_HOME=/path/to/kafka \
      PYTHONDONTWRITEBYTECODE=1 data/.venv/bin/python -m pytest -q -s \
      -p no:cacheprovider tests/e2e/test_c11_alert_probe.py

The integration test owns its loopback servers, disk image, and PostgreSQL
locale. Unit controls also run without starting any services.
"""

from contextlib import ExitStack, contextmanager
from datetime import datetime
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace

import psycopg
from psycopg import errors, sql
import pytest
from confluent_kafka import Producer
from confluent_kafka.admin import AdminClient, NewTopic

from miaoqia.acquisition import MIGRATIONS
from miaoqia.kernel import db
from tools.dev import c11_alert_probe as probe


DUMMY_PASSWORD = "fixture-secret-marker-5c776"
DUMMY_ENVELOPE = "fixture-envelope-marker-d344a"
DUMMY_URL = "https://fixture.invalid/private-marker-c395d"
TOPIC = "dlq.c11-exercise"
THRESHOLDS = {"source_interruption": 60, "queue_backlog": 0, "error_rate": 0.25,
              "dead_letter": 0, "outbox_backlog": 0, "disk_watermark": 0.20}
ATTEMPT = """
INSERT INTO acq_fetch_attempts (source_id, fingerprint, method, url, outcome, duration_ms, attempted_at)
VALUES ('fixture-source', 'sha256:' || repeat('a', 64), 'GET', %s, %s, 1,
        clock_timestamp() - make_interval(secs => %s))
"""


def free_ports(count):
    with ExitStack() as stack:
        sockets = [stack.enter_context(socket.socket()) for _ in range(count)]
        for sock in sockets:
            sock.bind(("127.0.0.1", 0))
        return [sock.getsockname()[1] for sock in sockets]


def command(*args, env=None):
    return subprocess.run(args, env=env, check=True, capture_output=True, text=True, timeout=30)


@contextmanager
def disposable_postgres(root):
    port, = free_ports(1)
    data = root / "pg"
    host = "127.0.0.1"
    # Keep setup independent of any libpq configuration on the test host.
    env = {key: value for key, value in os.environ.items() if not key.startswith("PG")}
    # Darwin PostgreSQL needs a valid locale even under an empty environment.
    env.update(LC_ALL="en_US.UTF-8", LANG="en_US.UTF-8")
    command("initdb", "--username=postgres", "--auth-local=trust", "--auth-host=scram-sha-256", "-D", data, env=env)
    command("pg_ctl", "-w", "-D", data, "-l", root / "pg.log",
            "-o", f"-h {host} -p {port} -k {root} -c fsync=off", "start", env=env)
    try:
        with psycopg.connect(host=str(root), port=port, dbname="postgres", user="postgres", autocommit=True) as admin:
            for name in ("acq", "facts"):
                admin.execute(sql.SQL("CREATE DATABASE {}").format(sql.Identifier(name)))
                admin.execute(sql.SQL("CREATE ROLE {} LOGIN PASSWORD {}").format(
                    sql.Identifier("probe_" + name), sql.Literal(DUMMY_PASSWORD)))
        yield port
    finally:
        command("pg_ctl", "-w", "-D", data, "-m", "immediate", "stop", env=env)


@contextmanager
def disposable_kafka(root, kafka_home):
    port, controller = free_ports(2)
    address = f"127.0.0.1:{port}"
    brokers = {"bootstrap.servers": address, "log_level": 0, "socket.timeout.ms": 2000,
               "allow.auto.create.topics": False}
    config = root / "kafka.properties"
    config.write_text(f"""
process.roles=broker,controller
node.id=1
listeners=PLAINTEXT://{address},CONTROLLER://127.0.0.1:{controller}
advertised.listeners=PLAINTEXT://{address}
listener.security.protocol.map=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT
controller.listener.names=CONTROLLER
inter.broker.listener.name=PLAINTEXT
controller.quorum.voters=1@127.0.0.1:{controller}
log.dirs={root / 'kafka'}
auto.create.topics.enable=false
""")
    scripts = kafka_home / "bin"
    env = dict(os.environ, KAFKA_HEAP_OPTS="-Xms128m -Xmx256m", LOG_DIR=str(root / "kafka-logs"))
    cluster_id = command(scripts / "kafka-storage.sh", "random-uuid", env=env).stdout.strip()
    command(scripts / "kafka-storage.sh", "format", "-t", cluster_id, "-c", config, env=env)
    with (root / "broker.log").open("w") as log, subprocess.Popen(
            [scripts / "kafka-server-start.sh", config], env=env, stdout=log, stderr=subprocess.STDOUT) as broker:
        try:
            admin = AdminClient(brokers)
            cluster = admin.describe_cluster(request_timeout=20).result(21)
            assert cluster.cluster_id == cluster_id
            admin.create_topics([NewTopic(TOPIC, 2, 1)], request_timeout=20)[TOPIC].result(21)
            yield address, Producer(brokers)
        finally:
            broker.terminate()
            try:
                broker.wait(timeout=15)
            except subprocess.TimeoutExpired:
                broker.kill()
                broker.wait()


@contextmanager
def disposable_volume(root):
    mount = root / "mount"
    mount.mkdir()
    command("hdiutil", "create", "-size", "32m", "-fs", "HFS+", "-type", "SPARSE",
            "-volname", "MQC11Fixture", root / "disk")
    command("hdiutil", "attach", "-nobrowse", "-mountpoint", mount, root / "disk.sparseimage")
    try:
        yield mount
    finally:
        command("hdiutil", "detach", mount)


def prepare_databases(root, port, monkeypatch):
    for key in tuple(os.environ):
        if key.startswith("PG"):
            monkeypatch.delenv(key)
    scopes = {}
    for name in ("acq", "facts"):
        role = "probe_" + name
        env = "C11_TEST_" + name.upper()
        monkeypatch.setenv(env, f"postgresql://{role}:{DUMMY_PASSWORD}@127.0.0.1:{port}/{name}")
        scopes[name] = {"database": name, "role": role, "dsn_env": env}
        db.migrate(f"postgresql:///{name}?host={root}&port={port}&user=postgres",
                   MIGRATIONS if name == "acq" else None)
        with psycopg.connect(host=str(root), port=port, dbname=name, user="postgres") as conn:
            conn.execute(sql.SQL("GRANT USAGE ON SCHEMA public TO {}").format(sql.Identifier(role)))
            conn.execute(sql.SQL("GRANT SELECT (published_at) ON kernel_outbox TO {}").format(sql.Identifier(role)))
            if name == "acq":
                for grant in (
                    "acq_sources (source_id, state)", "acq_source_health (source_id, checked_at, last_success_at)",
                    "acq_collection_plans (source_id, mode, settlement_reason, settled_at, not_before)",
                    "acq_fetch_attempts (source_id, attempted_at, outcome)",
                ):
                    table, columns = grant.split(" ", 1)
                    conn.execute(sql.SQL("GRANT SELECT {} ON {} TO probe_acq").format(
                        sql.SQL(columns), sql.Identifier(table)))
    return scopes


def assert_probe_read_role(port):
    with psycopg.connect(host="127.0.0.1", port=port, dbname="acq", user="probe_acq",
                         password=DUMMY_PASSWORD, autocommit=True) as conn:
        for query in ("SELECT envelope FROM kernel_outbox", "SELECT url FROM acq_fetch_attempts",
                      "SELECT * FROM acq_raw_artifacts", "UPDATE acq_sources SET state = 'blocked'",
                      "INSERT INTO acq_sources (source_id) VALUES ('forbidden')",
                      "CREATE TABLE public.forbidden (n int)", "SET ROLE postgres"):
            with pytest.raises(errors.InsufficientPrivilege):
                conn.execute(query)


def test_c11_isolated_faults_and_recovery(monkeypatch, capsys):
    kafka_location = os.environ.get("C11_EXERCISE_KAFKA_HOME")
    if not kafka_location or sys.platform != "darwin" or not all(shutil.which(p) for p in ("initdb", "pg_ctl")):
        pytest.skip("isolated exercise needs Kafka distribution, PostgreSQL tools and macOS hdiutil")
    real_time = probe.time
    with tempfile.TemporaryDirectory(prefix="mq-c11-exercise-", dir="/tmp") as scratch, ExitStack() as stack:
        root = Path(scratch).resolve()
        port = stack.enter_context(disposable_postgres(root))
        bootstrap, producer = stack.enter_context(disposable_kafka(root, Path(kafka_location).resolve()))
        mount = stack.enter_context(disposable_volume(root))
        scopes = prepare_databases(root, port, monkeypatch)
        assert_probe_read_role(port)
        owner = {"host": str(root), "port": port, "user": "postgres", "autocommit": True}
        acq = stack.enter_context(psycopg.connect(dbname="acq", **owner))
        facts = stack.enter_context(psycopg.connect(dbname="facts", **owner))
        acq.execute("INSERT INTO acq_sources (source_id, handler, access) VALUES ('fixture-source', 'fixture', '{}')")
        acq.execute("""
            INSERT INTO acq_source_health (source_id, state, reason_code, checked_at, last_success_at, last_run)
            VALUES ('fixture-source', 'healthy', 'ok', now(), now(), '{}')
        """)
        acq.execute(ATTEMPT, (DUMMY_URL, "blocked", 60))
        acq.execute("""
            INSERT INTO acq_collection_plans
                (source_id, mode, occurrence_key, due_at, not_before, window_start, window_end,
                 max_pages, refetch, handler, config_fingerprint, budget_seconds)
            SELECT 'fixture-source', 'incremental', 'future', t, t, current_date, current_date,
                   1, false, 'fixture', repeat('a', 64), 60
            FROM (SELECT now() + interval '1 hour' AS t) due
        """)
        config = {
            "profile": "fixture", "window_seconds": 1, "thresholds": THRESHOLDS,
            "acquisition": "acq", "sources": {"source": "fixture-source"}, "databases": scopes,
            "kafka": {"bootstrap": bootstrap, "topics": {"test_dlq": {"topic": TOPIC, "partitions": [0, 1]}}},
            "volume": {"alias": "disk", "mount": str(mount), "fsid": os.statvfs(mount).f_fsid},
        }
        config_path = root / "config.json"
        monkeypatch.setattr(sys, "argv", ["c11_alert_probe.py", "--config", str(config_path)])
        results = []

        def sample(phase, *, failure=False, dlq=False, attempt=True, expected_alert=None, unavailable=None):
            def write_during_window(seconds):
                if attempt:
                    acq.execute(ATTEMPT, (DUMMY_URL, "ok", 0))
                if failure:
                    acq.execute(ATTEMPT, (DUMMY_URL, "http_error", 0))
                if dlq:
                    delivered = []
                    for partition in (0, 1, 1):
                        producer.produce(TOPIC, b"synthetic", partition=partition,
                                         on_delivery=lambda err, _msg: delivered.append(err))
                    assert producer.flush(4) == 0 and delivered == [None, None, None]
                real_time.sleep(seconds)

            monkeypatch.setattr(probe, "time", SimpleNamespace(monotonic=real_time.monotonic,
                                                              sleep=write_during_window))
            config_path.write_text(json.dumps(config))
            code = probe.main()
            stdout, stderr = capsys.readouterr()
            assert not stderr
            assert all(marker not in stdout for marker in (DUMMY_PASSWORD, DUMMY_ENVELOPE, DUMMY_URL, str(mount)))
            result = json.loads(stdout)
            window = result["window"]
            start, end = [datetime.fromisoformat(window[key]) for key in ("start", "end")]
            assert (end - start).total_seconds() == config["window_seconds"]
            by_key = {(m["metric"], m["scope"]): m for m in result["measurements"]}
            expected_status = "unavailable" if unavailable else "alert" if expected_alert else "healthy"
            assert (code, result["status"]) == ({"healthy": 0, "alert": 1, "unavailable": 2}[expected_status],
                                                expected_status), result
            assert len(by_key) == 7
            assert {key for key, m in by_key.items() if m["alert"] is True} == (
                {expected_alert} if expected_alert else set())
            missing = {key: m["reason"] for key, m in by_key.items() if m["status"] == "unavailable"}
            assert missing == (unavailable or {})
            assert all(by_key[key]["value"] is by_key[key]["alert"] is None for key in missing)
            results.append((phase, code, {"/".join(key): m["value"] for key, m in by_key.items()}))
            return by_key

        baseline = sample("baseline")
        assert baseline["queue_backlog", "source"]["value"] == 0
        assert baseline["error_rate", "source"]["value"] == 0
        acq.execute("UPDATE acq_source_health SET last_success_at = now() - interval '2 minutes'")
        interrupted = sample("interruption", expected_alert=("source_interruption", "source"))
        assert interrupted["source_interruption", "source"]["value"] >= 120
        acq.execute("UPDATE acq_source_health SET checked_at = now(), last_success_at = now()")

        acq.execute("""
            INSERT INTO acq_collection_plans
                (source_id, mode, occurrence_key, due_at, not_before, window_start, window_end,
                 max_pages, refetch, handler, config_fingerprint, budget_seconds)
            VALUES ('fixture-source', 'backfill', 'due', now(), now(), current_date, current_date,
                    1, false, 'fixture', repeat('a', 64), 60)
        """)
        queued = sample("queue", expected_alert=("queue_backlog", "source"))
        assert queued["queue_backlog", "source"]["value"] == 1
        acq.execute("""
            INSERT INTO acq_collection_runs
                (source_id, window_start, window_end, max_pages, refetch, started_at, finished_at, report, mode)
            VALUES ('fixture-source', current_date, current_date, 1, false, now(), now(), '{}', 'backfill')
        """)
        acq.execute("""
            UPDATE acq_collection_plans SET run_id = (SELECT id FROM acq_collection_runs), settled_at = now()
            WHERE occurrence_key = 'due'
        """)
        failed = sample("errors", failure=True, expected_alert=("error_rate", "source"))
        assert failed["error_rate", "source"]["value"] == 0.5

        dead = sample("dlq", dlq=True, expected_alert=("dead_letter", "test_dlq"))
        assert dead["dead_letter", "test_dlq"]["value"] == 3

        facts.execute("""
            INSERT INTO kernel_outbox (event_id, topic, partition_key, envelope)
            VALUES ('pending', 'public.fixture', 'key', jsonb_build_object('marker', %s::text))
        """, (DUMMY_ENVELOPE,))
        backlog = sample("outbox", expected_alert=("outbox_backlog", "facts"))
        assert backlog["outbox_backlog", "facts"]["value"] == 1
        assert backlog["outbox_backlog", "acq"]["value"] == 0
        facts.execute("UPDATE kernel_outbox SET published_at = now() WHERE event_id = 'pending'")

        filler = mount / "allocated"
        with filler.open("wb") as stream:
            block = os.urandom(1024 * 1024)
            for _ in range(10):
                stream.write(block)
            stream.flush()
            os.fsync(stream.fileno())
        full = sample("volume", expected_alert=("disk_watermark", "disk"))
        assert full["disk_watermark", "disk"]["value"] > 0.20
        filler.unlink()
        sample("recovered")

        config["kafka"]["topics"]["test_dlq"]["partitions"] = [0]
        sample("omitted_partition", unavailable={("dead_letter", "test_dlq"): "dlq_partition_scope"})
        config["kafka"]["topics"]["test_dlq"]["partitions"] = [0, 1]
        acq.execute("UPDATE acq_sources SET state = 'blocked', state_reason = 'fixture-restriction'")
        sample("blocked_and_empty", attempt=False, unavailable={
            ("source_interruption", "source"): "source_access_blocked", ("error_rate", "source"): "no_attempts",
        })
        acq.execute("UPDATE acq_sources SET state = 'active', state_reason = NULL")
        symlink = root / "symlink"
        symlink.symlink_to(mount, target_is_directory=True)
        config["volume"]["mount"] = str(symlink)
        sample("symlink_mount", unavailable={("disk_watermark", "disk"): "volume_unavailable"})
        config["volume"]["mount"] = str(root)
        sample("parent_filesystem", unavailable={("disk_watermark", "disk"): "volume_not_mount"})
        config["volume"]["mount"] = str(mount)
        config["volume"]["fsid"] += 1
        sample("mount_identity", unavailable={("disk_watermark", "disk"): "volume_identity_changed"})
        config["volume"]["fsid"] -= 1
        bad_dsn = os.environ["C11_TEST_FACTS"].replace(DUMMY_PASSWORD, DUMMY_PASSWORD + "-wrong")
        with monkeypatch.context() as temporary:
            temporary.setenv("C11_TEST_FACTS", bad_dsn)
            sample("postgres_auth", unavailable={("outbox_backlog", "facts"): "postgres_outbox_read_failed"})
        facts.execute("GRANT SELECT (envelope) ON kernel_outbox TO probe_facts")
        sample("postgres_excess_grant", unavailable={("outbox_backlog", "facts"): "postgres_privileges"})
        facts.execute("REVOKE SELECT (envelope) ON kernel_outbox FROM probe_facts")
        with facts.transaction():
            facts.execute("LOCK TABLE kernel_outbox IN ACCESS EXCLUSIVE MODE")
            began = real_time.monotonic()
            sample("postgres_lock", unavailable={("outbox_backlog", "facts"): "postgres_outbox_read_failed"})
            assert real_time.monotonic() - began < 8
        for phase, code, values in results:
            print(json.dumps({"phase": phase, "exit": code, "values": values}, sort_keys=True))


@pytest.mark.parametrize("arguments,config,reason", [
    (["--strange;$(touch C11_BAD)"], None, "arguments"),
    ([], '{"profile":"fixture","profile":"shared"}', "duplicate_key"),
])
def test_invalid_input_is_sanitized_before_io(tmp_path, monkeypatch, capsys, arguments, config, reason):
    def unexpected(*_args, **_kwargs):
        pytest.fail("invalid config initiated backend I/O")

    monkeypatch.setattr(probe, "run", unexpected)
    if config:
        path = tmp_path / "config"
        path.write_text(config)
        arguments = ["--config", str(path)]
    monkeypatch.setattr(sys, "argv", ["c11_alert_probe.py", *arguments])
    if config:
        assert probe.main() == 2
    else:
        with pytest.raises(SystemExit) as exc:
            probe.main()
        assert exc.value.code == 2
    stdout, stderr = capsys.readouterr()
    assert not stderr
    assert json.loads(stdout) == {"status": "unavailable", "reason": reason, "measurements": []}


@pytest.mark.parametrize("identity,offsets,reason", [
    ("replacement", [(0, 14, 1)], "dlq_identity_changed"),
    ("original", [(0, 9, 1)], "dlq_offset_discontinuity"),
    ("original", [(11, 14, 1)], "dlq_offset_discontinuity"),
    ("original", [(0, 14, 2)], "dlq_offset_discontinuity"),
])
def test_untrusted_offset_delta_is_unavailable(identity, offsets, reason):
    result, = probe.kafka_measurements(THRESHOLDS, ["dlq"], {"dlq": ("original", [(0, 10, 1)])},
                                      {"dlq": (identity, offsets)}, True)
    assert result["status"] == "unavailable"
    assert result["reason"] == reason
    assert result["value"] is result["alert"] is None
