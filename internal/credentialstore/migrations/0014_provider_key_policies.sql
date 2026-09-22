CREATE TABLE provider_key_policies (
    provider_slug TEXT PRIMARY KEY
        CHECK (provider_slug ~ '^[a-z0-9]+([._-][a-z0-9]+)*$'),
    key_retirement_seconds INTEGER NOT NULL DEFAULT 0
        CHECK (key_retirement_seconds >= 0),
    keys_on_separate_accounts BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE provider_key_policy_audit (
    audit_id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider_slug TEXT NOT NULL,
    key_retirement_seconds INTEGER NOT NULL,
    keys_on_separate_accounts BOOLEAN NOT NULL,
    operation_actor TEXT NOT NULL CHECK (length(operation_actor) BETWEEN 1 AND 256),
    database_actor TEXT NOT NULL DEFAULT CURRENT_USER,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX provider_key_policy_audit_provider_time_idx
    ON provider_key_policy_audit (provider_slug, occurred_at DESC);

-- key_retirement_seconds = 0 means "unset": provider.KeyPolicy's zero value
-- already means "use DefaultKeyRetirement" (newKeyPool applies that fallback),
-- so no default-substitution logic is needed here or in Go. An operator who
-- wants the default explicitly recorded puts 0 rather than leaving no row —
-- there is no disable/delete verb for this table, only put, so the audit
-- trail always shows a deliberate action, not an absence.
CREATE FUNCTION kaana_put_provider_key_policy(
    p_provider_slug TEXT,
    p_key_retirement_seconds INTEGER,
    p_keys_on_separate_accounts BOOLEAN,
    p_operation_actor TEXT
) RETURNS VOID
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_put_policy$
BEGIN
    IF p_operation_actor IS NULL
       OR length(p_operation_actor) NOT BETWEEN 1 AND 256
       OR p_operation_actor <> btrim(p_operation_actor) THEN
        RAISE EXCEPTION 'operation actor is invalid';
    END IF;
    IF p_key_retirement_seconds IS NULL OR p_key_retirement_seconds < 0 THEN
        RAISE EXCEPTION 'key retirement seconds must not be negative';
    END IF;

    INSERT INTO public.provider_key_policies (
        provider_slug, key_retirement_seconds, keys_on_separate_accounts
    ) VALUES (
        p_provider_slug, p_key_retirement_seconds, p_keys_on_separate_accounts
    )
    ON CONFLICT (provider_slug) DO UPDATE SET
        key_retirement_seconds = EXCLUDED.key_retirement_seconds,
        keys_on_separate_accounts = EXCLUDED.keys_on_separate_accounts,
        updated_at = NOW();

    INSERT INTO public.provider_key_policy_audit (
        provider_slug, key_retirement_seconds, keys_on_separate_accounts,
        operation_actor, database_actor
    ) VALUES (
        p_provider_slug, p_key_retirement_seconds, p_keys_on_separate_accounts,
        p_operation_actor, session_user
    );
END
$kaana_put_policy$;

REVOKE ALL ON provider_key_policies FROM PUBLIC;
REVOKE ALL ON provider_key_policy_audit FROM PUBLIC;
REVOKE ALL ON SEQUENCE provider_key_policy_audit_audit_id_seq FROM kaana_runtime, kaana_credential_admin;
REVOKE ALL ON FUNCTION kaana_put_provider_key_policy(TEXT, INTEGER, BOOLEAN, TEXT) FROM PUBLIC;

GRANT SELECT ON provider_key_policies TO kaana_runtime;
GRANT SELECT ON provider_key_policies, provider_key_policy_audit TO kaana_credential_admin;
GRANT EXECUTE ON FUNCTION kaana_put_provider_key_policy(TEXT, INTEGER, BOOLEAN, TEXT) TO kaana_credential_admin;
