-- The controlled operator feed Oxy ingests (#72). Oxy owns balances, grants,
-- reservations and deployment ordering; to order deployments on evidence it
-- reads what Kaana measured. These two functions are the whole of what it can
-- read, through a separately signed path on the runtime: no label, no account
-- email, no commercial-use evidence text, no secret and no customer identity.
SET LOCAL lock_timeout = '5s';

CREATE INDEX provider_cost_events_feed_idx
    ON provider_cost_events (created_at, request_id, attempt_index);

-- Attempts after a keyset cursor, oldest first. A row is only visible once it
-- is older than the settle window: created_at is the writing transaction's
-- start, so a row committed a moment late could otherwise carry a position the
-- reader has already passed, and the feed would skip it forever.
CREATE FUNCTION kaana_read_provider_attempt_feed(
    p_after_created_at TIMESTAMPTZ,
    p_after_request_id TEXT,
    p_after_attempt_index INTEGER,
    p_limit INTEGER
) RETURNS TABLE (
    created_at TIMESTAMPTZ,
    request_id TEXT,
    attempt_index INTEGER,
    provider_slug TEXT,
    key_id TEXT,
    key_class TEXT,
    deployment_id TEXT,
    model_reference TEXT,
    currency TEXT,
    amount_picos TEXT,
    source TEXT,
    rate_card_version_id TEXT,
    complete BOOLEAN,
    served BOOLEAN,
    occurred_at TIMESTAMPTZ,
    usage_units JSONB,
    attempt_started_at TIMESTAMPTZ,
    latency_ms INTEGER,
    time_to_first_output_ms INTEGER,
    attempt_outcome TEXT,
    failure_code TEXT
)
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_read_provider_attempt_feed$
BEGIN
    IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 500
       OR ((p_after_created_at IS NULL) <> (p_after_request_id IS NULL))
       OR ((p_after_created_at IS NULL) <> (p_after_attempt_index IS NULL)) THEN
        RAISE EXCEPTION 'provider attempt feed cursor is invalid';
    END IF;
    RETURN QUERY
        SELECT e.created_at, e.request_id, e.attempt_index, e.provider_slug, e.key_id, c.key_class,
               e.deployment_id, e.model_reference, e.currency::TEXT, e.amount_picos::TEXT, e.source,
               e.rate_card_version_id, e.complete, e.served, e.occurred_at, e.usage_units,
               e.attempt_started_at, e.latency_ms, e.time_to_first_output_ms, e.attempt_outcome,
               e.failure_code
          FROM public.provider_cost_events e
          JOIN public.provider_credentials c
            ON c.provider_slug = e.provider_slug AND c.key_id = e.key_id
         WHERE e.created_at < clock_timestamp() - INTERVAL '15 seconds'
           AND (p_after_created_at IS NULL
                OR (e.created_at, e.request_id, e.attempt_index) >
                   (p_after_created_at, p_after_request_id, p_after_attempt_index))
         ORDER BY e.created_at, e.request_id, e.attempt_index
         LIMIT p_limit;
END
$kaana_read_provider_attempt_feed$;

-- The economic facts about each enabled platform key that Oxy may order on:
-- its stated class and capacity category, whether commercial use is permitted,
-- which opaque funding account it shares, what it may serve, and its latest
-- capacity evidence of each kind. The account label and the evidence text stay
-- in credential administration.
CREATE FUNCTION kaana_read_provider_credential_economics()
RETURNS TABLE (
    provider_slug TEXT,
    key_id TEXT,
    key_class TEXT,
    description_revision INTEGER,
    capacity_category TEXT,
    environment TEXT,
    commercial_use TEXT,
    funding_account_id TEXT,
    allowed_models TEXT[],
    allowed_capabilities TEXT[],
    capacity_evidence JSONB
)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_read_provider_credential_economics$
    SELECT c.provider_slug, c.key_id, c.key_class, d.revision, d.capacity_category, d.environment,
           d.commercial_use, d.funding_account_id, d.allowed_models, d.allowed_capabilities,
           COALESCE((
               SELECT jsonb_agg(jsonb_strip_nulls(jsonb_build_object(
                          'evidenceId', latest.evidence_id,
                          'kind', latest.kind,
                          'source', latest.source,
                          'observedAt', latest.observed_at,
                          'freshUntil', latest.fresh_until,
                          'quotaUnit', latest.quota_unit,
                          'quotaWindow', latest.quota_window,
                          'quotaLimit', latest.quota_limit,
                          'quotaRemaining', latest.quota_remaining,
                          'balanceCurrency', latest.balance_currency,
                          'balancePicos', latest.balance_picos::TEXT,
                          'expiresAt', latest.expires_at
                      )) ORDER BY latest.kind)
                 FROM (
                     SELECT DISTINCT ON (evidence.kind) evidence.*
                       FROM public.provider_credential_capacity_evidence evidence
                      WHERE evidence.provider_slug = c.provider_slug AND evidence.key_id = c.key_id
                      ORDER BY evidence.kind, evidence.observed_at DESC, evidence.recorded_at DESC
                 ) latest
           ), '[]'::JSONB)
      FROM public.provider_credentials c
      LEFT JOIN public.provider_credential_descriptions d
        ON d.provider_slug = c.provider_slug AND d.key_id = c.key_id
     WHERE c.enabled
     ORDER BY c.provider_slug, c.key_id;
$kaana_read_provider_credential_economics$;

REVOKE ALL ON FUNCTION kaana_read_provider_attempt_feed(TIMESTAMPTZ, TEXT, INTEGER, INTEGER) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_read_provider_credential_economics() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION kaana_read_provider_attempt_feed(TIMESTAMPTZ, TEXT, INTEGER, INTEGER) TO kaana_runtime;
GRANT EXECUTE ON FUNCTION kaana_read_provider_credential_economics() TO kaana_runtime;
