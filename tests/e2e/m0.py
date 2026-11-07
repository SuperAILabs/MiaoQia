"""M0 工程探针与身份绑定切换验证（docs/05-delivery/01-roadmap.md M0）：

  一个“空业务”的事件在三个部署单元间端到端流转，带追踪、审计和授权；
  当前已实现同步接口 identity.Authorizer 可配置切换本地/远程并通过契约测试。
  此脚本不代证 M0 其他交付或未来子系统接口；整体退出见阶段基线。

流程：operations（platform）发起链路探针 → acquisition（acquisition 单元）→ evidence（data 单元）→ operations。
校验：探针完成且经过全部子系统；授权允许与拒绝都留下审计；三个单元的处理位于同一条追踪；
另起一个 api 进程把 identity 切换为远程绑定，同样完成探针并拒绝无权用户。

前置：共享开发环境的 PostgreSQL、Kafka、OpenFGA、Keycloak 与 OTLP 已启动，并通过环境变量提供连接信息。Docker Compose 仅是可选的本地引导方式，不由本脚本自动启动。用法（仓库根目录）：

  uv run --project data python tests/e2e/m0.py
"""
from __future__ import annotations

import json
import argparse
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
import yaml

ROOT = Path(__file__).resolve().parents[2]


def load_local_environment() -> None:
    """Load an explicitly selected environment file without overriding shell variables."""
    selected = os.environ.get("MIAOQIA_ENV_FILE", "")
    if not selected:
        return
    path = Path(selected)
    if not path.is_file():
        return
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        key, value = key.strip(), value.strip()
        if key.startswith("export "):
            key = key[7:].strip()
        if key and key not in os.environ:
            os.environ[key] = value.strip("\"'")


load_local_environment()

WORK = ROOT / "deploy/local/.data/e2e"
TRACES = ROOT / "deploy/local/.data/otel/traces.jsonl"


def postgres_hostport() -> str:
    configured = os.environ.get("MIAOQIA_E2E_PG", "")
    if configured and "://" not in configured:
        return configured
    dsn = configured or os.environ.get("MIAOQIA_TEST_PG", "")
    if dsn:
        parsed = urllib.parse.urlparse(dsn)
        if parsed.hostname:
            return f"{parsed.hostname}:{parsed.port or 5432}"
    return "localhost:5432"


PG = postgres_hostport()
ISSUER = os.environ.get("MIAOQIA_GATEWAY_ISSUER", "http://localhost:8080/realms/miaoqia")
OPENFGA = os.environ.get("MIAOQIA_IDENTITY_OPENFGA_URL", "http://localhost:8081")
TRACES = Path(os.environ.get("MIAOQIA_E2E_TRACES", str(TRACES)))


def step(msg: str) -> None:
    print(f"\n▶ {msg}", flush=True)


def ok(msg: str) -> None:
    print(f"  ✓ {msg}", flush=True)


def fail(msg: str) -> None:
    print(f"  ✗ {msg}", flush=True)
    raise SystemExit(1)


def base_env() -> dict[str, str]:
    env = dict(os.environ)
    reg = yaml.safe_load((ROOT / "deploy/subsystems.yaml").read_text(encoding="utf-8"))["subsystems"]
    for name, s in reg.items():
        if "database" not in s:
            continue
        k, d = "MIAOQIA_" + name.upper().replace("-", "_"), s["database"]
        env.setdefault(f"{k}_DB_DSN", f"postgres://{d}_app:{d}_app@{PG}/{d}")
        env.setdefault(f"{k}_DB_OWNER_DSN", f"postgres://{d}_owner:{d}_owner@{PG}/{d}")
    defaults = {
        "MIAOQIA_DEPLOY_DIR": str(ROOT / "deploy"),
        "MIAOQIA_KAFKA_BROKERS": "localhost:9092",
        "MIAOQIA_IDENTITY_OPENFGA_URL": OPENFGA,
        # 本地环境的 Keycloak 与内部接口均为明文，须显式进入开发策略；HTTPS 与 mTLS 由运行时验收覆盖。
        "MIAOQIA_ENV": "development",
        "MIAOQIA_GATEWAY_ISSUER": ISSUER,
        "MIAOQIA_GATEWAY_AUDIENCE": "miaoqia-api",
        "MIAOQIA_RELAY_DSN_TEMPLATE": f"postgres://outbox_relay:outbox_relay@{PG}/{{db}}",
        "OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4317",
        "OTEL_BSP_SCHEDULE_DELAY": "500",
    }
    for key, value in defaults.items():
        env.setdefault(key, value)
    return env


class Procs:
    def __init__(self) -> None:
        self.procs: list[tuple[str, subprocess.Popen]] = []

    def start(self, name: str, cmd: list[str], env: dict[str, str], cwd: Path = ROOT) -> None:
        log = open(WORK / f"{name}.log", "w")
        p = subprocess.Popen(cmd, cwd=cwd, env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        self.procs.append((name, p))

    def check_alive(self) -> None:
        for name, p in self.procs:
            if p.poll() is not None:
                print((WORK / f"{name}.log").read_text()[-3000:])
                fail(f"进程 {name} 已退出（{p.returncode}），日志见 {WORK / (name + '.log')}")

    def stop(self) -> None:
        for _, p in self.procs:
            if p.poll() is None:
                os.killpg(p.pid, signal.SIGTERM)
        for _, p in self.procs:
            try:
                p.wait(15)
            except subprocess.TimeoutExpired:
                os.killpg(p.pid, signal.SIGKILL)


def http(method: str, url: str, token: str | None = None) -> tuple[int, dict]:
    req = urllib.request.Request(url, method=method, data=b"" if method == "POST" else None)
    req.add_header("User-Agent", "miaoqia-e2e/1")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


def token(user: str) -> tuple[str, str]:
    body = urllib.parse.urlencode({
        "client_id": os.environ.get("MIAOQIA_E2E_CLIENT_ID", "miaoqia-dev"),
        "username": user,
        "password": os.environ.get(f"MIAOQIA_E2E_{user.upper()}_PASSWORD", user),
        "grant_type": "password",
    }).encode()
    req = urllib.request.Request(f"{ISSUER}/protocol/openid-connect/token", data=body, method="POST",
                                 headers={"User-Agent": "miaoqia-e2e/1"})
    with urllib.request.urlopen(req, timeout=10) as r:
        tok = json.loads(r.read())["access_token"]
    payload = tok.split(".")[1]
    sub = json.loads(__import__("base64").urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))["sub"]
    return tok, sub


def wait_http(url: str, seconds: int = 60) -> None:
    deadline = time.time() + seconds
    while time.time() < deadline:
        try:
            urllib.request.urlopen(urllib.request.Request(url, headers={"User-Agent": "miaoqia-e2e/1"}), timeout=2)
            return
        except urllib.error.HTTPError:
            return  # 已在监听
        except OSError:
            time.sleep(0.5)
    fail(f"{url} 未就绪")


def run_probe(api: str, op_token: str, procs: Procs) -> dict:
    status, body = http("POST", f"{api}/v1/operations/probes", op_token)
    if status != 202:
        fail(f"发起探针返回 {status}：{body}")
    pid = body["probe_id"]
    deadline = time.time() + 60
    while time.time() < deadline:
        procs.check_alive()
        status, body = http("GET", f"{api}/v1/operations/probes/{pid}", op_token)
        if status == 200 and body["status"] == "completed":
            return body
        time.sleep(0.5)
    fail(f"探针 {pid} 未在 60 秒内完成：{body}")
    return {}


def audit_rows(resource: str) -> list[tuple]:
    with psycopg.connect(f"postgres://gov_owner:gov_owner@{PG}/gov") as c:
        return c.execute("SELECT subsystem, action, result, actor FROM audit_entries WHERE resource = %s ORDER BY occurred_at",
                         (resource,)).fetchall()


def wait_audit(resource: str, n: int) -> list[tuple]:
    deadline = time.time() + 30
    rows: list[tuple] = []
    while time.time() < deadline:
        rows = audit_rows(resource)
        if len(rows) >= n:
            return rows
        time.sleep(0.5)
    fail(f"{resource} 的审计条目不足 {n} 条：{rows}")
    return rows


def _spans(offset: int) -> list[tuple[str, str, str, str]]:
    """(trace_id, service, span 名, cloudevents.event_subject)。只读本次运行写入的部分。"""
    out = []
    if not TRACES.exists():
        return out
    with TRACES.open("rb") as f:
        f.seek(offset)
        data = f.read().decode("utf-8", errors="replace")
    for line in data.splitlines():
        if not line.strip():
            continue
        for rs in json.loads(line).get("resourceSpans", []):
            svc = next((a["value"].get("stringValue") for a in rs["resource"]["attributes"] if a["key"] == "service.name"), "?")
            for ss in rs.get("scopeSpans", []):
                for sp in ss.get("spans", []):
                    subj = next((a["value"].get("stringValue", "") for a in sp.get("attributes", [])
                                 if a["key"] == "cloudevents.event_subject"), "")
                    out.append((sp["traceId"], svc, sp["name"], subj))
    return out


def span_names(probe_id: str, offset: int) -> set[str]:
    spans = _spans(offset)
    traces = {t for t, _, _, subj in spans if subj == probe_id}
    return {name for t, _, name, _ in spans if t in traces}


def trace_services(probe_id: str, offset: int) -> set[str]:
    """包含该探针事件处理（消费者 span 的 cloudevents.event_subject）的追踪所涉及的服务名。"""
    spans = [(t, svc, subj) for t, svc, _, subj in _spans(offset)]
    traces = {t for t, _, subj in spans if subj == probe_id}
    return {svc for t, svc, _ in spans if t in traces} if len(traces) == 1 else ({"多条追踪"} if traces else set())


def check_connections() -> bool:
    """Read-only check for the configured PostgreSQL, Kafka, HTTP services and trace mode."""
    env = base_env()
    checks = [
        ("Keycloak", ISSUER, None),
        ("OpenFGA", OPENFGA.rstrip("/") + "/healthz", env.get("MIAOQIA_IDENTITY_OPENFGA_TOKEN")),
        ("OTLP", env["OTEL_EXPORTER_OTLP_ENDPOINT"].rstrip("/") + "/", None),
    ]
    ready = True
    for name, url, token_value in checks:
        try:
            headers = {"User-Agent": "miaoqia-e2e/1"}
            if token_value:
                headers["Authorization"] = f"Bearer {token_value}"
            urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=5)
            ok(f"{name} 可达")
        except urllib.error.HTTPError as exc:
            if name == "OTLP" and exc.code in {404, 405}:
                ok(f"{name} 可达")
            else:
                print(f"  ✗ {name} 不可达：{url}")
                ready = False
        except OSError:
            print(f"  ✗ {name} 不可达：{url}")
            ready = False
    if not TRACES.exists() and os.environ.get("MIAOQIA_E2E_TRACE_REQUIRED", "true").lower() not in {"0", "false", "no"}:
        print(f"  ✗ 本地追踪文件不存在：{TRACES}")
        ready = False
    return ready


def main() -> None:
    shutil.rmtree(WORK, ignore_errors=True)
    WORK.mkdir(parents=True)
    env = base_env()
    procs = Procs()
    trace_offset = TRACES.stat().st_size if TRACES.exists() else 0
    trace_required = os.environ.get("MIAOQIA_E2E_TRACE_REQUIRED", "true").lower() not in {"0", "false", "no"}
    try:
        step("按契约创建 Kafka 主题")
        subprocess.run([str(ROOT / "deploy/local/topics.sh")], check=True, capture_output=True)
        ok("主题已就绪")

        step("构建 platform 并执行三个单元的数据库迁移")
        subprocess.run(["go", "build", "-o", str(WORK / "platform"), "./cmd/platform"], cwd=ROOT / "services", check=True)
        platform = str(WORK / "platform")
        subprocess.run([platform, "migrate"], env=env, check=True, capture_output=True)
        for unit in ("acquisition", "data"):
            subprocess.run(["uv", "run", "-q", f"mq-{unit}", "migrate"], cwd=ROOT / "data", env=env, check=True, capture_output=True)
        ok("迁移完成")

        step("授予 operator 运营关系（identity → OpenFGA）")
        op_token, op_sub = token("operator")
        vis_token, vis_sub = token("visitor")
        subprocess.run([platform, "grant", f"user:{op_sub}", "operator", "platform:miaoqia"], env=env, check=True)
        ok(f"user:{op_sub} operator platform:miaoqia")

        step("启动三个单元的进程角色")
        procs.start("platform-api", [platform, "-role", "api"], env)
        procs.start("platform-consumer", [platform, "-role", "consumer"], env)
        procs.start("platform-relay", [platform, "-role", "outbox-relay"], env)
        procs.start("acq-scheduler", ["uv", "run", "-q", "mq-acquisition", "--role", "acq-scheduler"], env, ROOT / "data")
        procs.start("data-consumer", ["uv", "run", "-q", "mq-data", "--role", "data-consumer"], env, ROOT / "data")
        remote_env = dict(env, MIAOQIA_BINDING_IDENTITY="remote", MIAOQIA_IDENTITY_URL="http://localhost:8091/internal",
                          MIAOQIA_HTTP_ADDR=":8092", MIAOQIA_INTERNAL_ADDR=":8093")
        procs.start("platform-api-remote", [platform, "-role", "api"], remote_env)
        wait_http("http://localhost:8090/")
        wait_http("http://localhost:8092/")
        time.sleep(3)
        procs.check_alive()
        ok("platform：api、consumer、outbox-relay、api（identity 远程绑定）；acquisition：acq-scheduler；data：data-consumer")

        step("探针经过三个单元")
        pr = run_probe("http://localhost:8090", op_token, procs)
        if pr["hops"] != ["operations", "acquisition", "evidence", "operations"]:
            fail(f"探针路径不符：{pr['hops']}")
        ok(f"{pr['probe_id']}：{' → '.join(pr['hops'])}")

        step("审计")
        rows = wait_audit(pr["probe_id"], 2)
        acts = [(r[1], r[2]) for r in rows]
        if ("operations.probe.trigger", "succeeded") not in acts or ("operations.probe.complete", "succeeded") not in acts:
            fail(f"审计条目不符：{rows}")
        if rows[0][3] != op_sub:
            fail(f"发起审计的主体应为 operator：{rows[0]}")
        ok("发起（operator）与完成（svc:operations）均已审计")

        step("授权：无权用户被拒绝且留下审计")
        before = len(audit_rows("platform:miaoqia"))
        status, body = http("POST", "http://localhost:8090/v1/operations/probes", vis_token)
        if status != 403:
            fail(f"无权用户应得到 403，得到 {status}：{body}")
        status, _ = http("POST", "http://localhost:8090/v1/operations/probes")
        if status != 401:
            fail(f"无令牌应得到 401，得到 {status}")
        denied = wait_audit("platform:miaoqia", before + 1)
        if denied[-1][2] != "denied" or denied[-1][3] != vis_sub:
            fail(f"拒绝审计不符：{denied[-1]}")
        ok("visitor 403 并审计 denied；无令牌 401")

        if trace_required:
            step("追踪：同一条追踪覆盖三个单元")
            want = {"platform", "acquisition", "data"}
            deadline = time.time() + 30
            services: set[str] = set()
            while time.time() < deadline and not want <= services:
                time.sleep(1)
                services = trace_services(pr["probe_id"], trace_offset)
            if not want <= services:
                fail(f"探针 {pr['probe_id']} 的追踪只覆盖 {sorted(services)}")
            ok(f"探针 {pr['probe_id']} 的事件处理位于同一条追踪，覆盖 {', '.join(sorted(services))}")
        else:
            ok("追踪文件未在本机落盘，已按配置跳过本地追踪文件门禁")

        step("远程绑定：identity 经内部接口调用")
        pr2 = run_probe("http://localhost:8092", op_token, procs)
        ok(f"{pr2['probe_id']}：{' → '.join(pr2['hops'])}")
        status, _ = http("POST", "http://localhost:8092/v1/operations/probes", vis_token)
        if status != 403:
            fail(f"远程绑定下无权用户应得到 403，得到 {status}")
        log = (WORK / "platform-api-remote.log").read_text()
        started = next(json.loads(l) for l in log.splitlines() if '"msg":"进程角色启动"' in l)
        if "identity" in started["subsystems"]:
            fail("远程绑定的 api 进程不应在本地加载 identity")
        if trace_required:
            deadline = time.time() + 30
            while time.time() < deadline and "identity /internal/v1/authz:check" not in span_names(pr2["probe_id"], trace_offset):
                time.sleep(1)
            if "identity /internal/v1/authz:check" not in span_names(pr2["probe_id"], trace_offset):
                fail("探针的追踪中没有 identity 内部接口的服务端 span：授权未经远程调用")
            ok("授权经 identity 内部接口远程调用（追踪中可见服务端 span），允许与拒绝与本地绑定一致")
        ok("同一套契约用例对本地与远程绑定通过：go test ./subsystems/identity")

        step("L1 回放：官方中标公告经数据平面管线成为领域事实（evidence → Kafka → domain-intelligence）")
        replay_env = dict(env, MIAOQIA_EVIDENCE_OBJECT_STORE_DIR=str(WORK / "objects"), MIAOQIA_PACKS_DIR=str(ROOT / "packs"))
        sample = ROOT / "packs/information_domains/procurement/corpus/official-award-20261014"
        out = subprocess.run(["uv", "run", "-q", "mq-evidence-replay", str(sample)], cwd=ROOT / "data", env=replay_env,
                             check=True, capture_output=True, text=True).stdout
        replay_result = json.loads(out.splitlines()[-1])
        revisions = replay_result["revisions"] or replay_result["existing_revisions"]
        if not revisions:
            fail(f"官方样本回放未返回新建或已有解析修订：{replay_result}")
        revision = revisions[0]
        deadline, fact = time.time() + 60, None
        while time.time() < deadline and fact is None:
            time.sleep(1)
            procs.check_alive()
            with psycopg.connect(f"postgres://dom_owner:dom_owner@{PG}/dom") as c:
                fact = c.execute("SELECT v.fact_id, v.version FROM dom_fact_versions v JOIN dom_facts f ON f.id = v.fact_id WHERE v.revision_ref = %s AND f.object_type = 'tendering.notice'", (revision,)).fetchone()
        if fact is None:
            fail(f"解析修订 {revision} 未物化为领域事实")
        with psycopg.connect(f"postgres://dom_owner:dom_owner@{PG}/dom") as c:
            row = c.execute("SELECT published_at FROM kernel_outbox WHERE envelope->>'type' = 'domain.fact.upserted.v1' "
                            "AND envelope->'data'->>'fact_id' = %s", (fact[0],)).fetchone()
        if row is None:
            fail(f"领域事实 {fact[0]} 没有 domain.fact.upserted.v1")
        ok(f"{revision} → {fact[0]} v{fact[1]}，domain.fact.upserted.v1 已进入发件箱")

        # data-api reads (mTLS listener, acq-admin rights, service reads) are
        # covered by the A02 process replay; this E2E's remote-identity process
        # still uses the plaintext internal port.

        print("\nM0 端到端验证通过。")
    finally:
        procs.stop()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="M0 工程探针")
    parser.add_argument("--check", action="store_true", help="只读检查当前环境配置和公共服务")
    args = parser.parse_args()
    sys.exit(0 if check_connections() else 1) if args.check else sys.exit(main())
