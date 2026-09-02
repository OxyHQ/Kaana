package credentialstore

import (
	"context"
	"errors"
	"os"
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

	cipher := &recordingCustomerCipher{}
	writer := newFixedCustomerWriter(t, repository, cipher)
	reference, err := writer.Create(ctx, customerTestIdentity, []byte("customer-secret-v1"), "user:owner")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if reference.CredentialHandle != fixedCustomerHandle || reference.Revision != 1 {
		t.Fatalf("create reference = %#v", reference)
	}

	existing, err := writer.Create(ctx, customerTestIdentity, []byte("must-not-overwrite"), "user:owner")
	if !errors.Is(err, ErrCustomerCredentialExists) || existing != reference {
		t.Fatalf("duplicate Create = %#v, %v", existing, err)
	}
	providerMismatch := customerTestIdentity
	providerMismatch.Provider = "openai"
	if _, err := writer.Create(ctx, providerMismatch, []byte("must-not-overwrite"), "user:owner"); !errors.Is(err, ErrCustomerCredentialConflict) {
		t.Fatalf("provider-mismatched Create error = %v", err)
	}

	scope := CustomerCredentialScope{
		CustomerCredentialIdentity: customerTestIdentity,
		CredentialHandle:           reference.CredentialHandle,
		Revision:                   reference.Revision,
	}
	rotated, err := writer.Rotate(ctx, scope, []byte("customer-secret-v2"), "user:owner")
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if rotated.Revision != 2 {
		t.Fatalf("rotated revision = %d", rotated.Revision)
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

	revoked, err := writer.Revoke(ctx, rotatedScope, "user:owner")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if revoked.Revision != 3 {
		t.Fatalf("revoked revision = %d", revoked.Revision)
	}
	if _, err := resolver.ResolveForInference(ctx, rotatedScope); !errors.Is(err, ErrCustomerCredentialUnavailable) {
		t.Fatalf("revoked credential error = %v", err)
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
	if len(auditActions) != 3 || auditActions[0] != "create" || auditActions[1] != "rotate" || auditActions[2] != "revoke" {
		t.Fatalf("audit actions = %#v", auditActions)
	}

	privileges := map[string]bool{
		"control table select": false,
		"control create":       true,
		"control resolve":      false,
		"runtime table select": false,
		"runtime create":       false,
		"runtime resolve":      true,
	}
	queries := map[string]string{
		"control table select": `SELECT has_table_privilege('kaana_customer_credential_control', 'customer_provider_credentials', 'SELECT')`,
		"control create":       `SELECT has_function_privilege('kaana_customer_credential_control', 'kaana_create_customer_provider_credential(text,text,text,text,text,bytea,text,text)', 'EXECUTE')`,
		"control resolve":      `SELECT has_function_privilege('kaana_customer_credential_control', 'kaana_get_active_customer_provider_credential(text,text,text,text,text,bigint)', 'EXECUTE')`,
		"runtime table select": `SELECT has_table_privilege('kaana_runtime', 'customer_provider_credentials', 'SELECT')`,
		"runtime create":       `SELECT has_function_privilege('kaana_runtime', 'kaana_create_customer_provider_credential(text,text,text,text,text,bytea,text,text)', 'EXECUTE')`,
		"runtime resolve":      `SELECT has_function_privilege('kaana_runtime', 'kaana_get_active_customer_provider_credential(text,text,text,text,text,bigint)', 'EXECUTE')`,
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
