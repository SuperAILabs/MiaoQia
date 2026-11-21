-- Run once after all migrations, from postgres as the public cluster
-- administrator with ON_ERROR_STOP. Issue credentials only after success.
\set ON_ERROR_STOP on
BEGIN;
CREATE ROLE mq_acq_coverage_ro NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
CREATE ROLE mq_data_api_dom_ro NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
CREATE ROLE mq_data_api_evd_ro NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
CREATE ROLE mq_data_api_ent_ro NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
CREATE ROLE mq_evidence_assess_ro NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
CREATE ROLE mq_search_export_acq_ro NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
CREATE ROLE mq_search_export_dom NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
CREATE ROLE mq_search_export_evd_ro NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
CREATE ROLE mq_search_export_ent NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
GRANT CONNECT ON DATABASE acq TO mq_search_export_acq_ro, mq_acq_coverage_ro;
GRANT CONNECT ON DATABASE dom TO mq_data_api_dom_ro, mq_search_export_dom;
GRANT CONNECT ON DATABASE evd TO mq_data_api_evd_ro, mq_search_export_evd_ro, mq_evidence_assess_ro;
GRANT CONNECT ON DATABASE ent TO mq_data_api_ent_ro, mq_search_export_ent;
COMMIT;

\connect acq
BEGIN;
GRANT USAGE ON SCHEMA public, acq_rights TO mq_search_export_acq_ro;
GRANT SELECT ON public.acq_sources, acq_rights.public_source_state TO mq_search_export_acq_ro;
GRANT SELECT (singleton, epoch) ON acq_rights.state TO mq_search_export_acq_ro;
GRANT USAGE ON SCHEMA public TO mq_acq_coverage_ro;
GRANT SELECT (source_id, state, state_reason) ON public.acq_sources TO mq_acq_coverage_ro;
GRANT SELECT (source_id, state, reason_code, checked_at, last_success_at)
    ON public.acq_source_health TO mq_acq_coverage_ro;
GRANT SELECT (source_id, oldest_day, next_day) ON public.acq_initial_backfill TO mq_acq_coverage_ro;
GRANT SELECT (source_id, next_due_at) ON public.acq_collection_schedule TO mq_acq_coverage_ro;
GRANT SELECT (source_id, mode, settled_at) ON public.acq_collection_plans TO mq_acq_coverage_ro;
GRANT SELECT (source_id, day, last_run_id) ON public.acq_initial_backfill_gaps TO mq_acq_coverage_ro;
GRANT SELECT (id, run_id, source_id, download_id, status, outcome)
    ON public.acq_fetch_attempts TO mq_acq_coverage_ro;
GRANT SELECT (id, run_id, task_key) ON public.acq_download_starts TO mq_acq_coverage_ro;
GRANT SELECT (source_id, notice_key, version, attempt_id) ON public.acq_detail_checks TO mq_acq_coverage_ro;
GRANT SELECT (source_id, notice_key, version, published_on, channel, artifact_id, selector)
    ON public.acq_source_observations TO mq_acq_coverage_ro;
GRANT SELECT (source_id, id) ON public.acq_raw_artifacts TO mq_acq_coverage_ro;
GRANT SELECT (source_id, notice_key, version, required, status)
    ON public.acq_observation_attachments TO mq_acq_coverage_ro;

CREATE SCHEMA acq_coverage;
REVOKE ALL ON SCHEMA acq_coverage FROM PUBLIC;
GRANT USAGE ON SCHEMA acq_coverage TO mq_acq_coverage_ro;

CREATE FUNCTION acq_coverage.fields(payload jsonb, keys text[]) RETURNS jsonb
LANGUAGE sql IMMUTABLE STRICT SET search_path = pg_catalog AS $$
    SELECT CASE WHEN jsonb_typeof(payload) = 'object' THEN (
        SELECT coalesce(jsonb_object_agg(key, value), '{}'::jsonb)
        FROM jsonb_each(payload) WHERE key = ANY(keys)
    ) ELSE 'null'::jsonb END
$$;

CREATE FUNCTION acq_coverage.array_fields(payload jsonb, keys text[]) RETURNS jsonb
LANGUAGE sql IMMUTABLE STRICT SET search_path = pg_catalog AS $$
    SELECT CASE WHEN jsonb_typeof(payload) = 'array' THEN (
        SELECT coalesce(jsonb_agg(acq_coverage.fields(value, keys) ORDER BY ordinal), '[]'::jsonb)
        FROM jsonb_array_elements(payload) WITH ORDINALITY AS entry(value, ordinal)
    ) ELSE 'null'::jsonb END
$$;

CREATE FUNCTION acq_coverage.run_summary(summary jsonb) RETURNS jsonb
LANGUAGE sql IMMUTABLE STRICT SET search_path = pg_catalog AS $$
    SELECT CASE WHEN jsonb_typeof(summary) = 'object' THEN acq_coverage.fields(summary,
        ARRAY['pages', 'listed', 'archived', 'pending_identity', 'unchanged', 'skipped', 'failed',
              'truncated', 'blocked', 'deferred', 'wait_for_source_change']) || jsonb_build_object(
        'gaps', acq_coverage.array_fields(summary->'gaps',
            ARRAY['channel', 'since', 'until', 'page', 'reason', 'notice_key', 'part_key']),
        'failures', acq_coverage.array_fields(summary->'failures',
            ARRAY['channel', 'notice_key', 'part_key', 'reason', 'error_code', 'retryable']),
        'historical_scan', CASE WHEN jsonb_typeof(summary->'historical_scan'->'channels') = 'object' THEN
            jsonb_build_object('generation', summary->'historical_scan'->'generation', 'channels', (
                SELECT coalesce(jsonb_object_agg(key, acq_coverage.fields(value,
                    ARRAY['pages', 'end', 'manifest_unverified'])), '{}'::jsonb)
                FROM jsonb_each(summary->'historical_scan'->'channels')
            ))
        END
    ) ELSE 'null'::jsonb END
$$;
REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA acq_coverage FROM PUBLIC;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA acq_coverage TO mq_acq_coverage_ro;

-- Preserve the report query's relation and column shapes in the scoped search_path.
CREATE VIEW acq_coverage.acq_collection_runs AS
    SELECT id, source_id, mode, window_start, window_end, started_at, finished_at, generation, max_pages,
           acq_coverage.run_summary(report) AS report, acq_coverage.run_summary(pending_report) AS pending_report
    FROM public.acq_collection_runs;
CREATE VIEW acq_coverage.acq_request_tasks AS
    SELECT run_id, state, task_key, kind, fingerprint, last_attempt,
           CASE
               WHEN kind = 'list' THEN jsonb_build_array(scope->0, scope->1, scope->2, scope->3)
               WHEN kind = 'group' THEN jsonb_build_array(scope->0, scope->1, scope->2, scope->3, scope->4)
           END AS scope
    FROM public.acq_request_tasks;
CREATE VIEW acq_coverage.acq_discovery_pages AS
    SELECT attempt_id, run_id, channel, window_start, window_end, page, has_next,
           acq_coverage.array_fields(refs, ARRAY['notice_key', 'published_on', 'group_key', 'request_fingerprint']) AS refs
    FROM public.acq_discovery_pages;
CREATE VIEW acq_coverage.acq_group_receipts AS
    SELECT run_id, channel, window_start, window_end, group_key, task_key, attempt_id, list_attempt_id, source_id,
           acq_coverage.array_fields(members, ARRAY['published_on', 'outcome']) AS members
    FROM public.acq_group_receipts;
GRANT SELECT ON acq_coverage.acq_collection_runs, acq_coverage.acq_request_tasks,
    acq_coverage.acq_discovery_pages, acq_coverage.acq_group_receipts TO mq_acq_coverage_ro;
COMMIT;

\connect dom
BEGIN;
GRANT USAGE ON SCHEMA public TO mq_data_api_dom_ro, mq_search_export_dom;
GRANT SELECT (id, info_domain, object_type, current_version, source_id, source_key, ordinal)
    ON public.dom_facts TO mq_data_api_dom_ro, mq_search_export_dom;
GRANT SELECT (fact_id, version, recorded_at, content, changed_fields) ON public.dom_fact_versions TO mq_data_api_dom_ro;
GRANT SELECT ON public.dom_version_requirements, public.dom_notice_links, public.dom_enrichment_receipts TO mq_data_api_dom_ro;
GRANT SELECT (project_id, project_version, notice_id, notice_version)
    ON public.dom_project_dependencies TO mq_data_api_dom_ro;
GRANT SELECT (fact_id, version, content, snapshot_ref, revision_ref, schema_version, changed_fields, rights_tags)
    ON public.dom_fact_versions TO mq_search_export_dom;
GRANT SELECT ON public.dom_version_requirements TO mq_search_export_dom;
GRANT SELECT, INSERT ON public.dom_search_projection TO mq_search_export_dom;
GRANT UPDATE (public_version, revision, digest, source_ids, chunk_slots)
    ON public.dom_search_projection TO mq_search_export_dom;
COMMIT;

\connect evd
BEGIN;
GRANT USAGE ON SCHEMA public TO mq_data_api_evd_ro, mq_search_export_evd_ro, mq_evidence_assess_ro;
GRANT SELECT (receipt_seq, event_id, source_id, envelope) ON public.evd_pending_live_events TO mq_evidence_assess_ro;
GRANT SELECT ON public.evd_snapshots, public.evd_grouped_observations, public.evd_evidence_refs,
    public.evd_requirement_sets TO mq_data_api_evd_ro, mq_search_export_evd_ro;
GRANT SELECT (snapshot_id, is_anchor, event) ON public.evd_browser_observations
    TO mq_data_api_evd_ro, mq_search_export_evd_ro;
GRANT SELECT (id, snapshot_id, pack_id, pack_version, extractor, quality_status, facts)
    ON public.evd_extraction_revisions TO mq_data_api_evd_ro, mq_search_export_evd_ro;
GRANT SELECT (id, content_hash) ON public.evd_derivations TO mq_data_api_evd_ro, mq_search_export_evd_ro;
GRANT SELECT (name, version, content_hash, rights_tags) ON public.evd_reference_data TO mq_data_api_evd_ro;
GRANT USAGE ON SCHEMA evd_provenance TO mq_data_api_evd_ro;
GRANT SELECT ON evd_provenance.bindings TO mq_data_api_evd_ro;
COMMIT;

\connect ent
BEGIN;
GRANT USAGE ON SCHEMA public TO mq_data_api_ent_ro, mq_search_export_ent;
GRANT SELECT ON public.ent_organizations, public.ent_organization_versions
    TO mq_data_api_ent_ro, mq_search_export_ent;
GRANT SELECT (org_id, recorded_at) ON public.ent_organization_conflicts TO mq_data_api_ent_ro;
GRANT SELECT (org_id) ON public.ent_organization_conflicts TO mq_search_export_ent;
GRANT SELECT, INSERT ON public.ent_search_projection TO mq_search_export_ent;
GRANT UPDATE (public_version, revision, digest) ON public.ent_search_projection TO mq_search_export_ent;
COMMIT;

\connect postgres
