-- A disabled key that will never serve again keeps no secret.
--
-- Until now a key left the pool only by `disable`, which keeps its KMS
-- ciphertext forever. Temporary states never used it (a spent or refused key
-- is RETIRED with a return time, a failing deployment is the breaker's), so a
-- disabled row is a dead or superseded key whose ciphertext has no use and is
-- still a secret someone with database and KMS access could recover.
--
-- Destruction removes the ciphertext and keeps the opaque identity: cost
-- events, attempts, metadata and audit rows name the exact key that served,
-- and that history must keep resolving. A destroyed identity is final: no
-- put, import, rekey or rotation may write to it again.
--
-- The database cannot see a provider, so it records which proof the admin
-- task established (the provider rejected the secret, or the secret is
-- byte-identical to an enabled key of the same provider) and re-checks every
-- fact it CAN see inside the same locked transaction.

ALTER TABLE public.provider_credentials
    ALTER COLUMN encrypted_secret DROP NOT NULL,
    ADD COLUMN destroyed_at TIMESTAMPTZ,
    ADD CONSTRAINT provider_credentials_secret_unless_destroyed CHECK (
        (destroyed_at IS NULL AND encrypted_secret IS NOT NULL)
        OR (destroyed_at IS NOT NULL AND encrypted_secret IS NULL AND enabled = FALSE)
    );

CREATE FUNCTION public.kaana_refuse_destroyed_provider_credential_write() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path = pg_catalog
AS $kaana_refuse_destroyed$
BEGIN
    IF OLD.destroyed_at IS NOT NULL THEN
        RAISE EXCEPTION 'provider credential %/% was destroyed; its identity is never written again',
            OLD.provider_slug, OLD.key_id;
    END IF;
    RETURN NEW;
END
$kaana_refuse_destroyed$;

CREATE TRIGGER provider_credential_destroyed_is_final
BEFORE UPDATE ON public.provider_credentials
FOR EACH ROW EXECUTE FUNCTION public.kaana_refuse_destroyed_provider_credential_write();

ALTER TABLE public.provider_credential_audit
    DROP CONSTRAINT provider_credential_audit_action_check;

ALTER TABLE public.provider_credential_audit
    ADD CONSTRAINT provider_credential_audit_action_check
    CHECK (action IN (
        'put',
        'disable',
        'rekey_disable',
        'rekey_create',
        'deduplicate_disable',
        'destroy'
    ));

CREATE TABLE public.provider_credential_destructions (
    operation_id TEXT PRIMARY KEY
        CHECK (operation_id ~ '^kcd_[0-9a-f]{32}$'),
    provider_slug TEXT NOT NULL,
    key_id TEXT NOT NULL,
    proof TEXT NOT NULL
        CHECK (proof IN ('provider_rejected', 'duplicate_of_enabled')),
    duplicate_of_key_id TEXT,
    operation_actor TEXT NOT NULL
        CHECK (
            length(operation_actor) BETWEEN 1 AND 256
            AND operation_actor = btrim(operation_actor)
            AND operation_actor !~ E'[\r\n]'
        ),
    database_actor TEXT NOT NULL,
    destroyed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- A ledger by value, like provider_credential_audit: no foreign key, so
    -- the record outlives the credential row once nothing else names it.
    UNIQUE (provider_slug, key_id),
    CHECK ((proof = 'duplicate_of_enabled') = (duplicate_of_key_id IS NOT NULL)),
    CHECK (duplicate_of_key_id IS NULL OR duplicate_of_key_id <> key_id)
);

CREATE OR REPLACE VIEW public.provider_credential_metadata AS
SELECT provider_slug, key_id, kms_key_arn, key_class, budget_usd,
       position, enabled, created_at, updated_at, destroyed_at
FROM public.provider_credentials;

-- Prepare returns one 'candidate' row (the disabled key) and one 'reference'
-- row per enabled key of the same provider, whose ciphertexts the admin task
-- compares in memory. The advisory lock and the table lock are the ones every
-- credential admin operation takes, so no put, disable or rekey of this
-- provider can interleave before complete runs in the same transaction.
CREATE FUNCTION public.kaana_prepare_provider_credential_destruction(
    p_operation_id TEXT,
    p_provider_slug TEXT,
    p_key_id TEXT,
    p_operation_actor TEXT
) RETURNS TABLE(
    outcome_state TEXT,
    stored_proof TEXT,
    stored_duplicate_of_key_id TEXT,
    row_role TEXT,
    row_key_id TEXT,
    row_encrypted_secret BYTEA,
    row_kms_key_arn TEXT
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_destroy_prepare$
DECLARE
    existing public.provider_credential_destructions%ROWTYPE;
    candidate public.provider_credentials%ROWTYPE;
BEGIN
    IF p_operation_id IS NULL OR p_operation_id !~ '^kcd_[0-9a-f]{32}$'
       OR p_provider_slug IS NULL OR p_provider_slug !~ '^[a-z0-9]+([._-][a-z0-9]+)*$'
       OR p_key_id IS NULL OR p_key_id !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
       OR p_operation_actor IS NULL
       OR length(p_operation_actor) NOT BETWEEN 1 AND 256
       OR p_operation_actor <> btrim(p_operation_actor)
       OR p_operation_actor ~ E'[\r\n]' THEN
        RAISE EXCEPTION 'provider credential destruction request is invalid';
    END IF;

    PERFORM pg_advisory_xact_lock(hashtextextended(
        'kaana:provider-credential-admin:' || p_provider_slug,
        0
    ));
    LOCK TABLE public.provider_credentials IN SHARE ROW EXCLUSIVE MODE;

    SELECT * INTO existing
    FROM public.provider_credential_destructions AS stored
    WHERE stored.operation_id = p_operation_id;
    IF FOUND THEN
        IF existing.provider_slug = p_provider_slug AND existing.key_id = p_key_id THEN
            RETURN QUERY SELECT 'replayed'::TEXT, existing.proof, existing.duplicate_of_key_id,
                NULL::TEXT, NULL::TEXT, NULL::BYTEA, NULL::TEXT;
        ELSE
            RETURN QUERY SELECT 'conflict'::TEXT, NULL::TEXT, NULL::TEXT,
                NULL::TEXT, NULL::TEXT, NULL::BYTEA, NULL::TEXT;
        END IF;
        RETURN;
    END IF;

    SELECT * INTO candidate
    FROM public.provider_credentials AS stored
    WHERE stored.provider_slug = p_provider_slug
      AND stored.key_id = p_key_id;
    IF NOT FOUND THEN
        RETURN QUERY SELECT 'source_unavailable'::TEXT, NULL::TEXT, NULL::TEXT,
            NULL::TEXT, NULL::TEXT, NULL::BYTEA, NULL::TEXT;
        RETURN;
    END IF;
    IF candidate.destroyed_at IS NOT NULL THEN
        RETURN QUERY SELECT 'already_destroyed'::TEXT, NULL::TEXT, NULL::TEXT,
            NULL::TEXT, NULL::TEXT, NULL::BYTEA, NULL::TEXT;
        RETURN;
    END IF;
    IF candidate.enabled THEN
        RETURN QUERY SELECT 'enabled'::TEXT, NULL::TEXT, NULL::TEXT,
            NULL::TEXT, NULL::TEXT, NULL::BYTEA, NULL::TEXT;
        RETURN;
    END IF;
    IF EXISTS (
        SELECT 1 FROM public.provider_deployment_credential_bindings AS pin
        WHERE pin.provider_slug = p_provider_slug AND pin.key_id = p_key_id
    ) THEN
        RETURN QUERY SELECT 'pinned'::TEXT, NULL::TEXT, NULL::TEXT,
            NULL::TEXT, NULL::TEXT, NULL::BYTEA, NULL::TEXT;
        RETURN;
    END IF;

    RETURN QUERY SELECT 'execute'::TEXT, NULL::TEXT, NULL::TEXT,
        'candidate'::TEXT, candidate.key_id, candidate.encrypted_secret, candidate.kms_key_arn;
    RETURN QUERY SELECT 'execute'::TEXT, NULL::TEXT, NULL::TEXT,
        'reference'::TEXT, reference.key_id, reference.encrypted_secret, reference.kms_key_arn
    FROM public.provider_credentials AS reference
    WHERE reference.provider_slug = p_provider_slug
      AND reference.enabled = TRUE
    ORDER BY reference.key_id;
END
$kaana_destroy_prepare$;

CREATE FUNCTION public.kaana_complete_provider_credential_destruction(
    p_operation_id TEXT,
    p_provider_slug TEXT,
    p_key_id TEXT,
    p_operation_actor TEXT,
    p_proof TEXT,
    p_duplicate_of_key_id TEXT
) RETURNS TABLE(outcome_state TEXT, stored_proof TEXT, stored_duplicate_of_key_id TEXT)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_destroy_complete$
DECLARE
    existing public.provider_credential_destructions%ROWTYPE;
    changed_rows INTEGER;
BEGIN
    IF p_operation_id IS NULL OR p_operation_id !~ '^kcd_[0-9a-f]{32}$'
       OR p_provider_slug IS NULL OR p_provider_slug !~ '^[a-z0-9]+([._-][a-z0-9]+)*$'
       OR p_key_id IS NULL OR p_key_id !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
       OR p_operation_actor IS NULL
       OR length(p_operation_actor) NOT BETWEEN 1 AND 256
       OR p_operation_actor <> btrim(p_operation_actor)
       OR p_operation_actor ~ E'[\r\n]'
       OR p_proof IS NULL OR p_proof NOT IN ('provider_rejected', 'duplicate_of_enabled')
       OR (p_proof = 'duplicate_of_enabled') <> (p_duplicate_of_key_id IS NOT NULL)
       OR p_duplicate_of_key_id = p_key_id THEN
        RAISE EXCEPTION 'provider credential destruction completion is invalid';
    END IF;

    PERFORM pg_advisory_xact_lock(hashtextextended(
        'kaana:provider-credential-admin:' || p_provider_slug,
        0
    ));
    LOCK TABLE public.provider_credentials IN SHARE ROW EXCLUSIVE MODE;

    SELECT * INTO existing
    FROM public.provider_credential_destructions AS stored
    WHERE stored.operation_id = p_operation_id;
    IF FOUND THEN
        IF existing.provider_slug = p_provider_slug AND existing.key_id = p_key_id THEN
            RETURN QUERY SELECT 'replayed'::TEXT, existing.proof, existing.duplicate_of_key_id;
        ELSE
            RETURN QUERY SELECT 'conflict'::TEXT, NULL::TEXT, NULL::TEXT;
        END IF;
        RETURN;
    END IF;

    IF EXISTS (
        SELECT 1 FROM public.provider_deployment_credential_bindings AS pin
        WHERE pin.provider_slug = p_provider_slug AND pin.key_id = p_key_id
    ) THEN
        RETURN QUERY SELECT 'pinned'::TEXT, NULL::TEXT, NULL::TEXT;
        RETURN;
    END IF;
    IF p_duplicate_of_key_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM public.provider_credentials AS reference
        WHERE reference.provider_slug = p_provider_slug
          AND reference.key_id = p_duplicate_of_key_id
          AND reference.enabled = TRUE
    ) THEN
        RETURN QUERY SELECT 'source_unavailable'::TEXT, NULL::TEXT, NULL::TEXT;
        RETURN;
    END IF;

    UPDATE public.provider_credentials
    SET encrypted_secret = NULL,
        destroyed_at = NOW(),
        updated_at = NOW()
    WHERE provider_slug = p_provider_slug
      AND key_id = p_key_id
      AND enabled = FALSE
      AND destroyed_at IS NULL;
    GET DIAGNOSTICS changed_rows = ROW_COUNT;
    IF changed_rows <> 1 THEN
        RETURN QUERY SELECT 'source_unavailable'::TEXT, NULL::TEXT, NULL::TEXT;
        RETURN;
    END IF;

    INSERT INTO public.provider_credential_destructions (
        operation_id, provider_slug, key_id, proof, duplicate_of_key_id,
        operation_actor, database_actor
    ) VALUES (
        p_operation_id, p_provider_slug, p_key_id, p_proof, p_duplicate_of_key_id,
        p_operation_actor, session_user
    );
    INSERT INTO public.provider_credential_audit (
        provider_slug, key_id, action, operation_actor, database_actor
    ) VALUES (
        p_provider_slug, p_key_id, 'destroy', p_operation_actor, session_user
    );

    RETURN QUERY SELECT 'applied'::TEXT, p_proof, p_duplicate_of_key_id;
END
$kaana_destroy_complete$;

REVOKE ALL ON public.provider_credential_destructions FROM PUBLIC;
REVOKE ALL ON FUNCTION public.kaana_refuse_destroyed_provider_credential_write() FROM PUBLIC;
REVOKE ALL ON FUNCTION public.kaana_prepare_provider_credential_destruction(TEXT, TEXT, TEXT, TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.kaana_complete_provider_credential_destruction(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT) FROM PUBLIC;
GRANT SELECT ON public.provider_credential_destructions TO kaana_credential_admin;
GRANT EXECUTE ON FUNCTION public.kaana_prepare_provider_credential_destruction(TEXT, TEXT, TEXT, TEXT) TO kaana_credential_admin;
GRANT EXECUTE ON FUNCTION public.kaana_complete_provider_credential_destruction(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT) TO kaana_credential_admin;
