package credentialstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPlatformCredentialMutationIsAtomicAndIdempotentInPostgres(t *testing.T) {
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
	var platformRoleExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'kaana_platform_credential_control')`).Scan(&platformRoleExists); err != nil {
		t.Fatalf("checking platform control role: %v", err)
	}
	if !platformRoleExists {
		t.Skip("kaana_platform_credential_control role is not provisioned in the integration database")
	}
	repository := &Postgres{pool: pool}
	if err := repository.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	const (
		operationID  = "kpc_0123456789abcdef0123456789abcdef"
		providerSlug = "platform-control-test"
		keyID        = "123e4567-e89b-42d3-a456-426614174000"
	)
	for _, statement := range []string{
		`DELETE FROM platform_provider_credential_operations WHERE operation_id = $1 OR (provider_slug = $2 AND key_id = $3)`,
		`DELETE FROM provider_credential_audit WHERE provider_slug = $2 AND key_id = $3`,
		`DELETE FROM provider_credentials WHERE provider_slug = $2 AND key_id = $3`,
	} {
		if _, err := pool.Exec(ctx, statement, operationID, providerSlug, keyID); err != nil {
			t.Fatalf("resetting exact platform mutation fixture: %v", err)
		}
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		for _, statement := range []string{
			`DELETE FROM platform_provider_credential_operations WHERE operation_id = $1 OR (provider_slug = $2 AND key_id = $3)`,
			`DELETE FROM provider_credential_audit WHERE provider_slug = $2 AND key_id = $3`,
			`DELETE FROM provider_credentials WHERE provider_slug = $2 AND key_id = $3`,
		} {
			_, _ = pool.Exec(cleanupContext, statement, operationID, providerSlug, keyID)
		}
	})

	write := PlatformCredentialWrite{
		PlatformCredentialMutation: PlatformCredentialMutation{
			SchemaVersion: 1, OperationID: operationID, Provider: providerSlug,
			KeyID: keyID, Class: "free", Position: 999999, OperationActor: "operator:integration",
		},
		Ciphertext:        []byte("first-ciphertext"),
		KMSKeyARN:         "arn:aws:kms:eu-west-1:123456789012:key/00000000-0000-0000-0000-000000000001",
		SecretFingerprint: sha256.Sum256([]byte("same-secret")),
	}
	receipt, err := repository.PutPlatformCredential(ctx, write)
	if err != nil || receipt.Replayed || receipt.Outcome != "applied" {
		t.Fatalf("first write receipt=%+v error=%v", receipt, err)
	}
	replay := write
	replay.Ciphertext = []byte("randomized-second-encryption")
	receipt, err = repository.PutPlatformCredential(ctx, replay)
	if err != nil || !receipt.Replayed {
		t.Fatalf("replay receipt=%+v error=%v", receipt, err)
	}

	conflict := write
	conflict.SecretFingerprint = sha256.Sum256([]byte("different-secret"))
	if _, err := repository.PutPlatformCredential(ctx, conflict); !errors.Is(err, ErrPlatformCredentialConflict) {
		t.Fatalf("secret mismatch error=%v", err)
	}
	var ciphertext []byte
	var auditCount, operationCount int
	if err := pool.QueryRow(ctx, `SELECT encrypted_secret FROM provider_credentials WHERE provider_slug = $1 AND key_id = $2`, providerSlug, keyID).Scan(&ciphertext); err != nil {
		t.Fatalf("reading stored ciphertext: %v", err)
	}
	if string(ciphertext) != "first-ciphertext" {
		t.Fatalf("replay replaced ciphertext with %q", ciphertext)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_credential_audit WHERE provider_slug = $1 AND key_id = $2`, providerSlug, keyID).Scan(&auditCount); err != nil {
		t.Fatalf("counting audit rows: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM platform_provider_credential_operations WHERE operation_id = $1`, operationID).Scan(&operationCount); err != nil {
		t.Fatalf("counting operation rows: %v", err)
	}
	if auditCount != 1 || operationCount != 1 {
		t.Fatalf("audit rows=%d operation rows=%d, want one each", auditCount, operationCount)
	}
}
