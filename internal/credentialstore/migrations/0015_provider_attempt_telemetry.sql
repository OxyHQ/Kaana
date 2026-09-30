-- Per-attempt operator telemetry (#72). Every upstream attempt already has one
-- provider_cost_events row keyed by (request_id, attempt_index) and bound to the
-- exact (provider_slug, key_id). This adds what the attempt measured and how it
-- ended, so the row answers "what did this key do" and not only "what did it
-- cost". Rows written before this migration keep NULL: they were never
-- measured, and inventing a zero latency or an empty unit list for them would
-- be a fact nobody observed.
SET LOCAL lock_timeout = '5s';

ALTER TABLE provider_cost_events
    ADD COLUMN usage_units JSONB,
    ADD COLUMN attempt_started_at TIMESTAMPTZ,
    ADD COLUMN latency_ms INTEGER,
    ADD COLUMN time_to_first_output_ms INTEGER,
    ADD COLUMN attempt_outcome TEXT,
    ADD COLUMN failure_code TEXT;

ALTER TABLE provider_cost_events
    ADD CONSTRAINT provider_cost_events_attempt_telemetry_check
    CHECK (
        (attempt_outcome IS NULL AND usage_units IS NULL AND attempt_started_at IS NULL
            AND latency_ms IS NULL AND time_to_first_output_ms IS NULL AND failure_code IS NULL)
        OR (
            attempt_outcome IN ('succeeded', 'failed', 'cancelled')
            AND jsonb_typeof(usage_units) = 'array'
            AND attempt_started_at IS NOT NULL
            AND latency_ms >= 0
            AND (time_to_first_output_ms IS NULL OR time_to_first_output_ms BETWEEN 0 AND latency_ms)
            AND ((attempt_outcome = 'failed' AND failure_code ~ '^[a-z][a-z0-9_]{0,63}$')
                 OR (attempt_outcome <> 'failed' AND failure_code IS NULL))
        )
    ) NOT VALID;

CREATE INDEX provider_cost_events_key_outcome_time_idx
    ON provider_cost_events (provider_slug, key_id, attempt_outcome, occurred_at DESC);

-- The telemetry-bearing successor of kaana_record_provider_cost_events. A
-- replay must restate every fact, measurements included: a retried batch that
-- differs only in latency is a second observation claiming the first one's
-- identity, and it fails closed exactly like a different amount does.
CREATE FUNCTION kaana_record_provider_attempt_events(
    p_events JSONB
) RETURNS INTEGER
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_record_provider_attempt_events$
DECLARE
    event JSONB;
    existing provider_cost_events%ROWTYPE;
    expected_request_id TEXT;
    event_count INTEGER;
BEGIN
    IF p_events IS NULL OR jsonb_typeof(p_events) <> 'array'
       OR jsonb_array_length(p_events) NOT BETWEEN 1 AND 64 THEN
        RAISE EXCEPTION 'provider attempt event batch is invalid';
    END IF;

    SELECT count(*), min(parsed.request_id)
      INTO event_count, expected_request_id
    FROM jsonb_to_recordset(p_events) AS parsed(
        request_id TEXT,
        attempt_index INTEGER
    );

    IF event_count <> jsonb_array_length(p_events)
       OR expected_request_id IS NULL
       OR EXISTS (
            SELECT 1
            FROM jsonb_to_recordset(p_events) AS parsed(
                request_id TEXT,
                attempt_index INTEGER
            )
            WHERE parsed.request_id IS DISTINCT FROM expected_request_id
               OR parsed.attempt_index IS NULL
       )
       OR EXISTS (
            SELECT 1
            FROM jsonb_to_recordset(p_events) AS parsed(
                request_id TEXT,
                attempt_index INTEGER
            )
            GROUP BY parsed.request_id, parsed.attempt_index
            HAVING count(*) <> 1
       ) THEN
        RAISE EXCEPTION 'provider attempt event batch identity is invalid';
    END IF;

    FOR event IN SELECT value FROM jsonb_array_elements(p_events)
    LOOP
        IF event->>'attempt_outcome' IS NULL
           OR jsonb_typeof(event->'usage_units') IS DISTINCT FROM 'array'
           OR event->>'attempt_started_at' IS NULL
           OR event->>'latency_ms' IS NULL THEN
            RAISE EXCEPTION 'provider attempt event lacks its telemetry';
        END IF;

        INSERT INTO public.provider_cost_events (
            request_id, attempt_index, provider_slug, key_id, deployment_id,
            model_reference, currency, amount_picos, source, complete, served,
            occurred_at, rate_card_version_id, usage_units, attempt_started_at,
            latency_ms, time_to_first_output_ms, attempt_outcome, failure_code
        ) VALUES (
            event->>'request_id',
            (event->>'attempt_index')::INTEGER,
            event->>'provider_slug',
            event->>'key_id',
            event->>'deployment_id',
            event->>'model_reference',
            event->>'currency',
            (event->>'amount_picos')::NUMERIC,
            event->>'source',
            (event->>'complete')::BOOLEAN,
            (event->>'served')::BOOLEAN,
            (event->>'occurred_at')::TIMESTAMPTZ,
            event->>'rate_card_version_id',
            event->'usage_units',
            (event->>'attempt_started_at')::TIMESTAMPTZ,
            (event->>'latency_ms')::INTEGER,
            (event->>'time_to_first_output_ms')::INTEGER,
            event->>'attempt_outcome',
            event->>'failure_code'
        )
        ON CONFLICT (request_id, attempt_index) DO NOTHING;

        IF NOT FOUND THEN
            SELECT * INTO STRICT existing
            FROM public.provider_cost_events
            WHERE request_id = event->>'request_id'
              AND attempt_index = (event->>'attempt_index')::INTEGER;
            IF existing.provider_slug <> event->>'provider_slug'
               OR existing.key_id <> event->>'key_id'
               OR existing.deployment_id <> event->>'deployment_id'
               OR existing.model_reference <> event->>'model_reference'
               OR existing.currency IS DISTINCT FROM event->>'currency'
               OR existing.amount_picos IS DISTINCT FROM (event->>'amount_picos')::NUMERIC
               OR existing.source <> event->>'source'
               OR existing.complete <> (event->>'complete')::BOOLEAN
               OR existing.served <> (event->>'served')::BOOLEAN
               OR existing.occurred_at <> (event->>'occurred_at')::TIMESTAMPTZ
               OR existing.rate_card_version_id IS DISTINCT FROM event->>'rate_card_version_id'
               OR existing.usage_units IS DISTINCT FROM event->'usage_units'
               OR existing.attempt_started_at IS DISTINCT FROM (event->>'attempt_started_at')::TIMESTAMPTZ
               OR existing.latency_ms IS DISTINCT FROM (event->>'latency_ms')::INTEGER
               OR existing.time_to_first_output_ms IS DISTINCT FROM (event->>'time_to_first_output_ms')::INTEGER
               OR existing.attempt_outcome IS DISTINCT FROM event->>'attempt_outcome'
               OR existing.failure_code IS DISTINCT FROM event->>'failure_code' THEN
                RAISE EXCEPTION 'provider cost event identity conflict';
            END IF;
        END IF;
    END LOOP;

    RETURN event_count;
END
$kaana_record_provider_attempt_events$;

-- kaana_record_provider_cost_events stays granted: a task built before this
-- migration keeps recording through it until the rollout replaces it. Its rows
-- carry no telemetry, which the CHECK above admits as "never measured".
REVOKE ALL ON FUNCTION kaana_record_provider_attempt_events(JSONB) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION kaana_record_provider_attempt_events(JSONB) TO kaana_runtime;
