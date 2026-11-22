"""Provision and independently read back one disposable acq-browser fixture run."""

import argparse
from contextlib import closing
from datetime import date, datetime, timedelta, timezone
from hashlib import sha256
from ipaddress import IPv4Address
import json
import os
from pathlib import Path
import subprocess
import time

from botocore.exceptions import ClientError, EndpointConnectionError
import psycopg
from psycopg import sql
from psycopg.conninfo import conninfo_to_dict
from psycopg.rows import dict_row
from sqlalchemy import text
import yaml

from miaoqia.acquisition.api.browser_pairs import BROWSER_NOTICE_ARCHIVED, read_browser_pair
from miaoqia.acquisition.api.browser_projection import build_projection
from miaoqia.acquisition.api.handlers import FetchRequest
from miaoqia.acquisition.application import plans, sources
from miaoqia.kernel import db, reqctx
from miaoqia.kernel.objectstore import open_object_store, s3_client

from publisher import BODY, HOST, KEY, ROLE_DETAIL, ROLE_LIST, ROLE_SELECTOR, role_routes


SOURCE = "browser-fixture"
CHANNEL = "fixture"
PACKS = Path("/fixture-state/packs")
BUCKET = "acq-browser-fixture"
STORE = f"s3://{BUCKET}/role"
APP_DSN = "MIAOQIA_ACQUISITION_DB_DSN"
OWNER_DSN = "MIAOQIA_ACQUISITION_DB_OWNER_DSN"
ADMIN_DSN = "MQ_FIXTURE_PG_ADMIN_DSN"
LIST_VIEW, LIST_DATA, DETAIL_VIEW, DETAIL_DATA = (
    f"https://{HOST}/{route}/{endpoint}"
    for route in (ROLE_LIST, ROLE_DETAIL) for endpoint in ("view", "data")
)

HANDLER = '''\
from datetime import date
import json

from miaoqia.acquisition.api.handlers import BrowserDetailPlan, BrowserListPlan, DetailRef, FetchRequest, ListPage

DAY = date.fromisoformat(DAY_LABEL)


class Handler:
    source_id = SOURCE
    channels = (CHANNEL,)
    windowed = True
    contract_version = 1

    def browser_list(self, channel, window, page):
        if channel != CHANNEL or page != 1 or window.since != DAY or window.until != DAY:
            raise ValueError("unexpected fixture list scope")
        return BrowserListPlan(LIST_VIEW, FetchRequest("GET", LIST_DATA), SELECTOR)

    def parse_browser_list(self, channel, response, selection):
        value = json.loads(response.body.decode("utf-8"))
        if (channel != CHANNEL or response.url != LIST_DATA or response.body != LIST_BYTES
                or selection.html != LIST_HTML
                or value != {"key": KEY, "page": 1, "published_on": DAY.isoformat()}):
            raise ValueError("fixture list response and selection differ")
        return ListPage((DetailRef(value["key"], DETAIL_VIEW, DAY),), False, reported_page=value["page"])

    def browser_detail(self, channel, ref):
        if channel != CHANNEL or ref != DetailRef(KEY, DETAIL_VIEW, DAY):
            raise ValueError("unexpected fixture detail scope")
        return BrowserDetailPlan(DETAIL_VIEW, FetchRequest("GET", DETAIL_DATA), SELECTOR)

    def validate_browser_notice(self, ref, response, selection):
        if (ref != DetailRef(KEY, DETAIL_VIEW, DAY) or response.url != DETAIL_DATA
                or response.body != BODY or selection.html != DETAIL_HTML):
            raise ValueError("fixture detail response and selection differ")
        return DAY
'''


class FixtureError(ValueError):
    pass


def require(condition, stage):
    if not condition:
        raise FixtureError(stage)


def digest(data):
    return "sha256:" + sha256(data).hexdigest()


def selected_html(paragraph):
    return f'<article data-notice-id="{KEY}"><p>{paragraph}</p></article>'


def write_pack(day, list_bytes):
    pack = PACKS / "information_domains" / SOURCE
    (pack / "src").mkdir(parents=True)
    bindings = {
        "SOURCE": SOURCE, "CHANNEL": CHANNEL, "KEY": KEY, "SELECTOR": ROLE_SELECTOR,
        "LIST_VIEW": LIST_VIEW, "LIST_DATA": LIST_DATA, "DETAIL_VIEW": DETAIL_VIEW, "DETAIL_DATA": DETAIL_DATA,
        "LIST_BYTES": list_bytes, "LIST_HTML": selected_html(list_bytes.decode("utf-8")),
        "BODY": BODY, "DETAIL_HTML": selected_html(BODY.decode("cp1252").removesuffix("\r\n")),
        "DAY_LABEL": day.isoformat(),
    }
    declarations = "\n".join(f"{name} = {value!r}" for name, value in bindings.items())
    (pack / "src/browser_fixture.py").write_text(
        f"{declarations}\n\n{HANDLER}", encoding="utf-8",
    )
    (pack / "pack.yaml").write_text("pack: {provides: {sources: sources.yaml}}\n", encoding="utf-8")
    (pack / "source-inventory.yaml").write_text(
        f"sources: [{{source_id: {SOURCE}, readiness: capability_verified}}]\n", encoding="utf-8",
    )
    (pack / "sources.yaml").write_text(yaml.safe_dump({"sources": [{
        "source_id": SOURCE, "transport": "browser", "handler": "browser_fixture:Handler",
        "access": {"min_interval_seconds": 1, "throttle_backoff_seconds": 30, "max_attempts": 1},
        "schedule": {"every_minutes": 30, "lookback_days": 0, "max_pages": 1, "initial_backfill_days": 0},
    }]}), encoding="utf-8")


def create_role(connection, options):
    connection.execute(sql.SQL("CREATE ROLE {} LOGIN NOBYPASSRLS PASSWORD {}").format(
        sql.Identifier(options["user"]), sql.Literal(options["password"]),
    ))


def grant_app_defaults(connection, user):
    for grant in (
        "GRANT USAGE ON SCHEMA public TO {}",
        "ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO {}",
        "ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO {}",
    ):
        connection.execute(sql.SQL(grant).format(sql.Identifier(user)))


def provision(app_dsn):
    owner_dsn, admin_dsn = os.environ[OWNER_DSN], os.environ[ADMIN_DSN]
    admin, owner, app = (conninfo_to_dict(dsn) for dsn in (admin_dsn, owner_dsn, app_dsn))
    for options in (admin, owner, app):
        require({"host", "dbname", "user", "password"} <= options.keys(), "postgres_dsn")
        IPv4Address(options["host"])
        require("hostaddr" not in options and "service" not in options, "postgres_dsn")
    endpoint = (admin["host"], admin.get("port", "5432"))
    require(all((options["host"], options.get("port", "5432")) == endpoint for options in (owner, app))
            and owner["dbname"] == app["dbname"] != admin["dbname"]
            and len({admin["user"], owner["user"], app["user"]}) == 3, "postgres_dsn_scope")
    owner_role = sql.Identifier(owner["user"])
    database = sql.Identifier(app["dbname"])
    with psycopg.connect(admin_dsn, autocommit=True, connect_timeout=5) as connection:
        create_role(connection, owner)
        create_role(connection, app)
        connection.execute(sql.SQL("CREATE DATABASE {} OWNER {}").format(database, owner_role))
    with psycopg.connect(owner_dsn, autocommit=True, connect_timeout=5) as connection:
        grant_app_defaults(connection, app["user"])


def create_bucket():
    with closing(s3_client()) as client:
        deadline = time.monotonic() + 20
        while True:
            try:
                client.create_bucket(Bucket=BUCKET)
                return
            except ClientError as error:
                if error.response["ResponseMetadata"]["HTTPStatusCode"] not in (500, 502, 503, 504):
                    raise
            except EndpointConnectionError:
                pass
            if time.monotonic() >= deadline:
                raise TimeoutError("fixture bucket unavailable")
            time.sleep(0.25)


def materialize(app_dsn, now):
    declared = sources.load(PACKS, transport="browser")
    require(set(declared) == {SOURCE}, "source_pack")
    database = db.Database(app_dsn)
    try:
        with reqctx.use(reqctx.service("acquisition")), database.tx() as connection:
            require(plans.materialize(connection, declared[SOURCE], now) == 1, "materialize_count")
            return connection.execute(text("SELECT id FROM acq_collection_plans")).scalar_one()
    finally:
        database.close()


def settled_plan(connection, day):
    plan, = connection.execute("""
        SELECT p.id, p.mode, p.window_start, p.window_end, p.max_pages, p.run_id,
               p.settled_at, p.settlement_reason, r.finished_at, r.report
        FROM acq_collection_plans p LEFT JOIN acq_collection_runs r ON r.id = p.run_id
        WHERE p.source_id = %s
    """, (SOURCE,)).fetchall()
    if plan["settled_at"] is None:
        return None
    require(plan["mode"] == "incremental" and plan["window_start"] == plan["window_end"] == day
            and plan["max_pages"] == 1 and plan["run_id"] is not None and plan["settlement_reason"] is None
            and plan["finished_at"] is not None, "completed_plan")
    report = plan["report"]
    require((report["pages"], report["listed"], report["archived"], report["failed"]) == (1, 1, 1, 0)
            and not any(report[key] for key in ("blocked", "truncated", "gaps", "deferred")), "completed_run_report")
    return plan


def verify_list(connection, store, plan, day, list_bytes):
    listing, = connection.execute("""
        SELECT p.*, a.attempted_at, d.task_key, t.fingerprint, t.last_attempt, t.state, t.scope
        FROM acq_discovery_pages p
        JOIN acq_fetch_attempts a ON a.id = p.attempt_id AND a.run_id = p.run_id
        JOIN acq_download_starts d ON d.id = a.download_id AND d.run_id = p.run_id
        JOIN acq_request_tasks t ON t.task_key = d.task_key AND t.run_id = p.run_id AND t.kind = 'list'
        WHERE p.run_id = %s AND a.source_id = %s
    """, (plan["run_id"], SOURCE)).fetchall()
    list_hash = digest(list_bytes)
    attempt_id = listing["attempt_id"]
    day_label = day.isoformat()
    expected_ref = f"discovery/{SOURCE}/{attempt_id}/{list_hash[7:]}"
    require(listing["page"] == 1 and listing["channel"] == CHANNEL and not listing["has_next"]
            and listing["window_start"] == listing["window_end"] == day
            and listing["refs"] == [{"notice_key": KEY, "source_url": DETAIL_VIEW, "published_on": day_label}]
            and listing["response_hash"] == list_hash and listing["object_ref"] == expected_ref
            and listing["expired_at"] is None and listing["expires_at"] is not None, "list_receipt")
    retention_days = 30
    capture_time = listing["expires_at"] - timedelta(days=retention_days)
    require(listing["attempted_at"] <= capture_time <= plan["finished_at"], "list_retention")

    binding = listing["browser_binding"]
    body_fingerprint = FetchRequest("GET", LIST_DATA).fingerprint()
    require(isinstance(binding, dict) and binding["version"] == 1 and binding["task_key"] == listing["task_key"]
            and listing["last_attempt"] == attempt_id and listing["state"] == "settled"
            and listing["scope"][:4] == [CHANNEL, day_label, day_label, 1]
            and binding["request_fingerprint"] == listing["fingerprint"] == body_fingerprint
            and binding["selection_hash"] == digest(selected_html(list_bytes.decode("utf-8")).encode("utf-8"))
            and binding["size_bytes"] == len(list_bytes) and binding["media_type"] == "application/json"
            and all(binding[key] for key in ("frame_id", "loader_id", "navigation_id", "request_id"))
            and binding["navigation_id"] != binding["request_id"], "list_browser_binding")
    attempts = connection.execute("""
        SELECT a.id, a.fingerprint, a.url, d.generation, d.id AS dispatch_id
        FROM acq_fetch_attempts a JOIN acq_download_starts d ON d.id = a.download_id
        WHERE a.id IN (%s, %s) AND a.run_id = %s AND a.source_id = %s
          AND d.run_id = a.run_id AND d.task_key = %s AND d.fingerprint = a.fingerprint
          AND a.method = 'GET' AND a.status = 200 AND a.outcome = 'ok'
        ORDER BY a.id
    """, (binding["navigation_attempt_id"], attempt_id, plan["run_id"], SOURCE, listing["task_key"])).fetchall()
    require([(row["id"], row["url"], row["fingerprint"]) for row in attempts] == [
        (binding["navigation_attempt_id"], LIST_VIEW, FetchRequest("GET", LIST_VIEW).fingerprint()),
        (attempt_id, LIST_DATA, body_fingerprint),
    ] and attempts[0]["generation"] == attempts[1]["generation"]
        and attempts[0]["dispatch_id"] < attempts[1]["dispatch_id"], "list_browser_attempts")
    require(store.get(expected_ref, max_bytes=len(list_bytes)) == list_bytes, "list_s3_bytes")
    return {
        "list_attempt_id": attempt_id, "list_navigation_attempt_id": binding["navigation_attempt_id"],
        "list_ref": expected_ref, "list_bytes": len(list_bytes), "list_retention_days": retention_days,
        "list_expires_at": listing["expires_at"].isoformat(), "list_selection_sha256": binding["selection_hash"][7:],
    }


def verify_detail(connection, store, plan, listing, day):
    rows = connection.execute("""
        SELECT seq, envelope, envelope::text AS wire, partition_key, published_at FROM kernel_outbox
        WHERE envelope->>'type' = %s AND envelope->'data'->>'source_id' = %s ORDER BY seq
    """, (BROWSER_NOTICE_ARCHIVED, SOURCE)).fetchall()
    require(len(rows) == 1, "source_browser_outbox_count")
    outbox = rows[0]
    event = outbox["envelope"]["data"]
    binding = event["binding"]
    require(event["source_id"] == SOURCE and event["notice_key"] == KEY and event["source_url"] == DETAIL_VIEW
            and event["published_on"] == day.isoformat() and event["observation_version"] == 1
            and outbox["envelope"]["subject"] == event["artifact_ref"] and outbox["published_at"] is None
            and outbox["partition_key"] == f"{SOURCE}|{KEY}"
            and binding["run_id"] == plan["run_id"] and binding["list_attempt_id"] == listing["list_attempt_id"]
            and binding["request_url"] == DETAIL_DATA
            and binding["request_fingerprint"] == FetchRequest("GET", DETAIL_DATA).fingerprint(), "browser_event_scope")
    observation, = connection.execute("""
        SELECT o.version, o.notice_key, o.attempt_id, o.source_url, o.published_on, o.channel, o.selector,
               r.run_id, r.list_attempt_id, r.task_key, r.payload, a.id AS artifact_id,
               a.source_key AS artifact_source_key, a.content_hash, a.artifact_ref, a.size_bytes, a.media_type
        FROM acq_source_observations o
        JOIN acq_browser_receipts r ON r.attempt_id = o.browser_attempt_id AND r.artifact_id = o.artifact_id
            AND r.source_id = o.source_id AND r.notice_key = o.notice_key
        JOIN acq_raw_artifacts a ON a.id = o.artifact_id AND a.source_id = o.source_id
        WHERE o.source_id = %s
    """, (SOURCE,)).fetchall()
    require(observation["notice_key"] == KEY and observation["source_url"] == DETAIL_VIEW
            and observation["published_on"] == day and observation["channel"] == CHANNEL
            and observation["selector"] is None and observation["version"] == event["observation_version"]
            and observation["artifact_source_key"] == f"browser:{KEY}"
            and observation["payload"] == event
            and all(observation[key] == binding[key] for key in ("attempt_id", "list_attempt_id", "run_id", "task_key"))
            and all(observation[key] == event[key] for key in ("content_hash", "artifact_ref", "size_bytes", "media_type")),
            "browser_observation_binding")
    raw, document = read_browser_pair(store, event)
    paragraph = BODY.decode("cp1252").removesuffix("\r\n")
    expected_projection = build_projection(selected_html(paragraph))
    projection = event["projection"]
    require(raw == BODY and event["media_type"] == "text/plain", "original_s3_bytes")
    require(projection["content_hash"] == digest(expected_projection)
            and [block["text"] for block in document["blocks"]] == [paragraph], "projection_s3_bytes")
    return {
        "outbox_count": len(rows), "outbox_type": BROWSER_NOTICE_ARCHIVED, "outbox_seq": outbox["seq"],
        "outbox_envelope_sha256": digest(outbox["wire"].encode("utf-8"))[7:],
        "observation_version": observation["version"], "detail_attempt_id": binding["attempt_id"],
        "artifact_id": observation["artifact_id"], "original_ref": event["artifact_ref"], "original_bytes": len(raw),
        "projection_ref": projection["artifact_ref"], "projection_bytes": projection["size_bytes"],
        "projection_sha256": projection["content_hash"][7:], "extraction_scope": "selected_browser_paragraph",
        "provider_field_scope": "synthetic_key_page_publication_date_and_detail_paragraph",
    }


def verify(app_dsn, day, list_bytes):
    deadline = time.monotonic() + 20
    with psycopg.connect(app_dsn, connect_timeout=5, autocommit=True, row_factory=dict_row,
                         options="-c statement_timeout=5000") as connection:
        while True:
            with connection.transaction():
                connection.execute("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY")
                plan = settled_plan(connection, day)
                if plan is not None:
                    store = open_object_store(STORE, read_only=True)
                    listing = verify_list(connection, store, plan, day, list_bytes)
                    detail = verify_detail(connection, store, plan, listing, day)
                    return {"plan_id": plan["id"], "run_id": plan["run_id"], **listing, **detail}
            if time.monotonic() >= deadline:
                raise TimeoutError("fixture plan unsettled")
            time.sleep(0.25)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("init", "verify"))
    command = parser.parse_args().command
    stage = "configuration"
    try:
        published_on = os.environ["MQ_FIXTURE_PUBLISHED_ON"]
        day = date.fromisoformat(published_on)
        require(os.environ["MIAOQIA_PACKS_DIR"] == str(PACKS)
                and os.environ["MIAOQIA_ACQUISITION_OBJECT_STORE_DIR"] == STORE, "fixture_paths")
        list_bytes = role_routes(published_on)[ROLE_LIST]["data"][0]
        app_dsn = os.environ[APP_DSN]
        fields = {"source_id": SOURCE, "notice_key": KEY, "published_on": published_on, "store": STORE,
                  "list_sha256": digest(list_bytes)[7:], "original_sha256": digest(BODY)[7:]}
        if command == "init":
            now = datetime.now(timezone.utc)
            require(now.astimezone(plans.CHINA).date() == day, "fixture_china_date")
            stage = "fixture_pack"
            write_pack(day, list_bytes)
            stage = "postgres_provision"
            provision(app_dsn)
            stage = "s3_bucket"
            create_bucket()
            stage = "migrate"
            subprocess.run(["mq-acquisition", "migrate", "--role", "acq-browser"], check=True, timeout=30,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            stage = "materialize"
            fields.update(kind="role_archive_init", plan_id=materialize(app_dsn, now), plan_count=1)
        else:
            stage = "archive_readback"
            fields.update(verify(app_dsn, day, list_bytes), kind="role_archive_verified")
    except Exception as error:
        print(json.dumps({"kind": "role_archive_error", "command": command,
                          "stage": str(error) if isinstance(error, FixtureError) else stage,
                          "error_type": type(error).__name__}, sort_keys=True), flush=True)
        return 1
    print(json.dumps(fields, sort_keys=True), flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
