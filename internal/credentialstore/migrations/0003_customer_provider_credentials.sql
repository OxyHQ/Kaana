DO $kaana_customer_roles$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'kaana_customer_credential_control') THEN
        RAISE EXCEPTION 'required database role kaana_customer_credential_control does not exist';
    END IF;
    EXECUTE format(
        'GRANT CONNECT ON DATABASE %I TO kaana_customer_credential_control',
        current_database()
    );
END
$kaana_customer_roles$;

CREATE TABLE customer_provider_credentials (
    credential_handle TEXT PRIMARY KEY
        CHECK (credential_handle ~ '^kcred_[a-z2-7]{26}$'),
    provider_slug TEXT NOT NULL
        CHECK (provider_slug ~ '^[a-z0-9]+([._-][a-z0-9]+)*$'),
    owner_account_id TEXT NOT NULL
        CHECK (owner_account_id ~ '^[A-Za-z0-9_-]{1,64}$'),
    connection_id TEXT NOT NULL
        CHECK (connection_id ~ '^[A-Za-z0-9_-]{1,128}$'),
    environment TEXT NOT NULL
        CHECK (environment IN ('development', 'staging', 'production')),
    encrypted_secret BYTEA NOT NULL
        CHECK (octet_length(encrypted_secret) > 0),
    kms_key_arn TEXT NOT NULL
        CHECK (kms_key_arn ~ '^arn:[^:]+:kms:[^:]+:[0-9]+:key/[A-Za-z0-9-]+$'),
    revision BIGINT NOT NULL DEFAULT 1
        CHECK (revision > 0),
    status TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'revoked')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    rotated_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    UNIQUE (owner_account_id, connection_id, environment),
    CHECK (
        (status = 'active' AND revoked_at IS NULL)
        OR (status = 'revoked' AND revoked_at IS NOT NULL)
    )
);

CREATE TABLE customer_provider_credential_audit (
    audit_id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    credential_handle TEXT NOT NULL,
    provider_slug TEXT NOT NULL,
    owner_account_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    environment TEXT NOT NULL,
    revision BIGINT NOT NULL CHECK (revision > 0),
    action TEXT NOT NULL CHECK (action IN ('create', 'rotate', 'revoke')),
    operation_actor TEXT NOT NULL CHECK (length(operation_actor) BETWEEN 1 AND 256),
    database_actor TEXT NOT NULL DEFAULT CURRENT_USER,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX customer_provider_credential_audit_handle_time_idx
    ON customer_provider_credential_audit (credential_handle, occurred_at DESC);

CREATE VIEW customer_provider_credential_metadata AS
SELECT credential_handle, provider_slug, owner_account_id, connection_id,
       environment, revision, status, kms_key_arn, created_at, rotated_at, revoked_at
FROM customer_provider_credentials;

CREATE FUNCTION kaana_create_customer_provider_credential(
    p_credential_handle TEXT,
    p_provider_slug TEXT,
    p_owner_account_id TEXT,
    p_connection_id TEXT,
    p_environment TEXT,
    p_encrypted_secret BYTEA,
    p_kms_key_arn TEXT,
    p_operation_actor TEXT
) RETURNS TABLE(was_created BOOLEAN, resolved_handle TEXT, resolved_revision BIGINT)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_customer_create$
DECLARE
    inserted_rows INTEGER;
BEGIN
    IF p_operation_actor IS NULL
       OR length(p_operation_actor) NOT BETWEEN 1 AND 256
       OR p_operation_actor <> btrim(p_operation_actor) THEN
        RAISE EXCEPTION 'operation actor is invalid';
    END IF;

    INSERT INTO public.customer_provider_credentials (
        credential_handle, provider_slug, owner_account_id, connection_id,
        environment, encrypted_secret, kms_key_arn, revision, status
    ) VALUES (
        p_credential_handle, p_provider_slug, p_owner_account_id, p_connection_id,
        p_environment, p_encrypted_secret, p_kms_key_arn, 1, 'active'
    )
    ON CONFLICT (owner_account_id, connection_id, environment) DO NOTHING;
    GET DIAGNOSTICS inserted_rows = ROW_COUNT;

    IF inserted_rows = 1 THEN
        INSERT INTO public.customer_provider_credential_audit (
            credential_handle, provider_slug, owner_account_id, connection_id,
            environment, revision, action, operation_actor, database_actor
        ) VALUES (
            p_credential_handle, p_provider_slug, p_owner_account_id, p_connection_id,
            p_environment, 1, 'create', p_operation_actor, session_user
        );
        RETURN QUERY SELECT TRUE, p_credential_handle, 1::BIGINT;
        RETURN;
    END IF;

    RETURN QUERY
    SELECT FALSE, stored.credential_handle, stored.revision
    FROM public.customer_provider_credentials AS stored
    WHERE stored.provider_slug = p_provider_slug
      AND stored.owner_account_id = p_owner_account_id
      AND stored.connection_id = p_connection_id
      AND stored.environment = p_environment;

    IF NOT FOUND THEN
        RETURN QUERY SELECT FALSE, NULL::TEXT, NULL::BIGINT;
    END IF;
END
$kaana_customer_create$;

CREATE FUNCTION kaana_rotate_customer_provider_credential(
    p_credential_handle TEXT,
    p_provider_slug TEXT,
    p_owner_account_id TEXT,
    p_connection_id TEXT,
    p_environment TEXT,
    p_expected_revision BIGINT,
    p_encrypted_secret BYTEA,
    p_kms_key_arn TEXT,
    p_operation_actor TEXT
) RETURNS BOOLEAN
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_customer_rotate$
DECLARE
    changed_rows INTEGER;
BEGIN
    IF p_operation_actor IS NULL
       OR length(p_operation_actor) NOT BETWEEN 1 AND 256
       OR p_operation_actor <> btrim(p_operation_actor) THEN
        RAISE EXCEPTION 'operation actor is invalid';
    END IF;

    UPDATE public.customer_provider_credentials
    SET encrypted_secret = p_encrypted_secret,
        kms_key_arn = p_kms_key_arn,
        revision = p_expected_revision + 1,
        rotated_at = NOW()
    WHERE credential_handle = p_credential_handle
      AND provider_slug = p_provider_slug
      AND owner_account_id = p_owner_account_id
      AND connection_id = p_connection_id
      AND environment = p_environment
      AND revision = p_expected_revision
      AND status = 'active';
    GET DIAGNOSTICS changed_rows = ROW_COUNT;

    IF changed_rows = 1 THEN
        INSERT INTO public.customer_provider_credential_audit (
            credential_handle, provider_slug, owner_account_id, connection_id,
            environment, revision, action, operation_actor, database_actor
        ) VALUES (
            p_credential_handle, p_provider_slug, p_owner_account_id, p_connection_id,
            p_environment, p_expected_revision + 1, 'rotate', p_operation_actor, session_user
        );
    END IF;
    RETURN changed_rows = 1;
END
$kaana_customer_rotate$;

CREATE FUNCTION kaana_revoke_customer_provider_credential(
    p_credential_handle TEXT,
    p_provider_slug TEXT,
    p_owner_account_id TEXT,
    p_connection_id TEXT,
    p_environment TEXT,
    p_expected_revision BIGINT,
    p_operation_actor TEXT
) RETURNS BOOLEAN
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_customer_revoke$
DECLARE
    changed_rows INTEGER;
BEGIN
    IF p_operation_actor IS NULL
       OR length(p_operation_actor) NOT BETWEEN 1 AND 256
       OR p_operation_actor <> btrim(p_operation_actor) THEN
        RAISE EXCEPTION 'operation actor is invalid';
    END IF;

    UPDATE public.customer_provider_credentials
    SET status = 'revoked', revision = p_expected_revision + 1, revoked_at = NOW()
    WHERE credential_handle = p_credential_handle
      AND provider_slug = p_provider_slug
      AND owner_account_id = p_owner_account_id
      AND connection_id = p_connection_id
      AND environment = p_environment
      AND revision = p_expected_revision
      AND status = 'active';
    GET DIAGNOSTICS changed_rows = ROW_COUNT;

    IF changed_rows = 1 THEN
        INSERT INTO public.customer_provider_credential_audit (
            credential_handle, provider_slug, owner_account_id, connection_id,
            environment, revision, action, operation_actor, database_actor
        ) VALUES (
            p_credential_handle, p_provider_slug, p_owner_account_id, p_connection_id,
            p_environment, p_expected_revision + 1, 'revoke', p_operation_actor, session_user
        );
    END IF;
    RETURN changed_rows = 1;
END
$kaana_customer_revoke$;

CREATE FUNCTION kaana_get_active_customer_provider_credential(
    p_credential_handle TEXT,
    p_provider_slug TEXT,
    p_owner_account_id TEXT,
    p_connection_id TEXT,
    p_environment TEXT,
    p_revision BIGINT
) RETURNS TABLE(encrypted_secret BYTEA, kms_key_arn TEXT, resolved_revision BIGINT)
LANGUAGE sql
SECURITY DEFINER
STABLE
SET search_path = pg_catalog, public
AS $kaana_customer_get$
    SELECT stored.encrypted_secret, stored.kms_key_arn, stored.revision
    FROM public.customer_provider_credentials AS stored
    WHERE stored.credential_handle = p_credential_handle
      AND stored.provider_slug = p_provider_slug
      AND stored.owner_account_id = p_owner_account_id
      AND stored.connection_id = p_connection_id
      AND stored.environment = p_environment
      AND stored.revision = p_revision
      AND stored.status = 'active'
$kaana_customer_get$;

REVOKE ALL ON customer_provider_credentials FROM PUBLIC;
REVOKE ALL ON customer_provider_credential_audit FROM PUBLIC;
REVOKE ALL ON customer_provider_credential_metadata FROM PUBLIC;
REVOKE ALL ON SEQUENCE customer_provider_credential_audit_audit_id_seq FROM PUBLIC;
REVOKE ALL ON customer_provider_credentials FROM kaana_runtime, kaana_credential_admin, kaana_customer_credential_control;
REVOKE ALL ON customer_provider_credential_audit FROM kaana_runtime, kaana_credential_admin, kaana_customer_credential_control;
REVOKE ALL ON customer_provider_credential_metadata FROM kaana_runtime, kaana_credential_admin, kaana_customer_credential_control;
REVOKE ALL ON SEQUENCE customer_provider_credential_audit_audit_id_seq FROM kaana_runtime, kaana_credential_admin, kaana_customer_credential_control;
REVOKE ALL ON FUNCTION kaana_create_customer_provider_credential(TEXT, TEXT, TEXT, TEXT, TEXT, BYTEA, TEXT, TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_rotate_customer_provider_credential(TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, BYTEA, TEXT, TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_revoke_customer_provider_credential(TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_get_active_customer_provider_credential(TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT) FROM PUBLIC;

GRANT USAGE ON SCHEMA public TO kaana_customer_credential_control;
GRANT EXECUTE ON FUNCTION kaana_create_customer_provider_credential(TEXT, TEXT, TEXT, TEXT, TEXT, BYTEA, TEXT, TEXT) TO kaana_customer_credential_control;
GRANT EXECUTE ON FUNCTION kaana_rotate_customer_provider_credential(TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, BYTEA, TEXT, TEXT) TO kaana_customer_credential_control;
GRANT EXECUTE ON FUNCTION kaana_revoke_customer_provider_credential(TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, TEXT) TO kaana_customer_credential_control;
GRANT EXECUTE ON FUNCTION kaana_get_active_customer_provider_credential(TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT) TO kaana_runtime;
GRANT SELECT ON customer_provider_credential_metadata, customer_provider_credential_audit TO kaana_credential_admin;
