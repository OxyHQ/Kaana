package credentialstore

import (
	"strings"
	"testing"
)

func TestScopedClaimPreservesOriginalRecorderBodyAndAddsNoGrants(t *testing.T) {
	old := migration0011[strings.Index(migration0011, "CREATE FUNCTION kaana_record_provider_credential_attempt("):strings.Index(migration0011, "CREATE FUNCTION kaana_reset_provider_credential_runtime")]
	old = strings.Replace(old, "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION", 1)
	replacement := migration0020[strings.Index(migration0020, "CREATE OR REPLACE FUNCTION"):]
	start := strings.Index(replacement, "    -- This branch records only")
	offset := strings.Index(replacement, "        RETURN FOUND;")
	end := offset + strings.Index(replacement[offset:], "    IF p_request_id IS NULL")
	if start < 0 || end < 0 {
		t.Fatal("claim branch boundary missing")
	}
	stripped := replacement[:start] + replacement[end:]
	if strings.TrimSpace(stripped) != strings.TrimSpace(old) {
		t.Fatal("normal recorder body changed")
	}
	if strings.Contains(strings.ToUpper(migration0020), "GRANT ") {
		t.Fatal("migration adds authority")
	}
}
