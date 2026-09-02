package credentialstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCustomerCredentialPostgresLifecycleAndPrivileges(t *testing.T) {
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
	if err := repository.Migrate(ctx); err != nil {
		t.Fatalf("idempotent Migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT * FROM kaana_apply_customer_provider_credential_create(
		$1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		"operation_invalid_actor", "kcred_22222222222222222222222222", "anthropic",
		"acc_actor_guard", "conn_actor_guard", "production", []byte("ciphertext"),
		kmsTestKeyARN, strings.Repeat("0", 64), "user:owner\nforged",
	); err == nil {
		t.Fatal("the SECURITY DEFINER boundary accepted a multiline audit actor")
	}
	var invalidActorOperations int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM customer_provider_credential_operations WHERE operation_id = 'operation_invalid_actor'`).Scan(&invalidActorOperations); err != nil || invalidActorOperations != 0 {
		t.Fatalf("invalid actor operation count/error = %d/%v", invalidActorOperations, err)
	}

	cipher := &recordingCustomerCipher{}
	writer := newFixedCustomerWriter(t, repository, cipher)
	created, err := writer.Create(ctx, "operation_create_lifecycle", customerTestIdentity, []byte("customer-secret-v1"), "user:owner")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.CredentialHandle != fixedCustomerHandle || created.Revision != 1 || created.Status != CustomerCredentialOutcomeApplied {
		t.Fatalf("create outcome = %#v", created)
	}

	replayedCreate, err := writer.Create(ctx, "operation_create_lifecycle", customerTestIdentity, []byte("customer-secret-v1"), "user:owner")
	if err != nil || replayedCreate.CredentialHandle != created.CredentialHandle || replayedCreate.Revision != 1 || !replayedCreate.Replayed {
		t.Fatalf("replayed Create = %#v, %v", replayedCreate, err)
	}
	if _, err := writer.Create(ctx, "operation_create_lifecycle", customerTestIdentity, []byte("different-secret"), "user:owner"); !errors.Is(err, ErrCustomerCredentialConflict) {
		t.Fatalf("same create operation with another secret error = %v", err)
	}
	if _, err := writer.Create(ctx, "operation_create_lifecycle", customerTestIdentity, []byte("customer-secret-v1"), "user:other"); !errors.Is(err, ErrCustomerCredentialConflict) {
		t.Fatalf("same create operation with another actor error = %v", err)
	}
	if _, err := writer.Create(ctx, "operation_create_competing", customerTestIdentity, []byte("must-not-overwrite"), "user:owner"); !errors.Is(err, ErrCustomerCredentialConflict) {
		t.Fatalf("competing Create error = %v", err)
	}
	providerMismatch := customerTestIdentity
	providerMismatch.Provider = "openai"
	if _, err := writer.Create(ctx, "operation_create_wrong_provider", providerMismatch, []byte("must-not-overwrite"), "user:owner"); !errors.Is(err, ErrCustomerCredentialConflict) {
		t.Fatalf("provider-mismatched Create error = %v", err)
	}
	queriedCreate, err := writer.Outcome(ctx, created.Operation)
	if err != nil || queriedCreate.CredentialHandle != created.CredentialHandle || queriedCreate.Revision != created.Revision {
		t.Fatalf("lost-response create outcome = %#v, %v", queriedCreate, err)
	}
	wrongIdentity := created.Operation
	wrongIdentity.OwnerAccountID = "acc_other"
	if _, err := writer.Outcome(ctx, wrongIdentity); !errors.Is(err, ErrCustomerCredentialOutcomeUnavailable) {
		t.Fatalf("wrong-identity outcome error = %v", err)
	}
	wrongFingerprint := created.Operation
	wrongFingerprint.SecretSHA256 = strings.Repeat("0", 64)
	if _, err := writer.Outcome(ctx, wrongFingerprint); !errors.Is(err, ErrCustomerCredentialOutcomeUnavailable) {
		t.Fatalf("wrong-fingerprint outcome error = %v", err)
	}

	scope := CustomerCredentialScope{
		CustomerCredentialIdentity: customerTestIdentity,
		CredentialHandle:           created.CredentialHandle,
		Revision:                   created.Revision,
	}
	if _, err := writer.Revoke(ctx, "operation_create_lifecycle", scope, "user:owner"); !errors.Is(err, ErrCustomerCredentialConflict) {
		t.Fatalf("cross-action operation reuse error = %v", err)
	}
	rotated, err := writer.Rotate(ctx, "operation_rotate_lifecycle", scope, []byte("customer-secret-v2"), "user:owner")
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if rotated.Revision != 2 {
		t.Fatalf("rotated revision = %d", rotated.Revision)
	}
	replayedRotate, err := writer.Rotate(ctx, "operation_rotate_lifecycle", scope, []byte("customer-secret-v2"), "user:owner")
	if err != nil || replayedRotate.Revision != 2 || !replayedRotate.Replayed {
		t.Fatalf("replayed Rotate = %#v, %v", replayedRotate, err)
	}
	if _, err := writer.Rotate(ctx, "operation_rotate_lifecycle", scope, []byte("different-replay-secret"), "user:owner"); !errors.Is(err, ErrCustomerCredentialConflict) {
		t.Fatalf("same rotate operation with another secret error = %v", err)
	}
	if _, err := repository.GetActiveCustomer(ctx, scope); !errors.Is(err, ErrCustomerCredentialUnavailable) {
		t.Fatalf("old revision lookup error = %v", err)
	}

	rotatedScope := scope
	rotatedScope.Revision = rotated.Revision
	resolver, err := NewCustomerResolver(repository, cipher)
	if err != nil {
		t.Fatalf("NewCustomerResolver: %v", err)
	}
	plaintext, err := resolver.ResolveForInference(ctx, rotatedScope)
	if err != nil {
		t.Fatalf("ResolveForInference: %v", err)
	}
	if string(plaintext) != "customer-secret-v2" {
		clear(plaintext)
		t.Fatalf("resolved plaintext = %q", plaintext)
	}
	clear(plaintext)

	mismatch := rotatedScope
	mismatch.OwnerAccountID = "acc_other"
	if _, err := resolver.ResolveForInference(ctx, mismatch); !errors.Is(err, ErrCustomerCredentialUnavailable) {
		t.Fatalf("mismatched owner error = %v", err)
	}

	revoked, err := writer.Revoke(ctx, "operation_revoke_lifecycle", rotatedScope, "user:owner")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if revoked.Revision != 3 {
		t.Fatalf("revoked revision = %d", revoked.Revision)
	}
	replayedRevoke, err := writer.Revoke(ctx, "operation_revoke_lifecycle", rotatedScope, "user:owner")
	if err != nil || replayedRevoke.Revision != 3 || !replayedRevoke.Replayed {
		t.Fatalf("replayed Revoke = %#v, %v", replayedRevoke, err)
	}
	if _, err := resolver.ResolveForInference(ctx, rotatedScope); !errors.Is(err, ErrCustomerCredentialUnavailable) {
		t.Fatalf("revoked credential error = %v", err)
	}

	concurrentIdentity := customerTestIdentity
	concurrentIdentity.ConnectionID = "conn_customer_concurrent"
	concurrentHandle := "kcred_22222222222222222222222222"
	concurrentWriter, err := newCustomerWriter(repository, &recordingCustomerCipher{}, func() (string, error) {
		return concurrentHandle, nil
	})
	if err != nil {
		t.Fatalf("new concurrent writer: %v", err)
	}
	concurrentCreated, err := concurrentWriter.Create(ctx, "operation_create_concurrent", concurrentIdentity, []byte("concurrent-v1"), "user:owner")
	if err != nil {
		t.Fatalf("create concurrent fixture: %v", err)
	}
	concurrentScope := CustomerCredentialScope{
		CustomerCredentialIdentity: concurrentIdentity,
		CredentialHandle:           concurrentCreated.CredentialHandle,
		Revision:                   concurrentCreated.Revision,
	}
	type concurrentResult struct {
		outcome CustomerCredentialOutcome
		err     error
	}
	start := make(chan struct{})
	results := make(chan concurrentResult, 2)
	concurrentInputs := []struct {
		operationID string
		secret      string
	}{
		{operationID: "operation_rotate_concurrent_a", secret: "concurrent-v2-a"},
		{operationID: "operation_rotate_concurrent_b", secret: "concurrent-v2-b"},
	}
	for _, input := range concurrentInputs {
		input := input
		go func() {
			worker, workerErr := newCustomerWriter(repository, &recordingCustomerCipher{}, func() (string, error) {
				return concurrentHandle, nil
			})
			if workerErr != nil {
				results <- concurrentResult{err: workerErr}
				return
			}
			<-start
			outcome, rotateErr := worker.Rotate(ctx, input.operationID, concurrentScope, []byte(input.secret), "user:owner")
			results <- concurrentResult{outcome: outcome, err: rotateErr}
		}()
	}
	close(start)
	applied := 0
	conflicted := 0
	for range 2 {
		result := <-results
		switch {
		case result.err == nil && result.outcome.Status == CustomerCredentialOutcomeApplied:
			applied++
		case errors.Is(result.err, ErrCustomerCredentialConflict) && result.outcome.Status == CustomerCredentialOutcomeConflict:
			conflicted++
		default:
			t.Fatalf("unexpected concurrent result = %#v, %v", result.outcome, result.err)
		}
	}
	if applied != 1 || conflicted != 1 {
		t.Fatalf("concurrent applied/conflicted = %d/%d", applied, conflicted)
	}
	for _, input := range concurrentInputs {
		operation := operationForScope(input.operationID, CustomerCredentialActionRotate, concurrentScope)
		digest := sha256.Sum256([]byte(input.secret))
		operation.SecretSHA256 = hex.EncodeToString(digest[:])
		outcome, outcomeErr := concurrentWriter.Outcome(ctx, operation)
		if outcomeErr != nil || (outcome.Status != CustomerCredentialOutcomeApplied && outcome.Status != CustomerCredentialOutcomeConflict) {
			t.Fatalf("concurrent durable outcome %s = %#v, %v", input.operationID, outcome, outcomeErr)
		}
	}

	var auditActions []string
	rows, err := pool.Query(ctx, `SELECT action FROM customer_provider_credential_audit ORDER BY audit_id`)
	if err != nil {
		t.Fatalf("querying audit: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatalf("scanning audit: %v", err)
		}
		auditActions = append(auditActions, action)
	}
	if len(auditActions) != 5 || auditActions[0] != "create" || auditActions[1] != "rotate" || auditActions[2] != "revoke" || auditActions[3] != "create" || auditActions[4] != "rotate" {
		t.Fatalf("audit actions = %#v", auditActions)
	}

	privileges := map[string]bool{
		"control credential table access": false,
		"control operation table access":  false,
		"control create":                  true,
		"control rotate":                  true,
		"control revoke":                  true,
		"control outcome":                 true,
		"control resolve":                 false,
		"runtime credential table access": false,
		"runtime operation table access":  false,
		"runtime create":                  false,
		"runtime rotate":                  false,
		"runtime revoke":                  false,
		"runtime outcome":                 false,
		"runtime resolve":                 true,
	}
	queries := map[string]string{
		"control credential table access": `SELECT has_table_privilege('kaana_customer_credential_control', 'customer_provider_credentials', 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')`,
		"control operation table access":  `SELECT has_table_privilege('kaana_customer_credential_control', 'customer_provider_credential_operations', 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')`,
		"control create":                  `SELECT has_function_privilege('kaana_customer_credential_control', 'kaana_apply_customer_provider_credential_create(text,text,text,text,text,text,bytea,text,text,text)', 'EXECUTE')`,
		"control rotate":                  `SELECT has_function_privilege('kaana_customer_credential_control', 'kaana_apply_customer_provider_credential_rotate(text,text,text,text,text,text,bigint,bytea,text,text,text)', 'EXECUTE')`,
		"control revoke":                  `SELECT has_function_privilege('kaana_customer_credential_control', 'kaana_apply_customer_provider_credential_revoke(text,text,text,text,text,text,bigint,text)', 'EXECUTE')`,
		"control outcome":                 `SELECT has_function_privilege('kaana_customer_credential_control', 'kaana_get_customer_provider_credential_outcome(text,text,text,text,text,text,text,bigint,text)', 'EXECUTE')`,
		"control resolve":                 `SELECT has_function_privilege('kaana_customer_credential_control', 'kaana_get_active_customer_provider_credential(text,text,text,text,text,bigint)', 'EXECUTE')`,
		"runtime credential table access": `SELECT has_table_privilege('kaana_runtime', 'customer_provider_credentials', 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')`,
		"runtime operation table access":  `SELECT has_table_privilege('kaana_runtime', 'customer_provider_credential_operations', 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')`,
		"runtime create":                  `SELECT has_function_privilege('kaana_runtime', 'kaana_apply_customer_provider_credential_create(text,text,text,text,text,text,bytea,text,text,text)', 'EXECUTE')`,
		"runtime rotate":                  `SELECT has_function_privilege('kaana_runtime', 'kaana_apply_customer_provider_credential_rotate(text,text,text,text,text,text,bigint,bytea,text,text,text)', 'EXECUTE')`,
		"runtime revoke":                  `SELECT has_function_privilege('kaana_runtime', 'kaana_apply_customer_provider_credential_revoke(text,text,text,text,text,text,bigint,text)', 'EXECUTE')`,
		"runtime outcome":                 `SELECT has_function_privilege('kaana_runtime', 'kaana_get_customer_provider_credential_outcome(text,text,text,text,text,text,text,bigint,text)', 'EXECUTE')`,
		"runtime resolve":                 `SELECT has_function_privilege('kaana_runtime', 'kaana_get_active_customer_provider_credential(text,text,text,text,text,bigint)', 'EXECUTE')`,
	}
	for name, expected := range privileges {
		var actual bool
		if err := pool.QueryRow(ctx, queries[name]).Scan(&actual); err != nil {
			t.Fatalf("checking %s: %v", name, err)
		}
		if actual != expected {
			t.Fatalf("%s = %t, expected %t", name, actual, expected)
		}
	}
}
