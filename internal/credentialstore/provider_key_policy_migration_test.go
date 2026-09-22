package credentialstore

import (
	"strings"
	"testing"
)

func TestProviderKeyPoliciesAreAuditedAdminOnlyAndRuntimeReadOnly(t *testing.T) {
	required := []string{
		"provider_key_policies", "PRIMARY KEY",
		"provider_key_policy_audit", "SECURITY DEFINER",
		"GRANT SELECT ON provider_key_policies TO kaana_runtime",
		"GRANT EXECUTE ON FUNCTION kaana_put_provider_key_policy",
		"operation_actor", "database_actor",
	}
	for _, fragment := range required {
		if !strings.Contains(migration0014, fragment) {
			t.Errorf("migration 0014 lacks %q", fragment)
		}
	}
	for _, forbidden := range []string{"encrypted_secret", "GRANT INSERT", "GRANT UPDATE", "GRANT DELETE", "kaana_disable_provider_key_policy"} {
		if strings.Contains(migration0014, forbidden) {
			t.Errorf("migration 0014 contains forbidden authority/material %q", forbidden)
		}
	}
}
