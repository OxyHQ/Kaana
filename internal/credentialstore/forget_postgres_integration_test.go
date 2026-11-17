package credentialstore

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OxyHQ/Kaana/internal/contract"
)

func TestDestroyedCredentialsAreForgottenOnlyWhenNothingNamesThem(t *testing.T) {
	databaseURL := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if databaseURL == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	repository := &Postgres{pool: pool}
	if err := repository.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Other suites may leave destroyed rows; forget them so counts below are this test's.
	if _, err := repository.ForgetDestroyedCredentials(ctx, "operator:integration", MaxPurgeBatch, false); err != nil {
		t.Fatal(err)
	}
	store, err := New(repository, newFakeProviderKMS())
	if err != nil {
		t.Fatal(err)
	}

	slug := contract.ProviderSlug("forget-" + freshHex(t, "")[:8])
	const (
		live      = "123e4567-e89b-42d3-a456-426614174201" // enabled; its records are never touched
		bare      = "123e4567-e89b-42d3-a456-426614174202" // destroyed, nothing names it
		described = "123e4567-e89b-42d3-a456-426614174203" // destroyed, with a description and capacity evidence
		costed    = "123e4567-e89b-42d3-a456-426614174204" // destroyed, a cost event still names it
		claimed   = "123e4567-e89b-42d3-a456-426614174205" // destroyed, an unexpired claim names it
	)
	for position, keyID := range []string{live, bare, described, costed, claimed} {
		putProviderCredential(t, ctx, store, slug, keyID, position+1, "sk-"+keyID)
	}
	describe := func(keyID string) {
		t.Helper()
		metadata := CredentialMetadata{
			SchemaVersion: CredentialMetadataSchemaVersion, OperationID: freshHex(t, "kcm_"),
			Provider: slug, KeyID: keyID, Title: "Key " + keyID[len(keyID)-3:],
			FundingAccount:   FundingAccount{ID: freshHex(t, "kfa_"), Label: "operator@example.com"},
			CapacityCategory: CapacityTrial, Environment: "production",
			CommercialUse: CommercialUseClaim{Eligibility: CommercialUsePermitted, Evidence: "issued for commercial use, per the account owner"},
		}
		if _, err := repository.PutCredentialMetadata(ctx, metadata, "operator:integration"); err != nil {
			t.Fatalf("describing %s: %v", keyID, err)
		}
		evidence, err := ParseCapacityEvidence([]byte(strings.NewReplacer(
			"kce_0123456789abcdef0123456789abcdef", freshHex(t, "kce_"),
			`"provider":"cohere"`, `"provider":"`+string(slug)+`"`,
			"123e4567-e89b-42d3-a456-426614174000", keyID,
		).Replace(quotaEvidence)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repository.RecordCapacityEvidence(ctx, evidence, "operator:integration"); err != nil {
			t.Fatalf("evidence for %s: %v", keyID, err)
		}
	}
	describe(live)
	describe(described)
	describe(costed)

	event := settlementCostEvent(freshHex(t, "req_"), costed)
	event.Provider = slug
	if err := repository.WriteProviderCostEvent(ctx, event); err != nil {
		t.Fatalf("cost event: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO scoped_provider_attempt_claims
		(operation_id, deployment_id, provider_slug, key_id, claimed_at, expires_at)
		VALUES ($1, 'dep_forget_test', $2, $3, NOW(), NOW() + INTERVAL '1 hour')`, freshHex(t, "op_"), slug, claimed); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Each dead key is a duplicate of nothing and rejected by its provider.
	probe := &integrationProbe{verdicts: map[string]LivenessVerdict{}}
	for _, keyID := range []string{bare, described, costed, claimed} {
		probe.verdicts["sk-"+keyID] = LivenessRejected
		if _, err := store.Disable(ctx, Scope{Provider: slug, KeyID: keyID}, "operator:integration"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Destroy(ctx, DestroyOperation{OperationID: freshHex(t, "kcd_"), Scope: Scope{Provider: slug, KeyID: keyID}, Actor: "operator:integration"}, probe); err != nil {
			t.Fatalf("destroying %s: %v", keyID, err)
		}
	}

	// Freshly destroyed keys are inside the 30-day grace in which a request
	// that took the key before its disable may still write its cost.
	if eligible, err := repository.ForgetDestroyedCredentials(ctx, "purge-retention:dry-run", MaxPurgeBatch, true); err != nil || eligible != 0 {
		t.Fatalf("dry run inside the grace = %d, %v; want 0", eligible, err)
	}
	age := func(keyIDs ...string) {
		t.Helper()
		// destroyed_is_final refuses every update of a destroyed row; the
		// test backdates under replica mode, in its own transaction only.
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE provider_credentials SET destroyed_at = NOW() - INTERVAL '31 days'
			WHERE provider_slug = $1 AND key_id = ANY($2)`, slug, keyIDs); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	age(bare, described, costed, claimed)

	rows := func(keyID string) map[string]int {
		t.Helper()
		counts := map[string]int{}
		for _, table := range []string{
			"provider_credentials", "provider_credential_descriptions", "provider_credential_description_operations",
			"provider_credential_capacity_evidence", "provider_credential_destructions",
		} {
			counts[table] = countRows(t, ctx, pool, `SELECT count(*) FROM `+table+` WHERE provider_slug = $1 AND key_id = $2`, slug, keyID)
		}
		counts["forget audit"] = countRows(t, ctx, pool, `SELECT count(*) FROM provider_credential_audit WHERE provider_slug = $1 AND key_id = $2 AND action = 'forget'`, slug, keyID)
		return counts
	}

	if eligible, err := repository.ForgetDestroyedCredentials(ctx, "purge-retention:dry-run", MaxPurgeBatch, true); err != nil || eligible != 2 {
		t.Fatalf("dry run = %d, %v; want the bare and the described key", eligible, err)
	}
	if rows(bare)["provider_credentials"] != 1 {
		t.Fatal("a dry run forgot a key")
	}
	forgotten, err := repository.ForgetDestroyedCredentials(ctx, "operator:integration", MaxPurgeBatch, false)
	if err != nil || forgotten != 2 {
		t.Fatalf("forget = %d, %v; want 2", forgotten, err)
	}
	for _, keyID := range []string{bare, described} {
		got := rows(keyID)
		for table, want := range map[string]int{
			"provider_credentials": 0, "provider_credential_descriptions": 0, "provider_credential_description_operations": 0,
			"provider_credential_capacity_evidence": 0, "provider_credential_destructions": 1, "forget audit": 1,
		} {
			if got[table] != want {
				t.Errorf("%s: %s rows = %d, want %d", keyID, table, got[table], want)
			}
		}
	}
	for _, keyID := range []string{costed, claimed} {
		if rows(keyID)["provider_credentials"] != 1 {
			t.Errorf("%s was forgotten while something still names it", keyID)
		}
	}
	if got := rows(live); got["provider_credentials"] != 1 || got["provider_credential_descriptions"] != 1 ||
		got["provider_credential_description_operations"] != 1 || got["provider_credential_capacity_evidence"] != 1 {
		t.Fatalf("the live key's records were touched: %v", got)
	}

	// The ledger exception reaches only a destroyed key named by the forget
	// function: naming a live key in the setting deletes nothing.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('kaana.forget_credential', $1, true)`, string(slug)+"/"+live); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM provider_credential_capacity_evidence WHERE provider_slug = $1 AND key_id = $2`, slug, live); err == nil {
		t.Error("the ledger let a live key's evidence be deleted")
	}
	_ = tx.Rollback(ctx)
	if _, err := pool.Exec(ctx, `DELETE FROM provider_credential_capacity_evidence WHERE provider_slug = $1 AND key_id = $2`, slug, costed); err == nil {
		t.Error("the ledger let a destroyed key's evidence be deleted outside the forget function")
	}

	// Once the history ages out, the key goes on the next run.
	if _, err := pool.Exec(ctx, `DELETE FROM provider_cost_events WHERE provider_slug = $1 AND key_id = $2`, slug, costed); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE scoped_provider_attempt_claims SET claimed_at = NOW() - INTERVAL '2 hours', expires_at = NOW() - INTERVAL '1 hour'
		WHERE provider_slug = $1 AND key_id = $2`, slug, claimed); err != nil {
		t.Fatal(err)
	}
	if forgotten, err := repository.ForgetDestroyedCredentials(ctx, "operator:integration", MaxPurgeBatch, false); err != nil || forgotten != 2 {
		t.Fatalf("second forget = %d, %v; want the costed and the claimed key", forgotten, err)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM scoped_provider_attempt_claims WHERE provider_slug = $1`, slug); n != 0 {
		t.Fatalf("%d expired claims of a forgotten key remain", n)
	}
	if got := rows(costed); got["provider_credential_capacity_evidence"] != 0 || got["provider_credential_description_operations"] != 0 {
		t.Fatalf("the costed key's metadata outlived it: %v", got)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM provider_credentials WHERE provider_slug = $1`, slug); n != 1 {
		t.Fatalf("%d credential rows remain, want only the live key", n)
	}

	// A forgotten identity is never created again, by any insert path.
	if err := store.Put(ctx, EncryptedCredential{Scope: Scope{Provider: slug, KeyID: bare}, Position: 7}, []byte("sk-reborn"), "operator:integration"); err == nil {
		t.Fatal("put recreated a forgotten identity")
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM provider_credentials WHERE provider_slug = $1 AND key_id = $2`, slug, bare); n != 0 {
		t.Fatal("a forgotten identity exists again")
	}
}
