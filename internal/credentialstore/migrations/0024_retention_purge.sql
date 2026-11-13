-- Retention for the three tables that grow with traffic (docs/operating.md,
-- "Data retention"): provider_cost_events and provider_credential_attempt_events
-- (one row per upstream attempt) and the DELIVERED rows of
-- inference_settlement_outbox. Nothing here deletes anything by itself: these
-- functions exist so an operator one-shot (`kaana-credentials purge-retention`)
-- can, in bounded batches, after a dry run.
--
-- Safeguards that cannot be bypassed by a caller:
--   * a cutoff newer than 30 days is refused, whatever the policy says;
--   * the outbox purge deletes only rows Oxy acknowledged (delivered_at set),
--     never an undelivered settlement, whose delivery is the only copy of a charge;
--   * a batch is at most 10,000 rows, so one call never holds a long lock;
--   * dry_run counts and deletes nothing.
-- Audit tables are not purged: they are low volume and are the evidence.
SET LOCAL lock_timeout = '5s';

CREATE FUNCTION kaana_purge_provider_cost_events(
    p_older_than TIMESTAMPTZ,
    p_limit INTEGER,
    p_dry_run BOOLEAN
) RETURNS BIGINT
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_purge_provider_cost_events$
DECLARE
    affected BIGINT;
BEGIN
    IF p_older_than IS NULL OR p_older_than > clock_timestamp() - INTERVAL '30 days'
       OR p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 10000 OR p_dry_run IS NULL THEN
        RAISE EXCEPTION 'retention purge is invalid';
    END IF;
    IF p_dry_run THEN
        SELECT count(*) INTO affected FROM (
            SELECT 1 FROM public.provider_cost_events WHERE occurred_at < p_older_than LIMIT p_limit
        ) eligible;
        RETURN affected;
    END IF;
    WITH doomed AS (
        SELECT request_id, attempt_index FROM public.provider_cost_events
         WHERE occurred_at < p_older_than
         ORDER BY occurred_at
         LIMIT p_limit
           FOR UPDATE SKIP LOCKED
    )
    DELETE FROM public.provider_cost_events e
     USING doomed d
     WHERE e.request_id = d.request_id AND e.attempt_index = d.attempt_index;
    GET DIAGNOSTICS affected = ROW_COUNT;
    RETURN affected;
END
$kaana_purge_provider_cost_events$;

CREATE FUNCTION kaana_purge_provider_credential_attempt_events(
    p_older_than TIMESTAMPTZ,
    p_limit INTEGER,
    p_dry_run BOOLEAN
) RETURNS BIGINT
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_purge_provider_credential_attempt_events$
DECLARE
    affected BIGINT;
BEGIN
    IF p_older_than IS NULL OR p_older_than > clock_timestamp() - INTERVAL '30 days'
       OR p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 10000 OR p_dry_run IS NULL THEN
        RAISE EXCEPTION 'retention purge is invalid';
    END IF;
    IF p_dry_run THEN
        SELECT count(*) INTO affected FROM (
            SELECT 1 FROM public.provider_credential_attempt_events WHERE occurred_at < p_older_than LIMIT p_limit
        ) eligible;
        RETURN affected;
    END IF;
    WITH doomed AS (
        SELECT request_id, deployment_id, credential_attempt_index
          FROM public.provider_credential_attempt_events
         WHERE occurred_at < p_older_than
         ORDER BY occurred_at
         LIMIT p_limit
           FOR UPDATE SKIP LOCKED
    )
    DELETE FROM public.provider_credential_attempt_events e
     USING doomed d
     WHERE e.request_id = d.request_id AND e.deployment_id = d.deployment_id
       AND e.credential_attempt_index = d.credential_attempt_index;
    GET DIAGNOSTICS affected = ROW_COUNT;
    RETURN affected;
END
$kaana_purge_provider_credential_attempt_events$;

CREATE FUNCTION kaana_purge_delivered_settlements(
    p_older_than TIMESTAMPTZ,
    p_limit INTEGER,
    p_dry_run BOOLEAN
) RETURNS BIGINT
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_purge_delivered_settlements$
DECLARE
    affected BIGINT;
BEGIN
    IF p_older_than IS NULL OR p_older_than > clock_timestamp() - INTERVAL '30 days'
       OR p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 10000 OR p_dry_run IS NULL THEN
        RAISE EXCEPTION 'retention purge is invalid';
    END IF;
    IF p_dry_run THEN
        SELECT count(*) INTO affected FROM (
            SELECT 1 FROM public.inference_settlement_outbox
             WHERE delivered_at IS NOT NULL AND delivered_at < p_older_than LIMIT p_limit
        ) eligible;
        RETURN affected;
    END IF;
    WITH doomed AS (
        SELECT settlement_id FROM public.inference_settlement_outbox
         WHERE delivered_at IS NOT NULL AND delivered_at < p_older_than
         ORDER BY outbox_seq
         LIMIT p_limit
           FOR UPDATE SKIP LOCKED
    )
    DELETE FROM public.inference_settlement_outbox o
     USING doomed d
     WHERE o.settlement_id = d.settlement_id AND o.delivered_at IS NOT NULL;
    GET DIAGNOSTICS affected = ROW_COUNT;
    RETURN affected;
END
$kaana_purge_delivered_settlements$;

REVOKE ALL ON FUNCTION kaana_purge_provider_cost_events(TIMESTAMPTZ, INTEGER, BOOLEAN) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_purge_provider_credential_attempt_events(TIMESTAMPTZ, INTEGER, BOOLEAN) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_purge_delivered_settlements(TIMESTAMPTZ, INTEGER, BOOLEAN) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION kaana_purge_provider_cost_events(TIMESTAMPTZ, INTEGER, BOOLEAN) TO kaana_credential_admin;
GRANT EXECUTE ON FUNCTION kaana_purge_provider_credential_attempt_events(TIMESTAMPTZ, INTEGER, BOOLEAN) TO kaana_credential_admin;
GRANT EXECUTE ON FUNCTION kaana_purge_delivered_settlements(TIMESTAMPTZ, INTEGER, BOOLEAN) TO kaana_credential_admin;
