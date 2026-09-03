package credentialstore

import (
	"context"
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
	// The integration database is deliberately reusable: local runs and the
	// race gate may execute this package more than once against the same
	// PostgreSQL instance. Reset every customer-credential table together so
	// exact operation IDs from a prior run cannot turn this lifecycle into an
	// accidental replay while retaining all migrated schema and privileges.
	if _, err := pool.Exec(ctx, `TRUNCATE TABLE
		customer_provider_credential_validations,
		customer_provider_credential_audit,
		customer_provider_credentials,
		customer_provider_credential_operations RESTART IDENTITY`); err != nil {
		t.Fatalf("resetting customer credential test tables: %v", err)
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
	queriedCreate, err := writer.Outcome(ctx, customerOutcomeQueryFor(created.Operation))
	if err != nil || queriedCreate.CredentialHandle != created.CredentialHandle || queriedCreate.Revision != created.Revision {
		t.Fatalf("lost-response create outcome = %#v, %v", queriedCreate, err)
	}
	wrongIdentity := customerOutcomeQueryFor(created.Operation)
	wrongIdentity.OwnerAccountID = "acc_other"
	if _, err := writer.Outcome(ctx, wrongIdentity); !errors.Is(err, ErrCustomerCredentialOutcomeUnavailable) {
		t.Fatalf("wrong-identity outcome error = %v", err)
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

	validation := CustomerCredentialValidationOperation{
		OperationID: "operation_validation_before_topup", ApplicationID: "app_exact",
		CustomerCredentialScope: rotatedScope, DeploymentID: "deployment_exact",
	}
	claimed, err := repository.ClaimCustomerValidation(ctx, validation)
	if err != nil || claimed.State != CustomerCredentialValidationExecute || claimed.LeaseGeneration != 1 {
		t.Fatalf("claim validation = %+v, %v", claimed, err)
	}
	leased, err := repository.ClaimCustomerValidation(ctx, validation)
	if err != nil || leased.State != CustomerCredentialValidationPending || leased.LeaseGeneration != 0 {
		t.Fatalf("live validation lease = %+v, %v", leased, err)
	}
	rebound := validation
	rebound.ApplicationID = "app_rebound"
	conflict, err := repository.ClaimCustomerValidation(ctx, rebound)
	if err != nil || conflict.State != CustomerCredentialValidationConflict {
		t.Fatalf("validation selector conflict = %+v, %v", conflict, err)
	}
	billing := CustomerCredentialValidationOutcome{
		Operation: validation, State: CustomerCredentialValidationInconclusive,
		FailureCode: CustomerCredentialValidationForbidden, LeaseGeneration: claimed.LeaseGeneration,
	}
	if err := repository.CompleteCustomerValidation(ctx, billing); err != nil {
		t.Fatalf("complete billing validation: %v", err)
	}
	replayedBilling, err := repository.ClaimCustomerValidation(ctx, validation)
	if err != nil || replayedBilling.State != CustomerCredentialValidationInconclusive || replayedBilling.FailureCode != CustomerCredentialValidationForbidden {
		t.Fatalf("replayed billing validation = %+v, %v", replayedBilling, err)
	}
	afterTopup := validation
	afterTopup.OperationID = "operation_validation_after_topup"
	claimedAfterTopup, err := repository.ClaimCustomerValidation(ctx, afterTopup)
	if err != nil || claimedAfterTopup.State != CustomerCredentialValidationExecute || claimedAfterTopup.LeaseGeneration != 1 {
		t.Fatalf("post-top-up claim = %+v, %v", claimedAfterTopup, err)
	}
	if err := repository.CompleteCustomerValidation(ctx, CustomerCredentialValidationOutcome{
		Operation: afterTopup, State: CustomerCredentialValidationValid,
		LeaseGeneration: claimedAfterTopup.LeaseGeneration,
	}); err != nil {
		t.Fatalf("complete post-top-up validation: %v", err)
	}

	reclaimed := validation
	reclaimed.OperationID = "operation_validation_reclaimed_lease"
	firstLease, err := repository.ClaimCustomerValidation(ctx, reclaimed)
	if err != nil || firstLease.State != CustomerCredentialValidationExecute || firstLease.LeaseGeneration != 1 {
		t.Fatalf("first reclaimable lease = %+v, %v", firstLease, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE customer_provider_credential_validations
		SET lease_until = NOW() - INTERVAL '1 second'
		WHERE operation_id = $1 AND state = 'running'`, reclaimed.OperationID); err != nil {
		t.Fatalf("expiring first validation lease: %v", err)
	}
	secondLease, err := repository.ClaimCustomerValidation(ctx, reclaimed)
	if err != nil || secondLease.State != CustomerCredentialValidationExecute || secondLease.LeaseGeneration != 2 {
		t.Fatalf("reclaimed validation lease = %+v, %v", secondLease, err)
	}
	type validationCompletion struct {
		leaseGeneration int64
		err             error
	}
	completionStart := make(chan struct{})
	completionResults := make(chan validationCompletion, 2)
	for _, candidate := range []CustomerCredentialValidationOutcome{
		{
			Operation: reclaimed, State: CustomerCredentialValidationInvalid,
			FailureCode: CustomerCredentialValidationUnauthorized, LeaseGeneration: firstLease.LeaseGeneration,
		},
		{
			Operation: reclaimed, State: CustomerCredentialValidationValid,
			LeaseGeneration: secondLease.LeaseGeneration,
		},
	} {
		candidate := candidate
		go func() {
			<-completionStart
			completionResults <- validationCompletion{
				leaseGeneration: candidate.LeaseGeneration,
				err:             repository.CompleteCustomerValidation(ctx, candidate),
			}
		}()
	}
	close(completionStart)
	for range 2 {
		result := <-completionResults
		if result.leaseGeneration == firstLease.LeaseGeneration && result.err == nil {
			t.Fatal("expired validation lease completed after a newer worker reclaimed it")
		}
		if result.leaseGeneration == secondLease.LeaseGeneration && result.err != nil {
			t.Fatalf("current validation lease failed completion: %v", result.err)
		}
	}
	replayedReclaimed, err := repository.ClaimCustomerValidation(ctx, reclaimed)
	if err != nil || replayedReclaimed.State != CustomerCredentialValidationValid || replayedReclaimed.LeaseGeneration != 0 {
		t.Fatalf("reclaimed validation terminal replay = %+v, %v", replayedReclaimed, err)
	}
	if err := repository.CompleteCustomerValidation(ctx, CustomerCredentialValidationOutcome{
		Operation: reclaimed, State: CustomerCredentialValidationValid,
		LeaseGeneration: secondLease.LeaseGeneration,
	}); err != nil {
		t.Fatalf("idempotent current-lease completion: %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT kaana_complete_customer_provider_credential_validation(
		$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		"operation_invalid_null_failure", validation.ApplicationID, validation.Provider,
		validation.OwnerAccountID, validation.ConnectionID, validation.Environment,
		validation.CredentialHandle, validation.Revision, validation.DeploymentID,
		int64(1), "invalid", nil,
	); err == nil {
		t.Fatal("invalid validation without unauthorized failure code was accepted")
	}

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
		query := CustomerCredentialOutcomeQuery{
			OperationID: input.operationID, Action: CustomerCredentialActionRotate,
			CustomerCredentialIdentity: concurrentScope.CustomerCredentialIdentity,
			CredentialHandle:           concurrentScope.CredentialHandle, ExpectedRevision: concurrentScope.Revision,
		}
		outcome, outcomeErr := concurrentWriter.Outcome(ctx, query)
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
		"runtime validation table access": false,
		"runtime validation claim":        true,
		"runtime validation complete":     true,
		"legacy digest outcome exists":    false,
	}
	queries := map[string]string{
		"control credential table access": `SELECT has_table_privilege('kaana_customer_credential_control', 'customer_provider_credentials', 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')`,
		"control operation table access":  `SELECT has_table_privilege('kaana_customer_credential_control', 'customer_provider_credential_operations', 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')`,
		"control create":                  `SELECT has_function_privilege('kaana_customer_credential_control', 'kaana_apply_customer_provider_credential_create(text,text,text,text,text,text,bytea,text,text,text)', 'EXECUTE')`,
		"control rotate":                  `SELECT has_function_privilege('kaana_customer_credential_control', 'kaana_apply_customer_provider_credential_rotate(text,text,text,text,text,text,bigint,bytea,text,text,text)', 'EXECUTE')`,
		"control revoke":                  `SELECT has_function_privilege('kaana_customer_credential_control', 'kaana_apply_customer_provider_credential_revoke(text,text,text,text,text,text,bigint,text)', 'EXECUTE')`,
		"control outcome":                 `SELECT has_function_privilege('kaana_customer_credential_control', 'kaana_get_customer_provider_credential_outcome(text,text,text,text,text,text,text,bigint)', 'EXECUTE')`,
		"control resolve":                 `SELECT has_function_privilege('kaana_customer_credential_control', 'kaana_get_active_customer_provider_credential(text,text,text,text,text,bigint)', 'EXECUTE')`,
		"runtime credential table access": `SELECT has_table_privilege('kaana_runtime', 'customer_provider_credentials', 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')`,
		"runtime operation table access":  `SELECT has_table_privilege('kaana_runtime', 'customer_provider_credential_operations', 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')`,
		"runtime create":                  `SELECT has_function_privilege('kaana_runtime', 'kaana_apply_customer_provider_credential_create(text,text,text,text,text,text,bytea,text,text,text)', 'EXECUTE')`,
		"runtime rotate":                  `SELECT has_function_privilege('kaana_runtime', 'kaana_apply_customer_provider_credential_rotate(text,text,text,text,text,text,bigint,bytea,text,text,text)', 'EXECUTE')`,
		"runtime revoke":                  `SELECT has_function_privilege('kaana_runtime', 'kaana_apply_customer_provider_credential_revoke(text,text,text,text,text,text,bigint,text)', 'EXECUTE')`,
		"runtime outcome":                 `SELECT has_function_privilege('kaana_runtime', 'kaana_get_customer_provider_credential_outcome(text,text,text,text,text,text,text,bigint)', 'EXECUTE')`,
		"runtime resolve":                 `SELECT has_function_privilege('kaana_runtime', 'kaana_get_active_customer_provider_credential(text,text,text,text,text,bigint)', 'EXECUTE')`,
		"runtime validation table access": `SELECT has_table_privilege('kaana_runtime', 'customer_provider_credential_validations', 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')`,
		"runtime validation claim":        `SELECT has_function_privilege('kaana_runtime', 'kaana_claim_customer_provider_credential_validation(text,text,text,text,text,text,text,bigint,text)', 'EXECUTE')`,
		"runtime validation complete":     `SELECT has_function_privilege('kaana_runtime', 'kaana_complete_customer_provider_credential_validation(text,text,text,text,text,text,text,bigint,text,bigint,text,text)', 'EXECUTE')`,
		"legacy digest outcome exists":    `SELECT to_regprocedure('kaana_get_customer_provider_credential_outcome(text,text,text,text,text,text,text,bigint,text)') IS NOT NULL`,
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
