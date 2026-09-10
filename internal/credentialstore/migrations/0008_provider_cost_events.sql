CREATE TABLE provider_cost_events (
    request_id TEXT NOT NULL CHECK (length(request_id) BETWEEN 1 AND 256),
    attempt_index INTEGER NOT NULL CHECK (attempt_index >= 0),
    provider_slug TEXT NOT NULL,
    key_id TEXT NOT NULL,
    deployment_id TEXT NOT NULL CHECK (length(deployment_id) BETWEEN 1 AND 256),
    model_reference TEXT NOT NULL CHECK (length(model_reference) BETWEEN 1 AND 512),
    currency CHAR(3),
    amount_picos NUMERIC(30, 0),
    source TEXT NOT NULL CHECK (source IN ('provider_reported', 'rate_card', 'unknown')),
    complete BOOLEAN NOT NULL,
    served BOOLEAN NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (request_id, attempt_index),
    FOREIGN KEY (provider_slug, key_id)
        REFERENCES provider_credentials (provider_slug, key_id),
    CHECK ((source = 'unknown' AND currency IS NULL AND amount_picos IS NULL AND complete = FALSE) OR
           (source <> 'unknown' AND currency ~ '^[A-Z]{3}$' AND amount_picos >= 0))
);

CREATE INDEX provider_cost_events_key_time_idx
    ON provider_cost_events (provider_slug, key_id, occurred_at DESC);
CREATE INDEX provider_cost_events_deployment_time_idx
    ON provider_cost_events (deployment_id, occurred_at DESC);
CREATE INDEX provider_cost_events_model_time_idx
    ON provider_cost_events (model_reference, occurred_at DESC);

CREATE FUNCTION kaana_record_provider_cost_event(
    p_request_id TEXT,
    p_attempt_index INTEGER,
    p_provider_slug TEXT,
    p_key_id TEXT,
    p_deployment_id TEXT,
    p_model_reference TEXT,
    p_currency TEXT,
    p_amount_picos NUMERIC,
    p_source TEXT,
    p_complete BOOLEAN,
    p_served BOOLEAN,
    p_occurred_at TIMESTAMPTZ
) RETURNS BOOLEAN
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_record_provider_cost_event$
DECLARE
    existing provider_cost_events%ROWTYPE;
BEGIN
    INSERT INTO provider_cost_events (
        request_id, attempt_index, provider_slug, key_id, deployment_id,
        model_reference, currency, amount_picos, source, complete, served, occurred_at
    ) VALUES (
        p_request_id, p_attempt_index, p_provider_slug, p_key_id, p_deployment_id,
        p_model_reference, p_currency, p_amount_picos, p_source, p_complete, p_served, p_occurred_at
    )
    ON CONFLICT (request_id, attempt_index) DO NOTHING;
    IF FOUND THEN
        RETURN TRUE;
    END IF;

    SELECT * INTO STRICT existing
      FROM provider_cost_events
     WHERE request_id = p_request_id AND attempt_index = p_attempt_index;
    IF existing.provider_slug <> p_provider_slug OR existing.key_id <> p_key_id OR
       existing.deployment_id <> p_deployment_id OR existing.model_reference <> p_model_reference OR
       existing.currency IS DISTINCT FROM p_currency OR existing.amount_picos IS DISTINCT FROM p_amount_picos OR
       existing.source <> p_source OR existing.complete <> p_complete OR existing.served <> p_served OR
       existing.occurred_at <> p_occurred_at THEN
        RAISE EXCEPTION 'provider cost event identity conflict';
    END IF;
    RETURN TRUE;
END
$kaana_record_provider_cost_event$;

REVOKE ALL ON provider_cost_events FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_record_provider_cost_event(TEXT, INTEGER, TEXT, TEXT, TEXT, TEXT, TEXT, NUMERIC, TEXT, BOOLEAN, BOOLEAN, TIMESTAMPTZ) FROM PUBLIC;
GRANT SELECT ON provider_cost_events TO kaana_credential_admin;
GRANT EXECUTE ON FUNCTION kaana_record_provider_cost_event(TEXT, INTEGER, TEXT, TEXT, TEXT, TEXT, TEXT, NUMERIC, TEXT, BOOLEAN, BOOLEAN, TIMESTAMPTZ) TO kaana_runtime;
