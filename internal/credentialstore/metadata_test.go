package credentialstore

import (
	"strings"
	"testing"
)

const cohereTrialMetadata = `{
  "schemaVersion": 1,
  "operationId": "kcm_0123456789abcdef0123456789abcdef",
  "provider": "cohere",
  "keyId": "123e4567-e89b-42d3-a456-426614174000",
  "title": "Cohere trial key A",
  "fundingAccount": {"id": "kfa_0123456789abcdef0123456789abcdef", "label": "operator@example.com"},
  "capacityCategory": "trial",
  "environment": "production",
  "commercialUse": {"eligibility": "permitted", "evidence": "issued by email with explicit commercial-use permission, per the account owner"}
}`

func TestCredentialMetadataCarriesAnExplicitCommercialUseGrantOnATrialKey(t *testing.T) {
	metadata, err := ParseCredentialMetadata([]byte(cohereTrialMetadata))
	if err != nil {
		t.Fatalf("a trial key with an evidenced commercial-use grant was refused: %v", err)
	}
	if metadata.CapacityCategory != CapacityTrial || metadata.CommercialUse.Eligibility != CommercialUsePermitted {
		t.Fatalf("parsed metadata = %+v", metadata)
	}
}

func TestCredentialMetadataRefusesWhatWouldBeAmbiguous(t *testing.T) {
	for name, mutate := range map[string]func(string) string{
		"misspelled field": func(s string) string { return strings.Replace(s, `"title"`, `"titel"`, 1) },
		"grant without evidence": func(s string) string {
			return strings.Replace(s, `, "evidence": "issued by email with explicit commercial-use permission, per the account owner"`, "", 1)
		},
		"unknown with evidence": func(s string) string { return strings.Replace(s, `"permitted"`, `"unknown"`, 1) },
		"inferred category":     func(s string) string { return strings.Replace(s, `"trial"`, `"free"`, 1) },
		"label as identity": func(s string) string {
			return strings.Replace(s, `"kfa_0123456789abcdef0123456789abcdef"`, `"operator@example.com"`, 1)
		},
		"empty restriction list": func(s string) string {
			return strings.Replace(s, `"environment"`, `"restrictions": {"models": []}, "environment"`, 1)
		},
		"empty restrictions": func(s string) string {
			return strings.Replace(s, `"environment"`, `"restrictions": {}, "environment"`, 1)
		},
		"unpinnable model": func(s string) string {
			return strings.Replace(s, `"environment"`, `"restrictions": {"models": ["not a model"]}, "environment"`, 1)
		},
		"malformed operation id": func(s string) string { return strings.Replace(s, `kcm_0123`, `kcm_X123`, 1) },
		"untrimmed account label": func(s string) string {
			return strings.Replace(s, `"operator@example.com"`, `" operator@example.com"`, 1)
		},
	} {
		if _, err := ParseCredentialMetadata([]byte(mutate(cohereTrialMetadata))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	restricted := strings.Replace(cohereTrialMetadata, `"environment"`, `"restrictions": {"models": ["cohere/command-a@2026-09-01"], "capabilities": ["chat"]}, "environment"`, 1)
	if _, err := ParseCredentialMetadata([]byte(restricted)); err != nil {
		t.Fatalf("a restricted key was refused: %v", err)
	}
}

const quotaEvidence = `{"schemaVersion":1,"evidenceId":"kce_0123456789abcdef0123456789abcdef","provider":"cohere",
  "keyId":"123e4567-e89b-42d3-a456-426614174000","kind":"quota","source":"provider_console",
  "observedAt":"2026-09-30T10:00:00Z","freshUntil":"2026-09-30T11:00:00Z",
  "quota":{"unit":"requests","window":"month","limit":1000,"remaining":990}}`

func TestCapacityEvidenceIsExactlyOneKind(t *testing.T) {
	if _, err := ParseCapacityEvidence([]byte(quotaEvidence)); err != nil {
		t.Fatalf("quota evidence refused: %v", err)
	}
	balance := `{"schemaVersion":1,"evidenceId":"kce_0123456789abcdef0123456789abcdef","provider":"cohere",
	  "keyId":"123e4567-e89b-42d3-a456-426614174000","kind":"balance","source":"provider_api",
	  "observedAt":"2026-09-30T10:00:00Z","balance":{"currency":"USD","amount":"12.50"}}`
	if _, err := ParseCapacityEvidence([]byte(balance)); err != nil {
		t.Fatalf("balance evidence refused: %v", err)
	}
	for name, document := range map[string]string{
		"remaining above limit": strings.Replace(quotaEvidence, `"remaining":990`, `"remaining":1001`, 1),
		"two kinds":             strings.Replace(quotaEvidence, `"quota":{`, `"expiresAt":"2026-12-01T00:00:00Z","quota":{`, 1),
		"stale before observed": strings.Replace(quotaEvidence, `"freshUntil":"2026-09-30T11:00:00Z"`, `"freshUntil":"2026-09-30T09:00:00Z"`, 1),
		"floating balance":      strings.Replace(balance, `"12.50"`, `"1e3"`, 1),
		"kind without its fact": strings.Replace(balance, `"kind":"balance"`, `"kind":"expiry"`, 1),
		"unattributable source": strings.Replace(quotaEvidence, `"provider_console"`, `"rumour"`, 1),
	} {
		if _, err := ParseCapacityEvidence([]byte(document)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCredentialMetadataMigrationIsAdminOnly(t *testing.T) {
	for _, required := range []string{
		"CREATE TABLE provider_funding_accounts",
		"CREATE TABLE provider_credential_descriptions",
		"CREATE TABLE provider_credential_description_operations",
		"CREATE TABLE provider_credential_capacity_evidence",
		"capacity_category IN ('trial', 'promotional', 'prepaid', 'paid')",
		"commercial_use IN ('permitted', 'not_permitted', 'unknown')",
		"funding account identity conflict",
		"credential metadata operation conflict",
		"capacity evidence identity conflict",
		"is append-only",
		"TO kaana_credential_admin",
	} {
		if !strings.Contains(migration0017, required) {
			t.Errorf("credential metadata migration lost %q", required)
		}
	}
	// The runtime must not be able to read a label or an email address, so no
	// grant in this migration may name it.
	if strings.Contains(migration0017, "kaana_runtime") || strings.Contains(migration0017, "kaana_platform_credential_control") {
		t.Error("credential metadata is granted to a role outside credential administration")
	}
	for _, forbidden := range []string{"GRANT INSERT", "GRANT UPDATE", "GRANT DELETE", "COMMIT"} {
		if strings.Contains(strings.ToUpper(migration0017), forbidden) {
			t.Errorf("credential metadata migration contains forbidden %q", forbidden)
		}
	}
}
