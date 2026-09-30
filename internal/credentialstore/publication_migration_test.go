package credentialstore

import (
	"strings"
	"testing"
)

// Migration 0019 grants the publisher's role exactly one read, and that read
// is a SECURITY DEFINER function with a pinned search_path and a bounded
// window — never SELECT on the attempt table.
func TestPublicationEvidenceMigrationIsOneNarrowRead(t *testing.T) {
	for _, required := range []string{
		"SET LOCAL lock_timeout = '5s'",
		"CREATE FUNCTION kaana_read_deployment_failure_streaks(",
		"SECURITY DEFINER",
		"SET search_path = pg_catalog, public",
		"STABLE",
		"INTERVAL '8 days'",
		"e.occurred_at >= c.updated_at",
		"r.occurred_at > s.succeeded_at",
		"REVOKE ALL ON FUNCTION kaana_read_deployment_failure_streaks(TIMESTAMPTZ) FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION kaana_read_deployment_failure_streaks(TIMESTAMPTZ) TO kaana_runtime",
	} {
		if !strings.Contains(migration0019, required) {
			t.Errorf("migration 0019 lost %q", required)
		}
	}
	for _, forbidden := range []string{
		"GRANT SELECT", "GRANT INSERT", "GRANT UPDATE", "GRANT DELETE", "GRANT ALL",
		"encrypted_secret", "TO kaana_credential_admin",
	} {
		if strings.Contains(migration0019, forbidden) {
			t.Errorf("migration 0019 contains forbidden authority %q", forbidden)
		}
	}
}
