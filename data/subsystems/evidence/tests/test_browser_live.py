"""Evidence browser-pair tests with isolated PostgreSQL and a local archive."""
import json
from dataclasses import replace
from datetime import datetime, timezone
from unittest.mock import Mock, call

import pytest
from sqlalchemy.exc import DBAPIError

from miaoqia.acquisition.api.browser_pairs import BROWSER_COMPLETED, describe_projection, seal_browser_event
from miaoqia.acquisition.api.browser_projection import build_projection
from miaoqia.evidence import resolver
from miaoqia.evidence.api import ProvenanceIntegrityError, Unbound
from miaoqia.evidence.api.document_proofs import DocumentProofs
from miaoqia.evidence.application.live import COMPLETED, LEGACY_COMPLETED
from miaoqia.evidence.application.pipeline import Artifact
from miaoqia.evidence.domain.snapshot import normalized_hash, raw_hash
from miaoqia.evidence.extensions.packs import DocumentAdapter
from miaoqia.evidence.replay import assess_pending, replay_pending
from miaoqia.kernel import ids, reqctx
from miaoqia.kernel.consumer import Consumer
from miaoqia.sdk.derivation import fingerprint

from test_grouped_live import OUTBOX, PENDING, SNAPSHOTS, _event as grouped_event, grouped  # noqa: F401
from test_live import archived, deliver, live, rows, set_live_parse  # noqa: F401

SOURCE = "fixture-browser"
NOTICE = "9007199254740993"
MEDIA = "text/plain"
TITLE = "合成目标工程公开招标公告"
RAW = b"OPAQUE-BODY\xff\x00"
OBSERVATIONS = """
    SELECT observation_version, snapshot_id, event, is_anchor
    FROM evd_browser_observations ORDER BY observation_version
"""
CURSOR = "SELECT version FROM evd_live_observations"
TABLES = (OBSERVATIONS, CURSOR, SNAPSHOTS, OUTBOX,
          "SELECT id FROM evd_extraction_revisions", "SELECT id FROM evd_evidence_refs")


@pytest.fixture
def browser(live):
    _, ingest, _ = live
    ingest.adapters[SOURCE] = DocumentAdapter(SOURCE, MEDIA, live_parse=True, browser_projection=True)
    return live


def _event(store, version=1, *, raw=RAW, title=TITLE, source=SOURCE, notice=NOTICE):
    projection = build_projection(
        f"<h1>{title}</h1><script>window.invalid = '原件诱饵'</script>"
        "<p>招标人：合成目标建设单位</p><p>项目编号：BROWSER-01</p>"
    )
    digest = raw_hash(raw)
    ref = "raw/" + digest[7:]
    descriptor = describe_projection(projection)
    store.put(ref, raw)
    store.put(descriptor["artifact_ref"], projection)
    timestamp = "2026-11-20T02:00:00Z"
    data = seal_browser_event({
        "source_id": source, "notice_key": notice, "observation_version": version, "artifact_ref": ref,
        "rights_tags": [f"public:{source}"], "content_hash": digest, "fetched_at": timestamp,
        "source_url": f"https://example.org/notice?id={notice}", "published_on": "2026-11-19",
        "media_type": MEDIA, "size_bytes": len(raw), "content_encoding": "identity", "projection": descriptor,
        "binding": {
            "version": 1, "run_id": 1, "attempt_id": version + 2, "list_attempt_id": 2, "task_key": "a" * 64,
            "request_url": "https://example.org/api/notice", "request_fingerprint": "sha256:" + "b" * 64,
            "navigation_id": f"nav.{version}", "frame_id": "main", "loader_id": f"loader.{version}",
            "request_id": f"request.{version}",
        },
    })
    return {"source": "miaoqia/acquisition", "subject": ref, "type": BROWSER_COMPLETED,
            "data": data, "id": ids.new("evt"), "specversion": "1.0", "time": timestamp,
            "datacontenttype": "application/json", "miaoqiazone": "public"}


def _reseal(data, **changes):
    return seal_browser_event({k: v for k, v in data.items() if k != "binding_digest"} | changes)


def _keys(data):
    return data["artifact_ref"], data["projection"]["artifact_ref"]


def _state(database):
    return [rows(database, query) for query in TABLES]


def _require_no_materialization(database, store):
    assert all(result == [] for result in _state(database))
    assert not (store.root / "normalized").exists()


def _damage(store, ref, change):
    path = store.root / ref
    original = path.read_bytes()
    if change == "missing":
        path.unlink()
    else:
        path.write_bytes(bytes([original[0] ^ 1]) + original[1:])
    return path, original


def _consume(consumer, event):
    message = Mock()
    message.value.return_value = json.dumps(event).encode()
    message.topic.return_value = "public.acquisition"
    consumer._handle(message, Mock(), "dlq.evidence")


def test_browser_owner_proof_binds_both_objects_and_exact_projected_quote(browser, monkeypatch):
    database, ingest, store = browser
    event = _event(store)
    data = event["data"]
    put = ingest.pipeline.store.put
    monkeypatch.setattr(ingest.pipeline.store, "put", Mock(wraps=put))
    consumer = Consumer("evidence.live", "evidence", "unused", ["public.acquisition"],
                        {BROWSER_COMPLETED: ingest.on_completed}, database, max_attempts=1, backoff=0)
    monkeypatch.setattr(consumer, "_dead_letter", Mock())
    _consume(consumer, event)
    _consume(consumer, event)
    consumer._dead_letter.assert_not_called()
    assert rows(database, "SELECT event_id FROM kernel_inbox") == [(event["id"],)]
    ((snapshot, artifact_ref, digest, media),) = rows(database, SNAPSHOTS)
    assert (artifact_ref, digest, media) == (event["subject"], raw_hash(RAW), MEDIA)
    assert rows(database, OBSERVATIONS) == [(1, snapshot, data, True)]
    assert store.get(artifact_ref) == RAW
    ((ref, document_digest),) = rows(database, "SELECT document_ref, normalized_hash FROM evd_snapshots")
    document_bytes = store.get(ref)
    doc = json.loads(document_bytes)
    projection = json.loads(store.get(data["projection"]["artifact_ref"]))
    assert doc == dict(projection, snapshot_ref=snapshot)
    assert document_digest == normalized_hash(projection)
    assert "原件诱饵" not in json.dumps(doc, ensure_ascii=False)
    assert ingest.pipeline.store.put.call_args_list == [call(ref, document_bytes)]

    ((facts,),) = rows(database, "SELECT facts FROM evd_extraction_revisions")
    title = facts[0]["title"]
    assert title["value"] == TITLE
    evidence_ref = title["prov"]["evidence_ref"]
    owner = DocumentProofs(database, store, browser_store=store)
    ordinary = DocumentProofs(database, store)
    read = Mock(wraps=store.get)
    monkeypatch.setattr(store, "get", read)
    ctx = replace(reqctx.service("test"), purpose="object_page")
    with reqctx.use(ctx):
        closure = owner.requirements(snapshot, (evidence_ref,))
        assert set(kind for kind, _, _ in closure.dependencies) == {"snapshot", "evidence"}
        assert ("snapshot", snapshot, fingerprint({
            "id": snapshot, "source_id": SOURCE, "source_key": f"notice:{NOTICE}", "rights_tags": data["rights_tags"],
            "normalized_hash": document_digest, "raw_sha256": digest,
            "browser_binding": {
                "observation_version": "1", "projection": data["projection"], "binding_digest": data["binding_digest"],
            },
        })) in closure.dependencies
        with pytest.raises(Unbound, match="browser pair proof unavailable"):
            ordinary.prove(snapshot, evidence_ref, TITLE)
        assert resolver(database, store, display=lambda *_: "full_text").snapshot(snapshot) is None
        read.assert_not_called()
        source = owner.prove(snapshot, evidence_ref, TITLE)
        assert (source.source_key, source.raw_sha256) == (f"notice:{NOTICE}", raw_hash(RAW))
        assert [read_call.args[0] for read_call in read.call_args_list] == [*_keys(data), ref]
        with pytest.raises(ProvenanceIntegrityError, match="field value mismatch"):
            owner.prove(snapshot, evidence_ref, "different quote")
        for key in _keys(data):
            for change in ("missing", "tampered"):
                path, archived_bytes = _damage(store, key, change)
                with pytest.raises(ProvenanceIntegrityError, match="browser pair differs"):
                    owner.prove(snapshot, evidence_ref, TITLE)
                path.write_bytes(archived_bytes)


def test_browser_duplicates_late_and_changes_both_roles(browser):
    database, ingest, store = browser
    events = [_event(store), _event(store, 3), _event(store, 4, title=TITLE + "（更新）"),
              _event(store, 5, title=TITLE + "（更新）", raw=b"second"), _event(store, 6)]
    for event in events:
        deliver(database, ingest, event)
    snapshots = rows(database, SNAPSHOTS)
    assert len(snapshots) == 4
    assert snapshots[0][1:] == snapshots[1][1:] == snapshots[-1][1:]
    assert snapshots[2][1] != snapshots[1][1]
    assert rows(database, OBSERVATIONS) == [
        (event["data"]["observation_version"], snapshots[index][0], event["data"], anchor)
        for event, index, anchor in zip(events, (0, 0, 1, 2, 3), (True, False, True, True, True), strict=True)
    ]
    assert rows(database, CURSOR) == [(6,)]
    late = _event(store, 2, raw=b"late")
    (store.root / late["subject"]).unlink()
    baseline = _state(database)
    for event in events + [late]:
        deliver(database, ingest, dict(event, id=ids.new("evt")))
    with pytest.raises(ValueError, match="conflicts with the accepted version"):
        deliver(database, ingest, _event(store, 3, raw=b"equivocation"))
    assert _state(database) == baseline
    for statement in ("UPDATE evd_browser_observations SET is_anchor = NOT is_anchor",
                      "DELETE FROM evd_browser_observations"):
        with pytest.raises(DBAPIError, match="immutable"):
            rows(database, statement + " RETURNING observation_version")


@pytest.mark.parametrize("role", [0, 1], ids=["original", "projection"])
@pytest.mark.parametrize("change", ["missing", "tampered"])
def test_browser_bad_objects_roll_back_live_and_deferred_inbox(browser, role, change):
    database, ingest, store = browser
    event = _event(store)
    _damage(store, _keys(event["data"])[role], change)
    consumer = Consumer("evidence.live", "evidence", "unused", ["public.acquisition"],
                        {BROWSER_COMPLETED: ingest.on_completed}, database, max_attempts=1, backoff=0)
    consumer._dead_letter = Mock()
    for admission in (True, False):
        set_live_parse(ingest, SOURCE, admission)
        _consume(consumer, event)
        assert isinstance(consumer._dead_letter.call_args.args[-1],
                          FileNotFoundError if change == "missing" else ValueError)
        assert rows(database, "SELECT event_id FROM kernel_inbox") == []
        assert rows(database, PENDING) == []
        _require_no_materialization(database, store)


def test_browser_replay_rechecks_both_hashes_and_retains_failed_head(browser, monkeypatch):
    database, ingest, store = browser
    set_live_parse(ingest, SOURCE, False)
    first = _event(store)
    second = _event(store, 2, title=TITLE + "（更新）")
    deliver(database, ingest, first)
    deliver(database, ingest, dict(first, id=ids.new("evt")))
    with pytest.raises(ValueError, match="conflicts with the pending version"):
        deliver(database, ingest, _event(store, raw=b"pending-equivalent-version"))
    assert replay_pending(database, ingest, SOURCE, limit=2) == 0
    (assessment,) = assess_pending(database, ingest, SOURCE, limit=1)
    assert assessment["status"] == "assessed" and assessment["content_hash"] == first["data"]["content_hash"]
    assert assessment["revisions"][0]["fact_count"] == 1
    path, original = _damage(store, first["data"]["projection"]["artifact_ref"], "tampered")
    assert assess_pending(database, ingest, SOURCE, limit=1)[0]["status"] == "input_rejected"
    path.write_bytes(original)
    set_live_parse(ingest, SOURCE, True)
    deliver(database, ingest, second)
    pending = [(event["id"], SOURCE, event) for event in (first, second)]
    assert rows(database, PENDING) == pending
    _require_no_materialization(database, store)

    for key in _keys(first["data"]):
        for change in ("missing", "tampered"):
            path, original = _damage(store, key, change)
            with pytest.raises(FileNotFoundError if change == "missing" else ValueError):
                replay_pending(database, ingest, SOURCE, limit=2)
            assert rows(database, PENDING) == pending
            _require_no_materialization(database, store)
            path.write_bytes(original)
    read = Mock(wraps=ingest.store.get)
    monkeypatch.setattr(ingest.store, "get", read)
    assert replay_pending(database, ingest, SOURCE, limit=2) == 2
    assert [read_call.args[0] for read_call in read.call_args_list] == [
        key for event in (first, second) for key in _keys(event["data"])
    ]
    assert replay_pending(database, ingest, SOURCE, limit=2) == 0
    assert rows(database, PENDING) == []
    assert [r[2] for r in rows(database, OBSERVATIONS)] == [first["data"], second["data"]]
    assert rows(database, CURSOR) == [(2,)]


def test_browser_rejects_unadmitted_types_or_altered_binding_before_io(browser, monkeypatch):
    database, ingest, store = browser
    event = _event(store)
    data = event["data"]
    invalid = (
        dict(event, source="miaoqia/other"),
        dict(event, subject=data["projection"]["artifact_ref"]),
        dict(event, data={k: v for k, v in data.items() if k != "artifact_ref"}),
        dict(event, data=dict(data, binding=dict(data["binding"], loader_id="different-navigation"))),
        dict(event, data=dict(data, projection=data["projection"] | {"artifact_ref": event["subject"]})),
        dict(event, data=dict(data, binding=dict(data["binding"], cookie="invalid-closed-field"))),
        dict(event, data=_reseal(data, media_type="text/html")),
    )
    read = Mock(side_effect=AssertionError("invalid browser event must stop before object reads"))
    monkeypatch.setattr(ingest.store, "get", read)
    for forged in invalid:
        with pytest.raises(ValueError):
            deliver(database, ingest, forged)
    for adapter in (None, DocumentAdapter(SOURCE, MEDIA, bytes.decode, True),
                    DocumentAdapter(SOURCE, MEDIA, browser_projection=True)):
        ingest.adapters[SOURCE] = adapter
        with pytest.raises(ValueError, match="explicit source and parse admission"):
            deliver(database, ingest, event)
    read.assert_not_called()
    _require_no_materialization(database, store)


@pytest.mark.parametrize("browser_first", [False, True])
@pytest.mark.parametrize("standalone_kind", ["direct-notice", "direct-url", LEGACY_COMPLETED, COMPLETED])
def test_browser_rejects_standalone_stream_collisions(browser, browser_first, standalone_kind):
    database, ingest, store = browser
    adapter = ingest.adapters.pop(SOURCE)
    event = _event(store, 2 if not browser_first else 1)
    legacy = archived(store, SOURCE, "anhui_ggzy_detail_tender.html", event["data"]["source_url"])
    standalone = legacy
    if standalone_kind == COMPLETED:
        standalone = dict(legacy, type=COMPLETED, data=dict(legacy["data"], notice_key=NOTICE, observation_version=3))

    def send_standalone():
        ingest.adapters.pop(SOURCE, None)
        if standalone_kind.startswith("direct"):
            key = f"notice:{NOTICE}" if standalone_kind == "direct-notice" else legacy["data"]["source_url"]
            with reqctx.use(reqctx.service("evidence")), database.tx() as conn:
                ingest.pipeline.ingest(conn, Artifact(
                    SOURCE, key, "text/html", b"<p>direct</p>", (f"public:{SOURCE}",), datetime.now(timezone.utc)))
        else:
            deliver(database, ingest, standalone)

    def send_browser():
        ingest.adapters[SOURCE] = adapter
        deliver(database, ingest, event)

    first, second = (send_browser, send_standalone) if browser_first else (send_standalone, send_browser)
    first()
    baseline = _state(database)
    with pytest.raises(ValueError, match="conflicts"):
        second()
    assert _state(database) == baseline


@pytest.mark.parametrize("browser_first", [False, True])
def test_browser_rejects_grouped_stream_collisions(grouped, browser_first):
    database, ingest, store, handler = grouped
    event = grouped_event(store, handler)
    source, notice = handler.source_id, event["data"]["notice_key"]
    pair = _event(store, 2, source=source, notice=notice)
    grouped_adapter = ingest.adapters[source]
    adapter = DocumentAdapter(source, MEDIA, live_parse=True, browser_projection=True)
    first, second = ((pair, adapter), (event, grouped_adapter)) if browser_first else (
        (event, grouped_adapter), (pair, adapter))
    ingest.adapters[source] = first[1]
    deliver(database, ingest, first[0])
    baseline = _state(database)
    ingest.adapters[source] = second[1]
    with pytest.raises(ValueError, match="conflicts with the accepted observation type"):
        deliver(database, ingest, second[0])
    assert _state(database) == baseline


