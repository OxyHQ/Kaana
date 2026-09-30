package credentialstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCredentialMetadataPostgresLifecycle(t *testing.T) {
	databaseURL := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if databaseURL == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository := &Postgres{pool: pool}
	if err := repository.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// The ledgers are append-only, so every run uses fresh identities.
	fresh := func(prefix string) string {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		return prefix + hex.EncodeToString(raw)
	}
	providerSlug := "metadata-" + fresh("")[:12]
	keyA, keyB := "123e4567-e89b-42d3-a456-426614174001", "123e4567-e89b-42d3-a456-426614174002"
	for position, keyID := range []string{keyA, keyB} {
		if _, err := pool.Exec(ctx, `INSERT INTO provider_credentials
			(provider_slug, key_id, encrypted_secret, kms_key_arn, position, key_class)
			VALUES ($1, $2, '\x01', 'arn:aws:kms:eu-west-1:123456789012:key/11111111-1111-1111-1111-111111111111', $3, 'free')`,
			providerSlug, keyID, position+1); err != nil {
			t.Fatalf("creating credential %s: %v", keyID, err)
		}
	}

	account := fresh("kfa_")
	describe := func(keyID, title string) CredentialMetadata {
		return CredentialMetadata{
			SchemaVersion: CredentialMetadataSchemaVersion, OperationID: fresh("kcm_"),
			Provider: contract.ProviderSlug(providerSlug), KeyID: keyID, Title: title,
			FundingAccount:   FundingAccount{ID: account, Label: "operator@example.com"},
			CapacityCategory: CapacityTrial, Environment: "production",
			CommercialUse: CommercialUseClaim{Eligibility: CommercialUsePermitted,
				Evidence: "issued by email with explicit commercial-use permission, per the account owner"},
		}
	}
	first := describe(keyA, "Trial key A")
	for want, attempt := range []string{"applied", "replayed"} {
		outcome, err := repository.PutCredentialMetadata(ctx, first, "integration-test")
		if err != nil || outcome != attempt {
			t.Fatalf("put %d = %q/%v, want %q", want, outcome, err, attempt)
		}
	}
	reused := first
	reused.Title = "A different title under the same operation"
	if _, err := repository.PutCredentialMetadata(ctx, reused, "integration-test"); err == nil {
		t.Fatal("an operation id was reused for a different document")
	}
	relabelled := describe(keyB, "Trial key B")
	relabelled.FundingAccount.Label = "someone-else@example.com"
	if _, err := repository.PutCredentialMetadata(ctx, relabelled, "integration-test"); err == nil {
		t.Fatal("an existing funding account accepted a different label")
	}
	if outcome, err := repository.PutCredentialMetadata(ctx, describe(keyB, "Trial key B"), "integration-test"); err != nil || outcome != "applied" {
		t.Fatalf("second key on the shared account = %q/%v", outcome, err)
	}
	unknownKey := describe("123e4567-e89b-42d3-a456-426614174999", "Nobody")
	if _, err := repository.PutCredentialMetadata(ctx, unknownKey, "integration-test"); err == nil {
		t.Fatal("metadata was accepted for a key that does not exist")
	}
	revised := describe(keyA, "Trial key A, renamed")
	if _, err := repository.PutCredentialMetadata(ctx, revised, "integration-test"); err != nil {
		t.Fatalf("revising key A: %v", err)
	}

	evidence, err := ParseCapacityEvidence([]byte(strings.NewReplacer(
		"kce_0123456789abcdef0123456789abcdef", fresh("kce_"),
		`"provider":"cohere"`, `"provider":"`+providerSlug+`"`,
		"123e4567-e89b-42d3-a456-426614174000", keyA,
	).Replace(quotaEvidence)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"recorded", "replayed"} {
		if outcome, err := repository.RecordCapacityEvidence(ctx, evidence, "integration-test"); err != nil || outcome != want {
			t.Fatalf("evidence = %q/%v, want %q", outcome, err, want)
		}
	}
	changed := evidence
	changed.Quota = &QuotaEvidence{Unit: "requests", Window: "month", Remaining: 5}
	if _, err := repository.RecordCapacityEvidence(ctx, changed, "integration-test"); err == nil {
		t.Fatal("an evidence id was reused for a different observation")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM provider_credential_capacity_evidence WHERE evidence_id = $1`, evidence.EvidenceID); err == nil {
		t.Fatal("capacity evidence was deleted")
	}

	listings, err := repository.ListCredentialMetadata(ctx, time.Date(2026, time.September, 30, 10, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	var found []CredentialMetadataListing
	for _, listing := range listings {
		if string(listing.Provider) == providerSlug {
			found = append(found, listing)
		}
	}
	if len(found) != 2 {
		t.Fatalf("listed %d keys for the provider, want 2", len(found))
	}
	a := found[0]
	if a.KeyID != keyA || a.Revision != 2 || a.Title != "Trial key A, renamed" || len(a.SharedWithKeys) != 1 || a.SharedWithKeys[0] != keyB {
		t.Fatalf("key A listing = %+v", a)
	}
	if len(a.LatestCapacityEvidence) != 1 || !a.LatestCapacityEvidence[0].Fresh || *a.LatestCapacityEvidence[0].QuotaRemaining != 990 {
		t.Fatalf("key A evidence = %+v", a.LatestCapacityEvidence)
	}

	// The boundary itself: the inference runtime cannot read any of it.
	for _, table := range []string{"provider_funding_accounts", "provider_credential_descriptions",
		"provider_credential_description_operations", "provider_credential_capacity_evidence"} {
		var readable bool
		if err := pool.QueryRow(ctx, `SELECT has_table_privilege('kaana_runtime', $1, 'SELECT')`, table).Scan(&readable); err != nil {
			t.Fatal(err)
		}
		if readable {
			t.Errorf("kaana_runtime can read %s", table)
		}
	}
	var adminReads bool
	if err := pool.QueryRow(ctx, `SELECT has_table_privilege('kaana_credential_admin', 'provider_credential_descriptions', 'SELECT')`).Scan(&adminReads); err != nil || !adminReads {
		t.Fatalf("credential administration cannot read metadata: %v/%v", adminReads, err)
	}
}
