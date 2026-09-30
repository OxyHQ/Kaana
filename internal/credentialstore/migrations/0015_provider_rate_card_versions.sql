-- Append-only rate-card observations (#72). A cost event already names the
-- rate_card_version_id it was calculated from; until now nothing durable said
-- what that version priced. This table is that record. A version is written
-- once and never changes: a later price is a new version, so no observation
-- can rewrite the cost of an attempt that already happened.
SET LOCAL lock_timeout = '5s';

CREATE TABLE provider_rate_card_versions (
    version_id TEXT PRIMARY KEY CHECK (length(version_id) BETWEEN 1 AND 256 AND version_id = btrim(version_id)),
    source TEXT NOT NULL CHECK (source IN ('provider_api', 'provider_documentation', 'operator')),
    source_version TEXT NOT NULL CHECK (length(source_version) BETWEEN 1 AND 256 AND source_version = btrim(source_version)),
    observed_at TIMESTAMPTZ NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    rate_cards JSONB NOT NULL CHECK (jsonb_typeof(rate_cards) = 'array' AND jsonb_array_length(rate_cards) >= 1),
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    recorded_by TEXT NOT NULL DEFAULT SESSION_USER,
    CHECK (expires_at IS NULL OR expires_at > effective_at)
);

CREATE FUNCTION kaana_refuse_rate_card_mutation() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path = pg_catalog
AS $body$
BEGIN
    RAISE EXCEPTION 'provider rate card versions are append-only';
END
$body$;

CREATE TRIGGER provider_rate_card_versions_append_only
BEFORE UPDATE OR DELETE ON provider_rate_card_versions
FOR EACH ROW EXECUTE FUNCTION kaana_refuse_rate_card_mutation();

CREATE TRIGGER provider_rate_card_versions_no_truncate
BEFORE TRUNCATE ON provider_rate_card_versions
FOR EACH STATEMENT EXECUTE FUNCTION kaana_refuse_rate_card_mutation();

-- Registering the version a process loaded is idempotent: the same version with
-- the same facts is a replay. The same version id with any different fact is a
-- second observation claiming the first one's identity, and it fails closed.
CREATE FUNCTION kaana_register_provider_rate_card_version(
    p_version_id TEXT,
    p_source TEXT,
    p_source_version TEXT,
    p_observed_at TIMESTAMPTZ,
    p_effective_at TIMESTAMPTZ,
    p_expires_at TIMESTAMPTZ,
    p_rate_cards JSONB
) RETURNS TEXT
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_register_provider_rate_card_version$
DECLARE
    existing public.provider_rate_card_versions%ROWTYPE;
BEGIN
    INSERT INTO public.provider_rate_card_versions (
        version_id, source, source_version, observed_at, effective_at, expires_at, rate_cards
    ) VALUES (
        p_version_id, p_source, p_source_version, p_observed_at, p_effective_at, p_expires_at, p_rate_cards
    )
    ON CONFLICT (version_id) DO NOTHING;
    IF FOUND THEN
        RETURN 'recorded';
    END IF;

    SELECT * INTO STRICT existing
      FROM public.provider_rate_card_versions
     WHERE version_id = p_version_id;
    IF existing.source <> p_source OR existing.source_version <> p_source_version OR
       existing.observed_at <> p_observed_at OR existing.effective_at <> p_effective_at OR
       existing.expires_at IS DISTINCT FROM p_expires_at OR existing.rate_cards <> p_rate_cards THEN
        RAISE EXCEPTION 'provider rate card version conflict';
    END IF;
    RETURN 'replayed';
END
$kaana_register_provider_rate_card_version$;

REVOKE ALL ON provider_rate_card_versions FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_refuse_rate_card_mutation() FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_register_provider_rate_card_version(TEXT, TEXT, TEXT, TIMESTAMPTZ, TIMESTAMPTZ, TIMESTAMPTZ, JSONB) FROM PUBLIC;
GRANT SELECT ON provider_rate_card_versions TO kaana_credential_admin;
GRANT EXECUTE ON FUNCTION kaana_register_provider_rate_card_version(TEXT, TEXT, TEXT, TIMESTAMPTZ, TIMESTAMPTZ, TIMESTAMPTZ, JSONB) TO kaana_runtime;
