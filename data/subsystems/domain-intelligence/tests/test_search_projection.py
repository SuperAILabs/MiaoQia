"""Owner export against independent PostgreSQL stores; grants exist only inside the fixture."""

from concurrent.futures import ThreadPoolExecutor
from copy import deepcopy
from functools import partial, wraps
import http.client
from dataclasses import replace
import json
import os
import subprocess
import sys
import time
from urllib.parse import urlsplit

import psycopg
import pytest
from sqlalchemy import text

from miaoqia import evidence
from miaoqia.acquisition import MIGRATIONS as ACQ_MIGRATIONS
from miaoqia.acquisition.infrastructure.privileges import provision
from miaoqia.acquisition.infrastructure.rights import Registry
from miaoqia.domain_intelligence.application import search_projection
from miaoqia.domain_intelligence.application.search_snapshot import capture
from miaoqia.domain_intelligence.api.search_projection import owner
from miaoqia.evidence.replay import corpus_artifact
from miaoqia.kernel import db, reqctx
from miaoqia.kernel.internal_http import Problem, peer
from miaoqia.runtime_data.search_projection import (
    CLIENT_PEER, PATH, SERVER_PEER, RegistryDiscoveryPolicy, route, server,
)

from test_facts import CORPUS, REPO, _events, _facts, plane  # noqa: F401
from test_projects import _materialize

SOURCE = "official-qzygjy"
CTX = reqctx.service("search-projection-test")
WORKER = reqctx.service("platform-public-search-workflow-worker")
FACT_UPSERTED = "domain.fact.upserted.v1"


@pytest.fixture
def export(plane, pg_factory):
    dsns, operator = pg_factory(), pg_factory()
    db.migrate(dsns["owner"], ACQ_MIGRATIONS)
    reader_name = urlsplit(dsns["app"]).username
    operator_name = urlsplit(operator["app"]).username
    provision(dsns["owner"], reader_name, operator_name)
    with psycopg.connect(dsns["owner"]) as conn:
        conn.execute("INSERT INTO acq_sources(source_id,handler,access) VALUES (%s,'test:Handler','{}')", (SOURCE,))
    reader = Registry(dsns["app"])
    operator = Registry(dsns["app"].replace(reader_name + ":", operator_name + ":"))
    try:
        with reqctx.use(CTX):
            reader.ensure_reader()
        projection = route(owner(plane[1], RegistryDiscoveryPolicy(reader, [SOURCE, "not-registered"])))
        yield projection, partial(_change_rights, operator, dsns["owner"]), reader
    finally:
        operator.close()
        reader.close()


def _ingest(plane, sample, tags, *, source=SOURCE):
    art = corpus_artifact(CORPUS / f"official-building-{sample}-20261026")
    with reqctx.use(CTX), plane[0].tx() as conn:
        result = plane[2].ingest(conn, replace(art, rights_tags=tags, source_id=source))
    return plane[3].extractions.revision(result.revisions[0])


def _facts_events(plane):
    return _events(plane[1], FACT_UPSERTED)


def _prepared(handle, event, *, context=WORKER):
    with reqctx.use(context):
        prepared = handle(event, time.monotonic() + 4)

    @wraps(prepared.before_send)
    def confirm(deadline):
        with reqctx.use(context):
            prepared.before_send(deadline)

    return replace(prepared, before_send=confirm)


def _mutations(handle, event):
    response = _prepared(handle, event)
    response.before_send(time.monotonic() + 3)
    return response.payload["mutations"]


def _change_rights(operator, owner_dsn, action, expected_epoch):
    command = {"action": action, "source_id": SOURCE, "expected_epoch": expected_epoch}
    if action == "restore_public":
        command["evidence_hash"] = "sha256:" + "a" * 64
    with reqctx.use(CTX):
        epoch = operator.command(command, "fixture-operator", "test-only decision")
    # Read the actual after-commit event from the acquisition transaction's outbox.
    with psycopg.connect(owner_dsn) as conn:
        events = conn.execute("""
            SELECT envelope FROM kernel_outbox WHERE topic='public.acquisition'
              AND partition_key=%s AND envelope->'data'->>'rights_version'=%s
        """, (SOURCE, str(epoch))).fetchall()
    ((event,),) = events
    assert event["data"]["action"] == ("withdraw" if action == "revoke_public" else "retain")
    return epoch, event


def test_exact_dto_for_notice_and_project(plane, export):
    handle, _, _ = export
    _materialize(plane, _ingest(plane, "tender", [f"public:{SOURCE}"]))
    events = _facts_events(plane)
    assert len(events) == 2
    notice = _facts(plane[1])[0]
    rows = [_mutations(handle, e)[0] for e in events]
    assert [row["revision"] for row in rows] == [1, 1]
    notice_row, project_row = rows
    assert notice_row["ref"] == {"kind": "notice", "domain": "INF-PROC", "id": notice["id"], "version": "1"}
    assert notice_row["title"] == notice["title"]["value"]
    assert notice_row["identifiers"][0] == notice["id"]
    assert notice_row["identifiers"][-1] == "XCXCG-2025-008"
    assert notice_row["notice_type"] == notice["notice_type"]
    assert ("published_at" in notice_row) == ("published_at" in notice)
    assert project_row["ref"]["kind"] == "fact" and project_row["ref"]["id"].startswith("pp_")
    assert project_row["identifiers"][-1] == "XCXCG-2025-008"
    allowed = {"ref", "revision", "withdrawn", "identifiers", "region", "notice_type", "published_at", "title"}
    assert all(set(row) <= allowed and row["withdrawn"] is False for row in rows)
    serialized = json.dumps(rows)
    assert "public:" not in serialized and "evidence_ref" not in serialized and "text" not in serialized
    assert [_mutations(handle, e)[0] for e in events] == rows


def test_notice_chunk_projection_replays_source_withdrawal(plane, export):
    _, change_rights, registry = export
    _materialize(plane, _ingest(plane, "tender", [f"public:{SOURCE}"]))
    projection = owner(plane[1], RegistryDiscoveryPolicy(registry, [SOURCE]),
                       notice_chunks=evidence.notice_chunks(plane[0], plane[2].store))
    handle = route(projection)
    event = next(event for event in _facts_events(plane) if event["subject"].startswith("ntc_"))
    rows = _mutations(handle, event)
    parent, *chunks = rows
    assert 1 <= len(chunks) <= 12 and all(row["text"] and row["revision"] == parent["revision"] for row in chunks)
    for i, chunk in enumerate(chunks):
        assert chunk["ref"] == {**parent["ref"], "kind": "chunk", "id": f"{parent['ref']['id']}~{i:02d}"}
    assert _mutations(handle, event) == rows
    _, revoked = change_rights("revoke_public", 0)
    tombstones = _mutations(handle, revoked)
    assert tombstones == [{"ref": row["ref"], "withdrawn": True, "revision": row["revision"] + 1} for row in rows]
    assert _mutations(handle, revoked) == tombstones


def test_revocation_confirmation_tombstones_and_replay(plane, export):
    handle, change_rights, _ = export
    _materialize(plane, _ingest(plane, "tender", [f"public:{SOURCE}"]))
    events = _facts_events(plane)
    before = [_mutations(handle, e)[0] for e in events]
    in_flight = _prepared(handle, events[0])
    epoch, signal = change_rights("revoke_public", 0)
    with pytest.raises(Problem) as rejected:
        in_flight.before_send(time.monotonic() + 3)
    assert rejected.value.status == 503
    tombstones = _mutations(handle, signal)
    assert len(tombstones) == 2
    assert {row["ref"]["id"] for row in tombstones} == {row["ref"]["id"] for row in before}
    assert all(set(row) == {"ref", "revision", "withdrawn"} and row["revision"] == 2 and row["withdrawn"] for row in tombstones)
    assert _mutations(handle, signal) == tombstones
    with pytest.raises(Problem) as future:
        _mutations(handle, {**signal, "data": {**signal["data"], "rights_version": epoch + 1}})
    assert future.value.status == 503
    assert {_mutations(handle, e)[0]["revision"] for e in events} == {2}
    _, restored_signal = change_rights("restore_public", epoch)
    restored = _mutations(handle, restored_signal)
    assert len(restored) == 2 and all(row["revision"] == 3 and not row["withdrawn"] for row in restored)
    assert _mutations(handle, signal) == restored


def test_project_does_not_export_mixed_provenance_or_old_version(plane, export):
    handle, _, _ = export
    _materialize(plane, _ingest(plane, "tender", [f"public:{SOURCE}"]))
    project_event = _facts_events(plane)[-1]
    original = _mutations(handle, project_event)[0]
    stale_response = _prepared(handle, project_event)
    _materialize(plane, _ingest(plane, "award", [f"public:{SOURCE}", "test:special-source"]))
    notice2, project2 = _facts_events(plane)[-2:]
    assert _mutations(handle, project_event) == []
    assert _mutations(handle, notice2) == []
    with pytest.raises(Problem) as stale:
        stale_response.before_send(time.monotonic() + 3)
    assert stale.value.status == 503
    assert _mutations(handle, project2) == [
        {"ref": original["ref"], "revision": 2, "withdrawn": True},
    ]
    assert _mutations(handle, project_event) == []


def test_new_live_version_is_monotone_and_concurrent_retries_are_equal(plane, export):
    handle, _, _ = export
    revision = _ingest(plane, "tender", [f"public:{SOURCE}"])
    _materialize(plane, revision)
    e1 = _facts_events(plane)[0]
    v1 = _mutations(handle, e1)[0]
    changed = deepcopy(revision.facts)
    changed[0]["title"]["value"] = "合成更正标题"
    changed[0]["published_at"] = {**changed[0]["title"], "value": "2026-11-10T12:45:33+08:00", "precision": "second"}
    _materialize(plane, replace(revision, id="exr_" + "1" * 26, facts=changed))
    e2 = _facts_events(plane)[-1]
    assert e2["subject"] == e1["subject"] and e2["data"]["fact_version"] == 2
    with ThreadPoolExecutor(max_workers=4) as pool:
        rows = list(pool.map(lambda _: _mutations(handle, e2)[0], range(8)))
    assert all(row == rows[0] for row in rows)
    assert rows[0]["revision"] == v1["revision"] + 1 and rows[0]["ref"]["version"] == "2"
    assert rows[0]["title"] == "合成更正标题"
    assert rows[0]["published_at"] == "2026-11-10T04:45:33Z"
    assert _mutations(handle, e1) == []


def test_unapproved_missing_or_corrupted_closure_fails_closed(plane, export):
    handle, _, reader = export
    _materialize(plane, _ingest(plane, "tender", [f"public:{SOURCE}"]))
    event = _facts_events(plane)[0]
    assert _mutations(route(owner(plane[1], RegistryDiscoveryPolicy(reader, []))), event) == []
    dom_owner = db.Database(plane[4]["owner"])
    try:
        with reqctx.use(CTX), dom_owner.tx() as conn:
            conn.execute(text("ALTER TABLE dom_version_requirements DISABLE TRIGGER USER"))
            conn.execute(text("UPDATE dom_version_requirements SET proof_hash='broken' WHERE fact_id=:i"), {"i": event["subject"]})
        with pytest.raises(Problem) as corrupt:
            _mutations(handle, event)
        assert corrupt.value.status == 503
        with reqctx.use(CTX), dom_owner.tx() as conn:
            conn.execute(text("DELETE FROM dom_version_requirements WHERE fact_id=:i"), {"i": event["subject"]})
        assert _mutations(handle, event) == []
    finally:
        dom_owner.close()


def test_cert_boundary_and_unsupported_or_forged_event(plane, export):
    handle, _, _ = export
    _materialize(plane, _ingest(plane, "tender", [f"public:{SOURCE}"]))
    event = _facts_events(plane)[0]
    evidence = _events(plane[0], "evidence.extraction.revised.v1")[0]
    assert _mutations(handle, evidence) == []
    for context in (
        reqctx.service("platform-public-search-consumer"),
        replace(WORKER, tenant="tenant", cell="cell"),
        replace(WORKER, principal=reqctx.Principal(WORKER.principal.id, "user")),
    ):
        with pytest.raises(Problem) as denied:
            _prepared(handle, event, context=context)
        assert denied.value.status == 403
    variants = [
        {**event, "source": "miaoqia/evidence"}, {**event, "miaoqiazone": "platform"},
        {**event, "subject": "wrong"}, {**event, "data": {**event["data"], "rights_tags": ["public:unverified"]}},
        {**event, "data": {**event["data"], "confidential": "unknown"}},
    ]
    for mutated in variants:
        with pytest.raises(Problem) as malformed:
            _prepared(handle, mutated)
        assert malformed.value.status == 400
    with pytest.raises(Problem) as missing:
        _prepared(handle, {**event, "data": {**event["data"], "fact_version": 999}})
    assert missing.value.status == 503


def test_source_refresh_resumes_bounded_pages_and_retries(plane, export, monkeypatch):
    handle, change_rights, _ = export
    _materialize(plane, _ingest(plane, "tender", [f"public:{SOURCE}"]))
    events = _facts_events(plane)
    assert len(events) == 2
    assert all(_mutations(handle, event)[0]["revision"] == 1 for event in events)
    _, signal = change_rights("revoke_public", 0)
    monkeypatch.setattr(search_projection, "MAX_BATCH", 1)
    first = _prepared(handle, signal)
    first.before_send(time.monotonic() + 3)
    assert len(first.payload["mutations"]) == 1
    assert first.payload["mutations"][0]["withdrawn"]
    cursor = first.payload["next_after"]
    assert cursor == first.payload["mutations"][0]["ref"]["id"]
    assert _prepared(handle, signal).payload == first.payload
    with reqctx.use(CTX), plane[1].tx() as conn:
        assert conn.execute(text("SELECT revision FROM dom_search_projection ORDER BY fact_id")).scalars().all() == [2, 1]
    last = _prepared(handle, {"event": signal, "after": cursor})
    last.before_send(time.monotonic() + 3)
    assert set(last.payload) == {"mutations"}
    assert len(last.payload["mutations"]) == 1
    assert last.payload["mutations"][0]["withdrawn"]
    assert last.payload["mutations"][0]["ref"]["id"] > cursor
    assert _prepared(handle, {"event": signal, "after": cursor}).payload == last.payload
    with pytest.raises(Problem) as invalid:
        _prepared(handle, {"event": events[0], "after": cursor})
    assert invalid.value.status == 400



def test_rebuild_page_uses_the_same_revisions_and_revalidates(plane, export):
    handle, change_rights, reader = export
    _materialize(plane, _ingest(plane, "tender", [f"public:{SOURCE}"]))
    events = _facts_events(plane)
    mutations = [_mutations(handle, event)[0] for event in events]
    handles = [{"id": event["subject"], "version": event["data"]["fact_version"]} for event in reversed(events)]
    projection = owner(plane[1], RegistryDiscoveryPolicy(reader, [SOURCE]))
    with reqctx.use(CTX):
        deadline = time.monotonic() + 4
        prepared = projection.prepare_snapshot_page(handles, deadline=deadline)
        prepared.confirm(deadline=deadline)
        assert prepared.mutations == sorted(mutations, key=lambda m: m["ref"]["id"])
        with pytest.raises(search_projection.ProjectionUnavailable):
            projection.prepare_snapshot_page([{**handles[0], "version": 2}], deadline=deadline)
        with pytest.raises(ValueError):
            projection.prepare_snapshot_page([handles[0], handles[0]], deadline=deadline)
        change_rights("revoke_public", 0)
        with pytest.raises(search_projection.ProjectionUnavailable):
            prepared.confirm(deadline=deadline)
        withdrawn = projection.prepare_snapshot_page(handles, deadline=deadline)
        withdrawn.confirm(deadline=deadline)
        assert all(m["withdrawn"] and m["revision"] == 2 for m in withdrawn.mutations)



def test_explicitly_selected_but_unregistered_source_is_never_discovered(plane, export):
    handle, _, _ = export
    source = "not-registered"
    _materialize(plane, _ingest(plane, "tender", [f"public:{source}"], source=source))
    (event,) = _facts_events(plane)
    assert _mutations(handle, event) == []


def test_snapshot_page_cli_uses_dedicated_rights_reader(plane, export):
    handle, _, reader = export
    _materialize(plane, _ingest(plane, "tender", [f"public:{SOURCE}"]))
    event = _facts_events(plane)[0]
    expected = _mutations(handle, event)
    env = {
        **os.environ,
        "MIAOQIA_PUBLIC_SEARCH_DISCOVERY_SOURCES": json.dumps([SOURCE]),
        "MIAOQIA_DOMAIN_INTELLIGENCE_DB_DSN": plane[1].engine.url.render_as_string(hide_password=False),
        "MIAOQIA_ACQUISITION_RIGHTS_READER_DSN": reader.database.engine.url.render_as_string(hide_password=False),
    }
    command = [sys.executable, "-m", "miaoqia.runtime_data.search_projection", "snapshot-page"]
    result = subprocess.run(command, input=json.dumps([{"id": event["subject"], "version": 1}]),
                            text=True, env=env, capture_output=True, timeout=10)
    assert result.returncode == 0 and json.loads(result.stdout) == {"mutations": expected}
    rejected = subprocess.run(command, input=json.dumps([{"id": event["subject"], "version": 2}]),
                              text=True, env=env, capture_output=True, timeout=10)
    assert rejected.returncode == 1 and rejected.stdout == ""


def test_rights_event_skips_never_projected_facts_when_currently_revoked(plane, export, monkeypatch):
    handle, change_rights, _ = export
    _materialize(plane, _ingest(plane, "tender", [f"public:{SOURCE}"]))
    events = _facts_events(plane)
    once_projected = _mutations(handle, events[0])[0]
    _, signal = change_rights("revoke_public", 0)
    monkeypatch.setattr(search_projection, "MAX_BATCH", 1)
    assert _mutations(handle, signal) == [
        {"ref": once_projected["ref"], "revision": 2, "withdrawn": True},
    ]


def _sample_search_start(broker):
    from confluent_kafka import KafkaError, KafkaException, TopicPartition
    from confluent_kafka.admin import AdminClient, NewTopic, OffsetSpec

    admin = AdminClient({"bootstrap.servers": broker})
    topics = ("public.acquisition", "public.domain", "public.entity", "public.evidence")
    for future in admin.create_topics([NewTopic(topic, 1, 1) for topic in topics], request_timeout=5).values():
        try:
            future.result(timeout=6)
        except KafkaException as exc:
            if exc.args[0].code() != KafkaError.TOPIC_ALREADY_EXISTS:
                raise
    parts = {TopicPartition(topic, 0): OffsetSpec.latest() for topic in topics}
    deadline = time.monotonic() + 8
    while True:
        try:
            offsets = admin.list_offsets(parts, request_timeout=2)
            result = {part.topic: {part.partition: value.result(timeout=3).offset} for part, value in offsets.items()}
            assert all(value == {0: 0} for value in result.values()), "requires an empty disposable Kafka broker"
            return result
        except KafkaException as exc:
            if exc.args[0].code() != KafkaError.NOT_LEADER_FOR_PARTITION or time.monotonic() >= deadline:
                raise
            time.sleep(0.1)


def test_owner_over_real_certificate_only_https(plane, export, pg_factory, tls_factory, running_server, tmp_path):
    _, change_rights, reader = export
    _materialize(plane, _ingest(plane, "tender", [f"public:{SOURCE}"]))
    events = _facts_events(plane)
    event = next(item for item in events if item["subject"].startswith("ntc_"))
    tls = tls_factory("owner-server", SERVER_PEER)
    projection = owner(plane[1], RegistryDiscoveryPolicy(reader, [SOURCE]),
                       notice_chunks=evidence.notice_chunks(plane[0], plane[2].store))
    listener = server(("127.0.0.1", 0), tls, projection)
    running_server(listener)
    worker = tls_factory("owner-client", CLIENT_PEER)

    def request(payload, *, headers=None, credentials=worker):
        conn = http.client.HTTPSConnection("localhost", listener.server_port, context=credentials.context(), timeout=5)
        try:
            conn.connect()
            peer(conn.sock.getpeercert(), {SERVER_PEER})
            conn.request("POST", PATH, json.dumps(payload), {"Content-Type": "application/json", **(headers or {})})
            response = conn.getresponse()
            return response.status, json.loads(response.read())
        finally:
            conn.close()

    status, live = request(event)
    assert status == 200 and live["mutations"][0]["revision"] == 1
    assert any(item["ref"]["kind"] == "chunk" for item in live["mutations"])
    status, invalid = request({**event, "data": {"fact_id": event["subject"]}})
    assert status == 400 and invalid["status"] == 400
    status, denied = request(event, headers={"Miaoqia-Context": WORKER.encode()})
    assert status in (400, 403) and denied["status"] == status
    consumer = tls_factory("owner-wrong-peer", "spiffe://miaoqia/service/platform-public-search-consumer")
    with pytest.raises((OSError, http.client.HTTPException)):
        request(event, credentials=consumer)
    open_search = os.environ.get("MIAOQIA_TEST_OPENSEARCH")
    if open_search:
        broker = os.environ.get("MIAOQIA_TEST_SEARCH_OWNER_KAFKA")
        offsets = _sample_search_start(broker) if broker else None
        manifest = capture(plane[1], page_size=projection.page_size)
        pages = []
        with reqctx.use(CTX):
            for handles in manifest["pages"]:
                deadline = time.monotonic() + 5
                prepared = projection.prepare_snapshot_page(handles, deadline=deadline)
                prepared.confirm(deadline=deadline)
                pages.append({"mutations": prepared.mutations})
        assert len(pages) == 2 and pages[0] == live
    _, signal = change_rights("revoke_public", 0)
    status, withdrawn = request(signal)
    assert status == 200
    assert withdrawn["mutations"] == [
        {"ref": row["ref"], "withdrawn": True, "revision": 2} for row in live["mutations"]
    ]

    if open_search:
        assert withdrawn["next_after"] == live["mutations"][0]["ref"]["id"]
        data = {"snapshot_id": manifest["snapshot_id"], "as_of": manifest["as_of"],
                "pages": pages, "withdrawal": signal, "replay_start": offsets}
        scenario = tmp_path / "search-owner.json"
        scenario.write_text(json.dumps(data), encoding="utf-8")
        search_db = pg_factory()
        command = ["go", "test", "./cmd/platform", "-run", "^TestPublicProjectionAgainstPythonOwner$", "-count=1", "-v"]
        run = subprocess.run(
            command, cwd=REPO / "services", timeout=45, capture_output=True, text=True,
            env=dict(os.environ, MIAOQIA_TEST_SEARCH_OWNER_URL=f"https://localhost:{listener.server_port}",
                     MIAOQIA_TEST_SEARCH_SCENARIO=str(scenario), MIAOQIA_TEST_SEARCH_DB_OWNER=search_db["owner"],
                     MIAOQIA_TEST_SEARCH_DB_APP=search_db["app"],
                     MIAOQIA_TEST_SEARCH_DB_APP_ROLE=urlsplit(search_db["app"]).username,
                     MIAOQIA_INTERNAL_TLS_CERT=worker.cert, MIAOQIA_INTERNAL_TLS_KEY=worker.key,
                     MIAOQIA_INTERNAL_TLS_CA=worker.ca),
        )
        assert run.returncode == 0 and "--- PASS: TestPublicProjectionAgainstPythonOwner" in run.stdout, run.stdout + run.stderr
