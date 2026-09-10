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
