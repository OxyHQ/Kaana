package credentialstore

import (
	"strings"
	"testing"
)

func TestDeploymentBindingsAreExactAuditedAndRuntimeReadOnly(t *testing.T) {
	required := []string{
		"PRIMARY KEY", "FOREIGN KEY (provider_slug, key_id)",
		"provider_deployment_binding_operations", "operation_id ~ '^kdb_[0-9a-f]{32}$'",
		"SECURITY DEFINER", "GRANT SELECT ON provider_deployment_credential_bindings TO kaana_runtime",
		"GRANT EXECUTE ON FUNCTION kaana_bind_provider_deployment",
		"pg_advisory_xact_lock", "database_actor",
	}
	for _, fragment := range required {
		if !strings.Contains(migration0013, fragment) {
			t.Errorf("migration 0013 lacks %q", fragment)
		}
	}
	for _, forbidden := range []string{"encrypted_secret", "GRANT INSERT", "GRANT UPDATE", "GRANT DELETE"} {
		if strings.Contains(migration0013, forbidden) {
			t.Errorf("migration 0013 contains forbidden authority/material %q", forbidden)
		}
	}
}
