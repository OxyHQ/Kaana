CREATE TABLE customer_provider_credential_operations (
    operation_id TEXT PRIMARY KEY
        CHECK (operation_id ~ '^[A-Za-z0-9_-]{1,128}$'),
    action TEXT NOT NULL
        CHECK (action IN ('create', 'rotate', 'revoke')),
    provider_slug TEXT NOT NULL
        CHECK (provider_slug ~ '^[a-z0-9]+([._-][a-z0-9]+)*$'),
    owner_account_id TEXT NOT NULL
        CHECK (owner_account_id ~ '^[A-Za-z0-9_-]{1,64}$'),
    connection_id TEXT NOT NULL
        CHECK (connection_id ~ '^[A-Za-z0-9_-]{1,128}$'),
    environment TEXT NOT NULL
        CHECK (environment IN ('development', 'staging', 'production')),
    requested_credential_handle TEXT
        CHECK (
            requested_credential_handle IS NULL
            OR requested_credential_handle ~ '^kcred_[a-z2-7]{26}$'
        ),
    expected_revision BIGINT
        CHECK (expected_revision > 0 AND expected_revision < 9007199254740991),
    secret_digest BYTEA
        CHECK (octet_length(secret_digest) = 32),
    operation_actor TEXT NOT NULL
        CHECK (
            length(operation_actor) BETWEEN 1 AND 256
            AND operation_actor = btrim(operation_actor)
            AND operation_actor !~ E'[\\r\\n]'
        ),
    outcome_status TEXT NOT NULL
        CHECK (outcome_status IN ('pending', 'applied', 'conflict')),
    resolved_credential_handle TEXT
        CHECK (
            resolved_credential_handle IS NULL
            OR resolved_credential_handle ~ '^kcred_[a-z2-7]{26}$'
        ),
    resolved_revision BIGINT
        CHECK (resolved_revision > 0 AND resolved_revision <= 9007199254740991),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    CHECK (
        (action = 'create'
            AND requested_credential_handle IS NULL
            AND expected_revision IS NULL
            AND secret_digest IS NOT NULL)
        OR
        (action = 'rotate'
            AND requested_credential_handle IS NOT NULL
            AND expected_revision IS NOT NULL
            AND secret_digest IS NOT NULL)
        OR
        (action = 'revoke'
            AND requested_credential_handle IS NOT NULL
            AND expected_revision IS NOT NULL
            AND secret_digest IS NULL)
    ),
    CHECK (
        (outcome_status = 'pending'
            AND resolved_credential_handle IS NULL
            AND resolved_revision IS NULL
            AND completed_at IS NULL)
        OR
        (outcome_status = 'applied'
            AND resolved_credential_handle IS NOT NULL
            AND resolved_revision IS NOT NULL
            AND completed_at IS NOT NULL)
        OR
        (outcome_status = 'conflict'
            AND resolved_credential_handle IS NULL
            AND resolved_revision IS NULL
            AND completed_at IS NOT NULL)
    )
);

ALTER TABLE customer_provider_credential_audit
    ADD COLUMN operation_id TEXT REFERENCES customer_provider_credential_operations(operation_id);

CREATE UNIQUE INDEX customer_provider_credential_audit_operation_idx
    ON customer_provider_credential_audit (operation_id)
    WHERE operation_id IS NOT NULL;

DROP FUNCTION kaana_create_customer_provider_credential(TEXT, TEXT, TEXT, TEXT, TEXT, BYTEA, TEXT, TEXT);
DROP FUNCTION kaana_rotate_customer_provider_credential(TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, BYTEA, TEXT, TEXT);
DROP FUNCTION kaana_revoke_customer_provider_credential(TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, TEXT);

CREATE FUNCTION kaana_apply_customer_provider_credential_create(
    p_operation_id TEXT,
    p_credential_handle TEXT,
    p_provider_slug TEXT,
    p_owner_account_id TEXT,
    p_connection_id TEXT,
    p_environment TEXT,
    p_encrypted_secret BYTEA,
    p_kms_key_arn TEXT,
    p_secret_sha256 TEXT,
    p_operation_actor TEXT
) RETURNS TABLE(
    outcome_status TEXT,
    resolved_handle TEXT,
    resolved_revision BIGINT,
    was_replayed BOOLEAN
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_customer_create_v2$
DECLARE
    inserted_operation_rows INTEGER;
    inserted_credential_rows INTEGER;
BEGIN
    IF p_secret_sha256 IS NULL OR p_secret_sha256 !~ '^[0-9a-f]{64}$' THEN
        RAISE EXCEPTION 'secret fingerprint is invalid';
    END IF;
    IF p_operation_actor IS NULL
       OR length(p_operation_actor) NOT BETWEEN 1 AND 256
       OR p_operation_actor <> btrim(p_operation_actor)
       OR p_operation_actor ~ E'[\\r\\n]' THEN
        RAISE EXCEPTION 'operation actor is invalid';
    END IF;

    INSERT INTO public.customer_provider_credential_operations (
        operation_id, action, provider_slug, owner_account_id, connection_id,
        environment, requested_credential_handle, expected_revision,
        secret_digest, operation_actor, outcome_status
    ) VALUES (
        p_operation_id, 'create', p_provider_slug, p_owner_account_id,
        p_connection_id, p_environment, NULL, NULL, decode(p_secret_sha256, 'hex'),
        p_operation_actor, 'pending'
    )
    ON CONFLICT (operation_id) DO NOTHING;
    GET DIAGNOSTICS inserted_operation_rows = ROW_COUNT;

    IF inserted_operation_rows = 0 THEN
        RETURN QUERY
        SELECT stored.outcome_status,
               stored.resolved_credential_handle,
               stored.resolved_revision,
               TRUE
        FROM public.customer_provider_credential_operations AS stored
        WHERE stored.operation_id = p_operation_id
          AND stored.action = 'create'
          AND stored.provider_slug = p_provider_slug
          AND stored.owner_account_id = p_owner_account_id
          AND stored.connection_id = p_connection_id
          AND stored.environment = p_environment
          AND stored.requested_credential_handle IS NULL
          AND stored.expected_revision IS NULL
          AND encode(stored.secret_digest, 'hex') = p_secret_sha256
          AND stored.operation_actor = p_operation_actor
          AND stored.outcome_status IN ('applied', 'conflict');
        IF NOT FOUND THEN
            RETURN QUERY SELECT 'conflict'::TEXT, NULL::TEXT, NULL::BIGINT, TRUE;
        END IF;
        RETURN;
    END IF;

    INSERT INTO public.customer_provider_credentials (
        credential_handle, provider_slug, owner_account_id, connection_id,
        environment, encrypted_secret, kms_key_arn, revision, status
    ) VALUES (
        p_credential_handle, p_provider_slug, p_owner_account_id, p_connection_id,
        p_environment, p_encrypted_secret, p_kms_key_arn, 1, 'active'
    )
    ON CONFLICT (owner_account_id, connection_id, environment) DO NOTHING;
    GET DIAGNOSTICS inserted_credential_rows = ROW_COUNT;

    IF inserted_credential_rows = 0 THEN
        UPDATE public.customer_provider_credential_operations
        SET outcome_status = 'conflict', completed_at = NOW()
        WHERE operation_id = p_operation_id;
        RETURN QUERY SELECT 'conflict'::TEXT, NULL::TEXT, NULL::BIGINT, FALSE;
        RETURN;
    END IF;

    INSERT INTO public.customer_provider_credential_audit (
        credential_handle, provider_slug, owner_account_id, connection_id,
        environment, revision, action, operation_actor, database_actor,
        operation_id
    ) VALUES (
        p_credential_handle, p_provider_slug, p_owner_account_id, p_connection_id,
        p_environment, 1, 'create', p_operation_actor, session_user,
        p_operation_id
    );

    UPDATE public.customer_provider_credential_operations
    SET outcome_status = 'applied',
        resolved_credential_handle = p_credential_handle,
        resolved_revision = 1,
        completed_at = NOW()
    WHERE operation_id = p_operation_id;

    RETURN QUERY SELECT 'applied'::TEXT, p_credential_handle, 1::BIGINT, FALSE;
END
$kaana_customer_create_v2$;

CREATE FUNCTION kaana_apply_customer_provider_credential_rotate(
    p_operation_id TEXT,
    p_credential_handle TEXT,
    p_provider_slug TEXT,
    p_owner_account_id TEXT,
    p_connection_id TEXT,
    p_environment TEXT,
    p_expected_revision BIGINT,
    p_encrypted_secret BYTEA,
    p_kms_key_arn TEXT,
    p_secret_sha256 TEXT,
    p_operation_actor TEXT
) RETURNS TABLE(
    outcome_status TEXT,
    resolved_handle TEXT,
    resolved_revision BIGINT,
    was_replayed BOOLEAN
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_customer_rotate_v2$
DECLARE
    inserted_operation_rows INTEGER;
    changed_credential_rows INTEGER;
BEGIN
    IF p_secret_sha256 IS NULL OR p_secret_sha256 !~ '^[0-9a-f]{64}$' THEN
        RAISE EXCEPTION 'secret fingerprint is invalid';
    END IF;
    IF p_operation_actor IS NULL
       OR length(p_operation_actor) NOT BETWEEN 1 AND 256
       OR p_operation_actor <> btrim(p_operation_actor)
       OR p_operation_actor ~ E'[\\r\\n]' THEN
        RAISE EXCEPTION 'operation actor is invalid';
    END IF;

    INSERT INTO public.customer_provider_credential_operations (
        operation_id, action, provider_slug, owner_account_id, connection_id,
        environment, requested_credential_handle, expected_revision,
        secret_digest, operation_actor, outcome_status
    ) VALUES (
        p_operation_id, 'rotate', p_provider_slug, p_owner_account_id,
        p_connection_id, p_environment, p_credential_handle,
        p_expected_revision, decode(p_secret_sha256, 'hex'), p_operation_actor, 'pending'
    )
    ON CONFLICT (operation_id) DO NOTHING;
    GET DIAGNOSTICS inserted_operation_rows = ROW_COUNT;

    IF inserted_operation_rows = 0 THEN
        RETURN QUERY
        SELECT stored.outcome_status,
               stored.resolved_credential_handle,
               stored.resolved_revision,
               TRUE
        FROM public.customer_provider_credential_operations AS stored
        WHERE stored.operation_id = p_operation_id
          AND stored.action = 'rotate'
          AND stored.provider_slug = p_provider_slug
          AND stored.owner_account_id = p_owner_account_id
          AND stored.connection_id = p_connection_id
          AND stored.environment = p_environment
          AND stored.requested_credential_handle = p_credential_handle
          AND stored.expected_revision = p_expected_revision
          AND encode(stored.secret_digest, 'hex') = p_secret_sha256
          AND stored.operation_actor = p_operation_actor
          AND stored.outcome_status IN ('applied', 'conflict');
        IF NOT FOUND THEN
            RETURN QUERY SELECT 'conflict'::TEXT, NULL::TEXT, NULL::BIGINT, TRUE;
        END IF;
        RETURN;
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
    GET DIAGNOSTICS changed_credential_rows = ROW_COUNT;

    IF changed_credential_rows = 0 THEN
        UPDATE public.customer_provider_credential_operations
        SET outcome_status = 'conflict', completed_at = NOW()
        WHERE operation_id = p_operation_id;
        RETURN QUERY SELECT 'conflict'::TEXT, NULL::TEXT, NULL::BIGINT, FALSE;
        RETURN;
    END IF;

    INSERT INTO public.customer_provider_credential_audit (
        credential_handle, provider_slug, owner_account_id, connection_id,
        environment, revision, action, operation_actor, database_actor,
        operation_id
    ) VALUES (
        p_credential_handle, p_provider_slug, p_owner_account_id, p_connection_id,
        p_environment, p_expected_revision + 1, 'rotate', p_operation_actor,
        session_user, p_operation_id
    );

    UPDATE public.customer_provider_credential_operations
    SET outcome_status = 'applied',
        resolved_credential_handle = p_credential_handle,
        resolved_revision = p_expected_revision + 1,
        completed_at = NOW()
    WHERE operation_id = p_operation_id;

    RETURN QUERY
    SELECT 'applied'::TEXT, p_credential_handle, p_expected_revision + 1, FALSE;
END
$kaana_customer_rotate_v2$;

CREATE FUNCTION kaana_apply_customer_provider_credential_revoke(
    p_operation_id TEXT,
    p_credential_handle TEXT,
    p_provider_slug TEXT,
    p_owner_account_id TEXT,
    p_connection_id TEXT,
    p_environment TEXT,
    p_expected_revision BIGINT,
    p_operation_actor TEXT
) RETURNS TABLE(
    outcome_status TEXT,
    resolved_handle TEXT,
    resolved_revision BIGINT,
    was_replayed BOOLEAN
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_customer_revoke_v2$
DECLARE
    inserted_operation_rows INTEGER;
    changed_credential_rows INTEGER;
BEGIN
    IF p_operation_actor IS NULL
       OR length(p_operation_actor) NOT BETWEEN 1 AND 256
       OR p_operation_actor <> btrim(p_operation_actor)
       OR p_operation_actor ~ E'[\\r\\n]' THEN
        RAISE EXCEPTION 'operation actor is invalid';
    END IF;

    INSERT INTO public.customer_provider_credential_operations (
        operation_id, action, provider_slug, owner_account_id, connection_id,
        environment, requested_credential_handle, expected_revision,
        secret_digest, operation_actor, outcome_status
    ) VALUES (
        p_operation_id, 'revoke', p_provider_slug, p_owner_account_id,
        p_connection_id, p_environment, p_credential_handle,
        p_expected_revision, NULL, p_operation_actor, 'pending'
    )
    ON CONFLICT (operation_id) DO NOTHING;
    GET DIAGNOSTICS inserted_operation_rows = ROW_COUNT;

    IF inserted_operation_rows = 0 THEN
        RETURN QUERY
        SELECT stored.outcome_status,
               stored.resolved_credential_handle,
               stored.resolved_revision,
               TRUE
        FROM public.customer_provider_credential_operations AS stored
        WHERE stored.operation_id = p_operation_id
          AND stored.action = 'revoke'
          AND stored.provider_slug = p_provider_slug
          AND stored.owner_account_id = p_owner_account_id
          AND stored.connection_id = p_connection_id
          AND stored.environment = p_environment
          AND stored.requested_credential_handle = p_credential_handle
          AND stored.expected_revision = p_expected_revision
          AND stored.operation_actor = p_operation_actor
          AND stored.outcome_status IN ('applied', 'conflict');
        IF NOT FOUND THEN
            RETURN QUERY SELECT 'conflict'::TEXT, NULL::TEXT, NULL::BIGINT, TRUE;
        END IF;
        RETURN;
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
    GET DIAGNOSTICS changed_credential_rows = ROW_COUNT;

    IF changed_credential_rows = 0 THEN
        UPDATE public.customer_provider_credential_operations
        SET outcome_status = 'conflict', completed_at = NOW()
        WHERE operation_id = p_operation_id;
        RETURN QUERY SELECT 'conflict'::TEXT, NULL::TEXT, NULL::BIGINT, FALSE;
        RETURN;
    END IF;

    INSERT INTO public.customer_provider_credential_audit (
        credential_handle, provider_slug, owner_account_id, connection_id,
        environment, revision, action, operation_actor, database_actor,
        operation_id
    ) VALUES (
        p_credential_handle, p_provider_slug, p_owner_account_id, p_connection_id,
        p_environment, p_expected_revision + 1, 'revoke', p_operation_actor,
        session_user, p_operation_id
    );

    UPDATE public.customer_provider_credential_operations
    SET outcome_status = 'applied',
        resolved_credential_handle = p_credential_handle,
        resolved_revision = p_expected_revision + 1,
        completed_at = NOW()
    WHERE operation_id = p_operation_id;

    RETURN QUERY
    SELECT 'applied'::TEXT, p_credential_handle, p_expected_revision + 1, FALSE;
END
$kaana_customer_revoke_v2$;

CREATE FUNCTION kaana_get_customer_provider_credential_outcome(
    p_operation_id TEXT,
    p_action TEXT,
    p_provider_slug TEXT,
    p_owner_account_id TEXT,
    p_connection_id TEXT,
    p_environment TEXT,
    p_credential_handle TEXT,
    p_expected_revision BIGINT,
    p_secret_sha256 TEXT
) RETURNS TABLE(
    outcome_status TEXT,
    resolved_handle TEXT,
    resolved_revision BIGINT
)
LANGUAGE sql
SECURITY DEFINER
STABLE
SET search_path = pg_catalog, public
AS $kaana_customer_outcome$
    SELECT stored.outcome_status,
           stored.resolved_credential_handle,
           stored.resolved_revision
    FROM public.customer_provider_credential_operations AS stored
    WHERE stored.operation_id = p_operation_id
      AND stored.action = p_action
      AND stored.provider_slug = p_provider_slug
      AND stored.owner_account_id = p_owner_account_id
      AND stored.connection_id = p_connection_id
      AND stored.environment = p_environment
      AND stored.requested_credential_handle IS NOT DISTINCT FROM p_credential_handle
      AND stored.expected_revision IS NOT DISTINCT FROM p_expected_revision
      AND encode(stored.secret_digest, 'hex') IS NOT DISTINCT FROM p_secret_sha256
      AND stored.outcome_status IN ('applied', 'conflict')
$kaana_customer_outcome$;

REVOKE ALL ON customer_provider_credential_operations FROM PUBLIC;
REVOKE ALL ON customer_provider_credential_operations FROM kaana_runtime, kaana_credential_admin, kaana_customer_credential_control;
REVOKE ALL ON FUNCTION kaana_apply_customer_provider_credential_create(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, BYTEA, TEXT, TEXT, TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_apply_customer_provider_credential_rotate(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, BYTEA, TEXT, TEXT, TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_apply_customer_provider_credential_revoke(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_get_customer_provider_credential_outcome(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, TEXT) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION kaana_apply_customer_provider_credential_create(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, BYTEA, TEXT, TEXT, TEXT) TO kaana_customer_credential_control;
GRANT EXECUTE ON FUNCTION kaana_apply_customer_provider_credential_rotate(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, BYTEA, TEXT, TEXT, TEXT) TO kaana_customer_credential_control;
GRANT EXECUTE ON FUNCTION kaana_apply_customer_provider_credential_revoke(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, TEXT) TO kaana_customer_credential_control;
GRANT EXECUTE ON FUNCTION kaana_get_customer_provider_credential_outcome(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, TEXT) TO kaana_customer_credential_control;
