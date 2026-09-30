-- What the inventory publisher reads to decide whether a deployment can be
-- served right now (docs/inventory.md, "Withheld from publication"). The
-- publisher runs as kaana_runtime, which has no SELECT on provider_cost_events;
-- this one function is the whole of what it can read from that table.
--
-- It returns, per (deployment, exact key, failure code), the failed attempts
-- recorded AFTER that deployment's last success on that key: a streak, not a
-- rate. A success on the same key ends the streak. Cancelled attempts are
-- neither. Attempts recorded before the key's row last changed (a rotation or
-- any other operator put) are not evidence about the credential now in the row.
-- Classifying which failure codes count is the caller's decision, in Go beside
-- the rest of the rule, so this function states facts and decides nothing.
SET LOCAL lock_timeout = '5s';

CREATE FUNCTION kaana_read_deployment_failure_streaks(
    p_since TIMESTAMPTZ
) RETURNS TABLE (
    deployment_id TEXT,
    provider_slug TEXT,
    key_id TEXT,
    failure_code TEXT,
    failures INTEGER,
    first_failed_at TIMESTAMPTZ,
    last_failed_at TIMESTAMPTZ
)
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_read_deployment_failure_streaks$
BEGIN
    -- Bounded so a caller cannot turn this into a full-table scan: the
    -- publisher's lookback is at most seven days.
    IF p_since IS NULL OR p_since < clock_timestamp() - INTERVAL '8 days' THEN
        RAISE EXCEPTION 'deployment failure streak window is invalid';
    END IF;
    RETURN QUERY
        WITH recent AS (
            SELECT e.deployment_id, e.provider_slug, e.key_id, e.attempt_outcome,
                   e.failure_code, e.occurred_at
              FROM public.provider_cost_events e
              JOIN public.provider_credentials c
                ON c.provider_slug = e.provider_slug AND c.key_id = e.key_id
             -- created_at leads provider_cost_events_feed_idx; occurred_at is
             -- the attempt's own time and is what the streak is measured on.
             WHERE e.created_at >= p_since
               AND e.occurred_at >= p_since
               AND e.occurred_at >= c.updated_at
               AND c.enabled
               AND e.attempt_outcome IN ('succeeded', 'failed')
        ), last_success AS (
            SELECT r.deployment_id, r.provider_slug, r.key_id, max(r.occurred_at) AS succeeded_at
              FROM recent r
             WHERE r.attempt_outcome = 'succeeded'
             GROUP BY r.deployment_id, r.provider_slug, r.key_id
        )
        SELECT r.deployment_id, r.provider_slug, r.key_id, r.failure_code,
               count(*)::INTEGER, min(r.occurred_at), max(r.occurred_at)
          FROM recent r
          LEFT JOIN last_success s
            ON s.deployment_id = r.deployment_id AND s.provider_slug = r.provider_slug AND s.key_id = r.key_id
         WHERE r.attempt_outcome = 'failed'
           AND (s.succeeded_at IS NULL OR r.occurred_at > s.succeeded_at)
         GROUP BY r.deployment_id, r.provider_slug, r.key_id, r.failure_code
         ORDER BY r.deployment_id, r.provider_slug, r.key_id, r.failure_code;
END
$kaana_read_deployment_failure_streaks$;

REVOKE ALL ON FUNCTION kaana_read_deployment_failure_streaks(TIMESTAMPTZ) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION kaana_read_deployment_failure_streaks(TIMESTAMPTZ) TO kaana_runtime;
