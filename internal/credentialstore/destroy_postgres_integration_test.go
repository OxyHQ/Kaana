package credentialstore

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

type integrationProbe struct {
	verdicts map[string]LivenessVerdict
	calls    int
}

func (p *integrationProbe) Probe(_ context.Context, _ contract.ProviderSlug, secret []byte) (LivenessVerdict, error) {
	p.calls++
	if verdict, ok := p.verdicts[string(secret)]; ok {
		return verdict, nil
	}
	return LivenessUnknown, nil
}

func TestProviderCredentialDestructionPostgres(t *testing.T) {
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
	cipher := newFakeProviderKMS()
	store, err := New(repository, cipher)
	if err != nil {
		t.Fatal(err)
	}

	// Ledgers are append-only, so every run uses its own provider slug.
	slug := contract.ProviderSlug("destroy-" + freshHex(t, "")[:8])
	const (
		live      = "123e4567-e89b-42d3-a456-426614174101" // enabled, keeps serving
		duplicate = "123e4567-e89b-42d3-a456-426614174102" // disabled, same secret as live
		dead      = "123e4567-e89b-42d3-a456-426614174103" // disabled, the provider rejects it
		alive     = "123e4567-e89b-42d3-a456-426614174104" // disabled, the provider still accepts it
		pinned    = "123e4567-e89b-42d3-a456-426614174105" // disabled, a deployment pin names it
	)
	for position, row := range []struct{ keyID, secret string }{
		{live, "sk-live"}, {duplicate, "sk-live"}, {dead, "sk-dead"}, {alive, "sk-alive"}, {pinned, "sk-pinned"},
	} {
		putProviderCredential(t, ctx, store, slug, row.keyID, position+1, row.secret)
	}
	deployment := contract.DeploymentID("dep_destroy_" + freshHex(t, "")[:8])
	if _, err := repository.BindDeployment(ctx, freshHex(t, "kdb_"), provider.CredentialBinding{
		DeploymentID: deployment, Provider: slug, KeyID: pinned,
	}, "operator:integration"); err != nil {
		t.Fatalf("pinning: %v", err)
	}
	for _, keyID := range []string{duplicate, dead, alive, pinned} {
		if _, err := store.Disable(ctx, Scope{Provider: slug, KeyID: keyID}, "operator:integration"); err != nil {
			t.Fatalf("disabling %s: %v", keyID, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO provider_credential_runtime_state
		(provider_slug, key_id, state, evidence_source, retired_until, lease_until, observed_at)
		VALUES ($1, $2, 'rejected', 'response_status', NOW() + INTERVAL '1 hour', NULL, NOW())`, slug, duplicate); err != nil {
		t.Fatalf("seeding runtime state: %v", err)
	}

	probe := &integrationProbe{verdicts: map[string]LivenessVerdict{
		"sk-live": LivenessAccepted, "sk-dead": LivenessRejected, "sk-alive": LivenessAccepted, "sk-pinned": LivenessRejected,
	}}
	destroy := func(keyID, operationID, actor string) (DestructionReceipt, error) {
		return store.Destroy(ctx, DestroyOperation{OperationID: operationID, Scope: Scope{Provider: slug, KeyID: keyID}, Actor: actor}, probe)
	}
	secretPresent := func(keyID string) bool {
		t.Helper()
		var present bool
		var destroyed *time.Time
		if err := pool.QueryRow(ctx, `SELECT encrypted_secret IS NOT NULL, destroyed_at FROM provider_credentials
			WHERE provider_slug = $1 AND key_id = $2`, slug, keyID).Scan(&present, &destroyed); err != nil {
			t.Fatalf("reading %s: %v", keyID, err)
		}
		if present == (destroyed != nil) {
			t.Fatalf("%s: secret present = %v but destroyed_at = %v", keyID, present, destroyed)
		}
		return present
	}

	if _, err := destroy(live, freshHex(t, "kcd_"), "operator:integration"); !errors.Is(err, ErrCredentialEnabled) {
		t.Fatalf("destroying an enabled key = %v, want ErrCredentialEnabled", err)
	}

	duplicateOperation := freshHex(t, "kcd_")
	receipt, err := destroy(duplicate, duplicateOperation, "operator:integration")
	if err != nil {
		t.Fatalf("destroying the duplicate: %v", err)
	}
	if receipt.Proof != DestructionProofDuplicateOfEnabled || receipt.DuplicateOfKeyID != live || receipt.Replayed {
		t.Fatalf("duplicate receipt = %+v", receipt)
	}
	if probe.calls != 0 {
		t.Fatalf("the provider was asked %d times about a secret an enabled key holds", probe.calls)
	}
	if secretPresent(duplicate) {
		t.Fatal("the duplicate still holds its ciphertext")
	}
	if !secretPresent(live) {
		t.Fatal("destroying a duplicate touched the enabled key")
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM provider_credential_runtime_state WHERE provider_slug = $1 AND key_id = $2`, slug, duplicate); n != 0 {
		t.Fatalf("the destroyed key kept %d runtime state rows", n)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM provider_credential_audit WHERE provider_slug = $1 AND key_id = $2 AND action = 'destroy'`, slug, duplicate); n != 1 {
		t.Fatalf("destroy audit rows = %d, want 1", n)
	}
	metadata, err := repository.ListMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range metadata {
		if row.Provider == slug && row.KeyID == duplicate {
			t.Fatal("list still shows a destroyed key")
		}
	}

	_, decryptionsBefore := cipher.counts()
	replay, err := destroy(duplicate, duplicateOperation, "operator:recovery")
	if err != nil || !replay.Replayed || replay.Proof != DestructionProofDuplicateOfEnabled || replay.DuplicateOfKeyID != live {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	if _, decryptions := cipher.counts(); decryptions != decryptionsBefore {
		t.Fatal("a replay decrypted again")
	}
	if _, err := destroy(duplicate, freshHex(t, "kcd_"), "operator:integration"); !errors.Is(err, ErrCredentialAlreadyDestroyed) {
		t.Fatalf("a second operation on a destroyed key = %v, want ErrCredentialAlreadyDestroyed", err)
	}
	if _, err := destroy(dead, duplicateOperation, "operator:integration"); !errors.Is(err, ErrProviderCredentialAdminConflict) {
		t.Fatalf("an operation id reused for another key = %v, want a conflict", err)
	}

	// A destroyed identity is final, whatever path tries to write it.
	plaintext := []byte("sk-new")
	if err := store.Put(ctx, EncryptedCredential{Scope: Scope{Provider: slug, KeyID: duplicate}, Position: 9}, plaintext, "operator:integration"); err == nil {
		t.Fatal("put reused a destroyed identity")
	}
	if _, err := pool.Exec(ctx, `UPDATE provider_credentials SET enabled = TRUE WHERE provider_slug = $1 AND key_id = $2`, slug, duplicate); err == nil {
		t.Fatal("a direct update re-enabled a destroyed key")
	}
	// The CHECK constraint alone refuses the two writes above; only the trigger
	// refuses one that also clears destroyed_at, a resurrection.
	if _, err := pool.Exec(ctx, `UPDATE provider_credentials SET destroyed_at = NULL, encrypted_secret = '\x01', enabled = TRUE
		WHERE provider_slug = $1 AND key_id = $2`, slug, duplicate); err == nil {
		t.Fatal("a direct update resurrected a destroyed key")
	}
	if _, err := pool.Exec(ctx, `UPDATE provider_credentials SET encrypted_secret = NULL WHERE provider_slug = $1 AND key_id = $2`, slug, alive); err == nil {
		t.Fatal("a row lost its ciphertext without being destroyed")
	}

	deadReceipt, err := destroy(dead, freshHex(t, "kcd_"), "operator:integration")
	if err != nil || deadReceipt.Proof != DestructionProofProviderRejected || deadReceipt.DuplicateOfKeyID != "" {
		t.Fatalf("destroying a rejected key = %+v, %v", deadReceipt, err)
	}
	if secretPresent(dead) {
		t.Fatal("the rejected key still holds its ciphertext")
	}

	if _, err := destroy(alive, freshHex(t, "kcd_"), "operator:integration"); !errors.Is(err, ErrCredentialStillValid) {
		t.Fatalf("destroying a key the provider accepts = %v, want ErrCredentialStillValid", err)
	}
	if !secretPresent(alive) {
		t.Fatal("a refused destruction removed the ciphertext")
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM provider_credential_destructions WHERE provider_slug = $1 AND key_id = $2`, slug, alive); n != 0 {
		t.Fatal("a refused destruction was recorded")
	}

	if _, err := destroy(pinned, freshHex(t, "kcd_"), "operator:integration"); !errors.Is(err, ErrCredentialPinned) {
		t.Fatalf("destroying a pinned key = %v, want ErrCredentialPinned", err)
	}
	if !secretPresent(pinned) {
		t.Fatal("a pinned key lost its ciphertext")
	}

	if n := countRows(t, ctx, pool, `SELECT count(*) FROM provider_credential_destructions WHERE provider_slug = $1`, slug); n != 2 {
		t.Fatalf("destruction records = %d, want the duplicate and the rejected key", n)
	}
}
