package credentialstore

import (
	"strings"
	"testing"
)

func TestProviderCostMigrationKeepsTheOperatorOnlyBoundary(t *testing.T) {
	for _, required := range []string{
		"CREATE TABLE provider_cost_events",
		"PRIMARY KEY (request_id, attempt_index)",
		"FOREIGN KEY (provider_slug, key_id)",
		"source IN ('provider_reported', 'rate_card', 'unknown')",
		"source = 'unknown' AND currency IS NULL AND amount_picos IS NULL AND complete = FALSE",
		"SECURITY DEFINER",
		"SET search_path = pg_catalog, public",
		"ON CONFLICT (request_id, attempt_index) DO NOTHING",
		"provider cost event identity conflict",
		"REVOKE ALL ON provider_cost_events FROM PUBLIC",
		"GRANT SELECT ON provider_cost_events TO kaana_credential_admin",
		"TO kaana_runtime",
	} {
		if !strings.Contains(migration0008, required) {
			t.Errorf("provider cost migration lost %q", required)
		}
	}
	for _, forbidden := range []string{
		"funding_account",
		"capacity_grant",
		"reservation",
		"balance",
		"customer_account",
		"GRANT INSERT ON provider_cost_events",
		"GRANT UPDATE ON provider_cost_events",
		"GRANT DELETE ON provider_cost_events",
	} {
		if strings.Contains(strings.ToLower(migration0008), strings.ToLower(forbidden)) {
			t.Errorf("provider cost migration contains forbidden boundary %q", forbidden)
		}
	}
	if count := strings.Count(migration0008, "CREATE TABLE "); count != 1 {
		t.Errorf("provider cost table count = %d, want 1", count)
	}
	if count := strings.Count(migration0008, "CREATE FUNCTION "); count != 1 {
		t.Errorf("provider cost function count = %d, want 1", count)
	}
}

func TestProviderCostBatchMigrationIsOneAtomicRuntimeCall(t *testing.T) {
	for _, required := range []string{
		"CREATE FUNCTION kaana_record_provider_cost_events(",
		"ADD COLUMN rate_card_version_id TEXT",
		"provider_cost_events_rate_card_version_check",
		"p_events JSONB",
		"jsonb_array_length(p_events) NOT BETWEEN 1 AND 64",
		"parsed.request_id IS DISTINCT FROM expected_request_id",
		"GROUP BY parsed.request_id, parsed.attempt_index",
		"rate_card_version_id IS DISTINCT FROM event->>'rate_card_version_id'",
		"REVOKE ALL ON FUNCTION kaana_record_provider_cost_event",
		"FROM kaana_runtime",
		"GRANT EXECUTE ON FUNCTION kaana_record_provider_cost_events(JSONB) TO kaana_runtime",
	} {
		if !strings.Contains(migration0010, required) {
			t.Errorf("provider cost batch migration lost %q", required)
		}
	}
	for _, forbidden := range []string{
		"GRANT INSERT ON provider_cost_events",
		"GRANT UPDATE ON provider_cost_events",
		"GRANT DELETE ON provider_cost_events",
		"COMMIT",
	} {
		if strings.Contains(strings.ToUpper(migration0010), strings.ToUpper(forbidden)) {
			t.Errorf("provider cost batch migration contains forbidden operation %q", forbidden)
		}
	}
}

func TestProviderAttemptTelemetryMigrationReplaysEveryMeasuredFact(t *testing.T) {
	for _, required := range []string{
		"ADD COLUMN usage_units JSONB",
		"ADD COLUMN latency_ms INTEGER",
		"ADD COLUMN time_to_first_output_ms INTEGER",
		"attempt_outcome IN ('succeeded', 'failed', 'cancelled')",
		"time_to_first_output_ms BETWEEN 0 AND latency_ms",
		"CREATE FUNCTION kaana_record_provider_attempt_events(",
		"provider attempt event lacks its telemetry",
		"existing.usage_units IS DISTINCT FROM event->'usage_units'",
		"existing.latency_ms IS DISTINCT FROM (event->>'latency_ms')::INTEGER",
		"existing.failure_code IS DISTINCT FROM event->>'failure_code'",
		"provider cost event identity conflict",
		"SECURITY DEFINER",
		"REVOKE ALL ON FUNCTION kaana_record_provider_attempt_events(JSONB) FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION kaana_record_provider_attempt_events(JSONB) TO kaana_runtime",
	} {
		if !strings.Contains(migration0015, required) {
			t.Errorf("provider attempt telemetry migration lost %q", required)
		}
	}
	for _, forbidden := range []string{
		"GRANT INSERT", "GRANT UPDATE", "GRANT DELETE", "GRANT SELECT",
		"balance", "reservation", "account_label", "COMMIT",
	} {
		if strings.Contains(strings.ToUpper(migration0015), strings.ToUpper(forbidden)) {
			t.Errorf("provider attempt telemetry migration contains forbidden %q", forbidden)
		}
	}
}
