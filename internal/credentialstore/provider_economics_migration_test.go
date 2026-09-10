package credentialstore

import (
	"strings"
	"testing"
)

func TestProviderEconomicsMigrationKeepsIdentityAndMoneySeparate(t *testing.T) {
	for _, required := range []string{
		"CREATE TABLE provider_funding_accounts",
		"ALTER TABLE provider_credentials",
		"FOREIGN KEY (provider_slug, key_id)",
		"CREATE TABLE provider_capacity_grants",
		"CREATE TABLE provider_capacity_reservations",
		"UNIQUE (request_id, attempt_index)",
		"CREATE TABLE provider_cost_events",
		"CREATE TABLE provider_balance_observations",
		"CREATE TABLE provider_reconciliation_runs",
		"CREATE TABLE provider_rate_card_versions",
		"CREATE TABLE provider_rate_card_rates",
		"CREATE TABLE provider_usage_rollups",
		"amount_picos NUMERIC(30, 0)",
		"commercial_use_allowed BOOLEAN",
		"account_label TEXT",
		"CREATE FUNCTION kaana_reserve_provider_capacity",
		"FOR UPDATE OF g SKIP LOCKED",
		"CREATE FUNCTION kaana_settle_provider_capacity",
		"CREATE FUNCTION kaana_release_provider_capacity",
		"GRANT EXECUTE ON FUNCTION kaana_reserve_provider_capacity",
		"REVOKE ALL ON provider_funding_accounts",
	} {
		if !strings.Contains(migration0008, required) {
			t.Errorf("provider economics migration lacks %q", required)
		}
	}
	for _, forbidden := range []string{
		"DOUBLE PRECISION",
		"REAL",
		"encrypted_secret",
		"GRANT SELECT ON provider_credentials",
	} {
		if strings.Contains(migration0008, forbidden) {
			t.Errorf("provider economics migration contains forbidden %q", forbidden)
		}
	}
}
