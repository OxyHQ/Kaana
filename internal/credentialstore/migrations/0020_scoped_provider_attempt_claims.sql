-- Additive one-operation claim; CREATE OR REPLACE preserves the existing
-- function owner and EXECUTE ACL. No runtime table permissions are added.
SET LOCAL lock_timeout = '5s';
CREATE TABLE scoped_provider_attempt_claims (
    operation_id TEXT PRIMARY KEY CHECK (length(operation_id) BETWEEN 1 AND 256),
    deployment_id TEXT NOT NULL CHECK (length(deployment_id) BETWEEN 1 AND 256),
    provider_slug TEXT NOT NULL,
    key_id TEXT NOT NULL,
    claimed_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL CHECK (expires_at > claimed_at),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (provider_slug,key_id) REFERENCES provider_credentials(provider_slug,key_id)
);
REVOKE ALL ON scoped_provider_attempt_claims FROM PUBLIC;

CREATE OR REPLACE FUNCTION kaana_record_provider_credential_attempt(
    p_request_id TEXT, p_deployment_id TEXT, p_attempt_index INTEGER,
    p_provider_slug TEXT, p_key_id TEXT, p_outcome TEXT, p_evidence_source TEXT,
    p_occurred_at TIMESTAMPTZ, p_retired_until TIMESTAMPTZ
) RETURNS BOOLEAN
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog
AS $body$
DECLARE existing public.provider_credential_attempt_events%ROWTYPE;
BEGIN
    -- This branch records only an admission claim; it never records a provider outcome.
    IF p_outcome = 'attempt_claimed' THEN
        IF p_request_id IS NULL OR length(p_request_id) NOT BETWEEN 1 AND 256
           OR p_deployment_id IS NULL OR length(p_deployment_id) NOT BETWEEN 1 AND 256
           OR p_attempt_index IS DISTINCT FROM 0 OR p_evidence_source IS DISTINCT FROM 'local_admission'
           OR p_provider_slug IS NULL OR p_provider_slug !~ '^[a-z0-9]+([._-][a-z0-9]+)*$'
           OR p_key_id IS NULL OR p_key_id !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
           OR p_occurred_at IS NULL OR p_retired_until IS NULL
           OR p_occurred_at > clock_timestamp() + interval '5 seconds'
           OR p_retired_until <= clock_timestamp()
           OR p_retired_until <= p_occurred_at
           OR p_retired_until > p_occurred_at + interval '5 minutes' THEN
            RAISE EXCEPTION 'scoped attempt claim is invalid';
        END IF;
        IF NOT EXISTS (
            SELECT 1 FROM public.provider_deployment_credential_bindings b
            JOIN public.provider_credentials c USING (provider_slug,key_id)
            WHERE b.deployment_id = p_deployment_id AND b.provider_slug = p_provider_slug
              AND b.key_id = p_key_id AND c.enabled
        ) THEN
            RAISE EXCEPTION 'scoped attempt credential binding is unavailable';
        END IF;
        INSERT INTO public.scoped_provider_attempt_claims
            (operation_id,deployment_id,provider_slug,key_id,claimed_at,expires_at)
        VALUES (p_request_id,p_deployment_id,p_provider_slug,p_key_id,p_occurred_at,p_retired_until)
        ON CONFLICT (operation_id) DO NOTHING;
        RETURN FOUND;
    END IF;
    IF p_request_id IS NULL OR length(p_request_id) NOT BETWEEN 1 AND 256
       OR p_deployment_id IS NULL OR length(p_deployment_id) NOT BETWEEN 1 AND 256
       OR p_attempt_index IS NULL OR p_attempt_index < 0
       OR p_provider_slug IS NULL OR p_provider_slug !~ '^[a-z0-9]+([._-][a-z0-9]+)*$'
       OR p_key_id IS NULL OR p_key_id !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
       OR p_outcome IS NULL OR p_outcome NOT IN ('accepted', 'rejected', 'exhausted', 'request_fault', 'healthy_failure', 'transport_failure')
       OR p_evidence_source IS NULL OR p_evidence_source NOT IN ('response_status', 'provider_error', 'quota_header', 'transport')
       OR (p_outcome = 'accepted' AND p_evidence_source <> 'response_status')
       OR (p_outcome = 'transport_failure' AND p_evidence_source <> 'transport')
       OR (p_outcome IN ('rejected', 'request_fault', 'healthy_failure') AND p_evidence_source <> 'provider_error')
       OR (p_outcome = 'exhausted' AND p_evidence_source NOT IN ('provider_error', 'quota_header'))
       OR p_occurred_at IS NULL
       OR (p_outcome IN ('rejected', 'exhausted') AND (p_retired_until IS NULL OR p_retired_until <= p_occurred_at))
       OR (p_outcome NOT IN ('rejected', 'exhausted') AND p_retired_until IS NOT NULL) THEN
        RAISE EXCEPTION 'provider credential attempt is invalid';
    END IF;
    INSERT INTO public.provider_credential_attempt_events (
        request_id, deployment_id, credential_attempt_index, provider_slug,
        key_id, outcome, evidence_source, occurred_at
    ) VALUES (
        p_request_id, p_deployment_id, p_attempt_index, p_provider_slug,
        p_key_id, p_outcome, p_evidence_source, p_occurred_at
    ) ON CONFLICT DO NOTHING;
    IF NOT FOUND THEN
        SELECT * INTO STRICT existing FROM public.provider_credential_attempt_events
         WHERE request_id = p_request_id AND deployment_id = p_deployment_id
           AND credential_attempt_index = p_attempt_index;
        IF existing.provider_slug <> p_provider_slug OR existing.key_id <> p_key_id OR
           existing.outcome <> p_outcome OR existing.evidence_source <> p_evidence_source OR existing.occurred_at <> p_occurred_at THEN
            RAISE EXCEPTION 'provider credential attempt identity conflict';
        END IF;
        RETURN FALSE;
    END IF;

    IF p_outcome IN ('rejected', 'exhausted') THEN
        INSERT INTO public.provider_credential_runtime_state (
            provider_slug, key_id, state, evidence_source, retired_until, lease_until, observed_at
        ) VALUES (p_provider_slug, p_key_id, p_outcome, p_evidence_source, p_retired_until, NULL, p_occurred_at)
        ON CONFLICT (provider_slug, key_id) DO UPDATE SET
            state = EXCLUDED.state, evidence_source = EXCLUDED.evidence_source, retired_until = EXCLUDED.retired_until,
            lease_until = NULL, observed_at = EXCLUDED.observed_at
        WHERE public.provider_credential_runtime_state.observed_at <= EXCLUDED.observed_at;
    ELSIF p_outcome = 'accepted' THEN
        INSERT INTO public.provider_credential_runtime_state (
            provider_slug, key_id, state, evidence_source, retired_until, lease_until, observed_at
        ) VALUES (p_provider_slug, p_key_id, 'usable', p_evidence_source, NULL, NULL, p_occurred_at)
        ON CONFLICT (provider_slug, key_id) DO UPDATE SET
            state = 'usable', evidence_source = EXCLUDED.evidence_source,
            retired_until = NULL, lease_until = NULL, observed_at = EXCLUDED.observed_at
        WHERE public.provider_credential_runtime_state.observed_at <= EXCLUDED.observed_at;
    END IF;
    RETURN TRUE;
END
$body$;
