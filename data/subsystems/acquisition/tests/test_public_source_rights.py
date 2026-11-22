"""Real PostgreSQL controls for source-wide revocation and verified operator recovery."""

from concurrent.futures import ThreadPoolExecutor
from datetime import datetime
from hashlib import sha256
import json
import os
import subprocess
import time
from urllib.parse import urlsplit

import psycopg
from psycopg import sql
from psycopg.types.json import Jsonb
import pytest

from miaoqia.acquisition import MIGRATIONS
from miaoqia.acquisition.api import OPERATIONS
from miaoqia.acquisition.application.rights import Decisions
from miaoqia.acquisition.infrastructure.rights import Registry
from miaoqia.kernel import db, ids, outbox, payloads, reqctx

from test_remote import call, remote  # noqa: F401 (pytest fixture)
from test_rights import CTX, SOURCE, confirmed, declare_public, evaluate, request, seed


SYNTHETIC = "test-public-source"
ACTOR = "fixture-operator"
REASON = "synthetic rights review"
PUBLIC_COMMAND = "SELECT acq_rights.public_source_command(%s, %s, %s, %s)"
RIGHTS_CHANGED = "acquisition.source.rights_changed.v1"


def revoke(source=SYNTHETIC, epoch=0):
    return {"action": "revoke_public", "source_id": source, "expected_epoch": epoch}


def restore(source, epoch):
    return {
        "action": "restore_public",
        "source_id": source,
        "expected_epoch": epoch,
        "evidence_hash": "sha256:"
        + sha256(b"synthetic restoration evidence").hexdigest(),
    }


def write_command(registry, value):
    with reqctx.use(CTX):
        return registry.command(value, ACTOR, REASON)


def cli(dsns, *args, role="operator"):
    return subprocess.run(
        ["mq-source-rights", *args],
        env={
            **os.environ,
            "MIAOQIA_ACQUISITION_DB_OWNER_DSN": dsns["owner"],
            "MIAOQIA_ACQUISITION_RIGHTS_OPERATOR_DSN": dsns[role],
        },
        capture_output=True,
        timeout=10,
    )


def test_only_operator_command_can_mutate_public_rights(registry):
    read, write, dsns = registry
    declare_public(dsns, SYNTHETIC)
    assert (
        cli(
            dsns,
            "grant-roles",
            "--reader",
            dsns["reader_role"],
            "--operator",
            dsns["operator_role"],
        ).returncode
        == 0
    )
    for role in ("app", "operator"):
        for statement in (
            "INSERT INTO acq_rights.public_source_state VALUES ('test-public-source', true, 1)",
            "UPDATE acq_rights.public_source_state SET revoked=false",
            "DELETE FROM acq_rights.public_source_state",
            "TRUNCATE acq_rights.public_source_state",
            "UPDATE public.acq_sources SET state='active'",
            "INSERT INTO public.kernel_outbox(event_id, topic, partition_key, envelope) VALUES ('x', 'x', 'x', '{}')",
            "UPDATE public.kernel_outbox SET envelope='{}'",
            "DELETE FROM public.kernel_outbox",
        ):
            with pytest.raises(psycopg.errors.InsufficientPrivilege), psycopg.connect(
                dsns[role]
            ) as conn:
                conn.execute(statement)
    with pytest.raises(psycopg.errors.InsufficientPrivilege), psycopg.connect(
        dsns["app"]
    ) as conn:
        conn.execute(PUBLIC_COMMAND, (Jsonb(revoke()), ACTOR, REASON, ids.new("evt")))
    for invalid in (
        revoke("unregistered-source"),
        {**revoke(), "expected_epoch": False},
        {**revoke(), "extra": "disallowed"},
        {**revoke(), "topic": "platform.audit", "after": list(OPERATIONS)},
        restore(SYNTHETIC, 0),
    ):
        with pytest.raises(psycopg.errors.RaiseException), psycopg.connect(
            dsns["operator"]
        ) as conn:
            conn.execute(
                PUBLIC_COMMAND, (Jsonb(invalid), ACTOR, REASON, ids.new("evt"))
            )
    with pytest.raises(psycopg.errors.RaiseException), psycopg.connect(
        dsns["operator"]
    ) as conn:
        conn.execute(PUBLIC_COMMAND, (Jsonb(revoke()), ACTOR, REASON, "invalid-id"))
    with psycopg.connect(dsns["owner"]) as conn:
        assert conn.execute("SELECT count(*) FROM acq_rights.audit").fetchone() == (0,)
    with reqctx.use(CTX):
        read.ensure_reader()
        with pytest.raises(PermissionError):
            write.ensure_reader()


def test_source_withdrawal_denies_old_corpus_replay_and_public_receipts(
    registry, tmp_path
):
    read, write, dsns = registry
    # Deliberately share the reviewed fixture's source ID so an active historical alias exists.
    declare_public(dsns, SOURCE)
    declare_public(dsns, SYNTHETIC)
    seed(write)
    evidence = {**request(f"public:{SOURCE}"), "operation": "evidence_resolve"}
    replay = {**request(), "operation": "evidence_resolve"}
    unrelated = request(f"public:{SYNTHETIC}", source=SYNTHETIC)
    decisions = Decisions(read)
    with psycopg.connect(dsns["owner"]) as conn:
        conn.execute(
            "UPDATE acq_sources SET state='blocked', state_reason='synthetic_http_403' WHERE source_id=%s",
            (SOURCE,),
        )
    original = evaluate(decisions, evidence)
    historical = evaluate(decisions, replay)
    assert original["display"] == "full_text"
    assert historical["display"] == "excerpt"
    assert evaluate(decisions, confirmed(evidence, original), confirm=True)["allowed"]

    file = tmp_path / "public-rights.json"

    def apply(*commands, role="operator"):
        file.write_text(json.dumps(commands))
        return cli(
            dsns,
            "apply",
            "--file",
            str(file),
            "--actor",
            ACTOR,
            "--reason",
            REASON,
            role=role,
        ).returncode

    withdrawal = revoke(SOURCE)
    assert apply(withdrawal, role="app") == 1
    assert apply(withdrawal, revoke("unregistered-source")) == 1
    assert evaluate(decisions, confirmed(evidence, original), confirm=True)["allowed"]
    assert apply(withdrawal) == apply(withdrawal) == 0
    for req, old in ((evidence, original), (replay, historical)):
        assert evaluate(decisions, confirmed(req, old), confirm=True) == {
            "allowed": False,
            "display": "none",
            "redistribution": False,
            "epoch": 4,
        }
        assert not evaluate(Decisions(read), req)["allowed"]
    assert not evaluate(
        decisions,
        {"operation": "fact_read", "inputs": unrelated["inputs"] + replay["inputs"]},
    )["allowed"]
    assert evaluate(decisions, unrelated)["allowed"]

    recovery = restore(SOURCE, 4)
    assert apply({**recovery, "evidence_hash": "unverified-placeholder"}) == 1
    assert apply({**recovery, "expected_epoch": 3}) == 1
    assert apply(recovery) == apply(recovery) == 0
    assert apply(withdrawal) == 1
    assert not evaluate(decisions, confirmed(evidence, original), confirm=True)[
        "allowed"
    ]
    fresh = evaluate(decisions, evidence)
    assert fresh["display"] == "full_text" and not fresh["redistribution"]
    assert evaluate(decisions, confirmed(evidence, fresh), confirm=True)["allowed"]
    assert write_command(write, revoke(SOURCE, 5)) == 6
    assert apply(recovery) == 1
    assert not evaluate(decisions, evidence)["allowed"]
    with psycopg.connect(dsns["owner"]) as conn:
        assert conn.execute(
            "SELECT state, state_reason FROM acq_sources WHERE source_id=%s", (SOURCE,)
        ).fetchone() == (
            "blocked",
            "synthetic_http_403",
        )
        assert conn.execute(
            "SELECT epoch, actor, reason, db_actor, command, old_state->>'epoch', new_state->>'revoked' "
            "FROM acq_rights.audit WHERE epoch >= 4 ORDER BY epoch"
        ).fetchall() == [
            (4, ACTOR, REASON, dsns["operator_role"], withdrawal, None, "true"),
            (5, ACTOR, REASON, dsns["operator_role"], recovery, "4", "false"),
            (6, ACTOR, REASON, dsns["operator_role"], revoke(SOURCE, 5), "5", "true"),
        ]
        emitted = conn.execute(
            "SELECT o.event_id, o.topic, o.partition_key, o.envelope, a.epoch, a.at "
            "FROM public.kernel_outbox o LEFT JOIN acq_rights.audit a USING (event_id) ORDER BY o.seq"
        ).fetchall()

    assert [row[4] for row in emitted] == [4, 5, 6]
    for (event_id, topic, key, envelope, epoch, at), action in zip(
        emitted, ("withdraw", "retain", "withdraw"), strict=True
    ):
        assert (topic, key, envelope["subject"]) == (
            "public.acquisition",
            SOURCE,
            SOURCE,
        )
        assert (envelope["id"], envelope["source"], envelope["type"]) == (
            event_id,
            "miaoqia/acquisition",
            RIGHTS_CHANGED,
        )
        outbox.check_topic(topic, envelope)
        data = envelope["data"]
        payloads.validator(RIGHTS_CHANGED, {}).validate(data)
        assert data == {
            "source_id": SOURCE,
            "rights_version": epoch,
            "before": list(OPERATIONS) if action == "withdraw" else [],
            "after": [] if action == "withdraw" else list(OPERATIONS),
            "effective_at": envelope["time"],
            "action": action,
        }
        assert datetime.fromisoformat(data["effective_at"]) == at


def test_outbox_failure_rolls_back_rights_and_audit(registry):
    read, write, dsns = registry
    declare_public(dsns, SYNTHETIC)
    decisions = Decisions(read)
    req = request(f"public:{SYNTHETIC}", source=SYNTHETIC)
    original = evaluate(decisions, req)
    with psycopg.connect(dsns["owner"]) as owner:
        owner.execute(
            "ALTER TABLE public.kernel_outbox ADD CONSTRAINT reject_test_event CHECK (false) NOT VALID"
        )
    with pytest.raises(psycopg.errors.CheckViolation), psycopg.connect(
        dsns["operator"]
    ) as operator:
        operator.execute(
            PUBLIC_COMMAND, (Jsonb(revoke()), ACTOR, REASON, ids.new("evt"))
        )

    assert evaluate(decisions, confirmed(req, original), confirm=True)["allowed"]
    with psycopg.connect(dsns["owner"]) as owner:
        assert owner.execute("SELECT epoch FROM acq_rights.state").fetchone() == (0,)
        assert owner.execute("SELECT count(*) FROM acq_rights.audit").fetchone() == (0,)
        assert owner.execute(
            "SELECT count(*) FROM acq_rights.public_source_state"
        ).fetchone() == (0,)
        assert owner.execute(
            "SELECT count(*) FROM public.kernel_outbox"
        ).fetchone() == (0,)
        owner.execute(
            "ALTER TABLE public.kernel_outbox DROP CONSTRAINT reject_test_event"
        )
    assert write_command(write, revoke()) == write_command(write, revoke()) == 1
    assert not evaluate(decisions, confirmed(req, original), confirm=True)["allowed"]
    with psycopg.connect(dsns["owner"]) as owner:
        assert owner.execute(
            "SELECT count(*) FROM public.kernel_outbox"
        ).fetchone() == (1,)


def test_confirmation_waits_for_committed_revocation(registry):
    _, _, dsns = registry
    declare_public(dsns, SYNTHETIC)
    # Even with a database default of repeatable read, the lock waiter must see the new state.
    with psycopg.connect(dsns["owner"]) as conn:
        conn.execute(
            sql.SQL(
                "ALTER DATABASE {} SET default_transaction_isolation TO 'repeatable read'"
            ).format(sql.Identifier(conn.info.dbname))
        )
    reader = Registry(dsns["app"])
    decisions = Decisions(reader)
    req = request(f"public:{SYNTHETIC}", source=SYNTHETIC)
    try:
        original = evaluate(decisions, req)
        with ThreadPoolExecutor(max_workers=1) as pool, psycopg.connect(
            dsns["operator"]
        ) as operator:
            operator.execute(
                PUBLIC_COMMAND, (Jsonb(revoke()), ACTOR, REASON, ids.new("evt"))
            )
            waiting = pool.submit(
                evaluate, decisions, confirmed(req, original), confirm=True
            )
            with psycopg.connect(dsns["owner"], autocommit=True) as observer:
                assert observer.execute(
                    "SELECT count(*) FROM public.kernel_outbox"
                ).fetchone() == (0,)
                for _ in range(100):
                    if observer.execute(
                        "SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted "
                        "AND objid=682039154 AND database=(SELECT oid FROM pg_database WHERE datname=current_database())"
                    ).fetchone():
                        break
                    time.sleep(0.01)
                else:
                    pytest.fail("confirmation did not wait on the shared rights lock")
                operator.commit()
                assert observer.execute(
                    "SELECT count(*) FROM public.kernel_outbox o "
                    "JOIN acq_rights.audit a USING (event_id) "
                    "JOIN acq_rights.public_source_state s USING (epoch) WHERE s.revoked"
                ).fetchone() == (1,)
            result = waiting.result(timeout=3)
        assert result["epoch"] == 1 and not result["allowed"]
    finally:
        reader.close()


def test_reader_requires_explicit_public_source_state_grant(pg_factory):
    dsns = pg_factory()
    db.migrate(dsns["owner"], MIGRATIONS)
    declare_public(dsns, SYNTHETIC)
    reader = urlsplit(dsns["app"]).username
    with psycopg.connect(dsns["owner"]) as conn:
        conn.execute(sql.SQL("REVOKE ALL ON ALL TABLES IN SCHEMA public FROM {}").format(sql.Identifier(reader)))
        conn.execute(sql.SQL("GRANT USAGE ON SCHEMA acq_rights TO {}").format(sql.Identifier(reader)))
        for table in ("versions", "reviewed", "aliases", "current", "state", "audit"):
            conn.execute(sql.SQL("GRANT SELECT ON {} TO {}").format(
                sql.Identifier("acq_rights", table), sql.Identifier(reader)))
        conn.execute(sql.SQL("GRANT SELECT ON public.acq_sources TO {}").format(sql.Identifier(reader)))
    read = Registry(dsns["app"])
    try:
        with reqctx.use(CTX), pytest.raises(PermissionError):
            read.ensure_reader()
        with psycopg.connect(dsns["owner"]) as conn:
            conn.execute(sql.SQL("GRANT SELECT ON acq_rights.public_source_state TO {}").format(sql.Identifier(reader)))
            assert conn.execute("SELECT count(*) FROM acq_rights.public_source_state").fetchone() == (0,)
        with reqctx.use(CTX):
            read.ensure_reader()
        decision = evaluate(Decisions(read), request(f"public:{SYNTHETIC}", source=SYNTHETIC))
        assert decision["allowed"] and decision["epoch"] == 0
    finally:
        read.close()


def test_public_evidence_resolve_and_confirm_over_mtls(remote, registry):
    client, _, identity, _, _, _, write = remote
    declare_public(registry[2], SYNTHETIC)
    req = {
        **request(f"public:{SYNTHETIC}", source=SYNTHETIC),
        "operation": "evidence_resolve",
    }
    decision = call(client, body=req)
    assert decision["allowed"]
    write_command(write, revoke())
    assert not call(client, "confirm", confirmed(req, decision))["allowed"]
    assert not call(client, body=req)["allowed"]
    assert identity.sessions["seen"] == [CTX, CTX, CTX]
