package credentialstore

import (
	"os"
	"strings"
	"testing"
)

func TestPlatformCredentialMigrationKeepsTheEncryptOnlyBoundary(t *testing.T) {
	for _, required := range []string{
		"CREATE TABLE platform_provider_credential_operations",
		"secret_fingerprint BYTEA NOT NULL",
		"octet_length(secret_fingerprint) = 32",
		"CREATE FUNCTION kaana_put_platform_provider_credential(",
		"SECURITY DEFINER",
		"SET search_path = pg_catalog, public",
		"pg_advisory_xact_lock",
		"existing.secret_fingerprint = p_secret_fingerprint",
		"RETURN QUERY SELECT 'replayed'::TEXT",
		"RETURN QUERY SELECT 'conflict'::TEXT",
		"INSERT INTO public.provider_credentials",
		"INSERT INTO public.provider_credential_audit",
		"INSERT INTO public.platform_provider_credential_operations",
		"REVOKE ALL ON provider_credentials, provider_credential_audit, platform_provider_credential_operations FROM kaana_platform_credential_control",
		"GRANT EXECUTE ON FUNCTION kaana_put_platform_provider_credential",
		"TO kaana_platform_credential_control",
	} {
		if !strings.Contains(migration0009, required) {
			t.Errorf("platform credential migration lost %q", required)
		}
	}
	for _, forbidden := range []string{
		"GRANT SELECT ON provider_credentials TO kaana_platform_credential_control",
		"GRANT INSERT ON provider_credentials TO kaana_platform_credential_control",
		"GRANT UPDATE ON provider_credentials TO kaana_platform_credential_control",
		"GRANT SELECT ON platform_provider_credential_operations TO kaana_platform_credential_control",
		"kms:decrypt",
		"funding_account",
		"capacity_grant",
		"customer_account",
	} {
		if strings.Contains(strings.ToLower(migration0009), strings.ToLower(forbidden)) {
			t.Errorf("platform credential migration contains forbidden authority %q", forbidden)
		}
	}
	if count := strings.Count(migration0009, "CREATE TABLE "); count != 1 {
		t.Errorf("platform credential operation table count = %d, want 1", count)
	}
	if count := strings.Count(migration0009, "CREATE FUNCTION "); count != 1 {
		t.Errorf("platform credential function count = %d, want 1", count)
	}
}

func TestPlatformCredentialRoleBootstrapNeedsNoSecret(t *testing.T) {
	runbookBytes, err := os.ReadFile("../../docs/operating.md")
	if err != nil {
		t.Fatalf("reading operating runbook: %v", err)
	}
	runbook := string(runbookBytes)
	for _, required := range []string{
		"CREATE ROLE kaana_platform_credential_control NOLOGIN",
		"NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT",
		"kaana-credentials migrate",
		"step accepts or reads a provider key",
		"service is `ACTIVE`",
	} {
		if !strings.Contains(runbook, required) {
			t.Errorf("platform credential bootstrap lost %q", required)
		}
	}
	start := strings.Index(runbook, "The platform credential-control cutover")
	end := strings.Index(runbook, "### Add or rotate a key")
	if start < 0 || end <= start {
		t.Fatal("platform credential bootstrap section is absent or out of order")
	}
	section := runbook[start:end]
	for _, forbidden := range []string{" PASSWORD ", " LOGIN;", "provider-key.txt", "secretBase64", "aws ecs create-service"} {
		if strings.Contains(section, forbidden) {
			t.Errorf("role/function bootstrap contains forbidden secret or infrastructure mutation %q", forbidden)
		}
	}
}
