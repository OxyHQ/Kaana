package credentialstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fakeProviderKMSEntry struct {
	scope     Scope
	plaintext []byte
}

type fakeProviderKMS struct {
	mu          sync.Mutex
	sequence    int
	entries     map[string]fakeProviderKMSEntry
	encryptions int
	decryptions int
	failEncrypt bool
}

func newFakeProviderKMS() *fakeProviderKMS {
	return &fakeProviderKMS{entries: make(map[string]fakeProviderKMSEntry)}
}

func (f *fakeProviderKMS) Encrypt(_ context.Context, scope Scope, plaintext []byte) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.encryptions++
	if f.failEncrypt {
		return nil, "", errors.New("fake KMS encryption failure")
	}
	f.sequence++
	ciphertext := fmt.Sprintf("fake-kms-ciphertext-%d", f.sequence)
	f.entries[ciphertext] = fakeProviderKMSEntry{scope: scope, plaintext: append([]byte(nil), plaintext...)}
	return []byte(ciphertext), kmsTestKeyARN, nil
}

func (f *fakeProviderKMS) Decrypt(_ context.Context, scope Scope, ciphertext []byte, keyARN string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decryptions++
	entry, present := f.entries[string(ciphertext)]
	if !present || entry.scope != scope || keyARN != kmsTestKeyARN {
		return nil, errors.New("fake KMS context mismatch")
	}
	return append([]byte(nil), entry.plaintext...), nil
}

func (f *fakeProviderKMS) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.encryptions, f.decryptions
}

func TestProviderCredentialIDOperationsPostgresAndFakeKMS(t *testing.T) {
	databaseURL := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if databaseURL == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("opening test PostgreSQL: %v", err)
	}
	defer pool.Close()
	repository := &Postgres{pool: pool}
	if err := repository.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE TABLE
		provider_credential_admin_operations,
		provider_credential_audit,
		provider_credentials RESTART IDENTITY`); err != nil {
		t.Fatalf("resetting provider credential test tables: %v", err)
	}

	cipher := newFakeProviderKMS()
	store, err := New(repository, cipher)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	putProviderCredential(t, ctx, store, "groq", "legacy-alia-20260901", 1, "same-provider-secret")
	putProviderCredential(t, ctx, store, "groq", "relay-groq-20260902", 2, "same-provider-secret")

	deduplicate := CredentialIDOperation{
		OperationID: "kop_0af8007d9fdddd88d2622eabff99aeb9",
		Provider:    "groq", SourceKeyID: "relay-groq-20260902",
		DestinationKeyID: "legacy-alia-20260901", Actor: "operator:integration",
	}
	receipt, err := store.Deduplicate(ctx, deduplicate)
	if err != nil {
		t.Fatalf("Deduplicate: %v", err)
	}
	if receipt.Outcome != CredentialAdminOutcomeDeduplicated || receipt.Replayed {
		t.Fatalf("deduplication receipt = %+v", receipt)
	}
	assertProviderCredentialEnabled(t, ctx, pool, "groq", "legacy-alia-20260901", true)
	assertProviderCredentialEnabled(t, ctx, pool, "groq", "relay-groq-20260902", false)
	encryptionsBeforeReplay, decryptionsBeforeReplay := cipher.counts()
	deduplicationReplay := deduplicate
	deduplicationReplay.Actor = "operator:recovery"
	replayedDeduplication, err := store.Deduplicate(ctx, deduplicationReplay)
	if err != nil {
		t.Fatalf("replayed Deduplicate: %v", err)
	}
	if !replayedDeduplication.Replayed || replayedDeduplication.Outcome != CredentialAdminOutcomeDeduplicated {
		t.Fatalf("replayed deduplication receipt = %+v", replayedDeduplication)
	}
	if encryptions, decryptions := cipher.counts(); encryptions != encryptionsBeforeReplay || decryptions != decryptionsBeforeReplay {
		t.Fatalf("replayed deduplication called KMS: encryptions/decryptions = %d/%d, want %d/%d", encryptions, decryptions, encryptionsBeforeReplay, decryptionsBeforeReplay)
	}

	rekey := CredentialIDOperation{
		OperationID: "kop_3ac18ed3ab6c6bf97862b03193ef4357",
		Provider:    "groq", SourceKeyID: "legacy-alia-20260901",
		DestinationKeyID:        "8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1",
		PrerequisiteOperationID: deduplicate.OperationID, Actor: "operator:integration",
	}
	wrongBranch := rekey
	wrongBranch.OperationID = "kop_00000000000000000000000000000002"
	wrongBranch.DestinationKeyID = "f0c4e09f-a5f8-4af8-86b4-960e2d637ce1"
	wrongBranch.PrerequisiteOutcome = CredentialAdminOutcomeDifferent
	if _, err := store.RekeyID(ctx, wrongBranch); !errors.Is(err, ErrProviderCredentialPrerequisiteUnavailable) {
		t.Fatalf("wrong prerequisite branch error = %v", err)
	}
	receipt, err = store.RekeyID(ctx, rekey)
	if err != nil {
		t.Fatalf("RekeyID: %v", err)
	}
	if receipt.Outcome != CredentialAdminOutcomeApplied || receipt.Replayed {
		t.Fatalf("rekey receipt = %+v", receipt)
	}
	assertProviderCredentialEnabled(t, ctx, pool, "groq", "legacy-alia-20260901", false)
	assertProviderCredentialEnabled(t, ctx, pool, "groq", "8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1", true)
	var position int
	var keyClass string
	var budgetUSD float64
	if err := pool.QueryRow(ctx, `SELECT position, key_class, budget_usd::DOUBLE PRECISION FROM provider_credentials
		WHERE provider_slug = 'groq' AND key_id = '8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1'`).Scan(&position, &keyClass, &budgetUSD); err != nil {
		t.Fatalf("reading canonical metadata: %v", err)
	}
	if position != 1 || keyClass != string(provider.KeyClassPaid) || budgetUSD != 1 {
		t.Fatalf("canonical position/class/budget = %d/%q/%v", position, keyClass, budgetUSD)
	}
	pools, _, err := store.Load(ctx, []contract.ProviderSlug{"groq"})
	if err != nil {
		t.Fatalf("Load after canonicalization: %v", err)
	}
	if len(pools["groq"]) != 1 || pools["groq"][0].KeyID != rekey.DestinationKeyID || pools["groq"][0].Secret != "same-provider-secret" {
		t.Fatalf("canonical pool = %+v", pools["groq"])
	}
	var groqAdminAuditRows, groqCredentialHistoryRows int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM provider_credential_audit
		WHERE provider_slug = 'groq'
		  AND action IN ('deduplicate_disable', 'rekey_disable', 'rekey_create')`).Scan(&groqAdminAuditRows); err != nil {
		t.Fatalf("counting Groq admin audit rows: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM provider_credentials
		WHERE provider_slug = 'groq'`).Scan(&groqCredentialHistoryRows); err != nil {
		t.Fatalf("counting retained Groq credential rows: %v", err)
	}
	if groqAdminAuditRows != 3 || groqCredentialHistoryRows != 3 {
		t.Fatalf("Groq admin audit/history rows = %d/%d, want 3/3", groqAdminAuditRows, groqCredentialHistoryRows)
	}
	encryptionsBeforeReplay, decryptionsBeforeReplay = cipher.counts()
	rekeyReplay := rekey
	rekeyReplay.Actor = "operator:recovery"
	replayedRekey, err := store.RekeyID(ctx, rekeyReplay)
	if err != nil {
		t.Fatalf("replayed RekeyID: %v", err)
	}
	if !replayedRekey.Replayed || replayedRekey.Outcome != CredentialAdminOutcomeApplied {
		t.Fatalf("replayed rekey receipt = %+v", replayedRekey)
	}
	if encryptions, decryptions := cipher.counts(); encryptions != encryptionsBeforeReplay || decryptions != decryptionsBeforeReplay {
		t.Fatalf("replayed rekey called KMS: encryptions/decryptions = %d/%d, want %d/%d", encryptions, decryptions, encryptionsBeforeReplay, decryptionsBeforeReplay)
	}

	rebound := rekey
	rebound.Provider = "xai"
	if _, err := store.RekeyID(ctx, rebound); !errors.Is(err, ErrProviderCredentialAdminConflict) {
		t.Fatalf("rebound operation error = %v", err)
	}
	putProviderCredential(t, ctx, store, "openrouter", "legacy-alia-20260901", 1, "first-secret")
	putProviderCredential(t, ctx, store, "openrouter", "relay-openrouter-20260902", 2, "second-secret")
	different := CredentialIDOperation{
		OperationID: "kop_64722ac4d450f4ac2d5c6b6bd0fe0a15",
		Provider:    "openrouter", SourceKeyID: "relay-openrouter-20260902",
		DestinationKeyID: "legacy-alia-20260901", Actor: "operator:integration",
	}
	differentReceipt, err := store.Deduplicate(ctx, different)
	if err != nil {
		t.Fatalf("different Deduplicate: %v", err)
	}
	if differentReceipt.Outcome != CredentialAdminOutcomeDifferent {
		t.Fatalf("different receipt = %+v", differentReceipt)
	}
	assertProviderCredentialEnabled(t, ctx, pool, "openrouter", different.SourceKeyID, true)
	assertProviderCredentialEnabled(t, ctx, pool, "openrouter", different.DestinationKeyID, true)
	secondaryRekey := CredentialIDOperation{
		OperationID: "kop_0418afb5cc61a79a8ff2db4ddcd5b809",
		Provider:    "openrouter", SourceKeyID: "relay-openrouter-20260902",
		DestinationKeyID:        "2bdf7141-fdf6-4cbf-8332-3ea98202f52f",
		PrerequisiteOperationID: different.OperationID,
		PrerequisiteOutcome:     CredentialAdminOutcomeDifferent,
		Actor:                   "operator:integration",
	}
	secondaryReceipt, err := store.RekeyID(ctx, secondaryRekey)
	if err != nil || secondaryReceipt.Outcome != CredentialAdminOutcomeApplied {
		t.Fatalf("conditional secondary rekey = %+v, %v", secondaryReceipt, err)
	}
	assertProviderCredentialEnabled(t, ctx, pool, "openrouter", different.SourceKeyID, false)
	assertProviderCredentialEnabled(t, ctx, pool, "openrouter", secondaryRekey.DestinationKeyID, true)
	var operationActor, databaseActor string
	if err := pool.QueryRow(ctx, `SELECT operation_actor, database_actor
		FROM provider_credential_admin_operations WHERE operation_id = $1`, different.OperationID).Scan(&operationActor, &databaseActor); err != nil {
		t.Fatalf("reading different operation principals: %v", err)
	}
	if operationActor != "operator:integration" || databaseActor == "" {
		t.Fatalf("different operation principals = %q/%q", operationActor, databaseActor)
	}
	putProviderCredential(t, ctx, store, "cerebras", "cerebras-relay-main", 1, "cerebras-source")
	putProviderCredential(t, ctx, store, "cerebras", "43405cea-a7d1-49c2-ba73-5a84536d3abf", 2, "preexisting-destination")
	if _, err := store.Disable(ctx, Scope{Provider: "cerebras", KeyID: "43405cea-a7d1-49c2-ba73-5a84536d3abf"}, "operator:integration"); err != nil {
		t.Fatalf("disabling preexisting destination: %v", err)
	}
	destinationExists := CredentialIDOperation{
		OperationID: "kop_5b4f96c394a7a288754a1388fed0c5b2",
		Provider:    "cerebras", SourceKeyID: "cerebras-relay-main",
		DestinationKeyID: "43405cea-a7d1-49c2-ba73-5a84536d3abf", Actor: "operator:integration",
	}
	if _, err := store.RekeyID(ctx, destinationExists); !errors.Is(err, ErrProviderCredentialDestinationExists) {
		t.Fatalf("preexisting disabled destination error = %v", err)
	}
	assertProviderCredentialEnabled(t, ctx, pool, "cerebras", destinationExists.SourceKeyID, true)

	putProviderCredential(t, ctx, store, "xai", "legacy-alia-20260901", 1, "rollback-secret")
	missingSource := CredentialIDOperation{
		OperationID: "kop_00000000000000000000000000000001",
		Provider:    "xai", SourceKeyID: "not-the-position-one-id",
		DestinationKeyID: "1d72d527-81ca-41e5-9644-2d81a4b126ec", Actor: "operator:integration",
	}
	if _, err := store.RekeyID(ctx, missingSource); !errors.Is(err, ErrProviderCredentialSourceUnavailable) {
		t.Fatalf("missing exact source error = %v", err)
	}
	assertProviderCredentialEnabled(t, ctx, pool, "xai", "legacy-alia-20260901", true)
	cipher.failEncrypt = true
	rollbackOperation := CredentialIDOperation{
		OperationID: "kop_6f4d191e8834c4410049904de37952a6",
		Provider:    "xai", SourceKeyID: "legacy-alia-20260901",
		DestinationKeyID: "1d72d527-81ca-41e5-9644-2d81a4b126ec", Actor: "operator:integration",
	}
	if _, err := store.RekeyID(ctx, rollbackOperation); err == nil {
		t.Fatal("failed fake KMS encryption did not abort rekey")
	}
	cipher.failEncrypt = false
	assertProviderCredentialEnabled(t, ctx, pool, "xai", rollbackOperation.SourceKeyID, true)
	var rollbackDestinationCount, rollbackOperationCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM provider_credentials
		WHERE provider_slug = $1 AND key_id = $2`, rollbackOperation.Provider, rollbackOperation.DestinationKeyID).Scan(&rollbackDestinationCount); err != nil {
		t.Fatalf("counting rolled-back destination: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM provider_credential_admin_operations
		WHERE operation_id = $1`, rollbackOperation.OperationID).Scan(&rollbackOperationCount); err != nil {
		t.Fatalf("counting rolled-back operation: %v", err)
	}
	if rollbackDestinationCount != 0 || rollbackOperationCount != 0 {
		t.Fatalf("failed rekey destination/operation counts = %d/%d", rollbackDestinationCount, rollbackOperationCount)
	}

	assertProviderAdminSchemaHasNoSecretDigest(t, ctx, pool)
	assertProviderAdminPrivileges(t, ctx, pool)
}

func putProviderCredential(t *testing.T, ctx context.Context, store *Store, providerSlug contract.ProviderSlug, keyID string, position int, secret string) {
	t.Helper()
	plaintext := []byte(secret)
	defer clear(plaintext)
	budgetUSD := float64(position)
	input := EncryptedCredential{
		Scope: Scope{Provider: providerSlug, KeyID: keyID},
		Class: provider.KeyClassPaid, BudgetUSD: &budgetUSD, Position: position,
	}
	var err error
	if validOpaqueCredentialID(keyID) {
		err = store.Put(ctx, input, plaintext, "operator:integration")
	} else {
		err = store.ImportLegacy(ctx, input, plaintext, "operator:integration")
	}
	if err != nil {
		t.Fatalf("put %s/%s: %v", providerSlug, keyID, err)
	}
}

func assertProviderCredentialEnabled(t *testing.T, ctx context.Context, pool *pgxpool.Pool, providerSlug, keyID string, expected bool) {
	t.Helper()
	var enabled bool
	if err := pool.QueryRow(ctx, `SELECT enabled FROM provider_credentials
		WHERE provider_slug = $1 AND key_id = $2`, providerSlug, keyID).Scan(&enabled); err != nil {
		t.Fatalf("reading %s/%s enabled state: %v", providerSlug, keyID, err)
	}
	if enabled != expected {
		t.Fatalf("%s/%s enabled = %v, want %v", providerSlug, keyID, enabled, expected)
	}
}

func assertProviderAdminSchemaHasNoSecretDigest(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT column_name
		FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND table_name = 'provider_credential_admin_operations'
		ORDER BY ordinal_position`)
	if err != nil {
		t.Fatalf("listing provider admin operation columns: %v", err)
	}
	defer rows.Close()
	columns := make([]string, 0)
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scanning provider admin operation column: %v", err)
		}
		columns = append(columns, column)
	}
	expected := []string{"operation_id", "action", "provider_slug", "source_key_id", "destination_key_id", "prerequisite_operation_id", "prerequisite_outcome", "operation_actor", "database_actor", "outcome", "completed_at"}
	if fmt.Sprint(columns) != fmt.Sprint(expected) {
		t.Fatalf("provider admin operation columns = %v, want %v", columns, expected)
	}
}

func assertProviderAdminPrivileges(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var adminDML, runtimeOperationPrivileges, runtimeBaseTablePrivileges, runtimeActiveViewSelects int
	var adminFunctionGrants, runtimeFunctionGrants, publicFunctionGrants int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.role_table_grants
		WHERE grantee = 'kaana_credential_admin'
		  AND table_name = 'provider_credential_admin_operations'
		  AND privilege_type IN ('INSERT', 'UPDATE', 'DELETE')`).Scan(&adminDML); err != nil {
		t.Fatalf("checking credential-admin operation DML grants: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.role_table_grants
		WHERE grantee = 'kaana_runtime'
		  AND table_name = 'provider_credential_admin_operations'`).Scan(&runtimeOperationPrivileges); err != nil {
		t.Fatalf("checking runtime operation grants: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.role_table_grants
		WHERE grantee = 'kaana_runtime' AND table_name = 'provider_credentials'`).Scan(&runtimeBaseTablePrivileges); err != nil {
		t.Fatalf("checking runtime base credential grants: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.role_table_grants
		WHERE grantee = 'kaana_runtime' AND table_name = 'active_provider_credentials'
		  AND privilege_type = 'SELECT'`).Scan(&runtimeActiveViewSelects); err != nil {
		t.Fatalf("checking runtime active credential view grant: %v", err)
	}
	if adminDML != 0 || runtimeOperationPrivileges != 0 || runtimeBaseTablePrivileges != 0 || runtimeActiveViewSelects != 1 {
		t.Fatalf("admin DML/runtime operation/base/active-view grants = %d/%d/%d/%d", adminDML, runtimeOperationPrivileges, runtimeBaseTablePrivileges, runtimeActiveViewSelects)
	}
	const functionFilter = `routine_name IN (
		'kaana_prepare_provider_credential_rekey',
		'kaana_complete_provider_credential_rekey',
		'kaana_prepare_provider_credential_deduplication',
		'kaana_complete_provider_credential_deduplication'
	)`
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.routine_privileges
		WHERE grantee = 'kaana_credential_admin' AND `+functionFilter).Scan(&adminFunctionGrants); err != nil {
		t.Fatalf("checking credential-admin function grants: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.routine_privileges
		WHERE grantee = 'kaana_runtime' AND `+functionFilter).Scan(&runtimeFunctionGrants); err != nil {
		t.Fatalf("checking runtime function grants: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.routine_privileges
		WHERE grantee = 'PUBLIC' AND `+functionFilter).Scan(&publicFunctionGrants); err != nil {
		t.Fatalf("checking public function grants: %v", err)
	}
	if adminFunctionGrants != 4 || runtimeFunctionGrants != 0 || publicFunctionGrants != 0 {
		t.Fatalf("admin/runtime/public provider operation function grants = %d/%d/%d", adminFunctionGrants, runtimeFunctionGrants, publicFunctionGrants)
	}
}
