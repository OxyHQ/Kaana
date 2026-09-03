ALTER TABLE public.provider_credential_audit
    DROP CONSTRAINT provider_credential_audit_action_check;

ALTER TABLE public.provider_credential_audit
    ADD CONSTRAINT provider_credential_audit_action_check
    CHECK (action IN (
        'put',
        'disable',
        'rekey_disable',
        'rekey_create',
        'deduplicate_disable'
    ));

CREATE TABLE public.provider_credential_admin_operations (
    operation_id TEXT PRIMARY KEY
        CHECK (operation_id ~ '^kop_[0-9a-f]{32}$'),
    action TEXT NOT NULL
        CHECK (action IN ('rekey_id', 'deduplicate')),
    provider_slug TEXT NOT NULL
        CHECK (provider_slug ~ '^[a-z0-9]+([._-][a-z0-9]+)*$'),
    source_key_id TEXT NOT NULL
        CHECK (source_key_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
    destination_key_id TEXT NOT NULL
        CHECK (destination_key_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
    prerequisite_operation_id TEXT
        REFERENCES public.provider_credential_admin_operations(operation_id),
    prerequisite_outcome TEXT
        CHECK (prerequisite_outcome IN ('applied', 'deduplicated', 'different')),
    operation_actor TEXT NOT NULL
        CHECK (
            length(operation_actor) BETWEEN 1 AND 256
            AND operation_actor = btrim(operation_actor)
            AND operation_actor !~ E'[\r\n]'
        ),
    database_actor TEXT NOT NULL,
    outcome TEXT NOT NULL,
    completed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (source_key_id <> destination_key_id),
    CHECK (prerequisite_operation_id IS NULL OR prerequisite_operation_id <> operation_id),
    CHECK (prerequisite_operation_id IS NOT NULL OR prerequisite_outcome IS NULL),
    CHECK (
        (action = 'rekey_id' AND outcome = 'applied')
        OR (action = 'deduplicate' AND outcome IN ('deduplicated', 'different'))
    )
);

CREATE VIEW public.active_provider_credentials
WITH (security_barrier = true) AS
SELECT provider_slug, key_id, encrypted_secret, kms_key_arn,
       key_class, budget_usd, position
FROM public.provider_credentials
WHERE enabled = TRUE;

CREATE FUNCTION public.kaana_prepare_provider_credential_rekey(
    p_operation_id TEXT,
    p_provider_slug TEXT,
    p_source_key_id TEXT,
    p_destination_key_id TEXT,
    p_operation_actor TEXT,
    p_prerequisite_operation_id TEXT,
    p_prerequisite_outcome TEXT
) RETURNS TABLE(
    outcome_state TEXT,
    stored_outcome TEXT,
    source_encrypted_secret BYTEA,
    source_kms_key_arn TEXT,
    source_key_class TEXT,
    source_budget_usd DOUBLE PRECISION,
    source_position INTEGER
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_provider_rekey_prepare$
DECLARE
    existing_action TEXT;
    existing_provider_slug TEXT;
    existing_source_key_id TEXT;
    existing_destination_key_id TEXT;
    existing_prerequisite_operation_id TEXT;
    existing_prerequisite_outcome TEXT;
    existing_outcome TEXT;
BEGIN
    IF p_operation_id IS NULL OR p_operation_id !~ '^kop_[0-9a-f]{32}$'
       OR p_provider_slug IS NULL OR p_provider_slug !~ '^[a-z0-9]+([._-][a-z0-9]+)*$'
       OR p_source_key_id IS NULL OR p_source_key_id !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
       OR p_destination_key_id IS NULL
       OR p_destination_key_id !~ '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
       OR p_source_key_id = p_destination_key_id
       OR p_operation_actor IS NULL
       OR length(p_operation_actor) NOT BETWEEN 1 AND 256
       OR p_operation_actor <> btrim(p_operation_actor)
       OR p_operation_actor ~ E'[\r\n]'
       OR (p_prerequisite_operation_id IS NOT NULL
           AND (p_prerequisite_operation_id !~ '^kop_[0-9a-f]{32}$'
                OR p_prerequisite_operation_id = p_operation_id))
       OR (p_prerequisite_operation_id IS NULL AND p_prerequisite_outcome IS NOT NULL)
       OR (p_prerequisite_outcome IS NOT NULL
           AND p_prerequisite_outcome NOT IN ('applied', 'deduplicated', 'different')) THEN
        RAISE EXCEPTION 'provider credential rekey request is invalid';
    END IF;

    PERFORM pg_advisory_xact_lock(hashtextextended(
        'kaana:provider-credential-admin:' || p_provider_slug,
        0
    ));
    LOCK TABLE public.provider_credentials IN SHARE ROW EXCLUSIVE MODE;

    SELECT stored.action, stored.provider_slug, stored.source_key_id,
           stored.destination_key_id, stored.prerequisite_operation_id,
           stored.prerequisite_outcome, stored.outcome
      INTO existing_action, existing_provider_slug, existing_source_key_id,
           existing_destination_key_id, existing_prerequisite_operation_id,
           existing_prerequisite_outcome, existing_outcome
    FROM public.provider_credential_admin_operations AS stored
    WHERE stored.operation_id = p_operation_id
    FOR UPDATE;

    IF FOUND THEN
        IF existing_action = 'rekey_id'
           AND existing_provider_slug = p_provider_slug
           AND existing_source_key_id = p_source_key_id
           AND existing_destination_key_id = p_destination_key_id
           AND existing_prerequisite_operation_id IS NOT DISTINCT FROM p_prerequisite_operation_id
           AND existing_prerequisite_outcome IS NOT DISTINCT FROM p_prerequisite_outcome THEN
            RETURN QUERY SELECT 'replayed'::TEXT, existing_outcome,
                NULL::BYTEA, NULL::TEXT, NULL::TEXT,
                NULL::DOUBLE PRECISION, NULL::INTEGER;
        ELSE
            RETURN QUERY SELECT 'conflict'::TEXT, NULL::TEXT,
                NULL::BYTEA, NULL::TEXT, NULL::TEXT,
                NULL::DOUBLE PRECISION, NULL::INTEGER;
        END IF;
        RETURN;
    END IF;

    IF p_prerequisite_operation_id IS NOT NULL AND NOT EXISTS (
        SELECT 1
        FROM public.provider_credential_admin_operations AS prerequisite
        WHERE prerequisite.operation_id = p_prerequisite_operation_id
          AND prerequisite.provider_slug = p_provider_slug
          AND (p_prerequisite_outcome IS NULL OR prerequisite.outcome = p_prerequisite_outcome)
    ) THEN
        RETURN QUERY SELECT 'prerequisite_unavailable'::TEXT, NULL::TEXT,
            NULL::BYTEA, NULL::TEXT, NULL::TEXT,
            NULL::DOUBLE PRECISION, NULL::INTEGER;
        RETURN;
    END IF;

    IF EXISTS (
        SELECT 1
        FROM public.provider_credentials AS destination
        WHERE destination.provider_slug = p_provider_slug
          AND destination.key_id = p_destination_key_id
    ) THEN
        RETURN QUERY SELECT 'destination_exists'::TEXT, NULL::TEXT,
            NULL::BYTEA, NULL::TEXT, NULL::TEXT,
            NULL::DOUBLE PRECISION, NULL::INTEGER;
        RETURN;
    END IF;

    RETURN QUERY
    SELECT 'execute'::TEXT, NULL::TEXT, source.encrypted_secret,
           source.kms_key_arn, source.key_class,
           source.budget_usd::DOUBLE PRECISION, source.position
    FROM public.provider_credentials AS source
    WHERE source.provider_slug = p_provider_slug
      AND source.key_id = p_source_key_id
      AND source.enabled = TRUE
    FOR UPDATE;

    IF NOT FOUND THEN
        RETURN QUERY SELECT 'source_unavailable'::TEXT, NULL::TEXT,
            NULL::BYTEA, NULL::TEXT, NULL::TEXT,
            NULL::DOUBLE PRECISION, NULL::INTEGER;
    END IF;
END
$kaana_provider_rekey_prepare$;

CREATE FUNCTION public.kaana_complete_provider_credential_rekey(
    p_operation_id TEXT,
    p_provider_slug TEXT,
    p_source_key_id TEXT,
    p_destination_key_id TEXT,
    p_operation_actor TEXT,
    p_destination_encrypted_secret BYTEA,
    p_destination_kms_key_arn TEXT,
    p_prerequisite_operation_id TEXT,
    p_prerequisite_outcome TEXT
) RETURNS TABLE(outcome_state TEXT, stored_outcome TEXT)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_provider_rekey_complete$
DECLARE
    existing_action TEXT;
    existing_provider_slug TEXT;
    existing_source_key_id TEXT;
    existing_destination_key_id TEXT;
    existing_prerequisite_operation_id TEXT;
    existing_prerequisite_outcome TEXT;
    existing_outcome TEXT;
    source_kms_key_arn TEXT;
    source_key_class TEXT;
    source_budget_usd NUMERIC(14, 6);
    source_position INTEGER;
BEGIN
    IF p_operation_id IS NULL OR p_operation_id !~ '^kop_[0-9a-f]{32}$'
       OR p_provider_slug IS NULL OR p_provider_slug !~ '^[a-z0-9]+([._-][a-z0-9]+)*$'
       OR p_source_key_id IS NULL OR p_source_key_id !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
       OR p_destination_key_id IS NULL
       OR p_destination_key_id !~ '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
       OR p_source_key_id = p_destination_key_id
       OR p_operation_actor IS NULL
       OR length(p_operation_actor) NOT BETWEEN 1 AND 256
       OR p_operation_actor <> btrim(p_operation_actor)
       OR p_operation_actor ~ E'[\r\n]'
       OR p_destination_encrypted_secret IS NULL
       OR octet_length(p_destination_encrypted_secret) = 0
       OR p_destination_kms_key_arn IS NULL
       OR (p_prerequisite_operation_id IS NOT NULL
           AND (p_prerequisite_operation_id !~ '^kop_[0-9a-f]{32}$'
                OR p_prerequisite_operation_id = p_operation_id))
       OR (p_prerequisite_operation_id IS NULL AND p_prerequisite_outcome IS NOT NULL)
       OR (p_prerequisite_outcome IS NOT NULL
           AND p_prerequisite_outcome NOT IN ('applied', 'deduplicated', 'different')) THEN
        RAISE EXCEPTION 'provider credential rekey completion is invalid';
    END IF;

    PERFORM pg_advisory_xact_lock(hashtextextended(
        'kaana:provider-credential-admin:' || p_provider_slug,
        0
    ));
    LOCK TABLE public.provider_credentials IN SHARE ROW EXCLUSIVE MODE;

    SELECT stored.action, stored.provider_slug, stored.source_key_id,
           stored.destination_key_id, stored.prerequisite_operation_id,
           stored.prerequisite_outcome, stored.outcome
      INTO existing_action, existing_provider_slug, existing_source_key_id,
           existing_destination_key_id, existing_prerequisite_operation_id,
           existing_prerequisite_outcome, existing_outcome
    FROM public.provider_credential_admin_operations AS stored
    WHERE stored.operation_id = p_operation_id
    FOR UPDATE;

    IF FOUND THEN
        IF existing_action = 'rekey_id'
           AND existing_provider_slug = p_provider_slug
           AND existing_source_key_id = p_source_key_id
           AND existing_destination_key_id = p_destination_key_id
           AND existing_prerequisite_operation_id IS NOT DISTINCT FROM p_prerequisite_operation_id
           AND existing_prerequisite_outcome IS NOT DISTINCT FROM p_prerequisite_outcome THEN
            RETURN QUERY SELECT 'replayed'::TEXT, existing_outcome;
        ELSE
            RETURN QUERY SELECT 'conflict'::TEXT, NULL::TEXT;
        END IF;
        RETURN;
    END IF;

    IF p_prerequisite_operation_id IS NOT NULL AND NOT EXISTS (
        SELECT 1
        FROM public.provider_credential_admin_operations AS prerequisite
        WHERE prerequisite.operation_id = p_prerequisite_operation_id
          AND prerequisite.provider_slug = p_provider_slug
          AND (p_prerequisite_outcome IS NULL OR prerequisite.outcome = p_prerequisite_outcome)
    ) THEN
        RETURN QUERY SELECT 'prerequisite_unavailable'::TEXT, NULL::TEXT;
        RETURN;
    END IF;

    IF EXISTS (
        SELECT 1
        FROM public.provider_credentials AS destination
        WHERE destination.provider_slug = p_provider_slug
          AND destination.key_id = p_destination_key_id
    ) THEN
        RETURN QUERY SELECT 'destination_exists'::TEXT, NULL::TEXT;
        RETURN;
    END IF;

    SELECT source.kms_key_arn, source.key_class, source.budget_usd, source.position
      INTO source_kms_key_arn, source_key_class, source_budget_usd, source_position
    FROM public.provider_credentials AS source
    WHERE source.provider_slug = p_provider_slug
      AND source.key_id = p_source_key_id
      AND source.enabled = TRUE
    FOR UPDATE;

    IF NOT FOUND THEN
        RETURN QUERY SELECT 'source_unavailable'::TEXT, NULL::TEXT;
        RETURN;
    END IF;
    IF p_destination_kms_key_arn <> source_kms_key_arn THEN
        RAISE EXCEPTION 'provider credential rekey changed the KMS key identity';
    END IF;

    UPDATE public.provider_credentials
    SET enabled = FALSE, updated_at = NOW()
    WHERE provider_slug = p_provider_slug
      AND key_id = p_source_key_id
      AND enabled = TRUE;

    INSERT INTO public.provider_credentials (
        provider_slug, key_id, encrypted_secret, kms_key_arn,
        key_class, budget_usd, position, enabled
    ) VALUES (
        p_provider_slug, p_destination_key_id, p_destination_encrypted_secret,
        p_destination_kms_key_arn, source_key_class, source_budget_usd,
        source_position, TRUE
    );

    INSERT INTO public.provider_credential_audit (
        provider_slug, key_id, action, operation_actor, database_actor
    ) VALUES
        (p_provider_slug, p_source_key_id, 'rekey_disable', p_operation_actor, session_user),
        (p_provider_slug, p_destination_key_id, 'rekey_create', p_operation_actor, session_user);

    INSERT INTO public.provider_credential_admin_operations (
        operation_id, action, provider_slug, source_key_id,
        destination_key_id, prerequisite_operation_id, prerequisite_outcome,
        operation_actor, database_actor, outcome
    ) VALUES (
        p_operation_id, 'rekey_id', p_provider_slug, p_source_key_id,
        p_destination_key_id, p_prerequisite_operation_id, p_prerequisite_outcome,
        p_operation_actor, session_user, 'applied'
    );

    RETURN QUERY SELECT 'applied'::TEXT, 'applied'::TEXT;
END
$kaana_provider_rekey_complete$;

CREATE FUNCTION public.kaana_prepare_provider_credential_deduplication(
    p_operation_id TEXT,
    p_provider_slug TEXT,
    p_duplicate_key_id TEXT,
    p_keep_key_id TEXT,
    p_operation_actor TEXT
) RETURNS TABLE(
    outcome_state TEXT,
    stored_outcome TEXT,
    keep_encrypted_secret BYTEA,
    keep_kms_key_arn TEXT,
    keep_key_class TEXT,
    keep_budget_usd DOUBLE PRECISION,
    keep_position INTEGER,
    duplicate_encrypted_secret BYTEA,
    duplicate_kms_key_arn TEXT,
    duplicate_key_class TEXT,
    duplicate_budget_usd DOUBLE PRECISION,
    duplicate_position INTEGER
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_provider_deduplicate_prepare$
DECLARE
    existing_action TEXT;
    existing_provider_slug TEXT;
    existing_source_key_id TEXT;
    existing_destination_key_id TEXT;
    existing_outcome TEXT;
    keep_ciphertext BYTEA;
    keep_arn TEXT;
    keep_class TEXT;
    keep_budget DOUBLE PRECISION;
    keep_pool_position INTEGER;
    duplicate_ciphertext BYTEA;
    duplicate_arn TEXT;
    duplicate_class TEXT;
    duplicate_budget DOUBLE PRECISION;
    duplicate_pool_position INTEGER;
BEGIN
    IF p_operation_id IS NULL OR p_operation_id !~ '^kop_[0-9a-f]{32}$'
       OR p_provider_slug IS NULL OR p_provider_slug !~ '^[a-z0-9]+([._-][a-z0-9]+)*$'
       OR p_duplicate_key_id IS NULL OR p_duplicate_key_id !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
       OR p_keep_key_id IS NULL OR p_keep_key_id !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
       OR p_duplicate_key_id = p_keep_key_id
       OR p_operation_actor IS NULL
       OR length(p_operation_actor) NOT BETWEEN 1 AND 256
       OR p_operation_actor <> btrim(p_operation_actor)
       OR p_operation_actor ~ E'[\r\n]' THEN
        RAISE EXCEPTION 'provider credential deduplication request is invalid';
    END IF;

    PERFORM pg_advisory_xact_lock(hashtextextended(
        'kaana:provider-credential-admin:' || p_provider_slug,
        0
    ));
    LOCK TABLE public.provider_credentials IN SHARE ROW EXCLUSIVE MODE;

    SELECT stored.action, stored.provider_slug, stored.source_key_id,
           stored.destination_key_id, stored.outcome
      INTO existing_action, existing_provider_slug, existing_source_key_id,
           existing_destination_key_id, existing_outcome
    FROM public.provider_credential_admin_operations AS stored
    WHERE stored.operation_id = p_operation_id
    FOR UPDATE;

    IF FOUND THEN
        IF existing_action = 'deduplicate'
           AND existing_provider_slug = p_provider_slug
           AND existing_source_key_id = p_duplicate_key_id
           AND existing_destination_key_id = p_keep_key_id THEN
            RETURN QUERY SELECT 'replayed'::TEXT, existing_outcome,
                NULL::BYTEA, NULL::TEXT, NULL::TEXT, NULL::DOUBLE PRECISION, NULL::INTEGER,
                NULL::BYTEA, NULL::TEXT, NULL::TEXT, NULL::DOUBLE PRECISION, NULL::INTEGER;
        ELSE
            RETURN QUERY SELECT 'conflict'::TEXT, NULL::TEXT,
                NULL::BYTEA, NULL::TEXT, NULL::TEXT, NULL::DOUBLE PRECISION, NULL::INTEGER,
                NULL::BYTEA, NULL::TEXT, NULL::TEXT, NULL::DOUBLE PRECISION, NULL::INTEGER;
        END IF;
        RETURN;
    END IF;

    SELECT keep.encrypted_secret, keep.kms_key_arn, keep.key_class,
           keep.budget_usd::DOUBLE PRECISION, keep.position
      INTO keep_ciphertext, keep_arn, keep_class, keep_budget, keep_pool_position
    FROM public.provider_credentials AS keep
    WHERE keep.provider_slug = p_provider_slug
      AND keep.key_id = p_keep_key_id
      AND keep.enabled = TRUE
    FOR UPDATE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT 'source_unavailable'::TEXT, NULL::TEXT,
            NULL::BYTEA, NULL::TEXT, NULL::TEXT, NULL::DOUBLE PRECISION, NULL::INTEGER,
            NULL::BYTEA, NULL::TEXT, NULL::TEXT, NULL::DOUBLE PRECISION, NULL::INTEGER;
        RETURN;
    END IF;

    SELECT duplicate.encrypted_secret, duplicate.kms_key_arn, duplicate.key_class,
           duplicate.budget_usd::DOUBLE PRECISION, duplicate.position
      INTO duplicate_ciphertext, duplicate_arn, duplicate_class,
           duplicate_budget, duplicate_pool_position
    FROM public.provider_credentials AS duplicate
    WHERE duplicate.provider_slug = p_provider_slug
      AND duplicate.key_id = p_duplicate_key_id
      AND duplicate.enabled = TRUE
    FOR UPDATE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT 'source_unavailable'::TEXT, NULL::TEXT,
            NULL::BYTEA, NULL::TEXT, NULL::TEXT, NULL::DOUBLE PRECISION, NULL::INTEGER,
            NULL::BYTEA, NULL::TEXT, NULL::TEXT, NULL::DOUBLE PRECISION, NULL::INTEGER;
        RETURN;
    END IF;

    RETURN QUERY SELECT 'execute'::TEXT, NULL::TEXT,
        keep_ciphertext, keep_arn, keep_class, keep_budget, keep_pool_position,
        duplicate_ciphertext, duplicate_arn, duplicate_class,
        duplicate_budget, duplicate_pool_position;
END
$kaana_provider_deduplicate_prepare$;

CREATE FUNCTION public.kaana_complete_provider_credential_deduplication(
    p_operation_id TEXT,
    p_provider_slug TEXT,
    p_duplicate_key_id TEXT,
    p_keep_key_id TEXT,
    p_operation_actor TEXT,
    p_credentials_equal BOOLEAN
) RETURNS TABLE(outcome_state TEXT, stored_outcome TEXT)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_provider_deduplicate_complete$
DECLARE
    existing_action TEXT;
    existing_provider_slug TEXT;
    existing_source_key_id TEXT;
    existing_destination_key_id TEXT;
    existing_outcome TEXT;
BEGIN
    IF p_operation_id IS NULL OR p_operation_id !~ '^kop_[0-9a-f]{32}$'
       OR p_provider_slug IS NULL OR p_provider_slug !~ '^[a-z0-9]+([._-][a-z0-9]+)*$'
       OR p_duplicate_key_id IS NULL OR p_duplicate_key_id !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
       OR p_keep_key_id IS NULL OR p_keep_key_id !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
       OR p_duplicate_key_id = p_keep_key_id
       OR p_operation_actor IS NULL
       OR length(p_operation_actor) NOT BETWEEN 1 AND 256
       OR p_operation_actor <> btrim(p_operation_actor)
       OR p_operation_actor ~ E'[\r\n]'
       OR p_credentials_equal IS NULL THEN
        RAISE EXCEPTION 'provider credential deduplication completion is invalid';
    END IF;

    PERFORM pg_advisory_xact_lock(hashtextextended(
        'kaana:provider-credential-admin:' || p_provider_slug,
        0
    ));
    LOCK TABLE public.provider_credentials IN SHARE ROW EXCLUSIVE MODE;

    SELECT stored.action, stored.provider_slug, stored.source_key_id,
           stored.destination_key_id, stored.outcome
      INTO existing_action, existing_provider_slug, existing_source_key_id,
           existing_destination_key_id, existing_outcome
    FROM public.provider_credential_admin_operations AS stored
    WHERE stored.operation_id = p_operation_id
    FOR UPDATE;

    IF FOUND THEN
        IF existing_action = 'deduplicate'
           AND existing_provider_slug = p_provider_slug
           AND existing_source_key_id = p_duplicate_key_id
           AND existing_destination_key_id = p_keep_key_id THEN
            RETURN QUERY SELECT 'replayed'::TEXT, existing_outcome;
        ELSE
            RETURN QUERY SELECT 'conflict'::TEXT, NULL::TEXT;
        END IF;
        RETURN;
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM public.provider_credentials AS keep
        WHERE keep.provider_slug = p_provider_slug
          AND keep.key_id = p_keep_key_id
          AND keep.enabled = TRUE
    ) OR NOT EXISTS (
        SELECT 1
        FROM public.provider_credentials AS duplicate
        WHERE duplicate.provider_slug = p_provider_slug
          AND duplicate.key_id = p_duplicate_key_id
          AND duplicate.enabled = TRUE
    ) THEN
        RETURN QUERY SELECT 'source_unavailable'::TEXT, NULL::TEXT;
        RETURN;
    END IF;

    IF p_credentials_equal THEN
        UPDATE public.provider_credentials
        SET enabled = FALSE, updated_at = NOW()
        WHERE provider_slug = p_provider_slug
          AND key_id = p_duplicate_key_id
          AND enabled = TRUE;

        INSERT INTO public.provider_credential_audit (
            provider_slug, key_id, action, operation_actor, database_actor
        ) VALUES (
            p_provider_slug, p_duplicate_key_id, 'deduplicate_disable',
            p_operation_actor, session_user
        );

        INSERT INTO public.provider_credential_admin_operations (
            operation_id, action, provider_slug, source_key_id,
            destination_key_id, operation_actor, database_actor, outcome
        ) VALUES (
            p_operation_id, 'deduplicate', p_provider_slug, p_duplicate_key_id,
            p_keep_key_id, p_operation_actor, session_user, 'deduplicated'
        );
        RETURN QUERY SELECT 'applied'::TEXT, 'deduplicated'::TEXT;
        RETURN;
    END IF;

    INSERT INTO public.provider_credential_admin_operations (
        operation_id, action, provider_slug, source_key_id,
        destination_key_id, operation_actor, database_actor, outcome
    ) VALUES (
        p_operation_id, 'deduplicate', p_provider_slug, p_duplicate_key_id,
        p_keep_key_id, p_operation_actor, session_user, 'different'
    );
    RETURN QUERY SELECT 'applied'::TEXT, 'different'::TEXT;
END
$kaana_provider_deduplicate_complete$;

REVOKE ALL ON public.provider_credential_admin_operations FROM PUBLIC;
REVOKE ALL ON public.provider_credential_admin_operations
    FROM kaana_runtime, kaana_credential_admin, kaana_customer_credential_control;
REVOKE ALL ON public.provider_credentials FROM kaana_runtime;
REVOKE ALL ON public.active_provider_credentials FROM PUBLIC;
REVOKE ALL ON public.active_provider_credentials
    FROM kaana_credential_admin, kaana_customer_credential_control;
REVOKE ALL ON FUNCTION public.kaana_prepare_provider_credential_rekey(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.kaana_complete_provider_credential_rekey(TEXT, TEXT, TEXT, TEXT, TEXT, BYTEA, TEXT, TEXT, TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.kaana_prepare_provider_credential_deduplication(TEXT, TEXT, TEXT, TEXT, TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.kaana_complete_provider_credential_deduplication(TEXT, TEXT, TEXT, TEXT, TEXT, BOOLEAN) FROM PUBLIC;

GRANT SELECT ON public.provider_credential_admin_operations TO kaana_credential_admin;
GRANT SELECT ON public.active_provider_credentials TO kaana_runtime;
GRANT EXECUTE ON FUNCTION public.kaana_prepare_provider_credential_rekey(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, TEXT) TO kaana_credential_admin;
GRANT EXECUTE ON FUNCTION public.kaana_complete_provider_credential_rekey(TEXT, TEXT, TEXT, TEXT, TEXT, BYTEA, TEXT, TEXT, TEXT) TO kaana_credential_admin;
GRANT EXECUTE ON FUNCTION public.kaana_prepare_provider_credential_deduplication(TEXT, TEXT, TEXT, TEXT, TEXT) TO kaana_credential_admin;
GRANT EXECUTE ON FUNCTION public.kaana_complete_provider_credential_deduplication(TEXT, TEXT, TEXT, TEXT, TEXT, BOOLEAN) TO kaana_credential_admin;
