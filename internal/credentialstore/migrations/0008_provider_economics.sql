CREATE TABLE provider_funding_accounts (
    provider_slug TEXT NOT NULL
        CHECK (provider_slug ~ '^[a-z0-9]+([._-][a-z0-9]+)*$'),
    funding_account_id UUID NOT NULL,
    title TEXT NOT NULL CHECK (length(title) BETWEEN 1 AND 256),
    account_label TEXT NOT NULL CHECK (length(account_label) BETWEEN 1 AND 320),
    capacity_category TEXT NOT NULL
        CHECK (capacity_category IN ('free_trial', 'promotional', 'prepaid', 'paid', 'unknown')),
    commercial_use_allowed BOOLEAN NOT NULL DEFAULT FALSE,
    evidence_source TEXT NOT NULL
        CHECK (evidence_source IN ('provider_api', 'provider_response', 'provider_email', 'provider_documentation', 'operator')),
    evidence_observed_at TIMESTAMPTZ NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (provider_slug, funding_account_id)
);

ALTER TABLE provider_credentials
    ADD COLUMN funding_account_id UUID,
    ADD CONSTRAINT provider_credentials_funding_account_fk
        FOREIGN KEY (provider_slug, funding_account_id)
        REFERENCES provider_funding_accounts (provider_slug, funding_account_id);

CREATE TABLE provider_capacity_grants (
    grant_id UUID PRIMARY KEY,
    provider_slug TEXT NOT NULL,
    funding_account_id UUID NOT NULL,
    capacity_category TEXT NOT NULL
        CHECK (capacity_category IN ('free_trial', 'promotional', 'prepaid', 'paid')),
    unit TEXT NOT NULL
        CHECK (unit = 'requests' OR unit = 'input_tokens' OR unit = 'output_tokens' OR unit = 'images' OR unit = 'audio_seconds' OR unit ~ '^currency:[A-Z]{3}$'),
    granted_amount NUMERIC(30, 0) NOT NULL CHECK (granted_amount >= 0),
    reserved_amount NUMERIC(30, 0) NOT NULL DEFAULT 0 CHECK (reserved_amount >= 0),
    consumed_amount NUMERIC(30, 0) NOT NULL DEFAULT 0 CHECK (consumed_amount >= 0),
    max_request_reservation NUMERIC(30, 0) NOT NULL CHECK (max_request_reservation > 0),
    starts_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    reset_interval_seconds BIGINT CHECK (reset_interval_seconds IS NULL OR reset_interval_seconds > 0),
    last_reset_at TIMESTAMPTZ,
    source TEXT NOT NULL
        CHECK (source IN ('provider_api', 'provider_response', 'provider_email', 'provider_documentation', 'operator')),
    observed_at TIMESTAMPTZ NOT NULL,
    freshness_seconds BIGINT CHECK (freshness_seconds IS NULL OR freshness_seconds > 0),
    provider_enforced BOOLEAN NOT NULL DEFAULT FALSE,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (provider_slug, funding_account_id)
        REFERENCES provider_funding_accounts (provider_slug, funding_account_id),
    CHECK (expires_at IS NULL OR expires_at > starts_at),
    CHECK (last_reset_at IS NULL OR reset_interval_seconds IS NOT NULL)
);

CREATE INDEX provider_capacity_grants_selection_idx
    ON provider_capacity_grants (provider_slug, funding_account_id, capacity_category, expires_at)
    WHERE enabled = TRUE;

CREATE TABLE provider_capacity_reservations (
    reservation_id UUID PRIMARY KEY,
    request_id TEXT NOT NULL CHECK (length(request_id) BETWEEN 1 AND 256),
    attempt_index INTEGER NOT NULL CHECK (attempt_index >= 0),
    provider_slug TEXT NOT NULL,
    key_id TEXT NOT NULL,
    funding_account_id UUID NOT NULL,
    grant_id UUID NOT NULL REFERENCES provider_capacity_grants (grant_id),
    unit TEXT NOT NULL,
    reserved_amount NUMERIC(30, 0) NOT NULL CHECK (reserved_amount > 0),
    settled_amount NUMERIC(30, 0) CHECK (settled_amount IS NULL OR settled_amount >= 0),
    settlement_source TEXT
        CHECK (settlement_source IS NULL OR settlement_source IN ('provider_reported', 'rate_card', 'released')),
    state TEXT NOT NULL CHECK (state IN ('reserved', 'settled', 'released')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    settled_at TIMESTAMPTZ,
    UNIQUE (request_id, attempt_index),
    FOREIGN KEY (provider_slug, key_id)
        REFERENCES provider_credentials (provider_slug, key_id),
    FOREIGN KEY (provider_slug, funding_account_id)
        REFERENCES provider_funding_accounts (provider_slug, funding_account_id),
    CHECK ((state = 'reserved' AND settled_amount IS NULL AND settlement_source IS NULL AND settled_at IS NULL) OR
           (state IN ('settled', 'released') AND settled_amount IS NOT NULL AND settlement_source IS NOT NULL AND settled_at IS NOT NULL))
);

CREATE TABLE provider_cost_events (
    event_id UUID PRIMARY KEY,
    request_id TEXT NOT NULL CHECK (length(request_id) BETWEEN 1 AND 256),
    attempt_index INTEGER NOT NULL CHECK (attempt_index >= 0),
    provider_slug TEXT NOT NULL,
    key_id TEXT NOT NULL,
    funding_account_id UUID,
    deployment_id TEXT NOT NULL CHECK (length(deployment_id) BETWEEN 1 AND 256),
    model_reference TEXT NOT NULL CHECK (length(model_reference) BETWEEN 1 AND 512),
    currency CHAR(3),
    amount_picos NUMERIC(30, 0) CHECK (amount_picos IS NULL OR amount_picos >= 0),
    source TEXT NOT NULL CHECK (source IN ('provider_reported', 'rate_card', 'unknown')),
    complete BOOLEAN NOT NULL,
    served BOOLEAN NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (request_id, attempt_index),
    FOREIGN KEY (provider_slug, key_id)
        REFERENCES provider_credentials (provider_slug, key_id),
    FOREIGN KEY (provider_slug, funding_account_id)
        REFERENCES provider_funding_accounts (provider_slug, funding_account_id),
    CHECK ((source = 'unknown' AND currency IS NULL AND amount_picos IS NULL AND complete = FALSE) OR
           (source <> 'unknown' AND currency IS NOT NULL AND amount_picos IS NOT NULL))
);

CREATE INDEX provider_cost_events_key_time_idx
    ON provider_cost_events (provider_slug, key_id, occurred_at DESC);
CREATE INDEX provider_cost_events_model_time_idx
    ON provider_cost_events (model_reference, occurred_at DESC);

CREATE TABLE provider_balance_observations (
    observation_id UUID PRIMARY KEY,
    provider_slug TEXT NOT NULL,
    funding_account_id UUID NOT NULL,
    unit TEXT NOT NULL,
    available_amount NUMERIC(30, 0) NOT NULL CHECK (available_amount >= 0),
    reserved_amount NUMERIC(30, 0) CHECK (reserved_amount IS NULL OR reserved_amount >= 0),
    source TEXT NOT NULL CHECK (source IN ('provider_api', 'provider_response', 'operator')),
    observed_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (provider_slug, funding_account_id)
        REFERENCES provider_funding_accounts (provider_slug, funding_account_id),
    CHECK (expires_at IS NULL OR expires_at > observed_at)
);

CREATE INDEX provider_balance_observations_latest_idx
    ON provider_balance_observations (provider_slug, funding_account_id, unit, observed_at DESC);

CREATE TABLE provider_reconciliation_runs (
    reconciliation_id UUID PRIMARY KEY,
    provider_slug TEXT NOT NULL,
    funding_account_id UUID NOT NULL,
    unit TEXT NOT NULL,
    ledger_available_amount NUMERIC(30, 0) NOT NULL,
    provider_available_amount NUMERIC(30, 0) NOT NULL,
    drift_amount NUMERIC(30, 0) NOT NULL,
    observation_id UUID NOT NULL REFERENCES provider_balance_observations (observation_id),
    reconciled_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (provider_slug, funding_account_id)
        REFERENCES provider_funding_accounts (provider_slug, funding_account_id),
    CHECK (drift_amount = provider_available_amount - ledger_available_amount)
);

CREATE TABLE provider_rate_card_versions (
    rate_card_version_id UUID PRIMARY KEY,
    provider_slug TEXT NOT NULL,
    source TEXT NOT NULL CHECK (source IN ('provider_api', 'provider_documentation', 'operator')),
    source_version TEXT NOT NULL CHECK (length(source_version) BETWEEN 1 AND 256),
    observed_at TIMESTAMPTZ NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (provider_slug, source, source_version),
    CHECK (expires_at IS NULL OR expires_at > effective_at)
);

CREATE TABLE provider_rate_card_rates (
    rate_card_version_id UUID NOT NULL REFERENCES provider_rate_card_versions (rate_card_version_id),
    deployment_id TEXT NOT NULL CHECK (length(deployment_id) BETWEEN 1 AND 256),
    model_reference TEXT NOT NULL CHECK (length(model_reference) BETWEEN 1 AND 512),
    modality TEXT NOT NULL CHECK (length(modality) BETWEEN 1 AND 64),
    usage_unit TEXT NOT NULL CHECK (length(usage_unit) BETWEEN 1 AND 64),
    currency CHAR(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    amount_per_unit_picos NUMERIC(30, 0) NOT NULL CHECK (amount_per_unit_picos >= 0),
    PRIMARY KEY (rate_card_version_id, deployment_id, modality, usage_unit)
);

CREATE TABLE provider_usage_rollups (
    bucket_start TIMESTAMPTZ NOT NULL,
    bucket_seconds INTEGER NOT NULL CHECK (bucket_seconds IN (3600, 86400)),
    provider_slug TEXT NOT NULL,
    key_id TEXT NOT NULL,
    funding_account_id UUID,
    deployment_id TEXT NOT NULL,
    model_reference TEXT NOT NULL,
    requests BIGINT NOT NULL CHECK (requests >= 0),
    successes BIGINT NOT NULL CHECK (successes >= 0 AND successes <= requests),
    input_tokens BIGINT NOT NULL CHECK (input_tokens >= 0),
    output_tokens BIGINT NOT NULL CHECK (output_tokens >= 0),
    currency CHAR(3),
    cost_picos NUMERIC(30, 0) CHECK (cost_picos IS NULL OR cost_picos >= 0),
    cost_complete BOOLEAN NOT NULL,
    PRIMARY KEY (bucket_start, bucket_seconds, provider_slug, key_id, deployment_id, model_reference),
    FOREIGN KEY (provider_slug, key_id)
        REFERENCES provider_credentials (provider_slug, key_id),
    FOREIGN KEY (provider_slug, funding_account_id)
        REFERENCES provider_funding_accounts (provider_slug, funding_account_id)
);

CREATE VIEW provider_credential_economics AS
SELECT c.provider_slug,
       c.key_id,
       c.position,
       c.key_class,
       c.enabled,
       a.funding_account_id,
       a.title,
       a.account_label,
       a.capacity_category,
       a.commercial_use_allowed,
       a.evidence_source,
       a.evidence_observed_at
  FROM provider_credentials c
  LEFT JOIN provider_funding_accounts a
    ON a.provider_slug = c.provider_slug
   AND a.funding_account_id = c.funding_account_id;

CREATE FUNCTION kaana_reserve_provider_capacity(
    p_reservation_id UUID,
    p_request_id TEXT,
    p_attempt_index INTEGER,
    p_provider_slug TEXT,
    p_key_ids TEXT[],
    p_unit TEXT,
    p_now TIMESTAMPTZ
) RETURNS TABLE (key_id TEXT, funding_account_id UUID, grant_id UUID, reserved_amount NUMERIC)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
    existing provider_capacity_reservations%ROWTYPE;
    selected_key_id TEXT;
    selected_funding_account_id UUID;
    selected_grant_id UUID;
    selected_reservation NUMERIC(30, 0);
BEGIN
    IF p_reservation_id IS NULL OR p_request_id IS NULL OR length(p_request_id) NOT BETWEEN 1 AND 256 OR
       p_attempt_index < 0 OR p_provider_slug IS NULL OR cardinality(p_key_ids) = 0 OR
       p_unit IS NULL OR p_now IS NULL THEN
        RAISE EXCEPTION 'invalid provider capacity reservation';
    END IF;

    SELECT * INTO existing
      FROM provider_capacity_reservations r
     WHERE r.reservation_id = p_reservation_id OR
           (r.request_id = p_request_id AND r.attempt_index = p_attempt_index)
     FOR UPDATE;
    IF FOUND THEN
        IF existing.reservation_id <> p_reservation_id OR existing.request_id <> p_request_id OR
           existing.attempt_index <> p_attempt_index OR existing.provider_slug <> p_provider_slug OR
           existing.unit <> p_unit OR NOT (existing.key_id = ANY(p_key_ids)) THEN
            RAISE EXCEPTION 'provider capacity reservation identity conflict';
        END IF;
        RETURN QUERY SELECT existing.key_id, existing.funding_account_id,
                            existing.grant_id, existing.reserved_amount;
        RETURN;
    END IF;

    SELECT c.key_id, c.funding_account_id, g.grant_id, g.max_request_reservation
      INTO selected_key_id, selected_funding_account_id, selected_grant_id, selected_reservation
      FROM unnest(p_key_ids) WITH ORDINALITY requested(key_id, requested_order)
      JOIN provider_credentials c
        ON c.provider_slug = p_provider_slug AND c.key_id = requested.key_id AND c.enabled = TRUE
      JOIN provider_funding_accounts a
        ON a.provider_slug = c.provider_slug AND a.funding_account_id = c.funding_account_id AND a.enabled = TRUE
      JOIN provider_capacity_grants g
        ON g.provider_slug = a.provider_slug AND g.funding_account_id = a.funding_account_id
       AND g.unit = p_unit AND g.enabled = TRUE
       AND g.starts_at <= p_now AND (g.expires_at IS NULL OR g.expires_at > p_now)
       AND (g.freshness_seconds IS NULL OR g.observed_at + make_interval(secs => g.freshness_seconds) > p_now)
       AND g.granted_amount - g.consumed_amount - g.reserved_amount >= g.max_request_reservation
     ORDER BY CASE g.capacity_category
                  WHEN 'free_trial' THEN 1
                  WHEN 'promotional' THEN 2
                  WHEN 'prepaid' THEN 3
                  WHEN 'paid' THEN 4
                  ELSE 5
              END,
              requested.requested_order,
              g.expires_at NULLS LAST,
              g.grant_id
     FOR UPDATE OF g SKIP LOCKED
     LIMIT 1;

    IF NOT FOUND THEN
        RETURN;
    END IF;

    UPDATE provider_capacity_grants
       SET reserved_amount = reserved_amount + selected_reservation,
           updated_at = p_now
     WHERE provider_capacity_grants.grant_id = selected_grant_id;
    INSERT INTO provider_capacity_reservations (
        reservation_id, request_id, attempt_index, provider_slug, key_id,
        funding_account_id, grant_id, unit, reserved_amount, state, created_at
    ) VALUES (
        p_reservation_id, p_request_id, p_attempt_index, p_provider_slug,
        selected_key_id, selected_funding_account_id, selected_grant_id,
        p_unit, selected_reservation, 'reserved', p_now
    );
    RETURN QUERY SELECT selected_key_id, selected_funding_account_id,
                        selected_grant_id, selected_reservation;
END;
$$;

CREATE FUNCTION kaana_settle_provider_capacity(
    p_reservation_id UUID,
    p_settled_amount NUMERIC,
    p_source TEXT,
    p_now TIMESTAMPTZ
) RETURNS BOOLEAN
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
    reservation provider_capacity_reservations%ROWTYPE;
BEGIN
    IF p_reservation_id IS NULL OR p_settled_amount < 0 OR
       p_source NOT IN ('provider_reported', 'rate_card') OR p_now IS NULL THEN
        RAISE EXCEPTION 'invalid provider capacity settlement';
    END IF;
    SELECT * INTO reservation FROM provider_capacity_reservations
     WHERE reservation_id = p_reservation_id FOR UPDATE;
    IF NOT FOUND THEN
        RETURN FALSE;
    END IF;
    IF reservation.state <> 'reserved' THEN
        IF reservation.state = 'settled' AND reservation.settled_amount = p_settled_amount AND
           reservation.settlement_source = p_source THEN
            RETURN TRUE;
        END IF;
        RAISE EXCEPTION 'provider capacity settlement conflict';
    END IF;
    UPDATE provider_capacity_grants
       SET reserved_amount = reserved_amount - reservation.reserved_amount,
           consumed_amount = consumed_amount + p_settled_amount,
           updated_at = p_now
     WHERE provider_capacity_grants.grant_id = reservation.grant_id
       AND reserved_amount >= reservation.reserved_amount;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'provider capacity grant reservation drift';
    END IF;
    UPDATE provider_capacity_reservations
       SET state = 'settled', settled_amount = p_settled_amount,
           settlement_source = p_source, settled_at = p_now
     WHERE reservation_id = p_reservation_id;
    RETURN TRUE;
END;
$$;

CREATE FUNCTION kaana_release_provider_capacity(
    p_reservation_id UUID,
    p_now TIMESTAMPTZ
) RETURNS BOOLEAN
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
    reservation provider_capacity_reservations%ROWTYPE;
BEGIN
    IF p_reservation_id IS NULL OR p_now IS NULL THEN
        RAISE EXCEPTION 'invalid provider capacity release';
    END IF;
    SELECT * INTO reservation FROM provider_capacity_reservations
     WHERE reservation_id = p_reservation_id FOR UPDATE;
    IF NOT FOUND THEN
        RETURN FALSE;
    END IF;
    IF reservation.state <> 'reserved' THEN
        IF reservation.state = 'released' THEN
            RETURN TRUE;
        END IF;
        RAISE EXCEPTION 'provider capacity release conflict';
    END IF;
    UPDATE provider_capacity_grants
       SET reserved_amount = reserved_amount - reservation.reserved_amount,
           updated_at = p_now
     WHERE provider_capacity_grants.grant_id = reservation.grant_id
       AND reserved_amount >= reservation.reserved_amount;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'provider capacity grant reservation drift';
    END IF;
    UPDATE provider_capacity_reservations
       SET state = 'released', settled_amount = 0,
           settlement_source = 'released', settled_at = p_now
     WHERE reservation_id = p_reservation_id;
    RETURN TRUE;
END;
$$;

REVOKE ALL ON provider_funding_accounts, provider_capacity_grants,
    provider_capacity_reservations, provider_cost_events,
    provider_balance_observations, provider_reconciliation_runs,
    provider_rate_card_versions, provider_rate_card_rates,
    provider_usage_rollups FROM PUBLIC;
REVOKE ALL ON provider_credential_economics FROM PUBLIC;

GRANT SELECT ON provider_funding_accounts, provider_capacity_grants,
    provider_capacity_reservations, provider_cost_events,
    provider_balance_observations, provider_reconciliation_runs,
    provider_rate_card_versions, provider_rate_card_rates,
    provider_usage_rollups, provider_credential_economics TO kaana_credential_admin;

REVOKE ALL ON FUNCTION kaana_reserve_provider_capacity(UUID, TEXT, INTEGER, TEXT, TEXT[], TEXT, TIMESTAMPTZ) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_settle_provider_capacity(UUID, NUMERIC, TEXT, TIMESTAMPTZ) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_release_provider_capacity(UUID, TIMESTAMPTZ) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION kaana_reserve_provider_capacity(UUID, TEXT, INTEGER, TEXT, TEXT[], TEXT, TIMESTAMPTZ) TO kaana_runtime;
GRANT EXECUTE ON FUNCTION kaana_settle_provider_capacity(UUID, NUMERIC, TEXT, TIMESTAMPTZ) TO kaana_runtime;
GRANT EXECUTE ON FUNCTION kaana_release_provider_capacity(UUID, TIMESTAMPTZ) TO kaana_runtime;
