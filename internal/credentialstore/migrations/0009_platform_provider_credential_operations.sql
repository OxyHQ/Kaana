CREATE TABLE platform_provider_credential_operations (
    operation_id TEXT PRIMARY KEY
        CHECK (operation_id ~ '^kpc_[0-9a-f]{32}$'),
    provider_slug TEXT NOT NULL
        CHECK (provider_slug ~ '^[a-z0-9]+([._-][a-z0-9]+)*$'),
    key_id TEXT NOT NULL
        CHECK (key_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    key_class TEXT NOT NULL
        CHECK (key_class IN ('free', 'paid')),
    position INTEGER NOT NULL CHECK (position > 0),
    operation_actor TEXT NOT NULL
        CHECK (
            length(operation_actor) BETWEEN 1 AND 256
            AND operation_actor = btrim(operation_actor)
            AND operation_actor !~ E'[\r\n]'
        ),
    secret_fingerprint BYTEA NOT NULL
        CHECK (octet_length(secret_fingerprint) = 32),
    database_actor TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome = 'applied'),
    completed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE FUNCTION kaana_put_platform_provider_credential(
    p_operation_id TEXT,
    p_provider_slug TEXT,
    p_key_id TEXT,
    p_encrypted_secret BYTEA,
    p_kms_key_arn TEXT,
    p_key_class TEXT,
    p_position INTEGER,
    p_operation_actor TEXT,
    p_secret_fingerprint BYTEA
) RETURNS TABLE(outcome_state TEXT)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_put_platform_provider_credential$
DECLARE
    existing platform_provider_credential_operations%ROWTYPE;
BEGIN
    IF p_operation_id IS NULL OR p_operation_id !~ '^kpc_[0-9a-f]{32}$'
       OR p_provider_slug IS NULL OR p_provider_slug !~ '^[a-z0-9]+([._-][a-z0-9]+)*$'
       OR p_key_id IS NULL
       OR p_key_id !~ '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
       OR p_encrypted_secret IS NULL OR octet_length(p_encrypted_secret) = 0
       OR p_kms_key_arn IS NULL
       OR p_kms_key_arn !~ '^arn:[^:]+:kms:[^:]+:[0-9]+:key/[A-Za-z0-9-]+$'
       OR p_key_class IS NULL OR p_key_class NOT IN ('free', 'paid')
       OR p_position IS NULL OR p_position < 1
       OR p_operation_actor IS NULL
       OR length(p_operation_actor) NOT BETWEEN 1 AND 256
       OR p_operation_actor <> btrim(p_operation_actor)
       OR p_operation_actor ~ E'[\r\n]'
       OR p_secret_fingerprint IS NULL OR octet_length(p_secret_fingerprint) <> 32 THEN
        RETURN QUERY SELECT 'invalid'::TEXT;
        RETURN;
    END IF;

    PERFORM pg_advisory_xact_lock(hashtextextended(
        'kaana:platform-provider-credential:' || p_provider_slug,
        0
    ));

    SELECT stored.* INTO existing
    FROM public.platform_provider_credential_operations AS stored
    WHERE stored.operation_id = p_operation_id
    FOR UPDATE;

    IF FOUND THEN
        IF existing.provider_slug = p_provider_slug
           AND existing.key_id = p_key_id
           AND existing.key_class = p_key_class
           AND existing.position = p_position
           AND existing.operation_actor = p_operation_actor
           AND existing.secret_fingerprint = p_secret_fingerprint THEN
            RETURN QUERY SELECT 'replayed'::TEXT;
        ELSE
            RETURN QUERY SELECT 'conflict'::TEXT;
        END IF;
        RETURN;
    END IF;

    INSERT INTO public.provider_credentials (
        provider_slug, key_id, encrypted_secret, kms_key_arn,
        key_class, budget_usd, position, enabled
    ) VALUES (
        p_provider_slug, p_key_id, p_encrypted_secret, p_kms_key_arn,
        p_key_class, NULL, p_position, TRUE
    )
    ON CONFLICT (provider_slug, key_id) DO UPDATE SET
        encrypted_secret = EXCLUDED.encrypted_secret,
        kms_key_arn = EXCLUDED.kms_key_arn,
        key_class = EXCLUDED.key_class,
        budget_usd = NULL,
        position = EXCLUDED.position,
        enabled = TRUE,
        updated_at = NOW();

    INSERT INTO public.provider_credential_audit (
        provider_slug, key_id, action, operation_actor, database_actor
    ) VALUES (
        p_provider_slug, p_key_id, 'put', p_operation_actor, session_user
    );

    INSERT INTO public.platform_provider_credential_operations (
        operation_id, provider_slug, key_id, key_class, position,
        operation_actor, secret_fingerprint, database_actor, outcome
    ) VALUES (
        p_operation_id, p_provider_slug, p_key_id, p_key_class, p_position,
        p_operation_actor, p_secret_fingerprint, session_user, 'applied'
    );

    RETURN QUERY SELECT 'applied'::TEXT;
END
$kaana_put_platform_provider_credential$;

REVOKE ALL ON platform_provider_credential_operations FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_put_platform_provider_credential(TEXT, TEXT, TEXT, BYTEA, TEXT, TEXT, INTEGER, TEXT, BYTEA) FROM PUBLIC;
REVOKE ALL ON provider_credentials, provider_credential_audit, platform_provider_credential_operations FROM kaana_platform_credential_control;

GRANT USAGE ON SCHEMA public TO kaana_platform_credential_control;
GRANT EXECUTE ON FUNCTION kaana_put_platform_provider_credential(TEXT, TEXT, TEXT, BYTEA, TEXT, TEXT, INTEGER, TEXT, BYTEA) TO kaana_platform_credential_control;
