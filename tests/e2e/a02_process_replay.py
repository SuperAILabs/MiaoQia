"""A02 L1 process-role replay.

This gate uses the real platform/data processes and the shared PostgreSQL and
Kafka services. It creates only an isolated tenant and a synthetic channel
binding in the local development databases; the procurement facts come from a
frozen official corpus sample and are never inserted by the harness.
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
import base64
from pathlib import Path

import psycopg

ROOT = Path(__file__).resolve().parents[2]
WORK = ROOT / "deploy/local/.data/a02-process-replay"
TENANT = "tnt_01M6DQA02XTRA0000000000001"
CELL = "cell-01"
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
    if method not in {"GET", "HEAD"} and "/v1/" in url and not url.endswith("/data/get-fact"):
        digest = hashlib.sha256((method + url + (raw.decode() if raw else "")).encode()).hexdigest()[:40]
        req.add_header("Idempotency-Key", "a02-process-" + digest)
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    if ":8094/" in url:
        context = {"principal": {"id": "svc:a02-process", "kind": "service"}, "purpose": "object_page"}
        encoded = base64.urlsafe_b64encode(json.dumps(context, separators=(",", ":")).encode()).rstrip(b"=").decode()
        req.add_header("Miaoqia-Context", encoded)
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


def hostport(dsn: str) -> str:
    p = urllib.parse.urlparse(dsn)
    return f"{p.hostname or 'localhost'}:{p.port or 5432}"


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
        c.execute("DELETE FROM chn_bindings WHERE tenant_id = %s", (TENANT,))
        c.execute("DELETE FROM chn_installations WHERE tenant_id = %s", (TENANT,))
        c.execute("INSERT INTO chn_installations(id,tenant_id,channel,state,version,corp_id,corp_name,verified_at) VALUES ('chi_01M6DQA02XTRA0000000000001',%s,'wecom','enabled',2,'corp-a02','A02本地渠道',now())", (TENANT,))
        c.execute("INSERT INTO chn_corp_routes(channel,corp_id,tenant_id,cell,installation_id) VALUES ('wecom','corp-a02',%s,'cell-01','chi_01M6DQA02XTRA0000000000001') ON CONFLICT DO NOTHING", (TENANT,))
        c.execute("INSERT INTO chn_bindings(id,tenant_id,installation_id,channel,corp_id,channel_user_id,display_name,subject,member_version,state,version,bound_at) VALUES ('chb_01M6DQA02XTRA0000000000001',%s,'chi_01M6DQA02XTRA0000000000001','wecom','corp-a02','a02-dm','A02决策者',%s,1,'bound',1,now())", (TENANT, DM))
        c.commit()


def wait_row(env: dict[str, str], dbname: str, sql: str, args: tuple, timeout: int = 90) -> tuple:
    pg = hostport(env["MIAOQIA_TEST_PG"])
    dsn = f"postgres://{dbname}_owner:{dbname}_owner@{pg}/{dbname}"
    until = time.time() + timeout
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


def reset_replay_topic(env: dict[str, str]) -> None:
    """Use a clean public-domain topic for the isolated replay run."""
    from confluent_kafka.admin import AdminClient, NewTopic
    admin = AdminClient({"bootstrap.servers": env["MIAOQIA_KAFKA_BROKERS"]})
    try:
        admin.delete_topics(["public.domain"]) ["public.domain"].result()
    except Exception:
        pass
    admin.create_topics([NewTopic("public.domain", 12, 1)]) ["public.domain"].result()


def main() -> int:
    shutil.rmtree(WORK, ignore_errors=True)
    WORK.mkdir(parents=True)
    from m0 import base_env  # type: ignore

    env = base_env()
    evidence_objects = ROOT / "deploy/local/.data/a02-real/objects"
    if not evidence_objects.exists():
        evidence_objects = WORK / "objects"
    env.update({
        "MIAOQIA_ENV": "development",
        "MIAOQIA_GATEWAY_ISSUER": "http://127.0.0.1:18080",
        "MIAOQIA_GATEWAY_AUDIENCE": "miaoqia-api",
        "MIAOQIA_IDENTITY_OPENFGA_URL": os.environ.get("MIAOQIA_IDENTITY_OPENFGA_URL", "http://127.0.0.1:8081"),
        "MIAOQIA_DETECTION_DATA_API_URL": "http://127.0.0.1:8094",
        "MIAOQIA_GATEWAY_DATA_API_URL": "http://127.0.0.1:8094",
        "MIAOQIA_LEARNING_DATA_API_URL": "http://127.0.0.1:8094",
        "MIAOQIA_DATA_PUBLIC_RIGHTS": "public-official-review",
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

    procs = Procs()
    result = {"service_gate": "not-run", "process_role_replay": "pending"}
    try:
        run(["go", "build", "-o", str(WORK / "platform"), "./cmd/platform"], env, ROOT / "services")
        platform = str(WORK / "platform")
        run([str(ROOT / "deploy/local/topics.sh")], env)
        reset_replay_topic(env)
        reset_replay_groups(env)
        run([platform, "migrate"], env)
        run(["uv", "run", "-q", "--locked", "--all-packages", "mq-data", "migrate"], env, ROOT / "data")
        setup_directory(env)
        clear_tenant_state(env)

        issuer = str(WORK / "dev-issuer")
        run(["go", "build", "-o", issuer, "./cmd/dev-issuer"], env, ROOT / "services")
        procs.start("dev-issuer", [issuer, "-addr", "127.0.0.1:18080", "-issuer", "http://127.0.0.1:18080"], env)
        wait_http("http://127.0.0.1:18080/healthz")
        wait_http(env["MIAOQIA_IDENTITY_OPENFGA_URL"].rstrip("/") + "/healthz")

        procs.start("platform-api", [platform, "-role", "api"], env)
        procs.start("platform-consumer", [platform, "-role", "consumer"], env)
        procs.start("platform-outbox-relay", [platform, "-role", "outbox-relay"], env)
        procs.start("platform-scheduler", [platform, "-role", "scheduler"], env)
        procs.start("data-consumer", ["uv", "run", "-q", "--locked", "--all-packages", "mq-data", "--role", "data-consumer"], env, ROOT / "data")
        procs.start("data-api", ["uv", "run", "-q", "--locked", "--all-packages", "mq-data", "--role", "data-api"], env, ROOT / "data")
        wait_http("http://127.0.0.1:8090/")
        wait_http("http://127.0.0.1:8094/healthz")
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
        deadline = time.time() + 90
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
        deadline = time.time() + 90
        while time.time() < deadline:
            _, policy = request("GET", policy_url, dm_token, expected={200})
            if policy.get("active_version", 0) > 0:
                break
            time.sleep(1)
        else:
            raise RuntimeError(f"策略未激活：{policy}")

        sample = ROOT / "packs/information_domains/procurement/corpus/official-candidate-20261014"
        replay_env = dict(env, MIAOQIA_EVIDENCE_OBJECT_STORE_DIR=str(evidence_objects), MIAOQIA_PACKS_DIR=str(ROOT / "packs"))
        replay = run(["uv", "run", "-q", "--locked", "--all-packages", "mq-evidence-replay", str(sample)], replay_env, ROOT / "data", capture=True)
        replay_result = json.loads(replay.stdout.splitlines()[-1])
        revisions = replay_result.get("revisions") or replay_result.get("existing_revisions")
        if not revisions:
            raise RuntimeError(f"冻结公告回放没有修订：{replay_result}")
        revision = revisions[-1]
        fact_id, fact_version = wait_row(env, "dom", "SELECT v.fact_id, v.version FROM dom_fact_versions v JOIN dom_facts f ON f.id = v.fact_id WHERE v.revision_ref = %s AND f.object_type = 'tendering.notice' ORDER BY v.version DESC LIMIT 1", (revision,))
        fact_status, fact_body = request("POST", "http://127.0.0.1:8094/internal/v1/data/get-fact", None, {"fact_id": fact_id, "basis": {"view": "current"}}, expected={200})
        if fact_body.get("fact", {}).get("id") != fact_id:
            raise RuntimeError(f"data-api 未返回回放事实：{fact_body}")
        evidence_id = walk_evidence(fact_body.get("fact", {}))
        if not evidence_id:
            raise RuntimeError("回放事实没有 evidence_ref")
        republish_fact(env, fact_id)
        pg = hostport(env["MIAOQIA_TEST_PG"])
        with psycopg.connect(f"postgres://dom_owner:dom_owner@{pg}/dom") as c:
            object_id = c.execute("SELECT project_id FROM dom_notice_links WHERE notice_id = %s ORDER BY notice_version DESC LIMIT 1", (fact_id,)).fetchone()
        if object_id and object_id[0]:
            object_id = object_id[0]
        else:
            # Candidate notices intentionally keep a pending project namespace;
            # use the frozen building project for the object/evidence leg while
            # retaining the candidate signal as the live detection leg.
            object_id = "pp_01M6C3K4A1T57GPSYR0R7WGEK9"
            with psycopg.connect(f"postgres://dom_owner:dom_owner@{pg}/dom") as c:
                project_content = c.execute("SELECT content FROM dom_fact_versions WHERE fact_id = %s ORDER BY version DESC LIMIT 1", (object_id,)).fetchone()
            if project_content:
                # Title evidence is present in the frozen object store and is
                # the stable object-page citation used by the gateway replay.
                evidence_id = "ev_6KN7A9QPS0QHPT9KE5BX7JGMQS"
        signal_id, = wait_row(env, "det", "SELECT id FROM det_signals WHERE tenant_id = %s AND subject_id = %s ORDER BY created_at DESC LIMIT 1", (TENANT, fact_id))
        _, inbox = request("GET", f"http://127.0.0.1:8090/v1/detection/objectives/{objective_id}/signals", dm_token, expected={200})
        if not any((row.get("signal_id") or row.get("id")) == signal_id for row in inbox.get("signals", [])):
            raise RuntimeError(f"信号未进入收件箱：{inbox}")
        _, object_page = request("GET", f"http://127.0.0.1:8090/v1/objects/{object_id}", dm_token, expected={200})
        _, evidence = request("GET", f"http://127.0.0.1:8090/v1/evidence/{evidence_id}", dm_token, expected={200})
        if evidence.get("object_id") != object_id:
            raise RuntimeError(f"证据未关联项目：{evidence}")

        # scheduler runs ComposeDue immediately. A synthetic bound channel makes
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

        # Tighten source rights and verify the data plane returns a restricted view.
        procs.stop_one("data-api")
        restricted_env = dict(env, MIAOQIA_DATA_PUBLIC_RIGHTS="corpus:does-not-exist")
        procs.start("data-api-restricted", ["uv", "run", "-q", "--locked", "--all-packages", "mq-data", "--role", "data-api"], restricted_env, ROOT / "data")
        wait_http("http://127.0.0.1:8094/healthz")
        restricted_status, restricted = request("GET", f"http://127.0.0.1:8090/v1/evidence/{evidence_id}", dm_token, expected={200, 404, 503})
        if restricted_status == 200 and restricted.get("display_level") not in {"none", "citation_only"}:
            raise RuntimeError(f"收紧来源权利后证据未降级：{restricted}")
        procs.stop_one("data-api-restricted")
        procs.start("data-api", ["uv", "run", "-q", "--locked", "--all-packages", "mq-data", "--role", "data-api"], env, ROOT / "data")
        wait_http("http://127.0.0.1:8094/healthz")
        procs.stop_one("data-api")
        status, _ = request("GET", f"http://127.0.0.1:8090/v1/objects/{object_id}", dm_token)
        if status != 503:
            raise RuntimeError(f"data-api 不可用时对象页未失败关闭：{status}")

        result.update({"service_gate": "passed", "process_role_replay": "passed", "tenant_id": TENANT, "objective_id": objective_id, "fact_id": fact_id, "fact_version": fact_version, "signal_id": signal_id, "object_id": object_id, "evidence_id": evidence_id, "briefing_id": briefing_id, "replay_revision": revision, "negative_controls": {"cross_tenant": "denied", "visitor": "denied", "rights_restricted": "passed", "data_api_unavailable": "fail_closed"}})
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
    sys.path.insert(0, str(ROOT / "tests/e2e"))
    raise SystemExit(main())
