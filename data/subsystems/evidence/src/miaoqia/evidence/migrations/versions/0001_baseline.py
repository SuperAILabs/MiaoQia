"""Evidence baseline schema (evidence.md section 4).

Document bodies and raw artifacts live in the object store; the database keeps metadata, object keys, extraction
results (structured fields and locators), evidence locators, live observation bindings and the owner-approved
reference provenance. Snapshots, extraction revisions, evidence references and observation bindings are immutable:
triggers reject UPDATE/DELETE and corrections append new revisions.

Revision ID: 0001
Revises:
"""
import hashlib
import json
from importlib import resources

from alembic import op

revision = "0001"
down_revision = None
branch_labels = None
depends_on = None

# The finite owner approval is pinned; changing the reviewed manifest requires a new migration.
REVIEWED_MANIFEST = {"schema_version": 1, "references": [{
    "name": "proc-building-topic", "version": "0.1.0",
    "content_hash": "sha256:2b5aef8fb787639360a27918f62ee56c4fb1c573f0c564ed288e1660fd13e258",
    "rights_tags": ["review:building-topic-pair"], "source_kind": "reference",
    "source_id": "reference-proc-building-topic-v0-1"}]}


def upgrade() -> None:
    manifest = json.loads(resources.files("miaoqia.evidence").joinpath("resources/reference_manifest.json").read_text())
    if manifest != REVIEWED_MANIFEST:
        raise ValueError("reviewed manifest changed")
    manifest_hash = "sha256:" + hashlib.sha256(json.dumps(
        manifest, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()).hexdigest()
    op.execute("""
-- 来源快照：同一来源对象（source_id + source_key）的规范化内容与最新快照不同即为新快照，seq 自 1 递增；
-- 内容回到历史取值（A→B→A）同样是来源的一次变化，记为新快照，不复用历史快照。
CREATE TABLE evd_snapshots (
    id               text PRIMARY KEY CHECK (id ~ '^snp_[0-9A-HJKMNP-TV-Z]{26}$'),
    source_id        text NOT NULL CHECK (source_id <> ''),
    source_key       text NOT NULL CHECK (source_key <> ''),
    seq              integer NOT NULL CHECK (seq >= 1),
    normalized_hash  text NOT NULL CHECK (normalized_hash ~ '^sha256:[0-9a-f]{64}$'),
    raw_sha256       text NOT NULL CHECK (raw_sha256 ~ '^sha256:[0-9a-f]{64}$'),
    artifact_ref     text NOT NULL,          -- 原始制品对象键（raw/…），不是签名地址
    document_ref     text NOT NULL,          -- 规范化文档对象键（normalized/…）
    media_type       text NOT NULL,
    pipeline_version text NOT NULL,
    rights_tags      jsonb NOT NULL CHECK (jsonb_typeof(rights_tags) = 'array' AND jsonb_array_length(rights_tags) >= 1),
    observed_at      timestamptz NOT NULL,   -- 来源观察时间（采集或回放材料的取得时间）
    recorded_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source_id, source_key, seq)
);

-- 解析修订：一个快照在一个解析器版本下只有一个修订；人工修正与重放追加新修订。
CREATE TABLE evd_extraction_revisions (
    id             text PRIMARY KEY CHECK (id ~ '^exr_[0-9A-HJKMNP-TV-Z]{26}$'),
    snapshot_id    text NOT NULL REFERENCES evd_snapshots (id),
    extractor      text NOT NULL,           -- 解析器标识@版本
    pack_id        text NOT NULL CHECK (pack_id ~ '^INF-[A-Z]+$'),
    pack_version   text NOT NULL CHECK (pack_version ~ '^[0-9]+\\.[0-9]+\\.[0-9]+$'),
    facts          jsonb NOT NULL CHECK (jsonb_typeof(facts) = 'array'),
    quality_status text NOT NULL CHECK (quality_status IN ('ok', 'warning', 'suspect')),
    warnings       jsonb NOT NULL CHECK (jsonb_typeof(warnings) = 'array'),
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (snapshot_id, extractor)
);

-- 文档证据引用：快照、管线版本与定位器；quote_hash 用于复核原文未变。
CREATE TABLE evd_evidence_refs (
    id               text PRIMARY KEY CHECK (id ~ '^ev_[0-9A-HJKMNP-TV-Z]{26}$'),
    snapshot_id      text NOT NULL REFERENCES evd_snapshots (id),
    pipeline_version text NOT NULL,
    locator          jsonb NOT NULL CHECK (jsonb_typeof(locator) = 'object'),
    quote_hash       text NOT NULL CHECK (quote_hash ~ '^sha256:[0-9a-f]{64}$')
);
CREATE INDEX evd_evidence_refs_snapshot ON evd_evidence_refs (snapshot_id);

CREATE FUNCTION evd_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'evidence 记录不可变：%', TG_TABLE_NAME USING ERRCODE = 'integrity_constraint_violation';
END $$;
CREATE TRIGGER evd_snapshots_immutable BEFORE UPDATE OR DELETE ON evd_snapshots
    FOR EACH ROW EXECUTE FUNCTION evd_immutable();
CREATE TRIGGER evd_extraction_revisions_immutable BEFORE UPDATE OR DELETE ON evd_extraction_revisions
    FOR EACH ROW EXECUTE FUNCTION evd_immutable();
CREATE TRIGGER evd_evidence_refs_immutable BEFORE UPDATE OR DELETE ON evd_evidence_refs
    FOR EACH ROW EXECUTE FUNCTION evd_immutable();

-- 不可变参考内容和L3增强对象登记；正文/结果仍在对象存储。
CREATE TABLE evd_reference_data (
 name text NOT NULL, version text NOT NULL, object_ref text NOT NULL, content_hash text NOT NULL,
 rights_tags jsonb NOT NULL, PRIMARY KEY(name,version)
);
CREATE TABLE evd_enrichments (
 run_key text PRIMARY KEY, snapshot_ref text NOT NULL REFERENCES evd_snapshots(id),
 revision_ref text NOT NULL REFERENCES evd_extraction_revisions(id), recipe text NOT NULL,
 object_ref text NOT NULL, content_hash text NOT NULL, rights_requirements jsonb NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT now(), UNIQUE(snapshot_ref,revision_ref,recipe)
);
CREATE TABLE evd_derivations (
 id text PRIMARY KEY, fingerprint text NOT NULL UNIQUE, object_ref text NOT NULL, content_hash text NOT NULL,
 rights_requirements jsonb NOT NULL
);
CREATE TRIGGER evd_reference_data_immutable BEFORE UPDATE OR DELETE ON evd_reference_data
 FOR EACH ROW EXECUTE FUNCTION evd_immutable();
CREATE TRIGGER evd_enrichments_immutable BEFORE UPDATE OR DELETE ON evd_enrichments
 FOR EACH ROW EXECUTE FUNCTION evd_immutable();
CREATE TRIGGER evd_derivations_immutable BEFORE UPDATE OR DELETE ON evd_derivations
 FOR EACH ROW EXECUTE FUNCTION evd_immutable();

-- Immutable input closure and the single finite owner-approved reference binding.
CREATE TABLE evd_requirement_sets (
 kind text NOT NULL CHECK(kind IN ('derivation','enrichment')), key text NOT NULL,
 subject_hash text NOT NULL, closure jsonb NOT NULL, proof_hash text NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT now(), actor text NOT NULL, reason text NOT NULL,
 db_actor text NOT NULL DEFAULT session_user, PRIMARY KEY(kind,key));
CREATE FUNCTION evd_requirement_audit() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NEW.actor !~ '^[a-zA-Z0-9_-]{1,96}$' OR length(NEW.reason) NOT BETWEEN 1 AND 256 OR NEW.reason ~ '[[:cntrl:]]' THEN
 RAISE EXCEPTION 'invalid audit metadata'; END IF;
 NEW.db_actor=session_user; NEW.recorded_at=clock_timestamp(); RETURN NEW;
END $$;
CREATE TRIGGER evd_requirements_audit BEFORE INSERT ON evd_requirement_sets
 FOR EACH ROW EXECUTE FUNCTION evd_requirement_audit();
CREATE TRIGGER evd_requirements_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON evd_requirement_sets
 FOR EACH STATEMENT EXECUTE FUNCTION evd_immutable();
CREATE SCHEMA evd_provenance;
REVOKE ALL ON SCHEMA evd_provenance FROM PUBLIC;
CREATE TABLE evd_provenance.manifest (
 name text NOT NULL, version text NOT NULL, content_hash text NOT NULL, rights_tags jsonb NOT NULL,
 source_kind text NOT NULL CHECK(source_kind='reference'), source_id text NOT NULL, manifest_hash text NOT NULL,
 PRIMARY KEY(name,version));
INSERT INTO evd_provenance.manifest VALUES
 ('proc-building-topic','0.1.0','sha256:2b5aef8fb787639360a27918f62ee56c4fb1c573f0c564ed288e1660fd13e258',
 '["review:building-topic-pair"]','reference','reference-proc-building-topic-v0-1',
 '__MANIFEST_HASH__');
CREATE TABLE evd_provenance.bindings (
 name text NOT NULL, version text NOT NULL, content_hash text NOT NULL, rights_tags jsonb NOT NULL,
 source_kind text NOT NULL CHECK(source_kind='reference'), source_id text NOT NULL, manifest_hash text NOT NULL,
 actor text NOT NULL, reason text NOT NULL, db_actor text NOT NULL, bound_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(name,version), FOREIGN KEY(name,version) REFERENCES evd_provenance.manifest);
CREATE TRIGGER evd_manifest_immutable BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON evd_provenance.manifest
 FOR EACH STATEMENT EXECUTE FUNCTION public.evd_immutable();
CREATE TRIGGER evd_bindings_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON evd_provenance.bindings
 FOR EACH STATEMENT EXECUTE FUNCTION public.evd_immutable();
CREATE FUNCTION evd_provenance.bind_reference(n text,v text,a text,r text) RETURNS void
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE m evd_provenance.manifest%ROWTYPE; ref public.evd_reference_data%ROWTYPE; b evd_provenance.bindings%ROWTYPE;
BEGIN
 IF a IS NULL OR a !~ '^[a-zA-Z0-9_-]{1,96}$' OR r IS NULL OR length(r) NOT BETWEEN 1 AND 256 OR r ~ '[[:cntrl:]]' THEN
  RAISE EXCEPTION 'invalid audit metadata'; END IF;
 SELECT * INTO m FROM evd_provenance.manifest WHERE name=n AND version=v;
 IF NOT FOUND THEN RAISE EXCEPTION 'reference not reviewed'; END IF;
 PERFORM pg_advisory_xact_lock(hashtext('reference-binding:'||n||':'||v));
 SELECT * INTO ref FROM public.evd_reference_data WHERE name=n AND version=v;
 IF NOT FOUND OR ref.content_hash<>m.content_hash OR ref.rights_tags<>m.rights_tags THEN
  RAISE EXCEPTION 'reference identity mismatch'; END IF;
 SELECT * INTO b FROM evd_provenance.bindings WHERE name=n AND version=v;
 IF FOUND THEN
  IF b.content_hash<>m.content_hash OR b.rights_tags<>m.rights_tags OR b.source_id<>m.source_id OR b.manifest_hash<>m.manifest_hash THEN
   RAISE EXCEPTION 'binding conflict'; END IF;
  RETURN;
 END IF;
 INSERT INTO evd_provenance.bindings SELECT m.*,a,r,session_user,clock_timestamp();
END $$;
REVOKE ALL ON ALL TABLES IN SCHEMA evd_provenance FROM PUBLIC;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA evd_provenance FROM PUBLIC;
-- Global owner defaults can grant explicit DML even in a new schema. Remove it.
DO $$ DECLARE g record; BEGIN
 FOR g IN SELECT DISTINCT grantee FROM information_schema.table_privileges
   WHERE table_schema='evd_provenance' AND grantee NOT IN (current_user,'PUBLIC') LOOP
  EXECUTE format('REVOKE ALL ON ALL TABLES IN SCHEMA evd_provenance FROM %I',g.grantee);
 END LOOP;
 -- Function defaults can also create direct EXECUTE grants, independently of PUBLIC.
 FOR g IN SELECT DISTINCT r.rolname AS grantee FROM pg_proc p
   JOIN pg_namespace n ON n.oid=p.pronamespace
   CROSS JOIN LATERAL aclexplode(p.proacl) a JOIN pg_roles r ON r.oid=a.grantee
   WHERE n.nspname='evd_provenance' AND r.rolname<>current_user LOOP
  EXECUTE format('REVOKE ALL ON ALL FUNCTIONS IN SCHEMA evd_provenance FROM %I',g.grantee);
 END LOOP;
 -- Evidence consumers that read reference metadata get provenance SELECT; no binding mutation privilege.
 FOR g IN SELECT DISTINCT grantee FROM information_schema.table_privileges
   WHERE table_schema='public' AND table_name='evd_reference_data' AND privilege_type='SELECT'
     AND grantee NOT IN (current_user,'PUBLIC') LOOP
  EXECUTE format('GRANT USAGE ON SCHEMA evd_provenance TO %I',g.grantee);
  EXECUTE format('GRANT SELECT ON ALL TABLES IN SCHEMA evd_provenance TO %I',g.grantee);
 END LOOP;
END $$;

-- Live observation cursor and the source URLs already bound to an evidence stream.
CREATE TABLE evd_live_observations (
    source_id text NOT NULL,
    notice_key text NOT NULL CHECK (notice_key <> ''),
    stream_key text NOT NULL CHECK (stream_key <> ''),
    version bigint NOT NULL CHECK (version > 0),
    PRIMARY KEY (source_id, notice_key),
    UNIQUE (source_id, stream_key)
);
CREATE TABLE evd_live_urls (
    source_id text NOT NULL,
    source_url text NOT NULL,
    PRIMARY KEY (source_id, source_url)
);

-- Attachment receipts of each immutable document snapshot.
CREATE TABLE evd_snapshot_attachments (
    snapshot_id text PRIMARY KEY REFERENCES evd_snapshots,
    receipts jsonb NOT NULL CHECK (jsonb_typeof(receipts) = 'array')
);
CREATE TRIGGER evd_snapshot_attachments_immutable BEFORE UPDATE OR DELETE ON evd_snapshot_attachments
    FOR EACH ROW EXECUTE FUNCTION evd_immutable();

-- Deferred live observations in source receipt order.
CREATE TABLE evd_pending_live_events (
    event_id text PRIMARY KEY,
    source_id text NOT NULL,
    receipt_seq bigint GENERATED ALWAYS AS IDENTITY,
    envelope jsonb NOT NULL
);
CREATE UNIQUE INDEX evd_pending_live_events_source_order ON evd_pending_live_events (source_id, receipt_seq);

-- The immutable group selection of each accepted grouped observation.
CREATE TABLE evd_grouped_observations (
    source_id text NOT NULL,
    notice_key text NOT NULL,
    observation_version bigint NOT NULL CHECK (observation_version > 0),
    snapshot_id text NOT NULL REFERENCES evd_snapshots (id),
    selector jsonb NOT NULL CHECK (
        (jsonb_typeof(selector) = 'object'
            AND selector - ARRAY['kind', 'version', 'group_key', 'notice_key'] = '{}'::jsonb
            AND jsonb_typeof(selector->'kind') = 'string'
            AND selector->>'kind' ~ '^[a-z][a-z0-9_-]{0,95}$'
            AND jsonb_typeof(selector->'version') = 'number'
            AND selector->>'version' ~ '^[1-9][0-9]*$'
            AND selector->'version' <= '2147483647'::jsonb
            AND jsonb_typeof(selector->'group_key') = 'string'
            AND length(selector->>'group_key') BETWEEN 1 AND 512
            AND jsonb_typeof(selector->'notice_key') = 'string'
            AND selector->>'notice_key' = notice_key) IS TRUE
    ),
    published_on date NOT NULL,
    record_digest text NOT NULL CHECK (record_digest ~ '^sha256:[0-9a-f]{64}$'),
    raw_sha256 text NOT NULL CHECK (raw_sha256 ~ '^sha256:[0-9a-f]{64}$'),
    media_type text NOT NULL CHECK (length(media_type) BETWEEN 1 AND 128),
    size_bytes integer NOT NULL CHECK (size_bytes BETWEEN 1 AND 20971520),
    is_anchor boolean NOT NULL,
    PRIMARY KEY (source_id, notice_key, observation_version),
    FOREIGN KEY (source_id, notice_key) REFERENCES evd_live_observations (source_id, notice_key)
);
CREATE UNIQUE INDEX evd_grouped_observations_anchor
    ON evd_grouped_observations (snapshot_id) WHERE is_anchor;
CREATE TRIGGER evd_grouped_observations_immutable BEFORE UPDATE OR DELETE ON evd_grouped_observations
    FOR EACH ROW EXECUTE FUNCTION evd_immutable();
CREATE TRIGGER evd_grouped_observations_no_truncate BEFORE TRUNCATE ON evd_grouped_observations
    FOR EACH STATEMENT EXECUTE FUNCTION evd_immutable();

-- Each accepted browser pair and its immutable snapshot anchor.
CREATE TABLE evd_browser_observations (
    source_id text NOT NULL,
    notice_key text NOT NULL,
    observation_version bigint NOT NULL CHECK (observation_version > 0),
    snapshot_id text NOT NULL REFERENCES evd_snapshots (id),
    event jsonb NOT NULL CHECK (
        (jsonb_typeof(event) = 'object'
            AND event->'source_id' = to_jsonb(source_id)
            AND event->'notice_key' = to_jsonb(notice_key)
            AND event->'observation_version' = to_jsonb(observation_version)
            AND event->>'binding_digest' ~ '^sha256:[0-9a-f]{64}$') IS TRUE
    ),
    is_anchor boolean NOT NULL,
    PRIMARY KEY (source_id, notice_key, observation_version),
    FOREIGN KEY (source_id, notice_key) REFERENCES evd_live_observations (source_id, notice_key)
);
CREATE UNIQUE INDEX evd_browser_observations_anchor
    ON evd_browser_observations (snapshot_id) WHERE is_anchor;
CREATE INDEX evd_browser_observations_url ON evd_browser_observations (source_id, (event->>'source_url'));
CREATE TRIGGER evd_browser_observations_immutable BEFORE UPDATE OR DELETE ON evd_browser_observations
    FOR EACH ROW EXECUTE FUNCTION evd_immutable();
CREATE TRIGGER evd_browser_observations_no_truncate BEFORE TRUNCATE ON evd_browser_observations
    FOR EACH STATEMENT EXECUTE FUNCTION evd_immutable();
CREATE UNIQUE INDEX evd_pending_browser_version ON evd_pending_live_events
    (source_id, (envelope #>> '{data,notice_key}'), (envelope #>> '{data,observation_version}'))
    WHERE envelope->>'type' = 'acquisition.browser_notice.archived.v1';
""".replace("__MANIFEST_HASH__", manifest_hash))


def downgrade() -> None:
    raise RuntimeError("evidence history and reference provenance cannot be destructively downgraded")
