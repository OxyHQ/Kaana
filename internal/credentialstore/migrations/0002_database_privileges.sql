DO $kaana_privileges$
DECLARE
    required_role TEXT;
BEGIN
    FOREACH required_role IN ARRAY ARRAY[
        'kaana_runtime',
        'kaana_migrator',
        'kaana_credential_admin'
    ] LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = required_role) THEN
            RAISE EXCEPTION 'required database role % does not exist', required_role;
        END IF;
    END LOOP;

    EXECUTE format('REVOKE ALL ON DATABASE %I FROM PUBLIC', current_database());
    EXECUTE format(
        'GRANT CONNECT ON DATABASE %I TO kaana_runtime, kaana_migrator, kaana_credential_admin',
        current_database()
    );
END
$kaana_privileges$;

REVOKE ALL ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM PUBLIC;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM PUBLIC;

REVOKE ALL ON provider_credentials FROM kaana_runtime, kaana_credential_admin;
REVOKE ALL ON provider_credential_audit FROM kaana_runtime, kaana_credential_admin;
REVOKE ALL ON SEQUENCE provider_credential_audit_audit_id_seq FROM kaana_runtime, kaana_credential_admin;

CREATE VIEW provider_credential_metadata AS
SELECT provider_slug, key_id, kms_key_arn, key_class, budget_usd,
       position, enabled, created_at, updated_at
FROM provider_credentials;

CREATE FUNCTION kaana_put_provider_credential(
    p_provider_slug TEXT,
    p_key_id TEXT,
    p_encrypted_secret BYTEA,
    p_kms_key_arn TEXT,
    p_key_class TEXT,
    p_budget_usd DOUBLE PRECISION,
    p_position INTEGER,
    p_operation_actor TEXT
) RETURNS VOID
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_put$
BEGIN
    IF p_operation_actor IS NULL
       OR length(p_operation_actor) NOT BETWEEN 1 AND 256
       OR p_operation_actor <> btrim(p_operation_actor) THEN
        RAISE EXCEPTION 'operation actor is invalid';
    END IF;

    INSERT INTO public.provider_credentials (
        provider_slug, key_id, encrypted_secret, kms_key_arn,
        key_class, budget_usd, position, enabled
    ) VALUES (
        p_provider_slug, p_key_id, p_encrypted_secret, p_kms_key_arn,
        p_key_class, p_budget_usd, p_position, TRUE
    )
    ON CONFLICT (provider_slug, key_id) DO UPDATE SET
        encrypted_secret = EXCLUDED.encrypted_secret,
        kms_key_arn = EXCLUDED.kms_key_arn,
        key_class = EXCLUDED.key_class,
        budget_usd = EXCLUDED.budget_usd,
        position = EXCLUDED.position,
        enabled = TRUE,
        updated_at = NOW();

    INSERT INTO public.provider_credential_audit (
        provider_slug, key_id, action, operation_actor, database_actor
    ) VALUES (
        p_provider_slug, p_key_id, 'put', p_operation_actor, session_user
    );
END
$kaana_put$;

CREATE FUNCTION kaana_disable_provider_credential(
    p_provider_slug TEXT,
    p_key_id TEXT,
    p_operation_actor TEXT
) RETURNS BOOLEAN
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_disable$
DECLARE
    changed_rows INTEGER;
BEGIN
    IF p_operation_actor IS NULL
       OR length(p_operation_actor) NOT BETWEEN 1 AND 256
       OR p_operation_actor <> btrim(p_operation_actor) THEN
        RAISE EXCEPTION 'operation actor is invalid';
    END IF;

    UPDATE public.provider_credentials
    SET enabled = FALSE, updated_at = NOW()
    WHERE provider_slug = p_provider_slug
      AND key_id = p_key_id
      AND enabled = TRUE;
    GET DIAGNOSTICS changed_rows = ROW_COUNT;

    IF changed_rows = 1 THEN
        INSERT INTO public.provider_credential_audit (
            provider_slug, key_id, action, operation_actor, database_actor
        ) VALUES (
            p_provider_slug, p_key_id, 'disable', p_operation_actor, session_user
        );
    END IF;

    RETURN changed_rows = 1;
END
$kaana_disable$;

REVOKE ALL ON FUNCTION kaana_put_provider_credential(TEXT, TEXT, BYTEA, TEXT, TEXT, DOUBLE PRECISION, INTEGER, TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_disable_provider_credential(TEXT, TEXT, TEXT) FROM PUBLIC;
REVOKE ALL ON provider_credential_metadata FROM PUBLIC;

GRANT USAGE ON SCHEMA public TO kaana_runtime, kaana_credential_admin;
GRANT SELECT ON provider_credentials TO kaana_runtime;
GRANT SELECT ON provider_credential_metadata, provider_credential_audit TO kaana_credential_admin;
GRANT EXECUTE ON FUNCTION kaana_put_provider_credential(TEXT, TEXT, BYTEA, TEXT, TEXT, DOUBLE PRECISION, INTEGER, TEXT) TO kaana_credential_admin;
GRANT EXECUTE ON FUNCTION kaana_disable_provider_credential(TEXT, TEXT, TEXT) TO kaana_credential_admin;

ALTER DEFAULT PRIVILEGES FOR ROLE kaana_migrator IN SCHEMA public
    REVOKE ALL ON TABLES FROM PUBLIC;
ALTER DEFAULT PRIVILEGES FOR ROLE kaana_migrator IN SCHEMA public
    REVOKE ALL ON SEQUENCES FROM PUBLIC;
ALTER DEFAULT PRIVILEGES FOR ROLE kaana_migrator IN SCHEMA public
    REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;
