-- A destroyed key disappears once nothing still needs its identity.
--
-- Migration 0025 removes a dead key's secret and keeps its identity because
-- cost events, attempts and claims name it. Those rows age out under the
-- retention policy (docs/operating.md, "Data retention"); once none is left,
-- the identity has nothing to resolve and the key's own operator records (its
-- description, the documents that described it, its capacity evidence) are
-- protected metadata about a credential that no longer exists.
--
-- kaana_forget_destroyed_provider_credentials deletes such a key, destroyed
-- at least 30 days ago, entirely and
-- writes one 'forget' audit row. What remains, by value and without any
-- secret or label: the audit rows (who put, disabled, destroyed and forgot
-- it, and when) and its provider_credential_destructions record.
SET LOCAL lock_timeout = '5s';

ALTER TABLE public.provider_credential_audit
    DROP CONSTRAINT provider_credential_audit_action_check;

ALTER TABLE public.provider_credential_audit
    ADD CONSTRAINT provider_credential_audit_action_check
    CHECK (action IN (
        'put',
        'disable',
        'rekey_disable',
        'rekey_create',
        'deduplicate_disable',
        'destroy',
        'forget'
    ));

-- The append-only ledgers stay append-only, with one exception: the forget
-- function may delete the rows of the exact destroyed key it is forgetting.
-- It states that key in a transaction-local setting, and the row's key must
-- also be destroyed, so the exception cannot reach a live key's records.
-- Privileges are the outer boundary: no application role has DELETE on these
-- tables, only EXECUTE on the SECURITY DEFINER function.
CREATE OR REPLACE FUNCTION kaana_refuse_credential_ledger_mutation() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path = pg_catalog
AS $body$
BEGIN
    IF TG_OP = 'DELETE'
       AND TG_TABLE_NAME IN ('provider_credential_description_operations', 'provider_credential_capacity_evidence')
       AND current_setting('kaana.forget_credential', true) = OLD.provider_slug || '/' || OLD.key_id
       AND EXISTS (
           SELECT 1 FROM public.provider_credentials AS credential
           WHERE credential.provider_slug = OLD.provider_slug
             AND credential.key_id = OLD.key_id
             AND credential.destroyed_at IS NOT NULL
       ) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME;
END
$body$;

-- The destroyed-is-final trigger (0025) guards UPDATE of a row that still
-- exists; once forgotten there is no row, so a put, import or rekey could
-- INSERT the identity again. The destruction ledger outlives the row, and an
-- identity it names is never created again.
CREATE FUNCTION public.kaana_refuse_destroyed_provider_credential_identity() RETURNS TRIGGER
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog
AS $kaana_refuse_destroyed_identity$
BEGIN
    IF EXISTS (
        SELECT 1 FROM public.provider_credential_destructions AS destroyed
        WHERE destroyed.provider_slug = NEW.provider_slug AND destroyed.key_id = NEW.key_id
    ) THEN
        RAISE EXCEPTION 'provider credential %/% was destroyed; its identity is never created again',
            NEW.provider_slug, NEW.key_id;
    END IF;
    RETURN NEW;
END
$kaana_refuse_destroyed_identity$;

CREATE TRIGGER provider_credential_destroyed_identity_is_final
BEFORE INSERT ON public.provider_credentials
FOR EACH ROW EXECUTE FUNCTION public.kaana_refuse_destroyed_provider_credential_identity();

REVOKE ALL ON FUNCTION public.kaana_refuse_destroyed_provider_credential_identity() FROM PUBLIC;

CREATE FUNCTION public.kaana_forget_destroyed_provider_credentials(
    p_operation_actor TEXT,
    p_limit INTEGER,
    p_dry_run BOOLEAN
) RETURNS BIGINT
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_forget$
DECLARE
    candidate RECORD;
    affected BIGINT := 0;
BEGIN
    IF p_operation_actor IS NULL
       OR length(p_operation_actor) NOT BETWEEN 1 AND 256
       OR p_operation_actor <> btrim(p_operation_actor)
       OR p_operation_actor ~ E'[\r\n]'
       OR p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 10000
       OR p_dry_run IS NULL THEN
        RAISE EXCEPTION 'forgetting destroyed credentials is invalid';
    END IF;

    FOR candidate IN
        SELECT credential.provider_slug, credential.key_id
        FROM public.provider_credentials AS credential
        WHERE credential.destroyed_at IS NOT NULL
          -- Persisted history is not the only thing that names a key: a
          -- request that took the key from a pool before it was disabled
          -- writes its attempt and cost (with its settlement, atomically)
          -- only after the upstream answers. Forgetting under it would make
          -- that write fail its foreign key on every retry. No request lives
          -- for 30 days; the destroyed_at clock starts after the disable.
          AND credential.destroyed_at < clock_timestamp() - INTERVAL '30 days'
          AND NOT EXISTS (SELECT 1 FROM public.provider_cost_events AS e
                          WHERE e.provider_slug = credential.provider_slug AND e.key_id = credential.key_id)
          AND NOT EXISTS (SELECT 1 FROM public.provider_credential_attempt_events AS e
                          WHERE e.provider_slug = credential.provider_slug AND e.key_id = credential.key_id)
          AND NOT EXISTS (SELECT 1 FROM public.scoped_provider_attempt_claims AS c
                          WHERE c.provider_slug = credential.provider_slug AND c.key_id = credential.key_id
                            AND c.expires_at > clock_timestamp())
          AND NOT EXISTS (SELECT 1 FROM public.provider_deployment_credential_bindings AS b
                          WHERE b.provider_slug = credential.provider_slug AND b.key_id = credential.key_id)
        ORDER BY credential.destroyed_at, credential.provider_slug, credential.key_id
        LIMIT p_limit
        FOR UPDATE SKIP LOCKED
    LOOP
        IF p_dry_run THEN
            affected := affected + 1;
            CONTINUE;
        END IF;
        BEGIN
            PERFORM set_config('kaana.forget_credential', candidate.provider_slug || '/' || candidate.key_id, true);
            DELETE FROM public.scoped_provider_attempt_claims
             WHERE provider_slug = candidate.provider_slug AND key_id = candidate.key_id;
            DELETE FROM public.provider_credential_capacity_evidence
             WHERE provider_slug = candidate.provider_slug AND key_id = candidate.key_id;
            DELETE FROM public.provider_credential_descriptions
             WHERE provider_slug = candidate.provider_slug AND key_id = candidate.key_id;
            DELETE FROM public.provider_credential_description_operations
             WHERE provider_slug = candidate.provider_slug AND key_id = candidate.key_id;
            DELETE FROM public.provider_credentials
             WHERE provider_slug = candidate.provider_slug AND key_id = candidate.key_id
               AND destroyed_at IS NOT NULL;
            PERFORM set_config('kaana.forget_credential', '', true);
            INSERT INTO public.provider_credential_audit (
                provider_slug, key_id, action, operation_actor, database_actor
            ) VALUES (
                candidate.provider_slug, candidate.key_id, 'forget', p_operation_actor, session_user
            );
            affected := affected + 1;
        EXCEPTION WHEN foreign_key_violation THEN
            -- A row naming the key committed after the check (a cost event of
            -- a request still in flight when the key was disabled). The key
            -- keeps its identity and is retried on the next run.
            PERFORM set_config('kaana.forget_credential', '', true);
        END;
    END LOOP;
    RETURN affected;
END
$kaana_forget$;

REVOKE ALL ON FUNCTION public.kaana_forget_destroyed_provider_credentials(TEXT, INTEGER, BOOLEAN) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.kaana_forget_destroyed_provider_credentials(TEXT, INTEGER, BOOLEAN) TO kaana_credential_admin;
