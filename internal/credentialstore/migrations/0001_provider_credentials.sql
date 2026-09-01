CREATE TABLE provider_credentials (
    provider_slug TEXT NOT NULL
        CHECK (provider_slug ~ '^[a-z0-9]+([._-][a-z0-9]+)*$'),
    key_id TEXT NOT NULL
        CHECK (key_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
    encrypted_secret BYTEA NOT NULL
        CHECK (octet_length(encrypted_secret) > 0),
    kms_key_arn TEXT NOT NULL
        CHECK (kms_key_arn ~ '^arn:[^:]+:kms:[^:]+:[0-9]+:key/[A-Za-z0-9-]+$'),
    key_class TEXT NOT NULL DEFAULT ''
        CHECK (key_class IN ('', 'free', 'paid')),
    budget_usd NUMERIC(14, 6)
        CHECK (budget_usd IS NULL OR budget_usd >= 0),
    position INTEGER NOT NULL
        CHECK (position > 0),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (provider_slug, key_id)
);

CREATE UNIQUE INDEX provider_credentials_enabled_position_key
    ON provider_credentials (provider_slug, position)
    WHERE enabled = TRUE;

CREATE TABLE provider_credential_audit (
    audit_id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider_slug TEXT NOT NULL,
    key_id TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('put', 'disable')),
    operation_actor TEXT NOT NULL CHECK (length(operation_actor) BETWEEN 1 AND 256),
    database_actor TEXT NOT NULL DEFAULT CURRENT_USER,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX provider_credential_audit_credential_time_idx
    ON provider_credential_audit (provider_slug, key_id, occurred_at DESC);
