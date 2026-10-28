from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timedelta, timezone
import json
import os
import subprocess
import time
import psycopg
import pytest
from miaoqia.acquisition.api import policy
from miaoqia.acquisition.infrastructure.rights import validate_command
from miaoqia.acquisition.application.rights import Decisions
from miaoqia.kernel import reqctx

CTX = reqctx.Context(
    reqctx.Principal("user-a", "user"),
    "tnt_01JA8ZXXXXXXXXXXXXXXXXXXX1",
    "cell-01",
    "interactive",
)
TENDER = "corpus:official-building-tender-20261026"
AWARD = "corpus:official-building-award-20261026"
SOURCE = "official-qzygjy"


def grant(entry="building-tender", version=1, **kw):
    now = datetime.now(timezone.utc)
    return {
        "schema_version": 1,
        "entry_id": entry,
        "version": version,
        "source_id": SOURCE,
        "scopes": [{"tenant": CTX.tenant, "cell": CTX.cell}],
        "purposes": ["interactive"],
        "display": "excerpt",
        "effective_at": (now - timedelta(days=1)).isoformat(),
        "expires_at": (now + timedelta(days=1)).isoformat(),
        "redistribution": False,
        **kw,
    }


def request(*tags, source=SOURCE):
    return {
        "operation": "fact_read",
        "inputs": [
            {
                "source_id": source,
                "tags": sorted(tags or (TENDER, "public-official-review")),
            }
        ],
    }


def seed(write, tag=TENDER, entry="building-tender", **kw):
    p = grant(entry, **kw)
    cmds = [
        {"action": "register", "policy": p},
        {
            "action": "bind",
            "source_id": SOURCE,
            "tags": sorted([tag, "public-official-review"]),
            "entry_id": entry,
            "version": 1,
        },
        {"action": "activate", "entry_id": entry, "version": 1},
    ]
    with reqctx.use(CTX):
        for cmd in cmds:
            write.command(cmd, "test-operator", "reviewed fixture")
    return p, cmds


def evaluate(d, req=None, *, context=CTX, confirm=False):
    with reqctx.use(context):
        return d.evaluate(req or request(), confirm=confirm)


def confirmed(req, decision):
    return {**req, "receipt": decision["receipt"]}


def test_original_current_intersection_restart(registry):
    read, write, dsns = registry
    p, _ = seed(write)
    d = Decisions(read)
    a = evaluate(d)
    assert a["allowed"] and a["display"] == "excerpt" and not a["redistribution"]
    assert evaluate(d, confirmed(request(), a), confirm=True)["allowed"]
    with reqctx.use(CTX):
        write.command(
            {
                "action": "register",
                "policy": {
                    **p,
                    "version": 2,
                    "display": "full_text",
                    "redistribution": True,
                },
            },
            "op",
            "expand",
        )
        write.command(
            {"action": "activate", "entry_id": p["entry_id"], "version": 2},
            "op",
            "activate",
        )
    b = evaluate(d)
    assert b["display"] == "excerpt" and not b["redistribution"]
    assert not evaluate(d, confirmed(request(), a), confirm=True)["allowed"]
    with reqctx.use(CTX):
        write.command({"action": "revoke", "entry_id": p["entry_id"]}, "op", "revoke")
    assert not evaluate(d, confirmed(request(), b), confirm=True)["allowed"]
    from miaoqia.acquisition.infrastructure.rights import Registry

    restarted = Registry(dsns["app"])
    try:
        assert not evaluate(Decisions(restarted))["allowed"]
    finally:
        restarted.close()


@pytest.mark.parametrize(
    "bad",
    [
        request("public-official-review"),
        request(TENDER),
        request("corpus:unknown", "public-official-review"),
        request(TENDER, "public-official-review", "unknown"),
        request(TENDER, "public-official-review", source="official-enterprise-henan"),
        request(TENDER, "public-official-review", source="official-award"),
        request(TENDER + "x", "public-official-review"),
    ],
)
def test_exact_source_all_tags(registry, bad):
    read, write, _ = registry
    seed(write)
    assert evaluate(Decisions(read), bad) == {
        "allowed": False,
        "display": "none",
        "redistribution": False,
        "epoch": 3,
    }


def test_all_inputs_scope_and_receipt_binding(registry):
    read, write, _ = registry
    seed(write)
    seed(write, AWARD, "building-award", display="citation_only")
    d = Decisions(read)
    r = request()
    r["inputs"] += request(AWARD, "public-official-review")["inputs"]
    a = evaluate(d, r)
    assert a["display"] == "citation_only"
    for ctx in (
        reqctx.Context(
            reqctx.Principal("user-b", "user"), CTX.tenant, CTX.cell, CTX.purpose
        ),
        reqctx.Context(CTX.principal, "other-tenant", CTX.cell, CTX.purpose),
    ):
        assert not evaluate(d, confirmed(r, a), context=ctx, confirm=True)["allowed"]
    for other in (
        request(),
        {**r, "operation": "evidence_resolve"},
        request(source="official-award"),
    ):
        assert not evaluate(d, confirmed(other, a), confirm=True)["allowed"]
    with reqctx.use(CTX):
        write.command(
            {"action": "revoke", "entry_id": "building-award"}, "op", "one revoked"
        )
    assert not evaluate(d, r)["allowed"]


def test_same_epoch_expiry(registry):
    read, write, _ = registry
    seed(
        write,
        expires_at=(datetime.now(timezone.utc) + timedelta(seconds=1.4)).isoformat(),
    )
    d = Decisions(read)
    a = evaluate(d)
    assert a["allowed"]
    time.sleep(1.5)
    b = evaluate(d, confirmed(request(), a), confirm=True)
    assert b["epoch"] == a["epoch"] and not b["allowed"]


def test_privileges_immutable_audit_atomicity(registry):
    read, write, dsns = registry
    _, cmds = seed(write)
    with reqctx.use(CTX):
        assert [write.command(c, "op", "retry") for c in cmds] == [3, 3, 3]
    for role in ("app", "operator"):
        for statement in (
            "UPDATE acq_rights.current SET revoked=true",
            "TRUNCATE acq_rights.audit",
            "DELETE FROM acq_rights.aliases",
            "INSERT INTO acq_rights.audit(epoch,actor,reason,db_actor,command) VALUES(99,'a','b','c','{}')",
            "ALTER TABLE acq_rights.versions DISABLE TRIGGER ALL",
            "CREATE TABLE acq_rights.evil(x int)",
            "UPDATE acq_rights.state SET epoch=99",
            "DELETE FROM kernel_outbox",
        ):
            with psycopg.connect(dsns[role]) as c, pytest.raises(psycopg.Error):
                c.execute(statement)
    with reqctx.use(CTX), pytest.raises(Exception):
        read.command(
            {"action": "revoke", "entry_id": "building-tender"}, "op", "deny reader"
        )
    for table in ("versions", "aliases", "reviewed", "audit"):
        with psycopg.connect(dsns["owner"]) as c, pytest.raises(psycopg.Error):
            c.execute(f"TRUNCATE acq_rights.{table} CASCADE")
    with psycopg.connect(dsns["owner"]) as c:
        c.execute(
            "CREATE FUNCTION acq_rights.test_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fault'; END $$"
        )
        c.execute(
            "CREATE TRIGGER test_fault BEFORE INSERT ON acq_rights.audit FOR EACH ROW EXECUTE FUNCTION acq_rights.test_fault()"
        )
    with reqctx.use(CTX), pytest.raises(Exception):
        write.command(
            {"action": "revoke", "entry_id": "building-tender"}, "op", "rollback"
        )
    assert evaluate(Decisions(read))["allowed"]
    with psycopg.connect(dsns["owner"]) as c:
        assert c.execute("SELECT epoch FROM acq_rights.state").fetchone()[0] == 3
        assert c.execute("SELECT count(*) FROM acq_rights.audit").fetchone()[0] == 3
        c.execute("DROP TRIGGER test_fault ON acq_rights.audit")


def test_alias_rebind_and_source_conflict(registry):
    read, write, _ = registry
    p, _ = seed(write)
    with reqctx.use(CTX):
        write.command({"action": "register", "policy": grant("new-entry")}, "op", "new")
        for cmd in (
            {
                "action": "bind",
                "source_id": SOURCE,
                "tags": sorted([TENDER, "public-official-review"]),
                "entry_id": "new-entry",
                "version": 1,
            },
            {
                "action": "bind",
                "source_id": SOURCE,
                "tags": ["public-official-review"],
                "entry_id": "new-entry",
                "version": 1,
            },
            {
                "action": "register",
                "policy": {**p, "version": 2, "source_id": "wrong-source"},
            },
            {"action": "register", "policy": {**p, "display": "full_text"}},
        ):
            with pytest.raises(Exception):
                write.command(cmd, "op", "reject")
    assert evaluate(Decisions(read))["display"] == "excerpt"


def test_concurrent_revoke_confirm(registry):
    read, write, _ = registry
    seed(write)
    d = Decisions(read)
    a = evaluate(d)

    def revoke(_):
        with reqctx.use(CTX):
            return write.command(
                {"action": "revoke", "entry_id": "building-tender"}, "op", "concurrent"
            )

    with ThreadPoolExecutor(4) as pool:
        assert set(pool.map(revoke, range(4))) == {4}
    assert not evaluate(d, confirmed(request(), a), confirm=True)["allowed"]


def test_real_cli_batch_rollback_restart(registry, tmp_path):
    read, write, dsns = registry
    _, cmds = seed(write)
    file = tmp_path / "commands.json"

    def cli(values):
        file.write_text(json.dumps(values))
        return subprocess.run(
            [
                "mq-source-rights",
                "apply",
                "--file",
                str(file),
                "--actor",
                "fixture-operator",
                "--reason",
                "fixture review",
            ],
            env={
                **os.environ,
                "MIAOQIA_ACQUISITION_RIGHTS_OPERATOR_DSN": dsns["operator"],
            },
            capture_output=True,
            text=True,
        )

    assert cli(cmds).returncode == 0
    assert (
        cli(
            [
                {"action": "revoke", "entry_id": "building-tender"},
                {"action": "activate", "entry_id": "unknown", "version": 1},
            ]
        ).returncode
        == 1
    )
    assert evaluate(Decisions(read))["allowed"]
    assert cli([{"action": "revoke", "entry_id": "building-tender"}]).returncode == 0
    assert not evaluate(Decisions(read))["allowed"]
    assert (
        cli(
            [{"action": "register", "policy": {**grant(), "unexpected": "metadata"}}]
        ).returncode
        == 1
    )


@pytest.mark.parametrize(
    "change",
    [
        {"scopes": []},
        {"purposes": ["research"]},
        {"schema_version": True},
        {"version": True},
        {"display": "public"},
        {"redistribution": "yes"},
        {"extra": "value"},
        {"receipt": {"untrusted": "metadata"}},
    ],
)
def test_closed_policy(change):
    with pytest.raises(ValueError):
        policy({**grant(), **change})
    with pytest.raises(ValueError):
        validate_command({"action": "register", "policy": {**grant(), **change}})


def test_reader_role_startup_and_database_ddl_denied(registry):
    read, write, dsns = registry
    with reqctx.use(CTX):
        read.ensure_reader()
        with pytest.raises(PermissionError):
            write.ensure_reader()
    for role in ("app", "operator"):
        for statement in (
            "CREATE TABLE public.forbidden(x int)",
            "CREATE TEMP TABLE forbidden(x int)",
            f"SET ROLE {dsns['reader_role'].replace('_app', '_owner')}",
        ):
            with psycopg.connect(dsns[role]) as c, pytest.raises(psycopg.Error):
                c.execute(statement)


def test_schema_samples_match_runtime_closed_policy():
    from pathlib import Path

    cases = json.loads(
        (
            Path(__file__).resolve().parents[4]
            / "contracts/tests/acquisition.source-rights.json"
        ).read_text()
    )
    for case in cases["cases"]:
        if case["valid"]:
            assert policy(case["instance"]) == case["instance"]
        else:
            with pytest.raises((ValueError, TypeError)):
                policy(case["instance"])


def test_wire_schema_matches_provider_decisions(registry):
    from pathlib import Path
    from jsonschema import Draft202012Validator

    schema = json.loads(
        (
            Path(__file__).resolve().parents[4]
            / "contracts/schemas/acquisition/source-rights.schema.json"
        ).read_text()
    )

    def validate(kind, value):
        Draft202012Validator(
            {"$defs": schema["$defs"], "$ref": "#/$defs/" + kind}
        ).validate(value)

    read, write, _ = registry
    seed(write)
    d = Decisions(read)
    r = request()
    a = evaluate(d, r)
    c = confirmed(r, a)
    for kind, value in [
        ("source-rights-resolve-request", r),
        ("source-rights-decision", a),
        ("source-rights-confirm-request", c),
        ("source-rights-decision", evaluate(d, c, confirm=True)),
    ]:
        validate(kind, value)
    with reqctx.use(CTX):
        validate("source-rights-status", d.status())


def test_actual_role_identity_and_password_control(registry):
    read, write, dsns = registry
    for key, role in [
        ("app", dsns["reader_role"]),
        ("operator", dsns["operator_role"]),
    ]:
        with psycopg.connect(dsns[key]) as c:
            assert c.execute("SELECT current_user").fetchone()[0] == role
        if os.environ.get("MIAOQIA_TEST_REQUIRE_PASSWORD") == "1":
            with pytest.raises(psycopg.OperationalError):
                psycopg.connect(dsns[key].replace(":pw@", ":incorrect@"))


@pytest.mark.parametrize("restriction", ["scope", "future", "expired", "none"])
def test_current_restrictions_intersect_original(registry, restriction):
    read, write, _ = registry
    p, _ = seed(write)
    replacement = {**p, "version": 2}
    if restriction == "scope":
        replacement["scopes"] = [{"tenant": "other-tenant", "cell": "cell-01"}]
    elif restriction == "future":
        replacement["effective_at"] = (
            datetime.now(timezone.utc) + timedelta(hours=1)
        ).isoformat()
    elif restriction == "expired":
        replacement["expires_at"] = (
            datetime.now(timezone.utc) - timedelta(hours=1)
        ).isoformat()
    else:
        replacement["display"] = "none"
    with reqctx.use(CTX):
        write.command(
            {"action": "register", "policy": replacement}, "op", "current restriction"
        )
        write.command(
            {"action": "activate", "entry_id": p["entry_id"], "version": 2},
            "op",
            "activate",
        )
    assert not evaluate(Decisions(read))["allowed"]


def test_initial_manifest_is_exact_reviewed_and_inactive():
    from pathlib import Path

    root = Path(__file__).resolve().parents[4]
    commands = json.loads(
        (root / "deploy/acquisition/source-rights-frozen.json").read_text()
    )
    assert len(commands) == 4 and {c["action"] for c in commands} == {
        "register",
        "bind",
    }
    binds = [c for c in commands if c["action"] == "bind"]
    assert {tuple(c["tags"]) for c in binds} == {
        (TENDER, "public-official-review"),
        (AWARD, "public-official-review"),
    }
    assert {c["source_id"] for c in binds} == {SOURCE}
    for command in commands:
        if command["action"] == "register":
            value = policy(command["policy"])
            assert value["scopes"] == [{"tenant": CTX.tenant, "cell": CTX.cell}]
            assert value["display"] == "excerpt" and not value["redistribution"]


def test_reference_group_requires_independent_policy_and_explicit_activation(registry):
    read, write, _ = registry
    reference = "reference-proc-building-topic-v0-1"
    tags = ["review:building-topic-pair"]
    p = grant("building-topic-reference", source_id=reference)
    req = {"operation": "fact_read", "inputs": [{"source_id": reference, "tags": tags}]}
    with reqctx.use(CTX):
        write.command({"action": "register", "policy": p}, "reviewer", "reviewed reference only")
        write.command({"action": "bind", "source_id": reference, "tags": tags,
            "entry_id": p["entry_id"], "version": 1}, "reviewer", "exact reference alias")
        assert not Decisions(read).evaluate(req)["allowed"]
        write.command({"action": "activate", "entry_id": p["entry_id"], "version": 1}, "reviewer", "explicit test activation")
        assert Decisions(read).evaluate(req)["allowed"]
        with pytest.raises(Exception):
            write.command({"action": "bind", "source_id": SOURCE, "tags": [TENDER,"public-official-review"],
                "entry_id": p["entry_id"], "version": 1}, "reviewer", "cannot borrow official identity")
