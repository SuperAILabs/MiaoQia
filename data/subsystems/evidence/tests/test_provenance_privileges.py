"""Actual login roles, including explicit global default function/table grants."""
from concurrent.futures import ThreadPoolExecutor
from urllib.parse import urlsplit
import dataclasses
import json

import psycopg
from psycopg import sql
import pytest

from miaoqia import evidence
from miaoqia.evidence.infrastructure.objectstore import FilesystemObjectStore
from miaoqia.evidence.infrastructure.privileges import provision
from miaoqia.kernel import db, reqctx

REF = {"name": "proc-building-topic", "version": "0.1.0"}
CTX = dataclasses.replace(reqctx.service("test"), purpose="object_page")


@pytest.fixture
def provenance_roles(pg_factory, tmp_path):
    dsns, other = pg_factory(), pg_factory()
    reader, binder = urlsplit(dsns["app"]).username, urlsplit(other["app"]).username
    binder_dsn = dsns["app"].replace(reader+":", binder+":")
    with psycopg.connect(dsns["owner"]) as conn:
        for role in (reader, binder):
            conn.execute(sql.SQL("ALTER DEFAULT PRIVILEGES GRANT ALL ON TABLES TO {}").format(sql.Identifier(role)))
            conn.execute(sql.SQL("ALTER DEFAULT PRIVILEGES GRANT EXECUTE ON FUNCTIONS TO {}").format(sql.Identifier(role)))
    db.migrate(dsns["owner"], evidence.MIGRATIONS)
    # Migration itself strips direct EXECUTE defaults, before dedicated role provisioning.
    for dsn in (dsns["app"], binder_dsn):
        with psycopg.connect(dsn) as conn:
            assert not conn.execute("SELECT has_function_privilege(current_user,'evd_provenance.bind_reference(text,text,text,text)','EXECUTE')").fetchone()[0]
    database = db.Database(dsns["app"])
    service = evidence.enrichments(database, FilesystemObjectStore(tmp_path), facts=lambda *args: None,
                                  display=lambda *args: "full_text")
    recipe = evidence.load_recipe(__import__('pathlib').Path(__file__).resolve().parents[4] / "packs/industries/building-materials",
        "miaoqia_pack_building_materials.topic:build_recipe", __import__('pathlib').Path(__file__).resolve().parents[4] / "packs")
    with reqctx.use(CTX):
        service.register_reference(**REF, content=recipe.content, rights_tags=["review:building-topic-pair"])
    database.close()
    provision(dsns["owner"], reader, binder)
    return {**dsns, "binder": binder_dsn, "reader_role": reader, "binder_role": binder}


def bind(dsn, name=REF["name"], version=REF["version"], actor="reviewer", reason="approved reviewed frozen reference"):
    with psycopg.connect(dsn) as conn:
        conn.execute("SELECT evd_provenance.bind_reference(%s,%s,%s,%s)", (name, version, actor, reason))


def test_roles_are_scram_separate_immutable_and_audited(provenance_roles):
    roles = provenance_roles
    with psycopg.connect(roles["binder"]) as conn:
        assert conn.execute("SELECT current_user").fetchone()[0] == roles["binder_role"]
    with pytest.raises(psycopg.OperationalError):
        psycopg.connect(roles["binder"].replace(":pw@", ":wrong@"))
    for target in ("app", "binder"):
        for statement in ("INSERT INTO evd_provenance.manifest SELECT * FROM evd_provenance.manifest",
            "INSERT INTO evd_provenance.bindings SELECT * FROM evd_provenance.bindings",
            "UPDATE evd_provenance.manifest SET source_id='official-qzygjy'", "DELETE FROM evd_provenance.bindings",
            "TRUNCATE evd_provenance.manifest CASCADE", "ALTER TABLE evd_provenance.bindings DISABLE TRIGGER ALL",
            "CREATE TABLE public.binding_attack(i int)", "CREATE TEMP TABLE binding_attack(i int)", "SET ROLE "+urlsplit(roles["owner"]).username,
            "INSERT INTO evd_reference_data VALUES ('x','1','object','sha256:bad','[]')"):
            with pytest.raises(psycopg.Error), psycopg.connect(roles[target]) as conn:
                conn.execute(statement)
    with pytest.raises(psycopg.Error):
        bind(roles["app"])
    with ThreadPoolExecutor(max_workers=3) as pool:
        list(pool.map(bind, [roles["binder"]]*6))
    bind(roles["binder"], actor="another-reviewer", reason="retry preserves original audit")
    with psycopg.connect(roles["app"]) as conn:
        row = conn.execute("SELECT source_kind,source_id,rights_tags,actor,reason,db_actor,manifest_hash FROM evd_provenance.bindings").fetchone()
        assert row[:4] == ("reference", "reference-proc-building-topic-v0-1", ["review:building-topic-pair"], "reviewer")
        assert row[5] == roles["binder_role"] and row[6].startswith("sha256:")
        assert row[6] == "sha256:37713db5b5aa67954fa29ce0539e3ed23221cc611540b4969e8f42e363a4e155"
    for target in ("app", "binder", "owner"):
        for table in ("evd_provenance.manifest", "evd_provenance.bindings"):
            with pytest.raises(psycopg.Error), psycopg.connect(roles[target]) as conn:
                conn.execute("TRUNCATE "+table+" CASCADE")
    for name, version in (("test-identity-policy", "1"), ("official-qzygjy", "0.1.0"), (REF["name"], "0.2.0")):
        with pytest.raises(psycopg.Error):
            bind(roles["binder"], name=name, version=version)


def test_binder_fixed_search_path_and_atomic_unknown_batch(provenance_roles):
    roles = provenance_roles
    with pytest.raises(psycopg.Error), psycopg.connect(roles["binder"]) as conn:
        conn.execute("SELECT evd_provenance.bind_reference(%s,%s,'reviewer','first valid')", tuple(REF.values()))
        conn.execute("SELECT evd_provenance.bind_reference('unknown','1','reviewer','invalid batch member')")
    with psycopg.connect(roles["app"]) as conn:
        assert conn.execute("SELECT count(*) FROM evd_provenance.bindings").fetchone()[0] == 0
    with psycopg.connect(roles["binder"]) as conn:
        conn.execute("SET search_path=pg_temp,public")
        conn.execute("SELECT evd_provenance.bind_reference(%s,%s,'reviewer','fixed path')", tuple(REF.values()))
    with psycopg.connect(roles["app"]) as conn:
        assert conn.execute("SELECT count(*) FROM evd_provenance.bindings").fetchone()[0] == 1


@pytest.mark.parametrize("column,value", [("content_hash", "sha256:"+"0"*64), ("rights_tags", '["public-official-review"]')])
def test_binding_rejects_wrong_reference_hash_or_original_tags(provenance_roles, column, value):
    roles = provenance_roles
    with psycopg.connect(roles["owner"]) as conn:
        conn.execute("ALTER TABLE evd_reference_data DISABLE TRIGGER USER")
        conn.execute(sql.SQL("UPDATE evd_reference_data SET {}=%s").format(sql.Identifier(column)), (value,))
        conn.execute("ALTER TABLE evd_reference_data ENABLE TRIGGER USER")
    with pytest.raises(psycopg.Error):
        bind(roles["binder"])
    with psycopg.connect(roles["app"]) as conn:
        assert conn.execute("SELECT count(*) FROM evd_provenance.bindings").fetchone()[0] == 0


def test_installed_binder_cli_uses_restricted_role_and_rolls_back_batch(provenance_roles, tmp_path):
    import os
    from pathlib import Path
    import subprocess
    import sys
    roles = provenance_roles
    batch = tmp_path / "batch.json"
    env = {**os.environ, "MIAOQIA_EVIDENCE_BINDER_DSN": roles["binder"]}
    args = [str(Path(sys.executable).parent / "mq-provenance"), "bind", "--batch", str(batch),
            "--actor", "reviewer", "--reason", "explicit reviewed batch"]
    batch.write_text(json.dumps([REF, {"name": "z-unknown", "version": "1"}]))
    failed = subprocess.run(args, env=env, capture_output=True, text=True)
    assert failed.returncode == 2 and failed.stderr.strip() == "provenance operation failed"
    with psycopg.connect(roles["app"]) as conn:
        assert conn.execute("SELECT count(*) FROM evd_provenance.bindings").fetchone()[0] == 0
    batch.write_text(json.dumps([REF]))
    done = subprocess.run(args, env=env, capture_output=True, text=True)
    assert done.returncode == 0, done.stderr
    with psycopg.connect(roles["app"]) as conn:
        assert conn.execute("SELECT db_actor FROM evd_provenance.bindings").fetchone()[0] == roles["binder_role"]
