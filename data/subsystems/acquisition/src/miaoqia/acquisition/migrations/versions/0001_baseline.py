"""Acquisition baseline schema (acquisition.md section 4).

Source rights and their audit live in the acq_rights schema; collection state, request receipts, raw artifact metadata
and source observations live in public. Raw bytes live in the object store under raw/<sha256>; the database keeps
metadata only. Rights records, attempts, artifacts, observations and receipts are append-only: corrections are new
rows, never updates.

Revision ID: 0001
"""
from alembic import op

revision = "0001"
down_revision = None
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
DO $$ BEGIN EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC',current_database()); END $$;

-- Immutable original grants, the owner-reviewed allowlist and live revocation. No source activation.
CREATE SCHEMA acq_rights;
REVOKE ALL ON SCHEMA acq_rights FROM PUBLIC;
CREATE TABLE acq_rights.versions (
 entry_id text NOT NULL, version integer NOT NULL CHECK(version > 0), source_id text NOT NULL,
 policy jsonb NOT NULL CHECK(jsonb_typeof(policy)='object'), PRIMARY KEY(entry_id,version));
CREATE TABLE acq_rights.reviewed (
 source_id text NOT NULL, tags jsonb NOT NULL, PRIMARY KEY(source_id,tags));
INSERT INTO acq_rights.reviewed VALUES
 ('official-qzygjy','["corpus:official-building-tender-20261026","public-official-review"]'),
 ('official-qzygjy','["corpus:official-building-award-20261026","public-official-review"]'),
 ('reference-proc-building-topic-v0-1','["review:building-topic-pair"]');
CREATE TABLE acq_rights.aliases (
 source_id text NOT NULL, tags jsonb NOT NULL, entry_id text NOT NULL, version integer NOT NULL,
 PRIMARY KEY(source_id,tags), FOREIGN KEY(source_id,tags) REFERENCES acq_rights.reviewed,
 FOREIGN KEY(entry_id,version) REFERENCES acq_rights.versions);
CREATE TABLE acq_rights.current (
 entry_id text PRIMARY KEY, version integer NOT NULL, revoked boolean NOT NULL,
 FOREIGN KEY(entry_id,version) REFERENCES acq_rights.versions);
CREATE TABLE acq_rights.state (singleton boolean PRIMARY KEY CHECK(singleton), epoch bigint NOT NULL CHECK(epoch >= 0));
INSERT INTO acq_rights.state VALUES(true,0);
CREATE TABLE acq_rights.audit (
 epoch bigint PRIMARY KEY, actor text NOT NULL CHECK(length(actor) BETWEEN 1 AND 96),
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 256), db_actor text NOT NULL,
 at timestamptz NOT NULL DEFAULT clock_timestamp(), command jsonb NOT NULL, old_state jsonb, new_state jsonb,
 event_id text UNIQUE);
CREATE FUNCTION acq_rights.immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'immutable rights record' USING ERRCODE='integrity_constraint_violation'; END $$;
CREATE TRIGGER versions_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON acq_rights.versions FOR EACH STATEMENT EXECUTE FUNCTION acq_rights.immutable();
CREATE TRIGGER aliases_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON acq_rights.aliases FOR EACH STATEMENT EXECUTE FUNCTION acq_rights.immutable();
CREATE TRIGGER reviewed_immutable BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON acq_rights.reviewed FOR EACH STATEMENT EXECUTE FUNCTION acq_rights.immutable();
CREATE TRIGGER audit_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON acq_rights.audit FOR EACH STATEMENT EXECUTE FUNCTION acq_rights.immutable();
CREATE FUNCTION acq_rights.command(cmd jsonb, actor text, reason text) RETURNS bigint
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,acq_rights AS $$
DECLARE e bigint; v jsonb; old_v jsonb; new_v jsonb; p jsonb; src text;
BEGIN
 IF actor IS NULL OR actor !~ '^[a-zA-Z0-9_-]{1,96}$' OR reason IS NULL OR length(reason) NOT BETWEEN 1 AND 256 THEN
  RAISE EXCEPTION 'invalid audit metadata'; END IF;
 PERFORM pg_advisory_xact_lock(682039154);
 SELECT epoch INTO e FROM acq_rights.state WHERE singleton FOR UPDATE;
 IF cmd->>'action' = 'register' THEN
  p := cmd->'policy';
  IF p->>'schema_version' <> '1' OR p->>'entry_id' IS NULL OR p->>'source_id' IS NULL THEN RAISE EXCEPTION 'invalid policy'; END IF;
  SELECT source_id INTO src FROM acq_rights.versions WHERE entry_id=p->>'entry_id' LIMIT 1;
  IF src IS NOT NULL AND src <> p->>'source_id' THEN RAISE EXCEPTION 'source mismatch'; END IF;
  SELECT policy INTO v FROM acq_rights.versions WHERE entry_id=p->>'entry_id' AND version=(p->>'version')::integer;
  IF v IS NOT NULL THEN
   IF v = p THEN RETURN e; END IF; RAISE EXCEPTION 'immutable policy conflict'; END IF;
  INSERT INTO acq_rights.versions VALUES(p->>'entry_id',(p->>'version')::integer,p->>'source_id',p);
  new_v := p;
 ELSIF cmd->>'action' = 'bind' THEN
  SELECT source_id INTO src FROM acq_rights.versions WHERE entry_id=cmd->>'entry_id' AND version=(cmd->>'version')::integer;
  IF src IS NULL OR src <> cmd->>'source_id' THEN RAISE EXCEPTION 'source mismatch'; END IF;
  SELECT to_jsonb(a) INTO v FROM acq_rights.aliases a WHERE source_id=src AND tags=cmd->'tags';
  IF v IS NOT NULL THEN
   IF v->>'entry_id'=cmd->>'entry_id' AND v->>'version'=cmd->>'version' THEN RETURN e; END IF;
   RAISE EXCEPTION 'immutable alias conflict'; END IF;
  INSERT INTO acq_rights.aliases VALUES(src,cmd->'tags',cmd->>'entry_id',(cmd->>'version')::integer);
  new_v := cmd;
 ELSIF cmd->>'action' IN ('activate','revoke') THEN
  SELECT to_jsonb(c) INTO old_v FROM acq_rights.current c WHERE entry_id=cmd->>'entry_id';
  IF cmd->>'action'='activate' THEN
   IF NOT EXISTS(SELECT 1 FROM acq_rights.versions WHERE entry_id=cmd->>'entry_id' AND version=(cmd->>'version')::integer) THEN RAISE EXCEPTION 'unknown version'; END IF;
   new_v := jsonb_build_object('entry_id',cmd->>'entry_id','version',(cmd->>'version')::integer,'revoked',false);
  ELSE
   IF old_v IS NULL THEN RAISE EXCEPTION 'unknown entry'; END IF;
   new_v := old_v || jsonb_build_object('revoked',true);
  END IF;
  IF new_v=old_v THEN RETURN e; END IF;
  INSERT INTO acq_rights.current VALUES(new_v->>'entry_id',(new_v->>'version')::integer,(new_v->>'revoked')::boolean)
   ON CONFLICT(entry_id) DO UPDATE SET version=EXCLUDED.version,revoked=EXCLUDED.revoked;
 ELSE RAISE EXCEPTION 'invalid command'; END IF;
 e := e+1;
 UPDATE acq_rights.state SET epoch=e WHERE singleton;
 INSERT INTO acq_rights.audit(epoch,actor,reason,db_actor,command,old_state,new_state)
  VALUES(e,actor,reason,session_user,cmd,old_v,new_v);
 RETURN e;
END $$;

-- Source registry, fetch attempts, request fingerprints and raw artifact metadata.
CREATE TABLE acq_sources (
    source_id    text PRIMARY KEY CHECK (source_id ~ '^[a-z][a-z0-9_-]{0,95}$'),
    handler      text NOT NULL,
    access       jsonb NOT NULL CHECK (jsonb_typeof(access) = 'object'),
    state        text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'blocked')),
    state_reason text,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    -- Collection window lease; the foreign key to acq_collection_runs is added below.
    active_run_id    bigint,
    lease_generation integer CHECK (lease_generation > 0),
    lease_until      timestamptz,
    collection_fingerprint text CHECK (collection_fingerprint ~ '^[0-9a-f]{64}$'),
    CHECK ((state = 'blocked') = (state_reason IS NOT NULL)),
    CHECK ((active_run_id IS NULL) = (lease_generation IS NULL)),
    CHECK ((active_run_id IS NULL) = (lease_until IS NULL))
);

CREATE TABLE acq_collection_runs (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id      text NOT NULL REFERENCES acq_sources,
    window_start   date NOT NULL,
    window_end     date NOT NULL CHECK (window_end >= window_start),
    max_pages      integer NOT NULL CHECK (max_pages >= 1),
    refetch        boolean NOT NULL,
    started_at     timestamptz NOT NULL,
    finished_at    timestamptz,
    report         jsonb CHECK (jsonb_typeof(report) = 'object'),
    mode           text NOT NULL DEFAULT 'manual' CHECK (mode IN ('manual', 'incremental', 'backfill')),
    generation     integer NOT NULL DEFAULT 0 CHECK (generation >= 0),
    pending_report jsonb,
    CONSTRAINT acq_collection_runs_check1 CHECK ((finished_at IS NULL) = (report IS NULL)),
    CONSTRAINT acq_collection_runs_check2
        CHECK (pending_report IS NULL OR (jsonb_typeof(pending_report) = 'object' AND finished_at IS NULL))
);
CREATE INDEX acq_collection_runs_source ON acq_collection_runs (source_id, id DESC);
CREATE INDEX acq_collection_runs_recovery ON acq_collection_runs (source_id, mode, id)
    WHERE finished_at IS NULL AND generation > 0;
CREATE UNIQUE INDEX acq_collection_runs_plan_target ON acq_collection_runs (id, source_id, mode);
ALTER TABLE acq_sources ADD FOREIGN KEY (active_run_id) REFERENCES acq_collection_runs;

CREATE TABLE acq_fetch_attempts (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id    text NOT NULL REFERENCES acq_sources,
    fingerprint  text NOT NULL CHECK (fingerprint ~ '^sha256:[0-9a-f]{64}$'),
    method       text NOT NULL CHECK (method IN ('GET', 'POST')),
    url          text NOT NULL,
    status       integer,
    outcome      text NOT NULL CHECK (outcome IN ('ok', 'throttled', 'blocked', 'http_error', 'network_error', 'invalid')),
    duration_ms  integer NOT NULL CHECK (duration_ms >= 0),
    error        text,
    attempted_at timestamptz NOT NULL DEFAULT now(),
    run_id       bigint REFERENCES acq_collection_runs,
    -- Foreign keys to acq_download_starts and acq_observation_attachments are added below.
    download_id  bigint UNIQUE,
    attachment_notice_key text,
    attachment_version    bigint,
    attachment_part_key   text,
    CHECK ((attachment_notice_key IS NULL) = (attachment_version IS NULL)
           AND (attachment_notice_key IS NULL) = (attachment_part_key IS NULL))
);
CREATE INDEX acq_fetch_attempts_source ON acq_fetch_attempts (source_id, attempted_at);
CREATE INDEX acq_fetch_attempts_run ON acq_fetch_attempts (run_id, id) WHERE run_id IS NOT NULL;
CREATE INDEX acq_fetch_attempts_attachment ON acq_fetch_attempts
    (source_id, attachment_notice_key, attachment_version, attachment_part_key, id)
    WHERE attachment_notice_key IS NOT NULL;

-- Detail requests already archived; incremental runs skip them unless a refetch is requested.
CREATE TABLE acq_requests (
    fingerprint     text PRIMARY KEY CHECK (fingerprint ~ '^sha256:[0-9a-f]{64}$'),
    source_id       text NOT NULL REFERENCES acq_sources,
    source_key      text NOT NULL CHECK (source_key <> ''),
    content_hash    text NOT NULL CHECK (content_hash ~ '^sha256:[0-9a-f]{64}$'),
    last_fetched_at timestamptz NOT NULL
);

CREATE TABLE acq_raw_artifacts (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id           text NOT NULL REFERENCES acq_sources,
    source_key          text NOT NULL CHECK (source_key <> ''),
    content_hash        text NOT NULL CHECK (content_hash ~ '^sha256:[0-9a-f]{64}$'),
    artifact_ref        text NOT NULL CHECK (artifact_ref ~ '^raw/[0-9a-f]{64}$'),
    media_type          text NOT NULL,
    size_bytes          integer NOT NULL CHECK (size_bytes >= 0),
    source_url          text NOT NULL,
    request_fingerprint text NOT NULL,
    fetched_at          timestamptz NOT NULL,
    UNIQUE (source_id, source_key, content_hash),
    UNIQUE (source_id, id)
);

CREATE FUNCTION acq_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'append-only acquisition record' USING ERRCODE = 'integrity_constraint_violation'; END $$;
CREATE TRIGGER acq_fetch_attempts_append_only BEFORE UPDATE OR DELETE OR TRUNCATE ON acq_fetch_attempts
    FOR EACH STATEMENT EXECUTE FUNCTION acq_append_only();
CREATE TRIGGER acq_raw_artifacts_append_only BEFORE UPDATE OR DELETE OR TRUNCATE ON acq_raw_artifacts
    FOR EACH STATEMENT EXECUTE FUNCTION acq_append_only();

-- Current source health per source: the state from its latest collection run.
CREATE TABLE acq_source_health (
    source_id       text PRIMARY KEY REFERENCES acq_sources,
    state           text NOT NULL CHECK (state IN ('healthy', 'degraded', 'unavailable')),
    reason_code     text NOT NULL CHECK (reason_code <> ''),
    checked_at      timestamptz NOT NULL,
    last_success_at timestamptz,
    last_run        jsonb NOT NULL CHECK (jsonb_typeof(last_run) = 'object')
);

-- Operator-controlled current rights for registered public sources; changes go through the atomic outbox.
CREATE TABLE acq_rights.public_source_state (
    source_id text PRIMARY KEY REFERENCES public.acq_sources,
    revoked boolean NOT NULL,
    epoch bigint NOT NULL UNIQUE REFERENCES acq_rights.audit
);

CREATE FUNCTION acq_rights.public_source_command(cmd jsonb, actor text, reason text, event_identifier text)
RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE
    action text := cmd->>'action';
    source text := cmd->>'source_id';
    expected_epoch bigint;
    should_revoke boolean;
    e bigint;
    event_at timestamptz;
    event_time text;
    read_rights CONSTANT jsonb := '["fact_read", "evidence_resolve"]';
    previous acq_rights.public_source_state%ROWTYPE;
BEGIN
    IF actor IS NULL OR actor !~ '^[a-zA-Z0-9_-]{1,96}$' OR reason IS NULL
       OR length(reason) NOT BETWEEN 1 AND 256 OR reason ~ '[[:cntrl:]]' THEN
        RAISE EXCEPTION 'invalid audit metadata';
    END IF;
    IF event_identifier IS NULL OR event_identifier !~ '^evt_[0-9A-HJKMNP-TV-Z]{26}$' THEN
        RAISE EXCEPTION 'invalid rights event identifier';
    END IF;
    IF jsonb_typeof(cmd) IS DISTINCT FROM 'object'
       OR action IS NULL OR action NOT IN ('revoke_public', 'restore_public')
       OR jsonb_typeof(cmd->'source_id') IS DISTINCT FROM 'string'
       OR source !~ '^[a-z][a-z0-9_-]{0,95}$'
       OR jsonb_typeof(cmd->'expected_epoch') IS DISTINCT FROM 'number'
       OR cmd->>'expected_epoch' !~ '^(0|[1-9][0-9]{0,18})$' THEN
        RAISE EXCEPTION 'invalid public source command';
    END IF;
    expected_epoch := (cmd->>'expected_epoch')::bigint;
    should_revoke := action = 'revoke_public';
    IF should_revoke THEN
        IF cmd - ARRAY['action', 'source_id', 'expected_epoch'] <> '{}'::jsonb THEN
            RAISE EXCEPTION 'invalid public source revocation';
        END IF;
    ELSE
        IF cmd - ARRAY['action', 'source_id', 'expected_epoch', 'evidence_hash'] <> '{}'::jsonb
           OR cmd->>'evidence_hash' IS NULL OR cmd->>'evidence_hash' !~ '^sha256:[0-9a-f]{64}$' THEN
            RAISE EXCEPTION 'invalid public source restoration evidence';
        END IF;
    END IF;

    PERFORM pg_advisory_xact_lock(682039154);
    SELECT epoch INTO STRICT e FROM acq_rights.state WHERE singleton FOR UPDATE;
    PERFORM 1 FROM public.acq_sources WHERE source_id = source FOR KEY SHARE;
    IF NOT FOUND THEN RAISE EXCEPTION 'unknown public source'; END IF;
    SELECT * INTO previous FROM acq_rights.public_source_state WHERE source_id = source;
    IF COALESCE(previous.epoch, 0) <> expected_epoch THEN
        IF EXISTS (SELECT 1 FROM acq_rights.audit a WHERE a.epoch = previous.epoch AND a.command = cmd) THEN
            RETURN e;
        END IF;
        RAISE EXCEPTION 'stale public source rights epoch';
    END IF;
    IF COALESCE(previous.revoked, false) = should_revoke THEN
        RAISE EXCEPTION 'invalid public source rights transition';
    END IF;

    e := e + 1;
    event_at := clock_timestamp();
    event_time := to_char(event_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"');
    UPDATE acq_rights.state SET epoch=e WHERE singleton;
    INSERT INTO acq_rights.audit(epoch, actor, reason, db_actor, at, command, old_state, new_state, event_id)
      VALUES(e, actor, reason, session_user, event_at, cmd,
             CASE WHEN previous.epoch IS NOT NULL THEN to_jsonb(previous) END,
             jsonb_build_object('source_id', source, 'revoked', should_revoke, 'epoch', e),
             event_identifier);
    INSERT INTO acq_rights.public_source_state(source_id, revoked, epoch) VALUES(source, should_revoke, e)
      ON CONFLICT(source_id) DO UPDATE SET revoked=EXCLUDED.revoked, epoch=EXCLUDED.epoch;
    INSERT INTO public.kernel_outbox(event_id, topic, partition_key, envelope)
      VALUES(event_identifier, 'public.acquisition', source, jsonb_build_object(
        'specversion', '1.0',
        'id', event_identifier,
        'source', 'miaoqia/acquisition',
        'type', 'acquisition.source.rights_changed.v1',
        'time', event_time,
        'subject', source,
        'datacontenttype', 'application/json',
        'miaoqiazone', 'public',
        'data', jsonb_build_object(
          'source_id', source,
          'rights_version', e,
          'before', CASE WHEN should_revoke THEN read_rights ELSE '[]'::jsonb END,
          'after', CASE WHEN should_revoke THEN '[]'::jsonb ELSE read_rights END,
          'effective_at', event_time,
          'action', CASE WHEN should_revoke THEN 'withdraw' ELSE 'retain' END
        )
      ));
    PERFORM pg_notify('kernel_outbox', '');
    RETURN e;
END $$;

REVOKE ALL ON ALL TABLES IN SCHEMA acq_rights FROM PUBLIC;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA acq_rights FROM PUBLIC;

CREATE FUNCTION public.acq_collection_allowed(target text) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    IF NOT has_table_privilege(session_user, 'public.acq_sources', 'UPDATE') THEN
        RAISE insufficient_privilege;
    END IF;
    PERFORM pg_advisory_xact_lock_shared(682039154);
    RETURN EXISTS (SELECT 1 FROM public.acq_sources WHERE source_id = target) AND NOT EXISTS (
        SELECT 1 FROM acq_rights.public_source_state WHERE source_id = target AND revoked
    );
END
$$;

-- Page discovery receipts outlive the response bytes.
CREATE TABLE acq_discovery_pages (
    attempt_id bigint PRIMARY KEY REFERENCES acq_fetch_attempts,
    run_id bigint NOT NULL REFERENCES acq_collection_runs,
    channel text NOT NULL,
    window_start date NOT NULL,
    window_end date NOT NULL CHECK (window_end >= window_start),
    page integer NOT NULL CHECK (page > 0),
    response_hash text NOT NULL CHECK (response_hash ~ '^sha256:[0-9a-f]{64}$'),
    refs jsonb NOT NULL CHECK (jsonb_typeof(refs) = 'array'),
    has_next boolean NOT NULL,
    object_ref text CHECK (object_ref LIKE 'discovery/%'),
    expires_at timestamptz,
    expired_at timestamptz,
    -- A rendered listing receipt binds one navigation and network response.
    browser_binding jsonb CHECK (browser_binding IS NULL OR jsonb_typeof(browser_binding) = 'object'),
    CONSTRAINT acq_discovery_pages_check1 CHECK ((object_ref IS NULL) = (expires_at IS NULL))
);
CREATE INDEX acq_discovery_pages_run ON acq_discovery_pages (run_id, channel, window_start, page);
CREATE INDEX acq_discovery_pages_expiry ON acq_discovery_pages (expires_at, attempt_id)
    WHERE expires_at IS NOT NULL AND expired_at IS NULL;

-- Initial backfill scan cursor and the unfinished days retained independently of it.
CREATE TABLE acq_initial_backfill (
    source_id text PRIMARY KEY REFERENCES acq_sources,
    oldest_day date NOT NULL,
    next_day date,
    CHECK (next_day >= oldest_day)
);
CREATE TABLE acq_initial_backfill_gaps (
    source_id text NOT NULL REFERENCES acq_initial_backfill(source_id),
    day date NOT NULL,
    last_run_id bigint NOT NULL REFERENCES acq_collection_runs(id),
    attempted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (source_id, day)
);

-- Request intentions; every dispatched attempt is bound to its logical task.
CREATE TABLE acq_request_tasks (
    task_key text PRIMARY KEY CHECK (task_key ~ '^[0-9a-f]{64}$'),
    run_id bigint NOT NULL REFERENCES acq_collection_runs,
    kind text NOT NULL CHECK (kind IN ('list', 'body', 'attachment', 'group', 'browser')),
    scope jsonb NOT NULL CHECK (jsonb_typeof(scope) = 'array'),
    fingerprint text NOT NULL CHECK (fingerprint ~ '^sha256:[0-9a-f]{64}$'),
    state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'staged', 'settled', 'failed')),
    last_attempt bigint REFERENCES acq_fetch_attempts,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX acq_request_tasks_run_state ON acq_request_tasks (run_id, state);

-- Durable window leases and shared download pacing.
CREATE TABLE acq_download_pacing (
    scope text PRIMARY KEY,
    sent_at timestamptz NOT NULL,
    ready_at timestamptz NOT NULL CHECK (ready_at >= sent_at)
);
CREATE TABLE acq_download_starts (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id bigint NOT NULL REFERENCES acq_collection_runs,
    generation integer NOT NULL CHECK (generation > 0),
    fingerprint text NOT NULL CHECK (fingerprint ~ '^sha256:[0-9a-f]{64}$'),
    site text NOT NULL,
    dispatched_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    task_key text REFERENCES acq_request_tasks
);
CREATE INDEX acq_download_starts_run ON acq_download_starts (run_id, generation, id);
ALTER TABLE acq_fetch_attempts ADD FOREIGN KEY (download_id) REFERENCES acq_download_starts;

-- Collection schedule cursors and window plans pinned to the planner's active source contract.
CREATE TABLE acq_collection_schedule (
    source_id text PRIMARY KEY REFERENCES acq_sources(source_id),
    next_due_at timestamptz NOT NULL
);
CREATE TABLE acq_collection_plans (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id text NOT NULL REFERENCES acq_sources(source_id),
    mode text NOT NULL CHECK (mode IN ('incremental', 'backfill')),
    occurrence_key text NOT NULL CHECK (btrim(occurrence_key) <> ''),
    due_at timestamptz NOT NULL,
    not_before timestamptz NOT NULL CHECK (not_before >= due_at),
    window_start date NOT NULL,
    window_end date NOT NULL CHECK (window_end >= window_start),
    max_pages integer NOT NULL CHECK (max_pages > 0),
    refetch boolean NOT NULL,
    handler text NOT NULL CHECK (btrim(handler) <> ''),
    config_fingerprint text NOT NULL CHECK (config_fingerprint ~ '^[0-9a-f]{64}$'),
    budget_seconds integer NOT NULL CHECK (budget_seconds BETWEEN 1 AND 86400),
    run_id bigint UNIQUE,
    settled_at timestamptz,
    settlement_reason text,
    FOREIGN KEY (run_id, source_id, mode) REFERENCES acq_collection_runs (id, source_id, mode),
    CONSTRAINT acq_collection_plans_settlement CHECK (
        (settled_at IS NULL OR run_id IS NOT NULL OR settlement_reason IS NOT NULL)
        AND (settlement_reason IS NULL OR (settlement_reason = 'source_contract_changed'
                                            AND settled_at IS NOT NULL))
    )
);
CREATE INDEX acq_collection_plans_ready
    ON acq_collection_plans (mode, not_before, due_at, id) WHERE settled_at IS NULL;
CREATE INDEX acq_collection_plans_open_backfill_day
    ON acq_collection_plans (source_id, window_start) WHERE mode = 'backfill' AND settled_at IS NULL;
CREATE INDEX acq_collection_plans_incremental_day
    ON acq_collection_plans (source_id, window_end, config_fingerprint, due_at)
    WHERE mode = 'incremental';
CREATE UNIQUE INDEX acq_collection_plans_occurrence ON acq_collection_plans
    (source_id, mode, occurrence_key) WHERE settlement_reason IS NULL;

-- Grouped list responses: one validated original and request receipt shared by its member observations.
CREATE TABLE acq_group_receipts (
    attempt_id bigint PRIMARY KEY REFERENCES acq_fetch_attempts,
    task_key text NOT NULL UNIQUE REFERENCES acq_request_tasks,
    list_attempt_id bigint NOT NULL REFERENCES acq_discovery_pages,
    run_id bigint NOT NULL REFERENCES acq_collection_runs,
    source_id text NOT NULL,
    group_key text NOT NULL CHECK (length(group_key) BETWEEN 1 AND 512),
    artifact_id bigint NOT NULL,
    channel text NOT NULL CHECK (channel <> ''),
    window_start date NOT NULL,
    window_end date NOT NULL CHECK (window_end >= window_start),
    received_at timestamptz NOT NULL,
    media_type text NOT NULL CHECK (length(media_type) BETWEEN 1 AND 128),
    size_bytes integer NOT NULL CHECK (size_bytes BETWEEN 1 AND 20971520),
    members jsonb NOT NULL CHECK (jsonb_typeof(members) = 'array' AND jsonb_array_length(members) <= 256),
    CHECK (attempt_id <> list_attempt_id),
    UNIQUE (attempt_id, source_id, artifact_id, group_key),
    FOREIGN KEY (source_id, artifact_id) REFERENCES acq_raw_artifacts (source_id, id)
);
CREATE TRIGGER acq_group_receipts_append_only BEFORE UPDATE OR DELETE OR TRUNCATE ON acq_group_receipts
    FOR EACH STATEMENT EXECUTE FUNCTION acq_append_only();

-- Rendered browser receipts keep the original and projection roles distinct.
CREATE TABLE acq_browser_receipts (
    attempt_id bigint PRIMARY KEY REFERENCES acq_fetch_attempts,
    task_key text NOT NULL UNIQUE REFERENCES acq_request_tasks,
    list_attempt_id bigint NOT NULL REFERENCES acq_discovery_pages,
    run_id bigint NOT NULL REFERENCES acq_collection_runs,
    source_id text NOT NULL,
    notice_key text NOT NULL CHECK (length(notice_key) BETWEEN 1 AND 512),
    artifact_id bigint NOT NULL,
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    CHECK (attempt_id <> list_attempt_id),
    UNIQUE (attempt_id, source_id, notice_key, artifact_id),
    FOREIGN KEY (source_id, artifact_id) REFERENCES acq_raw_artifacts (source_id, id)
);
CREATE TRIGGER acq_browser_receipts_append_only
    BEFORE UPDATE OR DELETE OR TRUNCATE ON acq_browser_receipts
    FOR EACH STATEMENT EXECUTE FUNCTION acq_append_only();

-- Notice changes are recorded independently of content-addressed artifacts.
CREATE TABLE acq_source_observations (
    source_id       text NOT NULL REFERENCES acq_sources,
    notice_key      text NOT NULL CHECK (notice_key <> ''),
    version         bigint NOT NULL CHECK (version > 0),
    artifact_id     bigint NOT NULL REFERENCES acq_raw_artifacts,
    attempt_id      bigint NOT NULL REFERENCES acq_fetch_attempts,
    source_url      text NOT NULL,
    published_on    date,
    observed_at     timestamptz NOT NULL,
    channel         text,
    selector        jsonb,
    record_digest   text,
    group_key       text GENERATED ALWAYS AS (selector->>'group_key') STORED,
    browser_attempt_id bigint UNIQUE,
    PRIMARY KEY (source_id, notice_key, version),
    CONSTRAINT acq_source_observations_selector CHECK (
        (selector IS NULL AND record_digest IS NULL) OR (
            selector IS NOT NULL AND record_digest IS NOT NULL AND published_on IS NOT NULL
            AND record_digest ~ '^sha256:[0-9a-f]{64}$'
            AND (jsonb_typeof(selector) = 'object'
                 AND selector - ARRAY['kind', 'version', 'group_key', 'notice_key'] = '{}'::jsonb
                 AND jsonb_typeof(selector->'kind') = 'string'
                 AND selector->>'kind' ~ '^[a-z][a-z0-9_-]{0,95}$'
                 AND jsonb_typeof(selector->'version') = 'number'
                 AND selector->>'version' ~ '^[1-9][0-9]*$'
                 AND selector->'version' <= '2147483647'::jsonb
                 AND jsonb_typeof(selector->'group_key') = 'string'
                 AND jsonb_typeof(selector->'notice_key') = 'string'
                 AND selector->>'notice_key' = notice_key) IS TRUE
        )
    ),
    FOREIGN KEY (attempt_id, source_id, artifact_id, group_key)
        REFERENCES acq_group_receipts (attempt_id, source_id, artifact_id, group_key),
    CONSTRAINT acq_source_observations_browser CHECK (
        browser_attempt_id IS NULL OR (
            browser_attempt_id = attempt_id AND selector IS NULL AND published_on IS NOT NULL
        )
    ),
    FOREIGN KEY (browser_attempt_id, source_id, notice_key, artifact_id)
        REFERENCES acq_browser_receipts (attempt_id, source_id, notice_key, artifact_id)
);
CREATE TRIGGER acq_source_observations_append_only BEFORE UPDATE OR DELETE OR TRUNCATE ON acq_source_observations
    FOR EACH STATEMENT EXECUTE FUNCTION acq_append_only();
CREATE INDEX acq_source_observations_artifact ON acq_source_observations (artifact_id);
CREATE INDEX acq_source_observations_attempt ON acq_source_observations (attempt_id);
CREATE UNIQUE INDEX acq_source_observations_group_attempt_notice
    ON acq_source_observations (attempt_id, notice_key) WHERE selector IS NOT NULL;

CREATE TABLE acq_detail_checks (
    source_id       text NOT NULL,
    notice_key      text NOT NULL,
    version         bigint NOT NULL,
    attempt_id      bigint NOT NULL REFERENCES acq_fetch_attempts,
    legacy_artifact_id bigint REFERENCES acq_raw_artifacts,
    checked_at      timestamptz NOT NULL,
    PRIMARY KEY (source_id, notice_key),
    FOREIGN KEY (source_id, notice_key, version) REFERENCES acq_source_observations
);

CREATE TABLE acq_pending_identity (
    attempt_id bigint PRIMARY KEY REFERENCES acq_fetch_attempts,
    artifact_id bigint NOT NULL REFERENCES acq_raw_artifacts,
    source_url text NOT NULL,
    observed_at timestamptz NOT NULL
);
CREATE TRIGGER acq_pending_identity_append_only BEFORE UPDATE OR DELETE OR TRUNCATE ON acq_pending_identity
    FOR EACH STATEMENT EXECUTE FUNCTION acq_append_only();

-- Each attachment receipt is pinned to a body observation; retry attempts reference it.
CREATE TABLE acq_observation_attachments (
    source_id text NOT NULL,
    notice_key text NOT NULL,
    version bigint NOT NULL,
    part_key text NOT NULL CHECK (length(part_key) BETWEEN 1 AND 512),
    required boolean NOT NULL,
    name text CHECK (length(name) BETWEEN 1 AND 512),
    request_fingerprint text CHECK (request_fingerprint ~ '^sha256:[0-9a-f]{64}$'),
    status text NOT NULL CHECK (status IN ('pending', 'available')),
    artifact_id bigint,
    attempt_id bigint REFERENCES acq_fetch_attempts,
    PRIMARY KEY (source_id, notice_key, version, part_key),
    FOREIGN KEY (source_id, notice_key, version) REFERENCES acq_source_observations,
    FOREIGN KEY (source_id, artifact_id) REFERENCES acq_raw_artifacts (source_id, id),
    CHECK ((status = 'available') = (artifact_id IS NOT NULL)),
    CHECK ((artifact_id IS NULL) = (attempt_id IS NULL))
);
CREATE TRIGGER acq_observation_attachments_append_only BEFORE UPDATE OR DELETE OR TRUNCATE
    ON acq_observation_attachments FOR EACH STATEMENT EXECUTE FUNCTION acq_append_only();
CREATE INDEX acq_observation_attachments_pending ON acq_observation_attachments (source_id, notice_key, version)
    WHERE status = 'pending';
ALTER TABLE acq_fetch_attempts
    ADD FOREIGN KEY (source_id, attachment_notice_key, attachment_version, attachment_part_key)
        REFERENCES acq_observation_attachments;
""")


def downgrade():
    raise RuntimeError("source rights, audit and acquisition history cannot be destructively downgraded")
