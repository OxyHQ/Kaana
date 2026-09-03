package credentialstore

import (
	"strings"
	"testing"
)

func TestProviderCredentialIDMigrationKeepsTheSecurityBoundary(t *testing.T) {
	for _, required := range []string{
		"CREATE TABLE public.provider_credential_admin_operations",
		"CREATE VIEW public.active_provider_credentials",
		"WITH (security_barrier = true) AS",
		"SECURITY DEFINER",
		"SET search_path = pg_catalog, public",
		"pg_advisory_xact_lock",
		"LOCK TABLE public.provider_credentials IN SHARE ROW EXCLUSIVE MODE",
		"source.enabled = TRUE",
		"destination.key_id = p_destination_key_id",
		"SET enabled = FALSE",
		"'rekey_disable'",
		"'rekey_create'",
		"'deduplicate_disable'",
		"session_user",
		"REVOKE ALL ON public.provider_credential_admin_operations FROM PUBLIC",
		"REVOKE ALL ON public.provider_credentials FROM kaana_runtime",
		"GRANT SELECT ON public.active_provider_credentials TO kaana_runtime",
		"TO kaana_credential_admin",
	} {
		if !strings.Contains(migration0007, required) {
			t.Errorf("provider credential ID migration lost %q", required)
		}
	}
	if count := strings.Count(migration0007, "pg_advisory_xact_lock"); count != 4 {
		t.Errorf("provider credential ID advisory lock count = %d, want 4", count)
	}
	if count := strings.Count(migration0007, "LOCK TABLE public.provider_credentials IN SHARE ROW EXCLUSIVE MODE"); count != 4 {
		t.Errorf("provider credential ID table lock count = %d, want 4", count)
	}
	for _, forbidden := range []string{
		"DELETE FROM public.provider_credentials",
		"secret_sha",
		"digest(",
		"md5(",
		"GRANT EXECUTE ON FUNCTION public.kaana_prepare_provider_credential_rekey(TEXT, TEXT, TEXT, TEXT, TEXT, TEXT, TEXT) TO kaana_runtime",
	} {
		if strings.Contains(migration0007, forbidden) {
			t.Errorf("provider credential ID migration contains forbidden boundary %q", forbidden)
		}
	}
}
