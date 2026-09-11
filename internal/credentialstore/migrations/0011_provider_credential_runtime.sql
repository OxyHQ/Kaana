SET LOCAL lock_timeout = '5s';

CREATE TABLE provider_credential_runtime_state (
    provider_slug TEXT NOT NULL,
    key_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('usable', 'rejected', 'exhausted')),
    evidence_source TEXT NOT NULL CHECK (evidence_source IN ('response_status', 'provider_error', 'quota_header')),
    retired_until TIMESTAMPTZ,
    lease_until TIMESTAMPTZ,
    observed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (provider_slug, key_id),
    FOREIGN KEY (provider_slug, key_id)
        REFERENCES provider_credentials (provider_slug, key_id) ON DELETE CASCADE,
    CHECK ((state = 'usable' AND retired_until IS NULL AND lease_until IS NULL) OR
           (state <> 'usable' AND retired_until IS NOT NULL))
);

CREATE TABLE provider_credential_attempt_events (
    request_id TEXT NOT NULL,
    deployment_id TEXT NOT NULL,
    credential_attempt_index INTEGER NOT NULL CHECK (credential_attempt_index >= 0),
    provider_slug TEXT NOT NULL,
    key_id TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('accepted', 'rejected', 'exhausted', 'request_fault', 'healthy_failure', 'transport_failure')),
    evidence_source TEXT NOT NULL CHECK (evidence_source IN ('response_status', 'provider_error', 'quota_header', 'transport')),
    occurred_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (request_id, deployment_id, credential_attempt_index),
    FOREIGN KEY (provider_slug, key_id)
        REFERENCES provider_credentials (provider_slug, key_id)
);

CREATE INDEX provider_credential_attempt_events_key_time_idx
    ON provider_credential_attempt_events (provider_slug, key_id, occurred_at DESC);

CREATE FUNCTION kaana_claim_provider_credential_recovery(
    p_provider_slug TEXT, p_key_id TEXT, p_at TIMESTAMPTZ, p_lease_until TIMESTAMPTZ
) RETURNS TEXT
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog
AS $body$
DECLARE changed_rows INTEGER;
BEGIN
    IF p_provider_slug IS NULL OR p_provider_slug !~ '^[a-z0-9]+([._-][a-z0-9]+)*$'
       OR p_key_id IS NULL OR p_key_id !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
       OR p_at IS NULL OR p_lease_until IS NULL OR p_lease_until <= p_at THEN
        RAISE EXCEPTION 'credential recovery lease is invalid';
    END IF;
    UPDATE public.provider_credential_runtime_state
       SET lease_until = p_lease_until
     WHERE provider_slug = p_provider_slug AND key_id = p_key_id
       AND state <> 'usable' AND retired_until <= p_at
       AND (lease_until IS NULL OR lease_until <= p_at);
    GET DIAGNOSTICS changed_rows = ROW_COUNT;
    IF changed_rows = 1 THEN RETURN 'claimed'; END IF;
    IF EXISTS (SELECT 1 FROM public.provider_credential_runtime_state
               WHERE provider_slug = p_provider_slug AND key_id = p_key_id AND state <> 'usable') THEN
        RETURN 'busy';
    END IF;
    RETURN 'usable';
END
$body$;

CREATE FUNCTION kaana_record_provider_credential_attempt(
    p_request_id TEXT, p_deployment_id TEXT, p_attempt_index INTEGER,
    p_provider_slug TEXT, p_key_id TEXT, p_outcome TEXT, p_evidence_source TEXT,
    p_occurred_at TIMESTAMPTZ, p_retired_until TIMESTAMPTZ
) RETURNS BOOLEAN
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog
AS $body$
DECLARE existing public.provider_credential_attempt_events%ROWTYPE;
BEGIN
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

CREATE FUNCTION kaana_reset_provider_credential_runtime() RETURNS TRIGGER
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog
AS $body$
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.encrypted_secret IS DISTINCT FROM NEW.encrypted_secret THEN
        DELETE FROM public.provider_credential_runtime_state
         WHERE provider_slug = NEW.provider_slug AND key_id = NEW.key_id;
    END IF;
    RETURN NEW;
END
$body$;

CREATE TRIGGER provider_credential_runtime_reset_after_rotation
AFTER UPDATE OF encrypted_secret ON provider_credentials
FOR EACH ROW EXECUTE FUNCTION kaana_reset_provider_credential_runtime();

REVOKE ALL ON provider_credential_runtime_state, provider_credential_attempt_events FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_claim_provider_credential_recovery(TEXT, TEXT, TIMESTAMPTZ, TIMESTAMPTZ) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_record_provider_credential_attempt(TEXT, TEXT, INTEGER, TEXT, TEXT, TEXT, TEXT, TIMESTAMPTZ, TIMESTAMPTZ) FROM PUBLIC;
GRANT SELECT ON provider_credential_runtime_state, provider_credential_attempt_events TO kaana_credential_admin;
GRANT EXECUTE ON FUNCTION kaana_claim_provider_credential_recovery(TEXT, TEXT, TIMESTAMPTZ, TIMESTAMPTZ) TO kaana_runtime;
GRANT EXECUTE ON FUNCTION kaana_record_provider_credential_attempt(TEXT, TEXT, INTEGER, TEXT, TEXT, TEXT, TEXT, TIMESTAMPTZ, TIMESTAMPTZ) TO kaana_runtime;
