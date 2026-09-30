-- Protected operator metadata for platform provider credentials (#72).
--
-- A key's identity stays its opaque (provider_slug, key_id). Everything a human
-- needs to know about it — which account funds it, what that account is called,
-- whether its capacity is a trial or paid, whether commercial use was granted
-- and on what evidence, what it may serve — lives here, beside the secret row
-- and never in it. Only kaana_credential_admin can read or write any of it:
-- the inference runtime role has no grant, so no request path can load a
-- label or an email address, let alone put one in a response.
SET LOCAL lock_timeout = '5s';

-- One upstream account that funds one or more keys. Two keys naming the same
-- funding account are the operator stating that they share capacity; Kaana
-- never infers that two keys are independent.
CREATE TABLE provider_funding_accounts (
    funding_account_id TEXT PRIMARY KEY CHECK (funding_account_id ~ '^kfa_[0-9a-f]{32}$'),
    provider_slug TEXT NOT NULL CHECK (provider_slug ~ '^[a-z0-9]+([._-][a-z0-9]+)*$'),
    -- Protected: an account email or console name. Never an identity.
    account_label TEXT NOT NULL CHECK (length(account_label) BETWEEN 1 AND 320 AND account_label = btrim(account_label)),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE provider_credential_descriptions (
    provider_slug TEXT NOT NULL,
    key_id TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK (revision >= 1),
    title TEXT NOT NULL CHECK (length(title) BETWEEN 1 AND 200 AND title = btrim(title)),
    funding_account_id TEXT NOT NULL REFERENCES provider_funding_accounts (funding_account_id),
    capacity_category TEXT NOT NULL CHECK (capacity_category IN ('trial', 'promotional', 'prepaid', 'paid')),
    environment TEXT NOT NULL CHECK (environment IN ('production', 'staging', 'development')),
    commercial_use TEXT NOT NULL CHECK (commercial_use IN ('permitted', 'not_permitted', 'unknown')),
    -- What the eligibility rests on, for this exact key. It is never inferred
    -- from the capacity category: a trial key can carry an explicit grant.
    commercial_use_evidence TEXT CHECK (commercial_use_evidence IS NULL OR
        (length(commercial_use_evidence) BETWEEN 1 AND 2000 AND commercial_use_evidence = btrim(commercial_use_evidence))),
    -- NULL is unrestricted; a list names the only model references or
    -- capabilities the key may serve. An empty list would serve nothing and is
    -- refused rather than stored.
    allowed_models TEXT[] CHECK (allowed_models IS NULL OR cardinality(allowed_models) BETWEEN 1 AND 256),
    allowed_capabilities TEXT[] CHECK (allowed_capabilities IS NULL OR cardinality(allowed_capabilities) BETWEEN 1 AND 64),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (provider_slug, key_id),
    FOREIGN KEY (provider_slug, key_id) REFERENCES provider_credentials (provider_slug, key_id),
    CHECK ((commercial_use = 'unknown' AND commercial_use_evidence IS NULL) OR
           (commercial_use <> 'unknown' AND commercial_use_evidence IS NOT NULL))
);

CREATE INDEX provider_credential_descriptions_funding_idx
    ON provider_credential_descriptions (funding_account_id);

-- Every accepted put, whole, keyed by its exact operation id: the audit trail
-- and the idempotency ledger are the same rows.
CREATE TABLE provider_credential_description_operations (
    operation_id TEXT PRIMARY KEY CHECK (operation_id ~ '^kcm_[0-9a-f]{32}$'),
    provider_slug TEXT NOT NULL,
    key_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    document JSONB NOT NULL,
    operation_actor TEXT NOT NULL CHECK (length(operation_actor) BETWEEN 1 AND 256),
    database_actor TEXT NOT NULL DEFAULT SESSION_USER,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (provider_slug, key_id, revision)
);

-- Quota, balance and expiry facts about a key's capacity, as observed. Append
-- only: a newer observation is a new row, and freshness is the observation's
-- own fresh_until, so a stale balance is never read as a current one.
CREATE TABLE provider_credential_capacity_evidence (
    evidence_id TEXT PRIMARY KEY CHECK (evidence_id ~ '^kce_[0-9a-f]{32}$'),
    provider_slug TEXT NOT NULL,
    key_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('quota', 'balance', 'expiry')),
    source TEXT NOT NULL CHECK (source IN ('provider_api', 'provider_console', 'provider_email', 'operator')),
    observed_at TIMESTAMPTZ NOT NULL,
    fresh_until TIMESTAMPTZ,
    quota_unit TEXT CHECK (quota_unit IS NULL OR quota_unit ~ '^[a-z][a-z0-9_]{0,63}$'),
    quota_window TEXT CHECK (quota_window IS NULL OR quota_window IN ('minute', 'hour', 'day', 'month', 'lifetime')),
    quota_limit NUMERIC(30, 0) CHECK (quota_limit IS NULL OR quota_limit >= 0),
    quota_remaining NUMERIC(30, 0) CHECK (quota_remaining IS NULL OR quota_remaining >= 0),
    balance_currency CHAR(3) CHECK (balance_currency IS NULL OR balance_currency ~ '^[A-Z]{3}$'),
    balance_picos NUMERIC(30, 0) CHECK (balance_picos IS NULL OR balance_picos >= 0),
    expires_at TIMESTAMPTZ,
    note TEXT CHECK (note IS NULL OR (length(note) BETWEEN 1 AND 2000)),
    operation_actor TEXT NOT NULL CHECK (length(operation_actor) BETWEEN 1 AND 256),
    database_actor TEXT NOT NULL DEFAULT SESSION_USER,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (provider_slug, key_id) REFERENCES provider_credentials (provider_slug, key_id),
    CHECK (fresh_until IS NULL OR fresh_until > observed_at),
    CHECK (
        (kind = 'quota' AND quota_unit IS NOT NULL AND quota_window IS NOT NULL AND quota_remaining IS NOT NULL
            AND (quota_limit IS NULL OR quota_remaining <= quota_limit)
            AND balance_currency IS NULL AND balance_picos IS NULL AND expires_at IS NULL)
        OR (kind = 'balance' AND balance_currency IS NOT NULL AND balance_picos IS NOT NULL
            AND quota_unit IS NULL AND quota_window IS NULL AND quota_limit IS NULL AND quota_remaining IS NULL
            AND expires_at IS NULL)
        OR (kind = 'expiry' AND expires_at IS NOT NULL
            AND quota_unit IS NULL AND quota_window IS NULL AND quota_limit IS NULL AND quota_remaining IS NULL
            AND balance_currency IS NULL AND balance_picos IS NULL)
    )
);

CREATE INDEX provider_credential_capacity_evidence_key_time_idx
    ON provider_credential_capacity_evidence (provider_slug, key_id, kind, observed_at DESC);

CREATE FUNCTION kaana_refuse_credential_ledger_mutation() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path = pg_catalog
AS $body$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME;
END
$body$;

CREATE TRIGGER provider_credential_description_operations_append_only
BEFORE UPDATE OR DELETE ON provider_credential_description_operations
FOR EACH ROW EXECUTE FUNCTION kaana_refuse_credential_ledger_mutation();

CREATE TRIGGER provider_credential_capacity_evidence_append_only
BEFORE UPDATE OR DELETE ON provider_credential_capacity_evidence
FOR EACH ROW EXECUTE FUNCTION kaana_refuse_credential_ledger_mutation();

CREATE TRIGGER provider_funding_accounts_append_only
BEFORE UPDATE OR DELETE ON provider_funding_accounts
FOR EACH ROW EXECUTE FUNCTION kaana_refuse_credential_ledger_mutation();

-- Puts the whole metadata document for one exact key. The same operation id
-- with the same document is a replay; with any other document it is a
-- conflict. A funding account is created by its first use and its provider and
-- label never change afterwards: a different label under an existing id is a
-- different account and is refused.
CREATE FUNCTION kaana_put_provider_credential_description(
    p_operation_id TEXT,
    p_document JSONB,
    p_operation_actor TEXT
) RETURNS TEXT
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_put_provider_credential_description$
DECLARE
    v_provider TEXT := p_document->>'provider';
    v_key_id TEXT := p_document->>'keyId';
    v_account_id TEXT := p_document#>>'{fundingAccount,id}';
    v_account_label TEXT := p_document#>>'{fundingAccount,label}';
    v_revision INTEGER;
    v_models TEXT[];
    v_capabilities TEXT[];
    existing_operation public.provider_credential_description_operations%ROWTYPE;
    existing_account public.provider_funding_accounts%ROWTYPE;
BEGIN
    IF p_operation_actor IS NULL OR length(p_operation_actor) NOT BETWEEN 1 AND 256
       OR p_operation_actor <> btrim(p_operation_actor) THEN
        RAISE EXCEPTION 'operation actor is invalid';
    END IF;
    IF p_operation_id IS NULL OR p_operation_id !~ '^kcm_[0-9a-f]{32}$'
       OR p_document IS NULL OR jsonb_typeof(p_document) <> 'object'
       OR p_document->>'operationId' IS DISTINCT FROM p_operation_id THEN
        RAISE EXCEPTION 'credential metadata operation is invalid';
    END IF;

    SELECT * INTO existing_operation
      FROM public.provider_credential_description_operations
     WHERE operation_id = p_operation_id;
    IF FOUND THEN
        IF existing_operation.document <> p_document THEN
            RAISE EXCEPTION 'credential metadata operation conflict';
        END IF;
        RETURN 'replayed';
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM public.provider_credentials
         WHERE provider_slug = v_provider AND key_id = v_key_id
    ) THEN
        RAISE EXCEPTION 'credential metadata names no provider credential';
    END IF;

    SELECT * INTO existing_account
      FROM public.provider_funding_accounts
     WHERE funding_account_id = v_account_id;
    IF FOUND THEN
        IF existing_account.provider_slug <> v_provider OR existing_account.account_label <> v_account_label THEN
            RAISE EXCEPTION 'funding account identity conflict';
        END IF;
    ELSE
        INSERT INTO public.provider_funding_accounts (funding_account_id, provider_slug, account_label)
        VALUES (v_account_id, v_provider, v_account_label);
    END IF;

    IF jsonb_typeof(p_document#>'{restrictions,models}') = 'array' THEN
        SELECT array_agg(value ORDER BY value) INTO v_models
          FROM jsonb_array_elements_text(p_document#>'{restrictions,models}');
        v_models := COALESCE(v_models, ARRAY[]::TEXT[]);
    END IF;
    IF jsonb_typeof(p_document#>'{restrictions,capabilities}') = 'array' THEN
        SELECT array_agg(value ORDER BY value) INTO v_capabilities
          FROM jsonb_array_elements_text(p_document#>'{restrictions,capabilities}');
        v_capabilities := COALESCE(v_capabilities, ARRAY[]::TEXT[]);
    END IF;

    INSERT INTO public.provider_credential_descriptions (
        provider_slug, key_id, revision, title, funding_account_id, capacity_category,
        environment, commercial_use, commercial_use_evidence, allowed_models, allowed_capabilities
    ) VALUES (
        v_provider, v_key_id, 1, p_document->>'title', v_account_id, p_document->>'capacityCategory',
        p_document->>'environment', p_document#>>'{commercialUse,eligibility}',
        p_document#>>'{commercialUse,evidence}', v_models, v_capabilities
    )
    ON CONFLICT (provider_slug, key_id) DO UPDATE SET
        revision = public.provider_credential_descriptions.revision + 1,
        title = EXCLUDED.title,
        funding_account_id = EXCLUDED.funding_account_id,
        capacity_category = EXCLUDED.capacity_category,
        environment = EXCLUDED.environment,
        commercial_use = EXCLUDED.commercial_use,
        commercial_use_evidence = EXCLUDED.commercial_use_evidence,
        allowed_models = EXCLUDED.allowed_models,
        allowed_capabilities = EXCLUDED.allowed_capabilities,
        updated_at = NOW()
    RETURNING revision INTO v_revision;

    INSERT INTO public.provider_credential_description_operations (
        operation_id, provider_slug, key_id, revision, document, operation_actor, database_actor
    ) VALUES (
        p_operation_id, v_provider, v_key_id, v_revision, p_document, p_operation_actor, session_user
    );
    RETURN 'applied';
END
$kaana_put_provider_credential_description$;

-- Records one capacity observation. The same evidence id with the same facts
-- is a replay; with different facts it is a conflict.
CREATE FUNCTION kaana_record_provider_credential_capacity_evidence(
    p_document JSONB,
    p_operation_actor TEXT
) RETURNS TEXT
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_record_provider_credential_capacity_evidence$
DECLARE
    candidate public.provider_credential_capacity_evidence%ROWTYPE;
    existing public.provider_credential_capacity_evidence%ROWTYPE;
BEGIN
    IF p_operation_actor IS NULL OR length(p_operation_actor) NOT BETWEEN 1 AND 256
       OR p_operation_actor <> btrim(p_operation_actor) THEN
        RAISE EXCEPTION 'operation actor is invalid';
    END IF;
    IF p_document IS NULL OR jsonb_typeof(p_document) <> 'object' THEN
        RAISE EXCEPTION 'capacity evidence is invalid';
    END IF;

    candidate.evidence_id := p_document->>'evidenceId';
    candidate.provider_slug := p_document->>'provider';
    candidate.key_id := p_document->>'keyId';
    candidate.kind := p_document->>'kind';
    candidate.source := p_document->>'source';
    candidate.observed_at := (p_document->>'observedAt')::TIMESTAMPTZ;
    candidate.fresh_until := (p_document->>'freshUntil')::TIMESTAMPTZ;
    candidate.quota_unit := p_document#>>'{quota,unit}';
    candidate.quota_window := p_document#>>'{quota,window}';
    candidate.quota_limit := (p_document#>>'{quota,limit}')::NUMERIC;
    candidate.quota_remaining := (p_document#>>'{quota,remaining}')::NUMERIC;
    candidate.balance_currency := p_document#>>'{balance,currency}';
    candidate.balance_picos := (p_document#>>'{balance,amountPicos}')::NUMERIC;
    candidate.expires_at := (p_document->>'expiresAt')::TIMESTAMPTZ;
    candidate.note := p_document->>'note';

    INSERT INTO public.provider_credential_capacity_evidence (
        evidence_id, provider_slug, key_id, kind, source, observed_at, fresh_until,
        quota_unit, quota_window, quota_limit, quota_remaining, balance_currency, balance_picos,
        expires_at, note, operation_actor, database_actor
    ) VALUES (
        candidate.evidence_id, candidate.provider_slug, candidate.key_id, candidate.kind, candidate.source,
        candidate.observed_at, candidate.fresh_until, candidate.quota_unit, candidate.quota_window,
        candidate.quota_limit, candidate.quota_remaining, candidate.balance_currency, candidate.balance_picos,
        candidate.expires_at, candidate.note, p_operation_actor, session_user
    )
    ON CONFLICT (evidence_id) DO NOTHING;
    IF FOUND THEN
        RETURN 'recorded';
    END IF;

    SELECT * INTO STRICT existing
      FROM public.provider_credential_capacity_evidence
     WHERE evidence_id = candidate.evidence_id;
    IF existing.provider_slug <> candidate.provider_slug OR existing.key_id <> candidate.key_id
       OR existing.kind <> candidate.kind OR existing.source <> candidate.source
       OR existing.observed_at <> candidate.observed_at
       OR existing.fresh_until IS DISTINCT FROM candidate.fresh_until
       OR existing.quota_unit IS DISTINCT FROM candidate.quota_unit
       OR existing.quota_window IS DISTINCT FROM candidate.quota_window
       OR existing.quota_limit IS DISTINCT FROM candidate.quota_limit
       OR existing.quota_remaining IS DISTINCT FROM candidate.quota_remaining
       OR existing.balance_currency IS DISTINCT FROM candidate.balance_currency
       OR existing.balance_picos IS DISTINCT FROM candidate.balance_picos
       OR existing.expires_at IS DISTINCT FROM candidate.expires_at
       OR existing.note IS DISTINCT FROM candidate.note THEN
        RAISE EXCEPTION 'capacity evidence identity conflict';
    END IF;
    RETURN 'replayed';
END
$kaana_record_provider_credential_capacity_evidence$;

REVOKE ALL ON provider_funding_accounts, provider_credential_descriptions,
    provider_credential_description_operations, provider_credential_capacity_evidence FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_refuse_credential_ledger_mutation() FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_put_provider_credential_description(TEXT, JSONB, TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_record_provider_credential_capacity_evidence(JSONB, TEXT) FROM PUBLIC;

GRANT SELECT ON provider_funding_accounts, provider_credential_descriptions,
    provider_credential_description_operations, provider_credential_capacity_evidence TO kaana_credential_admin;
GRANT EXECUTE ON FUNCTION kaana_put_provider_credential_description(TEXT, JSONB, TEXT) TO kaana_credential_admin;
GRANT EXECUTE ON FUNCTION kaana_record_provider_credential_capacity_evidence(JSONB, TEXT) TO kaana_credential_admin;
