ALTER TABLE provider_cost_events
    ADD COLUMN rate_card_version_id TEXT;

ALTER TABLE provider_cost_events
    ADD CONSTRAINT provider_cost_events_rate_card_version_check
    CHECK (
        (source = 'rate_card' AND length(rate_card_version_id) BETWEEN 1 AND 256)
        OR (source <> 'rate_card' AND rate_card_version_id IS NULL)
    ) NOT VALID;

CREATE FUNCTION kaana_record_provider_cost_events(
    p_events JSONB
) RETURNS INTEGER
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_record_provider_cost_events$
DECLARE
    event JSONB;
    existing provider_cost_events%ROWTYPE;
    expected_request_id TEXT;
    event_count INTEGER;
BEGIN
    IF p_events IS NULL OR jsonb_typeof(p_events) <> 'array'
       OR jsonb_array_length(p_events) NOT BETWEEN 1 AND 64 THEN
        RAISE EXCEPTION 'provider cost event batch is invalid';
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
        RAISE EXCEPTION 'provider cost event batch identity is invalid';
    END IF;

    FOR event IN SELECT value FROM jsonb_array_elements(p_events)
    LOOP
        INSERT INTO public.provider_cost_events (
            request_id, attempt_index, provider_slug, key_id, deployment_id,
            model_reference, currency, amount_picos, source, complete, served,
            occurred_at, rate_card_version_id
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
            event->>'rate_card_version_id'
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
               OR existing.rate_card_version_id IS DISTINCT FROM event->>'rate_card_version_id' THEN
                RAISE EXCEPTION 'provider cost event identity conflict';
            END IF;
        END IF;
    END LOOP;

    RETURN event_count;
END
$kaana_record_provider_cost_events$;

REVOKE ALL ON FUNCTION kaana_record_provider_cost_event(TEXT, INTEGER, TEXT, TEXT, TEXT, TEXT, TEXT, NUMERIC, TEXT, BOOLEAN, BOOLEAN, TIMESTAMPTZ) FROM kaana_runtime;
REVOKE ALL ON FUNCTION kaana_record_provider_cost_events(JSONB) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION kaana_record_provider_cost_events(JSONB) TO kaana_runtime;
