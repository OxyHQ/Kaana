-- Outcome reconciliation is metadata-only. The secret digest remains private
-- to Kaana's mutation idempotency record and is never required from Oxy.

DROP FUNCTION kaana_get_customer_provider_credential_outcome(
    TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, TEXT
);

CREATE FUNCTION kaana_get_customer_provider_credential_outcome(
    p_operation_id TEXT,
    p_action TEXT,
    p_provider_slug TEXT,
    p_owner_account_id TEXT,
    p_connection_id TEXT,
    p_environment TEXT,
    p_credential_handle TEXT,
    p_expected_revision BIGINT
) RETURNS TABLE(
    outcome_status TEXT,
    resolved_handle TEXT,
    resolved_revision BIGINT
)
LANGUAGE sql
SECURITY DEFINER
STABLE
SET search_path = pg_catalog, public
AS $kaana_customer_outcome_without_digest$
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
      AND stored.outcome_status IN ('applied', 'conflict')
$kaana_customer_outcome_without_digest$;

REVOKE ALL ON FUNCTION kaana_get_customer_provider_credential_outcome(
    TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT
) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION kaana_get_customer_provider_credential_outcome(
    TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT
) TO kaana_customer_credential_control;
