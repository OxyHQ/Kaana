-- An exact deployment binding is an optional operator PIN (key-pools.md): a
-- deployment with none executes on its provider's whole key pool. Removing a
-- pin is therefore an ordinary, audited operator action, and this migration
-- adds the one mutation that does it. No runtime authority changes: serving
-- keeps reading the binding table it could already read.
SET LOCAL lock_timeout = '5s';

-- Every binding operation now says what it did. Existing rows were binds.
ALTER TABLE provider_deployment_binding_operations
    ADD COLUMN action TEXT NOT NULL DEFAULT 'bind' CHECK (action IN ('bind', 'unpin'));

-- The bind function is restated only so that a replay compares the action
-- too: an unpin's operation id replayed as a bind must be a conflict.
CREATE OR REPLACE FUNCTION kaana_bind_provider_deployment(TEXT, TEXT, TEXT, TEXT, TEXT)
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
        IF existing.action = 'bind' AND existing.deployment_id = $2 AND existing.provider_slug = $3 AND existing.key_id = $4 AND existing.operation_actor = $5 THEN RETURN 'replayed'; END IF;
        RETURN 'conflict';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.provider_credentials WHERE provider_slug = $3 AND key_id = $4 AND enabled) THEN RETURN 'invalid'; END IF;
    INSERT INTO public.provider_deployment_binding_operations(operation_id,deployment_id,provider_slug,key_id,operation_actor,database_actor,action) VALUES ($1,$2,$3,$4,$5,session_user,'bind');
    INSERT INTO public.provider_deployment_credential_bindings(deployment_id, provider_slug, key_id, last_operation_id, operation_actor)
      VALUES ($2,$3,$4,$1,$5)
      ON CONFLICT (deployment_id) DO UPDATE SET provider_slug=EXCLUDED.provider_slug,key_id=EXCLUDED.key_id,last_operation_id=EXCLUDED.last_operation_id,operation_actor=EXCLUDED.operation_actor,updated_at=NOW();
    RETURN 'applied';
END $kaana_bind$;

-- Removes one exact pin, compare-and-delete: the operator names the pin they
-- expect (provider and key), so a pin somebody changed meanwhile is a
-- conflict rather than silently removed. The operation row is the audit
-- record; the binding row goes, and the deployment executes on its pool from
-- the next credential reload.
--
-- Outcomes: applied, replayed (same operation id, same facts), conflict (the
-- id was used for something else, or the current pin is another key), absent
-- (no pin for that deployment), invalid.
CREATE FUNCTION kaana_unpin_provider_deployment(TEXT, TEXT, TEXT, TEXT, TEXT)
RETURNS TEXT LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $kaana_unpin$
DECLARE
    existing provider_deployment_binding_operations%ROWTYPE;
    current_pin provider_deployment_credential_bindings%ROWTYPE;
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
        IF existing.action = 'unpin' AND existing.deployment_id = $2 AND existing.provider_slug = $3 AND existing.key_id = $4 AND existing.operation_actor = $5 THEN RETURN 'replayed'; END IF;
        RETURN 'conflict';
    END IF;
    SELECT * INTO current_pin FROM public.provider_deployment_credential_bindings WHERE deployment_id = $2 FOR UPDATE;
    IF NOT FOUND THEN RETURN 'absent'; END IF;
    IF current_pin.provider_slug <> $3 OR current_pin.key_id <> $4 THEN RETURN 'conflict'; END IF;
    INSERT INTO public.provider_deployment_binding_operations(operation_id,deployment_id,provider_slug,key_id,operation_actor,database_actor,action) VALUES ($1,$2,$3,$4,$5,session_user,'unpin');
    DELETE FROM public.provider_deployment_credential_bindings WHERE deployment_id = $2;
    RETURN 'applied';
END $kaana_unpin$;

REVOKE ALL ON FUNCTION kaana_unpin_provider_deployment(TEXT,TEXT,TEXT,TEXT,TEXT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION kaana_unpin_provider_deployment(TEXT,TEXT,TEXT,TEXT,TEXT) TO kaana_credential_admin;
