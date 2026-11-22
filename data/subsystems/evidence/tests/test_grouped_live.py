"""PostgreSQL coverage for selected Hunan notices from shared originals."""
from __future__ import annotations

import json
from dataclasses import asdict, replace
from datetime import date
from hashlib import sha256
from time import monotonic
from unittest.mock import Mock

import pytest
from alembic import command
from alembic.config import Config
from sqlalchemy.exc import DBAPIError, ProgrammingError

from miaoqia.acquisition.api.handlers import FetchResponse, NoticeGroup
from miaoqia.evidence import MIGRATIONS, live_ingest, notice_chunks, resolver
from miaoqia.evidence.api import ProvenanceIntegrityError, Unbound
from miaoqia.evidence.api.document_proofs import DocumentProofs
from miaoqia.evidence.application.live import GROUPED_COMPLETED
from miaoqia.evidence.extensions.packs import DocumentAdapter
from miaoqia.evidence.replay import assess_pending, replay_pending
from miaoqia.kernel import db, ids, reqctx
from miaoqia.kernel.consumer import Consumer
from miaoqia.kernel.objectstore import FilesystemObjectStore
from miaoqia_pack_procurement.sources.regional_works import HunanWorks

from test_live import REPO, deny_original_writes, deliver, live, replay_store, rows, set_live_parse  # noqa: F401

MEDIA_TYPE = "application/json"
SECTION = "fixture-hunan-section"
TARGET = {
    "id": "fixture-hunan-tender",
    "constructionSectionId": SECTION,
    "bulletinType": "ZHAOBIAO_NOTICE",
    "noticeSendTime": "2026-11-12 17:31:04",
    "noticeName": "合成目标工程公开招标公告",
    "noticeContent": "<p>招标人：合成目标建设单位</p><p>项目编号：HUNAN-TARGET-01</p>",
}
NEIGHBOR = {
    "id": "fixture-hunan-clarification",
    "constructionSectionId": SECTION,
    "bulletinType": "CHENGQING_NOTICE",
    "noticeSendTime": "2026-11-18 15:02:40",
    "noticeName": "合成相邻工程澄清公告",
    "noticeContent": "<p>相邻公告正文 HUNAN-NEIGHBOR-01</p>",
}

OBSERVATIONS = """
    SELECT source_id, notice_key, observation_version, snapshot_id, selector, published_on,
           record_digest, raw_sha256, media_type, size_bytes, is_anchor
    FROM evd_grouped_observations ORDER BY notice_key, observation_version
"""
SNAPSHOTS = "SELECT id, artifact_ref, raw_sha256, media_type FROM evd_snapshots ORDER BY seq"
PENDING = "SELECT event_id, source_id, envelope FROM evd_pending_live_events ORDER BY receipt_seq"
OUTBOX = "SELECT event_id FROM kernel_outbox ORDER BY event_id"


@pytest.fixture
def grouped(live, monkeypatch):
    database, ingest, store = live
    handler = _selection_adapter(ingest)
    deny_original_writes(ingest, monkeypatch)
    return database, ingest, store, handler


def _selection_adapter(ingest):
    handler = HunanWorks()
    ingest.adapters[handler.source_id] = DocumentAdapter(
        source_id=handler.source_id, media_type=MEDIA_TYPE, to_html=None,
        live_parse=True, to_selected_html=handler.to_selected_html,
    )
    return handler


def _event(store, handler, version=1, *, target=TARGET, neighbor=NEIGHBOR,
           target_first=False, wrapper_time=1, archive=True):
    channel = handler.channels[0]
    group_key = target["constructionSectionId"]
    group = NoticeGroup(group_key, handler.group_request(channel, group_key))
    notices = [target, neighbor] if target_first else [neighbor, target]
    raw = json.dumps({"code": 200, "success": True, "time": wrapper_time,
                      "data": {"noticeList": notices}}, ensure_ascii=False).encode()
    response = FetchResponse(200, MEDIA_TYPE, raw, group.request.url)
    members = handler.expand_group(channel, group, response)
    (member,) = (member for member in members if member.ref.source_key == target["id"])
    digest = sha256(raw).hexdigest()
    ref = f"raw/{digest}"
    if archive:
        store.put(ref, raw)
    fetched_at = f"2026-11-20T02:00:{version:02d}Z"
    return {
        "source": "miaoqia/acquisition", "subject": ref, "type": GROUPED_COMPLETED,
        "id": ids.new("evt"), "specversion": "1.0", "time": fetched_at,
        "datacontenttype": "application/json", "miaoqiazone": "public",
        "data": {
            "source_id": handler.source_id, "notice_key": member.ref.source_key,
            "observation_version": version, "artifact_ref": ref, "content_hash": f"sha256:{digest}",
            "source_url": member.ref.source_url, "rights_tags": [f"public:{handler.source_id}"],
            "fetched_at": fetched_at, "selector": asdict(member.selector),
            "published_on": member.ref.published.isoformat(), "record_digest": member.record_digest,
            "media_type": response.media_type, "size_bytes": len(raw),
        },
    }


def _binding(event, snapshot, *, anchor):
    data = event["data"]
    return (data["source_id"], data["notice_key"], data["observation_version"], snapshot,
            data["selector"], date.fromisoformat(data["published_on"]), data["record_digest"],
            data["content_hash"], data["media_type"], data["size_bytes"], anchor)


def _assert_no_projection(database):
    for sql in (OBSERVATIONS, SNAPSHOTS, OUTBOX, "SELECT id FROM evd_extraction_revisions",
                "SELECT id FROM evd_evidence_refs", "SELECT version FROM evd_live_observations"):
        assert rows(database, sql) == []


def test_grouped_selects_only_the_target_for_facts_and_chunks(grouped, monkeypatch):
    database, ingest, store, handler = grouped
    event = _event(store, handler)
    deliver(database, ingest, event)

    ((snapshot, ref, raw_sha256, media_type),) = rows(database, SNAPSHOTS)
    assert (ref, raw_sha256, media_type) == (event["subject"], event["data"]["content_hash"], MEDIA_TYPE)
    assert rows(database, OBSERVATIONS) == [_binding(event, snapshot, anchor=True)]
    ((revision_snapshot, facts),) = rows(database, "SELECT snapshot_id, facts FROM evd_extraction_revisions")
    assert revision_snapshot == snapshot
    (fact,) = facts
    assert fact["title"]["value"] == TARGET["noticeName"]
    assert fact["project_code"]["value"] == "HUNAN-TARGET-01"
    assert fact["published_at"]["value"] == "2026-11-12T09:31:04Z"
    assert "HUNAN-NEIGHBOR-01" not in json.dumps(facts)
    ((document_ref,),) = rows(database, "SELECT document_ref FROM evd_snapshots")
    document = json.loads(store.get(document_ref))
    assert "source_url" not in document
    assert NEIGHBOR["noticeName"] not in json.dumps(document, ensure_ascii=False)

    ctx = replace(reqctx.service("test"), purpose="object_page")
    reader = resolver(database, store, display=lambda tags, purpose: "full_text")
    with reqctx.use(ctx):
        title = reader.resolve(fact["title"]["prov"]["evidence_ref"])
        assert (title.snapshot_ref, title.snippet) == (snapshot, TARGET["noticeName"])
        published = reader.resolve(fact["published_at"]["prov"]["evidence_ref"])
        assert (published.snapshot_ref, published.snippet) == (snapshot, TARGET["noticeSendTime"])
        chunks = notice_chunks(database, store).read(
            snapshot, lambda sources: True, deadline=monotonic() + database.pool_timeout + 5,
        )
        assert chunks.snapshot == snapshot
        chunk_text = "\n".join(span.text for span in chunks.spans)
        assert TARGET["noticeName"] in chunk_text and "HUNAN-TARGET-01" in chunk_text
        assert TARGET["noticeSendTime"] in chunk_text
        assert NEIGHBOR["noticeName"] not in chunk_text and "HUNAN-NEIGHBOR-01" not in chunk_text

        read = Mock(side_effect=AssertionError("grouped snapshot must not read any object"))
        monkeypatch.setattr(store, "get", read)
        assert reader.snapshot(snapshot) is None
        read.assert_not_called()


@pytest.mark.parametrize("admission", ["no-adapter", "unspecified"])
def test_grouped_requires_explicit_parse_admission(grouped, admission, monkeypatch):
    database, ingest, store, handler = grouped
    event = _event(store, handler)
    if admission == "no-adapter":
        del ingest.adapters[handler.source_id]
    else:
        set_live_parse(ingest, handler.source_id, None)
    read = Mock(side_effect=AssertionError("unadmitted event must not read objects"))
    monkeypatch.setattr(ingest.store, "get", read)

    with pytest.raises(ValueError):
        deliver(database, ingest, event)

    read.assert_not_called()
    assert rows(database, PENDING) == []
    _assert_no_projection(database)


@pytest.mark.parametrize("invalid", ["selector", "notice-key", "record-digest", "date", "media", "size", "hash"])
def test_grouped_rejects_invalid_bindings_atomically(grouped, invalid):
    database, ingest, store, handler = grouped
    event = _event(store, handler)
    data = event["data"].copy()
    if invalid == "hash":
        path = store.root / event["subject"]
        raw = path.read_bytes()
        tampered = raw.replace(b'"time": 1', b'"time": 9', 1)
        assert tampered != raw and len(tampered) == len(raw)
        path.write_bytes(tampered)
    else:
        changes = {
            "selector": {"selector": dict(data["selector"], version=data["selector"]["version"] + 1)},
            "notice-key": {"notice_key": NEIGHBOR["id"]},
            "record-digest": {"record_digest": "sha256:" + "0" * 64},
            "date": {"published_on": "2026-11-13"},
            "media": {"media_type": "text/plain"},
            "size": {"size_bytes": data["size_bytes"] + 1},
        }
        data.update(changes[invalid])

    with pytest.raises(ValueError):
        deliver(database, ingest, dict(event, data=data))

    assert rows(database, PENDING) == []
    _assert_no_projection(database)


def test_grouped_rejects_unreadable_normalized_document(grouped):
    database, ingest, store, handler = grouped
    target = TARGET | {"noticeContent": "<p>" + "合" * 1_400_000 + "</p>"}
    event = _event(store, handler, target=target)
    with pytest.raises(ValueError, match="normalized document exceeds the evidence read limit"):
        deliver(database, ingest, event)
    _assert_no_projection(database)
    assert not (store.root / "normalized").exists()


def test_grouped_stages_without_object_reads_and_retries_replay(replay_store, monkeypatch):
    database, ingest, store, overwrite = replay_store
    handler = _selection_adapter(ingest)
    source = handler.source_id
    first = _event(store, handler, archive=False)
    second = _event(store, handler, 2, target=TARGET | {"noticeContent": "<p>目标公告正文更新</p>"})
    set_live_parse(ingest, source, False)

    with monkeypatch.context() as patch:
        read = Mock(side_effect=AssertionError("staging must not read objects"))
        patch.setattr(ingest.store, "get", read)
        consumer = Consumer("evidence.live", "evidence", "unused", ["public.acquisition"],
                            {GROUPED_COMPLETED: ingest.on_completed}, database, max_attempts=1)
        dead_letter = Mock()
        patch.setattr(consumer, "_dead_letter", dead_letter)
        message = Mock()
        message.value.return_value = json.dumps(first).encode()
        message.topic.return_value = "public.acquisition"
        consumer._handle(message, Mock(), "dlq.evidence")
        consumer._handle(message, Mock(), "dlq.evidence")
        dead_letter.assert_not_called()
        assert replay_pending(database, ingest, source, limit=2) == 0
        set_live_parse(ingest, source, True)
        deliver(database, ingest, second)
        read.assert_not_called()

    pending = [(event["id"], source, event) for event in (first, second)]
    assert rows(database, PENDING) == pending
    assert rows(database, "SELECT event_id FROM kernel_inbox") == [(first["id"],)]
    _assert_no_projection(database)
    set_live_parse(ingest, source, False)
    missing, available = assess_pending(database, ingest, source, limit=2)
    assert missing["event_id"] == first["id"] and missing["status"] == "archive_missing"
    assert available["event_id"] == second["id"] and available["status"] == "assessed"
    assert rows(database, PENDING) == pending
    _assert_no_projection(database)
    set_live_parse(ingest, source, True)
    with pytest.raises(FileNotFoundError):
        replay_pending(database, ingest, source, limit=2)
    assert rows(database, PENDING) == pending
    _assert_no_projection(database)

    assert _event(store, handler)["subject"] == first["subject"]
    original = store.get(first["subject"])
    tampered = original.replace(b'"time": 1', b'"time": 9', 1)
    assert tampered != original and len(tampered) == len(original)
    overwrite(first["subject"], tampered)
    with pytest.raises(ValueError, match="content hash"):
        replay_pending(database, ingest, source, limit=2)
    assert rows(database, PENDING) == pending
    _assert_no_projection(database)

    overwrite(first["subject"], original)
    assert replay_pending(database, ingest, source, limit=1) == 1
    ((first_snapshot, _, _, _),) = rows(database, SNAPSHOTS)
    assert rows(database, OBSERVATIONS) == [_binding(first, first_snapshot, anchor=True)]
    assert rows(database, PENDING) == pending[1:]
    assert replay_pending(database, ingest, source, limit=2) == 1
    assert replay_pending(database, ingest, source, limit=2) == 0
    snapshots = rows(database, SNAPSHOTS)
    assert [row[1] for row in snapshots] == [first["subject"], second["subject"]]
    assert rows(database, OBSERVATIONS) == [
        _binding(event, row[0], anchor=True) for event, row in zip((first, second), snapshots, strict=True)
    ]
    assert rows(database, PENDING) == []


def test_grouped_owner_proof_uses_the_fixed_original_and_selection(grouped, monkeypatch):
    database, ingest, store, handler = grouped
    first = _event(store, handler)
    deliver(database, ingest, first)
    ((snapshot, _, _, _),) = rows(database, SNAPSHOTS)
    ((facts,),) = rows(database, "SELECT facts FROM evd_extraction_revisions")
    title = facts[0]["title"]
    ref = title["prov"]["evidence_ref"]
    regular = DocumentProofs(database, store)
    owner = DocumentProofs(database, store, grouped_store=store, grouped_adapters=ingest.adapters)
    with reqctx.use(reqctx.service("test")):
        before = owner.requirements(snapshot, (ref,))
        wrapped = _event(store, handler, 2, wrapper_time=2)
        deliver(database, ingest, wrapped)
        assert owner.requirements(snapshot, (ref,)) == before

        with monkeypatch.context() as patch:
            read = Mock(side_effect=AssertionError("grouped proof requires explicit owner capability"))
            patch.setattr(store, "get", read)
            with pytest.raises(Unbound, match="grouped original proof unavailable"):
                regular.prove(snapshot, ref, title["value"])
            read.assert_not_called()

        read = Mock(wraps=store.get)
        monkeypatch.setattr(store, "get", read)
        source = owner.prove(snapshot, ref, title["value"])
        assert (source.snapshot_ref, source.source_id, source.source_key, source.raw_sha256) == (
            snapshot, handler.source_id, f"notice:{TARGET['id']}", first["data"]["content_hash"],
        )
        assert [call.args[0] for call in read.call_args_list if call.args[0].startswith("raw/")] == [first["subject"]]
        with pytest.raises(ProvenanceIntegrityError, match="field value mismatch"):
            owner.prove(snapshot, ref, NEIGHBOR["noticeName"])

        # A present original and valid stored quote cannot cover a mismatched selection.
        adapter = ingest.adapters[handler.source_id]
        ingest.adapters[handler.source_id] = replace(adapter, to_selected_html=lambda *args, **kwargs: "<p>other</p>")
        with pytest.raises(ProvenanceIntegrityError, match="selection differs"):
            owner.prove(snapshot, ref, title["value"])
        ingest.adapters[handler.source_id] = adapter
        path = store.root / first["subject"]
        path.write_bytes(path.read_bytes().replace(b'"time": 1', b'"time": 9', 1))
        with pytest.raises(ProvenanceIntegrityError, match="original or selection differs"):
            owner.prove(snapshot, ref, title["value"])


def test_grouped_migration_refuses_to_discard_pending_or_accepted_bindings(pg_factory, tmp_path):
    dsns = pg_factory()
    db.migrate(dsns["owner"], MIGRATIONS)
    config = Config()
    config.set_main_option("script_location", MIGRATIONS)
    config.set_main_option("sqlalchemy.url", db._url(dsns["owner"]))
    database = db.Database(dsns["app"])
    try:
        command.downgrade(config, "0006")
        assert rows(database, "SELECT to_regclass('evd_grouped_observations')") == [(None,)]
        command.upgrade(config, "head")
        ingest = live_ingest(str(tmp_path), str(REPO / "packs"))
        handler = _selection_adapter(ingest)
        set_live_parse(ingest, handler.source_id, False)
        event = _event(FilesystemObjectStore(tmp_path), handler)
        deliver(database, ingest, event)
        with pytest.raises(ProgrammingError, match="cannot downgrade with grouped evidence observations"):
            command.downgrade(config, "0006")
        assert rows(database, PENDING) == [(event["id"], handler.source_id, event)]

        set_live_parse(ingest, handler.source_id, True)
        assert replay_pending(database, ingest, handler.source_id, limit=1) == 1
        with pytest.raises(ProgrammingError, match="cannot downgrade with grouped evidence observations"):
            command.downgrade(config, "0006")
        ((snapshot, _, _, _),) = rows(database, SNAPSHOTS)
        assert rows(database, OBSERVATIONS) == [_binding(event, snapshot, anchor=True)]
    finally:
        database.close()


def test_grouped_rewrap_and_neighbor_change_keep_immutable_observation_bindings(grouped):
    database, ingest, store, handler = grouped
    first = _event(store, handler)
    deliver(database, ingest, first)
    snapshots = rows(database, SNAPSHOTS)
    ((snapshot, _, _, _),) = snapshots
    revisions = rows(database, "SELECT id FROM evd_extraction_revisions")
    outbox = rows(database, OUTBOX)

    rewrapped = _event(store, handler, 2, wrapper_time=2, target_first=True)
    neighbor = NEIGHBOR | {"noticeContent": "<p>相邻公告更新 HUNAN-NEIGHBOR-02</p>"}
    neighbor_changed = _event(store, handler, 3, neighbor=neighbor, wrapper_time=2, target_first=True)
    events = (first, rewrapped, neighbor_changed)
    assert len({event["subject"] for event in events}) == 3
    assert len({event["data"]["record_digest"] for event in events}) == 1

    for event in (rewrapped, neighbor_changed, neighbor_changed, dict(rewrapped, id=ids.new("evt"))):
        deliver(database, ingest, event)

    bindings = [_binding(event, snapshot, anchor=event is first) for event in events]
    assert rows(database, OBSERVATIONS) == bindings
    assert rows(database, SNAPSHOTS) == snapshots
    assert rows(database, "SELECT id FROM evd_extraction_revisions") == revisions
    assert rows(database, OUTBOX) == outbox

    for statement in ("UPDATE evd_grouped_observations SET is_anchor = NOT is_anchor",
                      "DELETE FROM evd_grouped_observations"):
        with pytest.raises(DBAPIError):
            rows(database, statement + " RETURNING observation_version")
    assert rows(database, OBSERVATIONS) == bindings


def test_grouped_semantic_changes_create_snapshots_and_conflicting_group_is_rejected(grouped):
    database, ingest, store, handler = grouped
    # bulletinType contributes to the record digest even when the projected text is unchanged.
    retyped = TARGET | {"bulletinType": "GENGZHENG_NOTICE"}
    edited = retyped | {"noticeContent": "<p>招标人：合成目标建设单位</p><p>项目编号：HUNAN-TARGET-02</p>"}
    redated = edited | {"noticeSendTime": "2026-11-13 17:31:04"}
    versions = (TARGET, retyped, edited, redated, TARGET)
    events = [_event(store, handler, version, target=target) for version, target in enumerate(versions, 1)]
    assert len({event["data"]["record_digest"] for event in events[:-1]}) == 4
    for event in events:
        deliver(database, ingest, event)

    snapshots = rows(database, SNAPSHOTS)
    assert [row[1] for row in snapshots] == [event["subject"] for event in events]
    bindings = [
        _binding(event, row[0], anchor=True) for event, row in zip(events, snapshots, strict=True)
    ]
    assert rows(database, OBSERVATIONS) == bindings
    outbox = rows(database, OUTBOX)

    other_section = {"constructionSectionId": "different-hunan-section"}
    conflicting = _event(store, handler, 6, target=edited | other_section, neighbor=NEIGHBOR | other_section)
    with pytest.raises(ValueError):
        deliver(database, ingest, conflicting)
    assert rows(database, OBSERVATIONS) == bindings
    assert rows(database, SNAPSHOTS) == snapshots
    assert rows(database, OUTBOX) == outbox
