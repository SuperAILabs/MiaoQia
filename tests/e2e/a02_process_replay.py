"""A02 L1 process-role replay.

This gate uses the real platform/data/acquisition processes and the shared
PostgreSQL and Kafka services. data-api runs its mTLS listener with source-rights
admission through acq-admin; detection reads as a tenant-scoped monitoring
service and learning as a simulation service. It creates only an isolated
tenant, its source-rights entries and a synthetic channel binding in the local
development databases; the procurement facts come from the frozen official
building tender/award pair and are never inserted by the harness.
"""
from __future__ import annotations

import json
import hashlib
import os
import shutil
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

import psycopg

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tests/e2e"))
from data_plane import hostport  # noqa: E402
WORK = ROOT / "deploy/local/.data/a02-process-replay"
TENANT = "tnt_01M6DQA02XTRA0000000000001"
CELL = "cell-01"
# Event-driven steps wait long enough for a remote shared broker (consumer
# group joins over the VPS tunnel can take minutes); local runs finish early.
EVENT_WAIT = 300
DM, BD, APPROVER, VISITOR, OTHER = "a02-dm", "a02-bd", "a02-approver", "a02-visitor", "a02-other"


def run(cmd: list[str], env: dict[str, str], cwd: Path = ROOT, *, capture: bool = False) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, cwd=cwd, env=env, check=True, text=True, capture_output=capture)


class Procs:
    def __init__(self) -> None:
        self.items: dict[str, tuple[subprocess.Popen, object]] = {}

    def start(self, name: str, cmd: list[str], env: dict[str, str], cwd: Path = ROOT) -> None:
        log = (WORK / f"{name}.log").open("w", encoding="utf-8")
        p = subprocess.Popen(cmd, cwd=cwd, env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        self.items[name] = (p, log)

    def stop_one(self, name: str) -> None:
        item = self.items.get(name)
        if not item:
            return
        p, log = item
        if p.poll() is None:
            os.killpg(p.pid, signal.SIGTERM)
            try:
                p.wait(15)
            except subprocess.TimeoutExpired:
                os.killpg(p.pid, signal.SIGKILL)
        log.close()

    def check(self) -> None:
        for name, (p, _) in self.items.items():
            if p.poll() is not None:
                log = WORK / f"{name}.log"
                raise RuntimeError(f"进程 {name} 已退出（{p.returncode}）：{log}\n{log.read_text(encoding='utf-8')[-4000:]}")

    def stop(self) -> None:
        for name in list(self.items)[::-1]:
            self.stop_one(name)


def request(method: str, url: str, token: str | None = None, body: dict | None = None, *, expected: set[int] | None = None) -> tuple[int, dict]:
    raw = None if body is None else json.dumps(body, ensure_ascii=False).encode()
    req = urllib.request.Request(url, method=method, data=raw)
    req.add_header("User-Agent", "miaoqia-a02-process-replay/1")
    if raw is not None:
        req.add_header("Content-Type", "application/json")
    if method not in {"GET", "HEAD"} and "/v1/" in url:
        digest = hashlib.sha256((method + url + (raw.decode() if raw else "")).encode()).hexdigest()[:40]
        req.add_header("Idempotency-Key", "a02-process-" + digest)
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            status, data = r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        status, data = e.code, json.loads(e.read() or b"{}")
    if expected is not None and status not in expected:
        raise RuntimeError(f"{method} {url} 返回 {status}：{data}")
    return status, data


def wait_http(url: str, seconds: int = 60) -> None:
    until = time.time() + seconds
    while time.time() < until:
        try:
            urllib.request.urlopen(url, timeout=2)
            return
        except urllib.error.HTTPError:
            return
        except (urllib.error.URLError, OSError):
            time.sleep(0.5)
    raise RuntimeError(f"{url} 未就绪")


def token(issuer: str, subject: str, *, tenant: str = TENANT, mfa: bool = False) -> str:
    body = urllib.parse.urlencode({"subject": subject, "tenant": tenant, "cell": CELL, "mfa": "1" if mfa else "0"}).encode()
    req = urllib.request.Request(issuer.rstrip("/") + "/token", method="POST", data=body)
    with urllib.request.urlopen(req, timeout=5) as r:
        return json.loads(r.read())["access_token"]


def setup_directory(env: dict[str, str]) -> None:
    pg = hostport(env["MIAOQIA_TEST_PG"])
    dsn = f"postgres://iam_owner:iam_owner@{pg}/iam"
    with psycopg.connect(dsn) as c:
        other_tenant = "tnt_01M6DQA02XTRB0000000000001"
        c.execute("DELETE FROM identity_resources WHERE tenant_id IN (%s, %s)", (TENANT, other_tenant))
        c.execute("DELETE FROM identity_approvals WHERE tenant_id IN (%s, %s)", (TENANT, other_tenant))
        c.execute("DELETE FROM identity_channel_revocations WHERE tenant_id IN (%s, %s)", (TENANT, other_tenant))
        c.execute("DELETE FROM identity_channel_sessions WHERE tenant_id IN (%s, %s)", (TENANT, other_tenant))
        c.execute("DELETE FROM identity_commands WHERE tenant_id IN (%s, %s)", (TENANT, other_tenant))
        c.execute("DELETE FROM identity_invitation_unit_roles WHERE tenant_id IN (%s, %s)", (TENANT, other_tenant))
        c.execute("DELETE FROM identity_invitations WHERE tenant_id IN (%s, %s)", (TENANT, other_tenant))
        c.execute("DELETE FROM identity_tenant_bindings WHERE tenant_id IN (%s, %s)", (TENANT, other_tenant))
        c.execute("DELETE FROM identity_member_unit_roles WHERE tenant_id IN (%s, %s)", (TENANT, other_tenant))
        c.execute("DELETE FROM identity_org_units WHERE tenant_id IN (%s, %s)", (TENANT, other_tenant))
        c.execute("DELETE FROM identity_members WHERE tenant_id IN (%s, %s)", (TENANT, other_tenant))
        c.execute("DELETE FROM identity_tenants WHERE id IN (%s, %s)", (TENANT, other_tenant))
        c.execute("INSERT INTO identity_tenants(id,name,cell,state,version) VALUES (%s,'A02进程回放','cell-01','active',1),(%s,'其他租户','cell-01','active',1)", (TENANT, other_tenant))
        c.execute("INSERT INTO identity_members(tenant_id,subject,state,roles,version) VALUES (%s,%s,'active',ARRAY['admin','decision_maker','domain_expert'],1),(%s,%s,'active',ARRAY['business_developer'],1),(%s,%s,'active',ARRAY['decision_maker'],1),(%s,%s,'active',ARRAY[]::text[],1),(%s,%s,'active',ARRAY['admin'],1)", (TENANT, DM, TENANT, BD, TENANT, APPROVER, TENANT, VISITOR, other_tenant, OTHER))
        c.commit()


def clear_tenant_state(env: dict[str, str]) -> None:
    """Remove the previous isolated replay rows before creating a new run."""
    pg = hostport(env["MIAOQIA_TEST_PG"])
    with psycopg.connect(f"postgres://ctx_owner:ctx_owner@{pg}/ctx") as c:
        for table in ("ctx_policy_activations", "ctx_policy_requests", "ctx_policy_versions", "ctx_policy_simulations", "ctx_policy_revisions", "ctx_policies", "ctx_assignment_rules", "ctx_commands", "ctx_snapshots", "ctx_profile_versions", "ctx_contexts", "ctx_objectives"):
            c.execute(f"DELETE FROM {table} WHERE tenant_id = %s", (TENANT,))
        c.commit()
    with psycopg.connect(f"postgres://sea_owner:sea_owner@{pg}/sea") as c:
        c.execute("DELETE FROM sea_watch_queries WHERE tenant_id = %s", (TENANT,)); c.commit()
    with psycopg.connect(f"postgres://det_owner:det_owner@{pg}/det") as c:
        for table in ("det_signal_batch_items", "det_signal_batches", "det_signal_updates", "det_feedback", "det_pursuit_links", "det_triage_attempts", "det_triage_ops", "det_view_snapshots", "det_signals"):
            c.execute(f"DELETE FROM {table} WHERE tenant_id = %s", (TENANT,))
        c.commit()
    # scheduler.ComposeDue only considers bound channel members. This binding is
    # a local channel bootstrap; the production WeCom adapter remains an L2 gate.
    chn = f"postgres://chn_owner:chn_owner@{pg}/chn"
    with psycopg.connect(chn) as c:
        # Briefings are one per subject and period: clear the replay tenant's
        # briefing state so each run composes its own briefing.
        for table in ("chn_deliveries", "chn_briefing_links", "chn_card_links", "chn_notices", "chn_briefing_windows", "chn_compose_marks", "chn_briefings", "chn_briefing_preferences"):
            c.execute(f"DELETE FROM {table} WHERE tenant_id = %s", (TENANT,))
        # The default schedule (07:30 on weekdays) composes nothing before the
        # send time or at weekends. A 00:00 daily schedule makes today's period
        # due whenever the replay runs; the API would only apply it from the
        # next send boundary.
        c.execute("INSERT INTO chn_briefing_preferences(tenant_id,subject,version,send_time,timezone,send_days,quiet_start,quiet_end,push_empty,effective_from,created_at) VALUES (%s,%s,1,'00:00','Asia/Shanghai',ARRAY[1,2,3,4,5,6,7],'21:00','07:00',true,'2026-01-01T00:00:00Z',now())", (TENANT, DM))
        c.execute("DELETE FROM chn_bindings WHERE tenant_id = %s", (TENANT,))
        c.execute("DELETE FROM chn_installations WHERE tenant_id = %s", (TENANT,))
        c.execute("INSERT INTO chn_installations(id,tenant_id,channel,state,version,corp_id,corp_name,verified_at) VALUES ('chi_01M6DQA02XTRA0000000000001',%s,'wecom','enabled',2,'corp-a02','A02本地渠道',now())", (TENANT,))
        c.execute("INSERT INTO chn_corp_routes(channel,corp_id,tenant_id,cell,installation_id) VALUES ('wecom','corp-a02',%s,'cell-01','chi_01M6DQA02XTRA0000000000001') ON CONFLICT DO NOTHING", (TENANT,))
        c.execute("INSERT INTO chn_bindings(id,tenant_id,installation_id,channel,corp_id,channel_user_id,display_name,subject,member_version,state,version,bound_at) VALUES ('chb_01M6DQA02XTRA0000000000001',%s,'chi_01M6DQA02XTRA0000000000001','wecom','corp-a02','a02-dm','A02决策者',%s,1,'bound',1,now())", (TENANT, DM))
        c.commit()


def wait_row(env: dict[str, str], dbname: str, sql: str, args: tuple, timeout: int | None = None) -> tuple:
    pg = hostport(env["MIAOQIA_TEST_PG"])
    dsn = f"postgres://{dbname}_owner:{dbname}_owner@{pg}/{dbname}"
    until = time.time() + (timeout or EVENT_WAIT)
    while time.time() < until:
        with psycopg.connect(dsn) as c:
            row = c.execute(sql, args).fetchone()
        if row is not None:
            return row
        time.sleep(1)
    raise RuntimeError(f"等待 {dbname} 查询超时：{sql}")


def walk_evidence(value: object) -> str | None:
    if isinstance(value, dict):
        for k, v in value.items():
            if k == "evidence_ref" and isinstance(v, str) and v.startswith("ev_"):
                return v
            found = walk_evidence(v)
            if found:
                return found
    elif isinstance(value, list):
        for v in value:
            found = walk_evidence(v)
            if found:
                return found
    return None


def republish_fact(env: dict[str, str], fact_id: str) -> None:
    """Emit a fresh public-domain event for an already frozen immutable fact.

    The replay command is idempotent and may return an existing revision. A new
    envelope is therefore required to prove the live detection consumer path;
    the fact payload itself remains the frozen data-plane record.
    """
    pg = hostport(env["MIAOQIA_TEST_PG"])
    event_id = "evt_" + f"{time.time_ns():026X}"[-26:]
    with psycopg.connect(f"postgres://dom_owner:dom_owner@{pg}/dom") as c:
        raw = c.execute("SELECT envelope FROM kernel_outbox WHERE envelope->>'type' = 'domain.fact.upserted.v1' AND envelope->'data'->>'fact_id' = %s ORDER BY seq DESC LIMIT 1", (fact_id,)).fetchone()
        if raw is None:
            raise RuntimeError(f"没有事实 {fact_id} 的 domain.fact.upserted.v1 发件箱记录")
        envelope = raw[0]
        envelope["id"] = event_id
        envelope["time"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        c.execute("INSERT INTO kernel_outbox(event_id, topic, partition_key, envelope) VALUES (%s, 'public.domain', %s, %s)", (event_id, envelope["subject"], json.dumps(envelope)))
        c.commit()
    # The outbox relay remains the authoritative path and marks this row
    # published. Publish the same immutable envelope once directly as a
    # bounded replay nudge so a pre-existing Kafka group offset cannot hide the
    # live consumer assertion in a shared development broker.
    from confluent_kafka import Producer
    producer = Producer({"bootstrap.servers": env["MIAOQIA_KAFKA_BROKERS"]})
    producer.produce("public.domain", key=envelope["subject"], value=json.dumps(envelope).encode())
    producer.flush(30)


def republish_revision(env: dict[str, str], revision: str) -> None:
    """Re-emit evidence.extraction.revised.v1 for a replayed revision.

    mq-evidence-replay is idempotent: a revision recorded by an earlier run
    returns as existing without a new event, and skip_existing_evidence moved
    the consumer group past earlier events. A fresh envelope through the outbox
    relay lets domain-intelligence derive the facts (idempotently) in this run.
    """
    pg = hostport(env["MIAOQIA_TEST_PG"])
    with psycopg.connect(f"postgres://evd_owner:evd_owner@{pg}/evd") as c:
        row = c.execute("SELECT topic, partition_key, envelope FROM kernel_outbox WHERE envelope->>'type' = 'evidence.extraction.revised.v1' AND envelope->'data'->>'revision_ref' = %s ORDER BY seq DESC LIMIT 1", (revision,)).fetchone()
        if row is None:
            raise RuntimeError(f"没有修订 {revision} 的 evidence.extraction.revised.v1 发件箱记录")
        topic, key, envelope = row
        envelope["id"] = "evt_" + f"{time.time_ns():026X}"[-26:]
        envelope["time"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        c.execute("INSERT INTO kernel_outbox(event_id, topic, partition_key, envelope) VALUES (%s, %s, %s, %s)", (envelope["id"], topic, key, json.dumps(envelope)))
        c.commit()


def reset_replay_groups(env: dict[str, str]) -> None:
    """Start this isolated replay from the current Kafka high-water mark."""
    from confluent_kafka.admin import AdminClient
    groups = [
        "detection.fact-matching", "detection.policy-watches", "domain-intelligence.facts",
        "customer-context.objective-participants", "customer-context.policy-approvals",
        "customer-context.policy-simulations", "learning.policy-simulations",
        "channels.member-changes", "pursuit.member-changes", "pursuit.objective-changes",
    ]
    admin = AdminClient({"bootstrap.servers": env["MIAOQIA_KAFKA_BROKERS"]})
    futures = admin.delete_consumer_groups(groups)
    for future in futures.values():
        try:
            future.result()
        except Exception:
            # Non-existent groups are the normal first-run case.
            pass


def skip_existing_evidence(env: dict[str, str]) -> None:
    """Commit the replay's evidence consumer group at the current end of public.evidence.

    reset_replay_groups deletes the replay groups, which would otherwise restart
    from the earliest offset and reprocess evidence events whose revisions no
    longer exist locally. Only this replay-owned group is touched.
    """
    from confluent_kafka import Consumer, TopicPartition
    consumer = Consumer({"bootstrap.servers": env["MIAOQIA_KAFKA_BROKERS"], "group.id": "domain-intelligence.facts",
                         "enable.auto.commit": False})
    try:
        metadata = consumer.list_topics("public.evidence", timeout=30).topics["public.evidence"]
        ends = []
        for partition in metadata.partitions:
            _, high = consumer.get_watermark_offsets(TopicPartition("public.evidence", partition), timeout=30)
            ends.append(TopicPartition("public.evidence", partition, high))
        consumer.commit(offsets=ends, asynchronous=False)
    finally:
        consumer.close()


def reset_replay_topic(env: dict[str, str]) -> None:
    """Use a clean public-domain topic for the isolated replay run."""
    from confluent_kafka.admin import AdminClient, NewTopic
    admin = AdminClient({"bootstrap.servers": env["MIAOQIA_KAFKA_BROKERS"]})
    try:
        admin.delete_topics(["public.domain"]) ["public.domain"].result()
    except Exception:
        pass
    admin.create_topics([NewTopic("public.domain", 12, 1)]) ["public.domain"].result()


# The rights allowlist (acquisition 0001/0002) covers only the reviewed
# building tender/award pair, so the replay uses that pair. The replay tenant
# gets its own @1 entries with all three purposes; the deploy manifest stays
# untouched. Fixed instants keep the immutable registration identical across
# runs.
RIGHTS_PAIR = (("building-tender", "corpus:official-building-tender-20261026"),
               ("building-award", "corpus:official-building-award-20261026"))
CORPUS = ROOT / "packs/information_domains/procurement/corpus"


def rights_batch() -> list[dict]:
    commands = []
    for entry, tag in RIGHTS_PAIR:
        commands += [
            {"action": "register", "policy": {
                "schema_version": 1, "entry_id": entry, "version": 1, "source_id": "official-qzygjy",
                "scopes": [{"tenant": TENANT, "cell": CELL}], "purposes": ["interactive", "monitoring", "simulation"],
                "display": "excerpt", "effective_at": "2026-10-26T00:00:00Z", "expires_at": "2027-12-31T00:00:00Z",
                "redistribution": False}},
            {"action": "bind", "source_id": "official-qzygjy", "tags": sorted([tag, "public-official-review"]),
             "entry_id": entry, "version": 1},
            {"action": "activate", "entry_id": entry, "version": 1},
        ]
    return commands


def wait_tcp(address: str, seconds: int = 60) -> None:
    import socket
    host, port = address.rsplit(":", 1)
    until = time.time() + seconds
    while time.time() < until:
        try:
            socket.create_connection((host, int(port)), timeout=1).close()
            return
        except OSError:
            time.sleep(0.5)
    raise RuntimeError(f"{address} 未就绪")


def user(subject: str, tenant: str = TENANT):
    from miaoqia.kernel import reqctx
    return reqctx.Context(reqctx.Principal(subject, "user"), tenant, CELL, "interactive")


def main() -> int:
    shutil.rmtree(WORK, ignore_errors=True)
    WORK.mkdir(parents=True)
    from m0 import base_env  # type: ignore
    import data_plane  # type: ignore

    env = base_env()
    evidence_objects = Path(os.environ.get("MIAOQIA_EVIDENCE_OBJECT_STORE_DIR") or ROOT / "deploy/local/.data/a02-objects")
    evidence_objects.mkdir(parents=True, exist_ok=True)
    env.update({
        "MIAOQIA_ENV": "development",
        "MIAOQIA_GATEWAY_ISSUER": "http://127.0.0.1:18080",
        "MIAOQIA_GATEWAY_AUDIENCE": "miaoqia-api",
        "MIAOQIA_IDENTITY_OPENFGA_URL": os.environ.get("MIAOQIA_IDENTITY_OPENFGA_URL", "http://127.0.0.1:8081"),
        "MIAOQIA_CUSTOMER_CONTEXT_PACKS_DIR": str(ROOT / "packs"),
        "MIAOQIA_DETECTION_PACKS_DIR": str(ROOT / "packs"),
        "MIAOQIA_LEARNING_PACKS_DIR": str(ROOT / "packs"),
        "MIAOQIA_CHANNELS_PACKS_DIR": str(ROOT / "packs"),
        "MIAOQIA_PURSUIT_PACKS_DIR": str(ROOT / "packs"),
        "MIAOQIA_COMMERCE_COVERAGE": "INF-PROC=*",
        "OTEL_EXPORTER_OTLP_ENDPOINT": "",
        "MIAOQIA_EVIDENCE_OBJECT_STORE_DIR": str(evidence_objects),
        "MIAOQIA_GATEWAY_CURSOR_KEY": "YTAyLXByb2Nlc3MtcmVwbGF5LWN1cnNvci1rZXktMzJieXRlcw==",
        "MIAOQIA_CHANNELS_SIGNING_KEY": "YTAyLXByb2Nlc3MtcmVwbGF5LWNoYW5uZWxzLWtleS0zMmJ5dGVz",
        "MIAOQIA_PURSUIT_SIGNING_KEY": "YTAyLXByb2Nlc3MtcmVwbGF5LXB1cnN1aXQta2V5LTMyYnl0ZXM=",
    })
    env["MIAOQIA_TEST_PG"] = os.environ.get("MIAOQIA_TEST_PG", "postgres://postgres@localhost:5432/postgres")
    env["MIAOQIA_DEPLOY_DIR"] = str(ROOT / "deploy")
    # The replay is intentionally bound to a configured broker; it must not
    # silently fall back to a local Docker broker.
    if not env.get("MIAOQIA_KAFKA_BROKERS"):
        raise RuntimeError("未配置 MIAOQIA_KAFKA_BROKERS；请提供共享 Kafka 地址")

    topology = data_plane.Topology(WORK)
    platform_env = {role: dict(env, **topology.platform(role)) for role in ("api", "consumer", "scheduler", "outbox-relay")}
    data_api_env = dict(env, **topology.data_api())
    harness = topology.harness()
    procs = Procs()
    result = {"service_gate": "not-run", "process_role_replay": "pending"}
    try:
        run(["go", "build", "-o", str(WORK / "platform"), "./cmd/platform"], env, ROOT / "services")
        platform = str(WORK / "platform")
        run([str(ROOT / "deploy/local/topics.sh")], env)
        reset_replay_topic(env)
        reset_replay_groups(env)
        skip_existing_evidence(env)
        run([platform, "migrate"], env)
        run(["uv", "run", "-q", "--locked", "--all-packages", "mq-data", "migrate"], env, ROOT / "data")
        data_plane.provision_rights(env, ROOT, topology)
        data_plane.apply_rights(env, ROOT, topology, rights_batch(), WORK / "rights-batch.json", "A02 process replay tenant rights")
        setup_directory(env)
        clear_tenant_state(env)

        issuer = str(WORK / "dev-issuer")
        run(["go", "build", "-o", issuer, "./cmd/dev-issuer"], env, ROOT / "services")
        procs.start("dev-issuer", [issuer, "-addr", "127.0.0.1:18080", "-issuer", "http://127.0.0.1:18080"], env)
        wait_http("http://127.0.0.1:18080/healthz")
        wait_http(env["MIAOQIA_IDENTITY_OPENFGA_URL"].rstrip("/") + "/healthz")

        procs.start("platform-api", [platform, "-role", "api"], platform_env["api"])
        procs.start("platform-consumer", [platform, "-role", "consumer"], platform_env["consumer"])
        procs.start("platform-outbox-relay", [platform, "-role", "outbox-relay"], platform_env["outbox-relay"])
        procs.start("acq-admin", ["uv", "run", "-q", "--locked", "--all-packages", "mq-acquisition", "--role", "acq-admin"],
                    dict(env, **topology.acq_admin(hostport(env["MIAOQIA_TEST_PG"]))), ROOT / "data")
        procs.start("data-consumer", ["uv", "run", "-q", "--locked", "--all-packages", "mq-data", "--role", "data-consumer"], env, ROOT / "data")
        procs.start("data-api", ["uv", "run", "-q", "--locked", "--all-packages", "mq-data", "--role", "data-api"], data_api_env, ROOT / "data")
        wait_http("http://127.0.0.1:8090/")
        wait_tcp(data_plane.PLATFORM_INTERNAL)
        wait_tcp(data_plane.RIGHTS_LISTEN)
        wait_tcp(data_plane.DATA_LISTEN)
        procs.check()

        dm_token = token(env["MIAOQIA_GATEWAY_ISSUER"], DM)
        approver_token = token(env["MIAOQIA_GATEWAY_ISSUER"], APPROVER, mfa=True)
        visitor_token = token(env["MIAOQIA_GATEWAY_ISSUER"], VISITOR)
        other_token = token(env["MIAOQIA_GATEWAY_ISSUER"], OTHER, tenant="tnt_01M6DQA02XTRB0000000000001")

        profile = {"subject_type": "organization", "entities": [{"org_id": "org_01JA8ZXXXXXXXXXXXXXXXXXXX1", "qualifications_complete": True}], "value_chain_roles": ["ROLE-MFR"], "industry_packs": ["IND-BUILDING-MATERIALS"], "offering_categories": [], "brands": ["示例品牌"], "service_regions": [], "qualifications": []}
        request("PUT", "http://127.0.0.1:8090/v1/customer-context/profile", dm_token, {"expected_version": 0, "content": profile}, expected={200})
        _, objective = request("POST", "http://127.0.0.1:8090/v1/customer-context/objectives", dm_token, {"template_id": "APP-TENDER.A02", "org_unit_id": "root", "participants": [BD]}, expected={200})
        objective_id = objective["id"]
        policy_url = f"http://127.0.0.1:8090/v1/customer-context/objectives/{objective_id}/policy"
        _, policy = request("GET", policy_url, dm_token, expected={200})
        active = policy.get("active") or {}
        draft_initial = policy.get("draft") or {}
        form = active.get("form") or draft_initial.get("form")
        if form is None:
            raise RuntimeError(f"A02 默认策略没有 active.form：{policy}")
        # The frozen official award carries winner evidence but does not claim
        # a normalized category or region. Keep those dimensions explicitly
        # unknown in this L1 replay and disable only their optional rules so a
        # real winner-result signal can still be evaluated.
        form["rules"] = [{"id": rule_id, "enabled": False} for rule_id in ("R-CAT", "R-REGION", "R-WINNER")]
        _, policy = request("PUT", policy_url + "/draft", dm_token, {"expected_version": policy["draft_version"], "form": form}, expected={200})
        deadline = time.time() + EVENT_WAIT
        while time.time() < deadline:
            _, policy = request("GET", policy_url, dm_token, expected={200})
            draft = policy.get("draft") or {}
            sim = draft.get("simulation") or {}
            if sim.get("status") == "completed":
                break
            time.sleep(1)
        else:
            raise RuntimeError(f"策略仿真未完成：{policy}")
        draft = policy["draft"]
        _, approval = request("POST", policy_url + "/activation-requests", token(env["MIAOQIA_GATEWAY_ISSUER"], DM, mfa=True), {"expected_version": policy["draft_version"], "revision": draft["revision"], "simulation_id": draft["simulation"]["simulation_id"], "note": "A02 process replay"}, expected={200})
        approval_id = approval["approval_id"]
        _, approval_state = request("GET", f"http://127.0.0.1:8090/v1/identity/approvals/{approval_id}", approver_token, expected={200})
        request("POST", f"http://127.0.0.1:8090/v1/identity/approvals/{approval_id}/decide", approver_token, {"expected_version": approval_state["version"], "decision": "approve"}, expected={200})
        deadline = time.time() + EVENT_WAIT
        while time.time() < deadline:
            _, policy = request("GET", policy_url, dm_token, expected={200})
            if policy.get("active_version", 0) > 0:
                break
            time.sleep(1)
        else:
            raise RuntimeError(f"策略未激活：{policy}")

        # Replay the reviewed building pair: the tender first, then the award
        # notice that carries the result units detection evaluates.
        replay_env = dict(env, MIAOQIA_EVIDENCE_OBJECT_STORE_DIR=str(evidence_objects), MIAOQIA_PACKS_DIR=str(ROOT / "packs"))
        revision = None
        for sample in ("official-building-tender-20261026", "official-building-award-20261026"):
            replay = run(["uv", "run", "-q", "--locked", "--all-packages", "mq-evidence-replay", str(CORPUS / sample)], replay_env, ROOT / "data", capture=True)
            replay_result = json.loads(replay.stdout.splitlines()[-1])
            revisions = replay_result.get("revisions") or replay_result.get("existing_revisions")
            if not revisions:
                raise RuntimeError(f"冻结公告 {sample} 回放没有修订：{replay_result}")
            revision = revisions[-1]
            if not replay_result.get("revisions"):
                republish_revision(env, revision)
        fact_id, fact_version = wait_row(env, "dom", "SELECT v.fact_id, v.version FROM dom_fact_versions v JOIN dom_facts f ON f.id = v.fact_id WHERE v.revision_ref = %s AND f.object_type = 'tendering.notice' ORDER BY v.version DESC LIMIT 1", (revision,))
        object_id, = wait_row(env, "dom", "SELECT project_id FROM dom_notice_links WHERE notice_id = %s AND project_id IS NOT NULL ORDER BY notice_version DESC LIMIT 1", (fact_id,))

        # The harness reads data-api over mTLS as the replay decision maker.
        status, fact_body = data_plane.read(harness, "/internal/v1/data/get-fact", {"fact_id": object_id, "basis": {"view": "current", "recorded_cutoff": None}, "version": None}, user(DM))
        if status != 200 or (fact_body or {}).get("fact", {}).get("id") != object_id:
            raise RuntimeError(f"data-api 未返回回放项目：{status} {fact_body}")
        evidence_id = walk_evidence(fact_body["fact"])
        if not evidence_id:
            raise RuntimeError("回放项目没有 evidence_ref")
        status, resolved = data_plane.read(harness, "/internal/v1/data/resolve-evidence", {"evidence_ref": evidence_id}, user(DM))
        if status != 200 or (resolved or {}).get("evidence", {}).get("id") != evidence_id:
            raise RuntimeError(f"data-api 未解析证据 {evidence_id}：{status} {resolved}")
        status, _ = data_plane.read(harness, "/internal/v1/data/get-fact", {"fact_id": object_id, "basis": {"view": "current", "recorded_cutoff": None}, "version": None}, user(OTHER, "tnt_01M6DQA02XTRB0000000000001"))
        if status != 403:
            raise RuntimeError(f"没有来源权利的租户读取未被拒绝：{status}")

        republish_fact(env, fact_id)
        signal_id, = wait_row(env, "det", "SELECT id FROM det_signals WHERE tenant_id = %s AND subject_id = %s ORDER BY created_at DESC LIMIT 1", (TENANT, fact_id))
        _, inbox = request("GET", f"http://127.0.0.1:8090/v1/detection/objectives/{objective_id}/signals", dm_token, expected={200})
        if not any((row.get("signal_id") or row.get("id")) == signal_id for row in inbox.get("signals", [])):
            raise RuntimeError(f"信号未进入收件箱：{inbox}")
        _, object_page = request("GET", f"http://127.0.0.1:8090/v1/objects/{object_id}", dm_token, expected={200})
        # The gateway evidence panel is not wired to data-api yet (A02 gap
        # D-11); it must fail closed until it is, and match the object after.
        evidence_status, evidence = request("GET", f"http://127.0.0.1:8090/v1/evidence/{evidence_id}", dm_token, expected={200, 503})
        if evidence_status == 200 and evidence.get("object_id") != object_id:
            raise RuntimeError(f"证据未关联项目：{evidence}")

        # The scheduler starts only now: its first ComposeDue runs immediately,
        # and a run before the objective and signal exist would mark today's
        # period as skipped (no audience) and never compose it.
        procs.start("platform-scheduler", [platform, "-role", "scheduler"], platform_env["scheduler"])
        # A synthetic bound channel makes
        # the in-app briefing observable without claiming the production WeCom adapter.
        briefing_id = wait_row(env, "chn", "SELECT id FROM chn_briefings WHERE tenant_id = %s AND subject = %s ORDER BY assembled_at DESC LIMIT 1", (TENANT, DM))[0]
        _, briefing = request("GET", f"http://127.0.0.1:8090/v1/channels/briefings/{briefing_id}", dm_token, expected={200})

        # Negative controls: another tenant and a member without A02 rights are denied.
        s, _ = request("GET", f"http://127.0.0.1:8090/v1/detection/objectives/{objective_id}/signals", other_token)
        if s not in {403, 404}:
            raise RuntimeError(f"跨租户对象访问没有拒绝：{s}")
        s, _ = request("GET", f"http://127.0.0.1:8090/v1/detection/objectives/{objective_id}/signals", visitor_token)
        if s not in {403, 404}:
            raise RuntimeError(f"visitor 收件箱访问没有拒绝：{s}")

        # Revoke the award rights: the object page closure includes the award,
        # so the data plane must stop serving it. The next run re-activates.
        data_plane.apply_rights(env, ROOT, topology, [{"action": "revoke", "entry_id": "building-award"}], WORK / "rights-revoke.json", "A02 process replay revocation control")
        restricted_status, _ = request("GET", f"http://127.0.0.1:8090/v1/objects/{object_id}", dm_token)
        if restricted_status == 200:
            raise RuntimeError("撤销来源权利后对象页仍返回内容")
        status, _ = data_plane.read(harness, "/internal/v1/data/get-fact", {"fact_id": object_id, "basis": {"view": "current", "recorded_cutoff": None}, "version": None}, user(DM))
        if status != 403:
            raise RuntimeError(f"撤销来源权利后 data-api 读取未被拒绝：{status}")
        procs.stop_one("data-api")
        status, _ = request("GET", f"http://127.0.0.1:8090/v1/objects/{object_id}", dm_token)
        if status != 503:
            raise RuntimeError(f"data-api 不可用时对象页未失败关闭：{status}")

        result.update({"service_gate": "passed", "process_role_replay": "passed", "tenant_id": TENANT, "objective_id": objective_id, "fact_id": fact_id, "fact_version": fact_version, "signal_id": signal_id, "object_id": object_id, "evidence_id": evidence_id, "briefing_id": briefing_id, "replay_revision": revision,
                       "data_api": {"transport": "mtls", "detection_read": "monitoring", "learning_read": "simulation", "harness_read": "interactive"},
                       "gateway_evidence_panel": "passed" if evidence_status == 200 else "fail_closed (D-11)",
                       "negative_controls": {"cross_tenant": "denied", "visitor": "denied", "rights_other_tenant": "denied", "rights_revoked": "denied", "object_page_after_revoke": restricted_status, "data_api_unavailable": "fail_closed"}})
        (WORK / "result.json").write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        print(json.dumps(result, ensure_ascii=False, indent=2))
        return 0
    except Exception as exc:
        result["error"] = str(exc)
        (WORK / "result.json").write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        raise
    finally:
        procs.stop()


if __name__ == "__main__":
    raise SystemExit(main())
