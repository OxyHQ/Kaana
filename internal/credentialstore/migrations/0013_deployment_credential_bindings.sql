CREATE TABLE provider_deployment_credential_bindings (
    deployment_id TEXT PRIMARY KEY CHECK (length(deployment_id) BETWEEN 1 AND 256),
    provider_slug TEXT NOT NULL,
    key_id TEXT NOT NULL,
    last_operation_id TEXT NOT NULL CHECK (last_operation_id ~ '^kdb_[0-9a-f]{32}$'),
    operation_actor TEXT NOT NULL CHECK (length(operation_actor) BETWEEN 1 AND 256),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (provider_slug, key_id)
        REFERENCES provider_credentials(provider_slug, key_id)
        ON UPDATE CASCADE ON DELETE RESTRICT
);

CREATE TABLE provider_deployment_binding_operations (
    operation_id TEXT PRIMARY KEY CHECK (operation_id ~ '^kdb_[0-9a-f]{32}$'),
    deployment_id TEXT NOT NULL,
    provider_slug TEXT NOT NULL,
    key_id TEXT NOT NULL,
    operation_actor TEXT NOT NULL,
    database_actor TEXT NOT NULL,
    completed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE provider_deployment_credential_bindings
    ADD FOREIGN KEY (last_operation_id)
    REFERENCES provider_deployment_binding_operations(operation_id)
    ON UPDATE RESTRICT ON DELETE RESTRICT;

CREATE FUNCTION kaana_bind_provider_deployment(TEXT, TEXT, TEXT, TEXT, TEXT)
RETURNS TEXT LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $kaana_bind$
DECLARE existing provider_deployment_binding_operations%ROWTYPE;
BEGIN
    IF $1 !~ '^kdb_[0-9a-f]{32}$' OR length($2) NOT BETWEEN 1 AND 256
       OR $3 !~ '^[a-z0-9]+([._-][a-z0-9]+)*$' OR length($4) NOT BETWEEN 1 AND 128
       OR length($5) NOT BETWEEN 1 AND 256 OR $5 <> btrim($5) OR $5 ~ E'[\r\n]' THEN
        RETURN 'invalid';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('kaana:deployment-binding-operation:' || $1, 0));
    PERFORM pg_advisory_xact_lock(hashtextextended('kaana:deployment-credential-binding:' || $2, 0));
    SELECT * INTO existing FROM public.provider_deployment_binding_operations WHERE operation_id = $1 FOR UPDATE;
    IF FOUND THEN
        IF existing.deployment_id = $2 AND existing.provider_slug = $3 AND existing.key_id = $4 AND existing.operation_actor = $5 THEN RETURN 'replayed'; END IF;
        RETURN 'conflict';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.provider_credentials WHERE provider_slug = $3 AND key_id = $4 AND enabled) THEN RETURN 'invalid'; END IF;
    INSERT INTO public.provider_deployment_binding_operations(operation_id,deployment_id,provider_slug,key_id,operation_actor,database_actor) VALUES ($1,$2,$3,$4,$5,session_user);
    INSERT INTO public.provider_deployment_credential_bindings(deployment_id, provider_slug, key_id, last_operation_id, operation_actor)
      VALUES ($2,$3,$4,$1,$5)
      ON CONFLICT (deployment_id) DO UPDATE SET provider_slug=EXCLUDED.provider_slug,key_id=EXCLUDED.key_id,last_operation_id=EXCLUDED.last_operation_id,operation_actor=EXCLUDED.operation_actor,updated_at=NOW();
    RETURN 'applied';
END $kaana_bind$;

REVOKE ALL ON provider_deployment_credential_bindings FROM PUBLIC;
REVOKE ALL ON provider_deployment_binding_operations FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_bind_provider_deployment(TEXT,TEXT,TEXT,TEXT,TEXT) FROM PUBLIC;
GRANT SELECT ON provider_deployment_credential_bindings TO kaana_runtime;
CREATE VIEW provider_deployment_binding_metadata AS
SELECT b.deployment_id, b.provider_slug, b.key_id, b.last_operation_id,
       b.operation_actor, o.database_actor, b.updated_at
FROM provider_deployment_credential_bindings b
JOIN provider_deployment_binding_operations o ON o.operation_id = b.last_operation_id;
REVOKE ALL ON provider_deployment_binding_metadata FROM PUBLIC;
GRANT SELECT ON provider_deployment_binding_metadata, provider_deployment_binding_operations TO kaana_credential_admin;
GRANT EXECUTE ON FUNCTION kaana_bind_provider_deployment(TEXT,TEXT,TEXT,TEXT,TEXT) TO kaana_credential_admin;
