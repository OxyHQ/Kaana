CREATE TABLE customer_provider_credential_validations (
    operation_id TEXT PRIMARY KEY
        CHECK (operation_id ~ '^[A-Za-z0-9_-]{1,128}$'),
    application_id TEXT NOT NULL
        CHECK (application_id ~ '^[A-Za-z0-9_-]{1,64}$'),
    provider_slug TEXT NOT NULL
        CHECK (provider_slug ~ '^[a-z0-9]+([._-][a-z0-9]+)*$'),
    owner_account_id TEXT NOT NULL
        CHECK (owner_account_id ~ '^[A-Za-z0-9_-]{1,64}$'),
    connection_id TEXT NOT NULL
        CHECK (connection_id ~ '^[A-Za-z0-9_-]{1,128}$'),
    environment TEXT NOT NULL
        CHECK (environment IN ('development', 'staging', 'production')),
    credential_handle TEXT NOT NULL
        CHECK (credential_handle ~ '^kcred_[a-z2-7]{26}$'),
    credential_revision BIGINT NOT NULL
        CHECK (credential_revision > 0 AND credential_revision <= 9007199254740991),
    deployment_id TEXT NOT NULL
        CHECK (
            length(deployment_id) BETWEEN 1 AND 128
            AND deployment_id = btrim(deployment_id)
            AND deployment_id !~ E'[\r\n]'
        ),
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'running', 'valid', 'invalid', 'inconclusive')),
    failure_code TEXT
        CHECK (failure_code IN ('unauthorized', 'forbidden', 'not_found', 'rate_limited', 'network', 'unknown')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    lease_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    CHECK (
        (state = 'pending' AND failure_code IS NULL AND lease_until IS NULL AND completed_at IS NULL)
        OR (state = 'running' AND failure_code IS NULL AND lease_until IS NOT NULL AND completed_at IS NULL)
        OR (state = 'valid' AND failure_code IS NULL AND lease_until IS NULL AND completed_at IS NOT NULL)
        OR (state = 'invalid' AND failure_code = 'unauthorized' AND lease_until IS NULL AND completed_at IS NOT NULL)
        OR (state = 'inconclusive' AND failure_code IS NOT NULL AND lease_until IS NULL AND completed_at IS NOT NULL)
    )
);

CREATE INDEX customer_provider_credential_validations_connection_generation_idx
    ON customer_provider_credential_validations (
        connection_id, credential_handle, credential_revision, created_at DESC
    );

CREATE FUNCTION kaana_claim_customer_provider_credential_validation(
    p_operation_id TEXT,
    p_application_id TEXT,
    p_provider_slug TEXT,
    p_owner_account_id TEXT,
    p_connection_id TEXT,
    p_environment TEXT,
    p_credential_handle TEXT,
    p_credential_revision BIGINT,
    p_deployment_id TEXT
) RETURNS TABLE(outcome_state TEXT, outcome_failure_code TEXT, outcome_lease_generation BIGINT)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_customer_validation_claim$
DECLARE
    inserted_rows INTEGER;
    stored_state TEXT;
    stored_failure_code TEXT;
    stored_lease_until TIMESTAMPTZ;
    stored_attempt_count INTEGER;
BEGIN
    INSERT INTO public.customer_provider_credential_validations (
        operation_id, application_id, provider_slug, owner_account_id,
        connection_id, environment, credential_handle, credential_revision,
        deployment_id
    ) VALUES (
        p_operation_id, p_application_id, p_provider_slug, p_owner_account_id,
        p_connection_id, p_environment, p_credential_handle,
        p_credential_revision, p_deployment_id
    )
    ON CONFLICT (operation_id) DO NOTHING;
    GET DIAGNOSTICS inserted_rows = ROW_COUNT;

    SELECT stored.state, stored.failure_code, stored.lease_until, stored.attempt_count
      INTO stored_state, stored_failure_code, stored_lease_until, stored_attempt_count
    FROM public.customer_provider_credential_validations AS stored
    WHERE stored.operation_id = p_operation_id
      AND stored.application_id = p_application_id
      AND stored.provider_slug = p_provider_slug
      AND stored.owner_account_id = p_owner_account_id
      AND stored.connection_id = p_connection_id
      AND stored.environment = p_environment
      AND stored.credential_handle = p_credential_handle
      AND stored.credential_revision = p_credential_revision
      AND stored.deployment_id = p_deployment_id
    FOR UPDATE;

    IF NOT FOUND THEN
        RETURN QUERY SELECT 'conflict'::TEXT, NULL::TEXT, NULL::BIGINT;
        RETURN;
    END IF;

    IF stored_state IN ('valid', 'invalid', 'inconclusive') THEN
        RETURN QUERY SELECT stored_state, stored_failure_code, NULL::BIGINT;
        RETURN;
    END IF;

    IF stored_state = 'running' AND stored_lease_until > NOW() THEN
        RETURN QUERY SELECT 'pending'::TEXT, NULL::TEXT, NULL::BIGINT;
        RETURN;
    END IF;

    UPDATE public.customer_provider_credential_validations
    SET state = 'running',
        attempt_count = attempt_count + 1,
        lease_until = NOW() + INTERVAL '60 seconds'
    WHERE operation_id = p_operation_id
    RETURNING attempt_count INTO stored_attempt_count;

    RETURN QUERY SELECT 'execute'::TEXT, NULL::TEXT, stored_attempt_count::BIGINT;
END
$kaana_customer_validation_claim$;

CREATE FUNCTION kaana_complete_customer_provider_credential_validation(
    p_operation_id TEXT,
    p_application_id TEXT,
    p_provider_slug TEXT,
    p_owner_account_id TEXT,
    p_connection_id TEXT,
    p_environment TEXT,
    p_credential_handle TEXT,
    p_credential_revision BIGINT,
    p_deployment_id TEXT,
    p_lease_generation BIGINT,
    p_state TEXT,
    p_failure_code TEXT
) RETURNS BOOLEAN
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_customer_validation_complete$
DECLARE
    changed_rows INTEGER;
    already_terminal BOOLEAN;
BEGIN
    IF p_lease_generation <= 0
       OR p_lease_generation > 2147483647
       OR p_state NOT IN ('valid', 'invalid', 'inconclusive')
       OR (p_state = 'valid' AND p_failure_code IS NOT NULL)
       OR (p_state = 'invalid' AND (p_failure_code IS NULL OR p_failure_code <> 'unauthorized'))
       OR (p_state = 'inconclusive' AND (p_failure_code IS NULL OR p_failure_code NOT IN ('forbidden', 'not_found', 'rate_limited', 'network', 'unknown'))) THEN
        RAISE EXCEPTION 'validation outcome is invalid';
    END IF;

    UPDATE public.customer_provider_credential_validations
    SET state = p_state,
        failure_code = p_failure_code,
        lease_until = NULL,
        completed_at = NOW()
    WHERE operation_id = p_operation_id
      AND application_id = p_application_id
      AND provider_slug = p_provider_slug
      AND owner_account_id = p_owner_account_id
      AND connection_id = p_connection_id
      AND environment = p_environment
      AND credential_handle = p_credential_handle
      AND credential_revision = p_credential_revision
      AND deployment_id = p_deployment_id
      AND attempt_count = p_lease_generation
      AND state = 'running';
    GET DIAGNOSTICS changed_rows = ROW_COUNT;
    IF changed_rows = 1 THEN
        RETURN TRUE;
    END IF;

    SELECT EXISTS (
        SELECT 1
        FROM public.customer_provider_credential_validations AS stored
        WHERE stored.operation_id = p_operation_id
          AND stored.application_id = p_application_id
          AND stored.provider_slug = p_provider_slug
          AND stored.owner_account_id = p_owner_account_id
          AND stored.connection_id = p_connection_id
          AND stored.environment = p_environment
          AND stored.credential_handle = p_credential_handle
          AND stored.credential_revision = p_credential_revision
          AND stored.deployment_id = p_deployment_id
          AND stored.attempt_count = p_lease_generation
          AND stored.state = p_state
          AND stored.failure_code IS NOT DISTINCT FROM p_failure_code
    ) INTO already_terminal;
    RETURN already_terminal;
END
$kaana_customer_validation_complete$;

REVOKE ALL ON customer_provider_credential_validations FROM PUBLIC;
REVOKE ALL ON customer_provider_credential_validations FROM kaana_runtime, kaana_credential_admin, kaana_customer_credential_control;
REVOKE ALL ON FUNCTION kaana_claim_customer_provider_credential_validation(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_complete_customer_provider_credential_validation(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, TEXT, BIGINT, TEXT, TEXT) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION kaana_claim_customer_provider_credential_validation(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, TEXT) TO kaana_runtime;
GRANT EXECUTE ON FUNCTION kaana_complete_customer_provider_credential_validation(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, TEXT, BIGINT, TEXT, TEXT) TO kaana_runtime;
GRANT SELECT ON customer_provider_credential_validations TO kaana_credential_admin;
