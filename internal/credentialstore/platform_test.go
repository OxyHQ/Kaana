package credentialstore

import (
	"context"
	"errors"
	"testing"
)

type platformRepositoryFake struct {
	write PlatformCredentialWrite
}

func (r *platformRepositoryFake) PutPlatformCredential(_ context.Context, write PlatformCredentialWrite) (PlatformCredentialReceipt, error) {
	write.Ciphertext = append([]byte(nil), write.Ciphertext...)
	r.write = write
	return PlatformCredentialReceipt{OperationID: write.OperationID, Provider: write.Provider, KeyID: write.KeyID, Outcome: "applied"}, nil
}

type encryptOnlyCipherFake struct {
	scope Scope
}

func (c *encryptOnlyCipherFake) Encrypt(_ context.Context, scope Scope, plaintext []byte) ([]byte, string, error) {
	c.scope = scope
	return append([]byte("cipher:"), plaintext...), "arn:aws:kms:eu-west-1:123456789012:key/00000000-0000-0000-0000-000000000001", nil
}

func TestPlatformWriterBindsExactIdentityAndFingerprint(t *testing.T) {
	repository := &platformRepositoryFake{}
	cipher := &encryptOnlyCipherFake{}
	writer, err := NewPlatformWriter(repository, cipher)
	if err != nil {
		t.Fatalf("NewPlatformWriter: %v", err)
	}
	mutation := PlatformCredentialMutation{
		SchemaVersion: 1, OperationID: "kpc_0123456789abcdef0123456789abcdef",
		Provider: "cohere", KeyID: "123e4567-e89b-42d3-a456-426614174000",
		Class: "free", Position: 1, OperationActor: "operator:nate",
	}
	secret := []byte("trial-secret")
	if _, err := writer.Import(context.Background(), mutation, secret); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if cipher.scope.Provider != "cohere" || cipher.scope.KeyID != mutation.KeyID {
		t.Fatalf("KMS scope = %+v", cipher.scope)
	}
	if repository.write.SecretFingerprint == [32]byte{} {
		t.Fatal("write-only replay fingerprint is absent")
	}
	if string(repository.write.Ciphertext) != "cipher:trial-secret" {
		t.Fatal("repository did not receive encrypted bytes")
	}
}

func TestPlatformWriterRejectsNonOpaqueKeyBeforeKMS(t *testing.T) {
	writer, _ := NewPlatformWriter(&platformRepositoryFake{}, &encryptOnlyCipherFake{})
	_, err := writer.Import(context.Background(), PlatformCredentialMutation{
		SchemaVersion: 1, OperationID: "kpc_0123456789abcdef0123456789abcdef",
		Provider: "cohere", KeyID: "email-account-key", Class: "free", Position: 1,
		OperationActor: "operator:nate",
	}, []byte("secret"))
	if !errors.Is(err, ErrPlatformCredentialInvalid) {
		t.Fatalf("Import error = %v", err)
	}
}
