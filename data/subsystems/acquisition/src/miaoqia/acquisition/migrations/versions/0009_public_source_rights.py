"""Operator-controlled current rights and atomic outbox for registered public sources."""

from alembic import op

revision = "0009"
down_revision = "0008"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
CREATE TABLE acq_rights.public_source_state (
    source_id text PRIMARY KEY REFERENCES public.acq_sources,
    revoked boolean NOT NULL,
    epoch bigint NOT NULL UNIQUE REFERENCES acq_rights.audit
);

ALTER TABLE acq_rights.audit ADD COLUMN event_id text UNIQUE;

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

REVOKE ALL ON TABLE acq_rights.public_source_state FROM PUBLIC;
REVOKE ALL ON FUNCTION acq_rights.public_source_command(jsonb,text,text,text) FROM PUBLIC;
""")


def downgrade():
    raise RuntimeError("public source rights/audit history cannot be destructively downgraded")
