package credentialstore

import (
	"strings"
	"testing"
)

func TestRuntimeMigrationHasExactKeyStateLeaseAndAttemptAuthorities(t *testing.T) {
	for _, required := range []string{
		"PRIMARY KEY (provider_slug, key_id)",
		"SET LOCAL lock_timeout = '5s'",
		"kaana_claim_provider_credential_recovery",
		"RETURN 'claimed'", "RETURN 'busy'", "RETURN 'usable'",
		"state = 'usable'", "observed_at <= EXCLUDED.observed_at",
		"provider_credential_attempt_events",
		"PRIMARY KEY (request_id, deployment_id, credential_attempt_index)",
		"AFTER UPDATE OF encrypted_secret ON provider_credentials",
		"TO kaana_runtime",
		"SET search_path = pg_catalog",
	} {
		if !strings.Contains(migration0011, required) {
			t.Errorf("runtime migration lost %q", required)
		}
	}
	for _, forbidden := range []string{"encrypted_secret BYTEA", "GRANT INSERT", "GRANT UPDATE", "GRANT DELETE"} {
		if strings.Contains(migration0011, forbidden) {
			t.Errorf("runtime migration contains forbidden authority %q", forbidden)
		}
	}
}

func TestRuntimeStateReadAuthorityIsNarrow(t *testing.T) {
	for _, required := range []string{
		"SET LOCAL lock_timeout = '5s'",
		"GRANT SELECT ON provider_credential_runtime_state TO kaana_runtime",
	} {
		if !strings.Contains(migration0012, required) {
			t.Errorf("runtime state read-authority migration lost %q", required)
		}
	}
	for _, forbidden := range []string{
		"provider_credential_attempt_events",
		"GRANT INSERT", "GRANT UPDATE", "GRANT DELETE", "GRANT ALL",
	} {
		if strings.Contains(migration0012, forbidden) {
			t.Errorf("runtime state read-authority migration contains forbidden authority %q", forbidden)
		}
	}
}
