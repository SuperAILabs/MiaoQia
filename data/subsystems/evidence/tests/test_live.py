"""Live ingest of acquisition.fetch.completed.v1 on a real PostgreSQL, using responses recorded from the pilot sources."""
from __future__ import annotations

import hashlib
from pathlib import Path

import pytest
from sqlalchemy import text

from miaoqia.evidence import MIGRATIONS, live_ingest
from miaoqia.kernel import db, reqctx
from miaoqia.kernel.objectstore import FilesystemObjectStore

REPO = Path(__file__).resolve().parents[4]
FIXTURES = REPO / "packs/information_domains/procurement/tests/fixtures/sources"


@pytest.fixture
def live(pg_factory, tmp_path):
    dsns = pg_factory()
    db.migrate(dsns["owner"], MIGRATIONS)
    database = db.Database(dsns["app"])
    objects = tmp_path / "objects"
    ingest = live_ingest(str(objects), str(REPO / "packs"))
    yield database, ingest, FilesystemObjectStore(objects)
    database.close()


def archived(store, source_id: str, fixture: str, url: str) -> dict:
    """Archive like acquisition does and return the fetch.completed envelope."""
    raw = (FIXTURES / fixture).read_bytes()
    digest = hashlib.sha256(raw).hexdigest()
    store.put(f"raw/{digest}", raw)
    data = {"source_id": source_id, "artifact_ref": f"raw/{digest}", "rights_tags": [f"public:{source_id}"],
            "content_hash": f"sha256:{digest}", "fetched_at": "2026-11-11T08:00:00Z", "source_url": url}
    return {"source": "miaoqia/acquisition", "subject": data["artifact_ref"], "type": "acquisition.fetch.completed.v1",
            "data": data}


def deliver(database, ingest, env):
    with reqctx.use(reqctx.service("evidence")), database.tx() as conn:
        ingest.on_completed(conn, env)


def rows(database, sql):
    with reqctx.use(reqctx.service("test")), database.tx() as conn:
        return [tuple(r) for r in conn.execute(text(sql))]


CASES = [
    ("anhui-ggzy-construction", "anhui_ggzy_detail_tender.html",
     "https://ggzy.ah.gov.cn/jsgc/newDetail?guid=15a09741-1789-4d86-96e2-b67972e7f5a8&tenderProjectType=1&bulletinNature=1",
     "proc.tender-notice@0.4.0"),
    ("anhui-government-procurement-spa", "anhui_ccgp_detail.json",
     "https://www.ccgp-anhui.gov.cn/portal/detail?articleId=ZW2UGlPzLzKA0ISW4BGnSA%3D%3D&parentId=541080",
     "proc.tender-notice@0.4.0"),
    ("national-public-resources", "national_detail_body.html",
     "https://www.ggzy.gov.cn/information/deal/html/a/420000/0201/20261111/f391aca0239344f9172d9c50c88e35d4.html",
     "proc.result-notice@0.2.0"),
]


@pytest.mark.parametrize("source_id, fixture, url, extractor", CASES)
def test_fetch_completed_becomes_snapshot_and_revision(live, source_id, fixture, url, extractor):
    database, ingest, store = live
    env = archived(store, source_id, fixture, url)
    deliver(database, ingest, env)
    (snap,) = rows(database, "SELECT source_id, source_key, artifact_ref, rights_tags, media_type FROM evd_snapshots")
    media = "application/json" if fixture.endswith(".json") else "text/html"
    assert snap == (source_id, url, env["data"]["artifact_ref"], [f"public:{source_id}"], media)
    assert rows(database, "SELECT extractor FROM evd_extraction_revisions") == [(extractor,)]
    # The original is archived unchanged under the same content-addressed key as acquisition's.
    assert store.get(env["data"]["artifact_ref"]) == (FIXTURES / fixture).read_bytes()
    # Redelivery is idempotent.
    deliver(database, ingest, env)
    assert rows(database, "SELECT count(*) FROM evd_snapshots") == [(1,)]
    assert rows(database, "SELECT count(*) FROM evd_extraction_revisions") == [(1,)]


def test_rejects_hash_mismatch_and_foreign_source(live):
    database, ingest, store = live
    env = archived(store, *CASES[0][:3])
    forged = {**env, "data": {**env["data"], "content_hash": "sha256:" + "0" * 64}}
    with pytest.raises(ValueError, match="content hash"):
        deliver(database, ingest, forged)
    with pytest.raises(ValueError, match="lineage"):
        deliver(database, ingest, {**env, "source": "miaoqia/evidence"})
    assert rows(database, "SELECT count(*) FROM evd_snapshots") == [(0,)]


def test_live_ingest_disabled_without_object_store():
    assert live_ingest("", str(REPO / "packs")) is None
