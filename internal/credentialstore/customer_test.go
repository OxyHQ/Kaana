package credentialstore

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

const fixedCustomerHandle = "kcred_abcdefghijklmnopqrstuvwxyz"

var customerTestIdentity = CustomerCredentialIdentity{
	Provider:       "anthropic",
	OwnerAccountID: "acc_customer_01",
	ConnectionID:   "conn_customer_01",
	Environment:    contract.EnvironmentProduction,
}

type recordingCustomerCipher struct {
	encryptedScope CustomerCredentialScope
	decryptedScope CustomerCredentialScope
	decryptErr     error
}

func (c *recordingCustomerCipher) EncryptCustomer(_ context.Context, scope CustomerCredentialScope, plaintext []byte) ([]byte, string, error) {
	c.encryptedScope = scope
	return append([]byte("cipher:"), plaintext...), kmsTestKeyARN, nil
}

func (c *recordingCustomerCipher) DecryptCustomer(_ context.Context, scope CustomerCredentialScope, ciphertext []byte, _ string) ([]byte, error) {
	c.decryptedScope = scope
	if c.decryptErr != nil {
		return nil, c.decryptErr
	}
	return bytes.TrimPrefix(ciphertext, []byte("cipher:")), nil
}

type recordingCustomerRepository struct {
	created        EncryptedCustomerCredential
	existing       *CustomerCredentialReference
	rotateChanged  bool
	rotated        EncryptedCustomerCredential
	rotateExpected int64
	revokeChanged  bool
	revoked        CustomerCredentialScope
	active         EncryptedCustomerCredential
	activeErr      error
	operationActor string
}

func (r *recordingCustomerRepository) CreateCustomer(_ context.Context, row EncryptedCustomerCredential, actor string) (*CustomerCredentialReference, error) {
	r.created = row
	r.operationActor = actor
	return r.existing, nil
}

func (r *recordingCustomerRepository) RotateCustomer(_ context.Context, row EncryptedCustomerCredential, expected int64, actor string) (bool, error) {
	r.rotated = row
	r.rotateExpected = expected
	r.operationActor = actor
	return r.rotateChanged, nil
}

func (r *recordingCustomerRepository) RevokeCustomer(_ context.Context, scope CustomerCredentialScope, actor string) (bool, error) {
	r.revoked = scope
	r.operationActor = actor
	return r.revokeChanged, nil
}

func (r *recordingCustomerRepository) GetActiveCustomer(_ context.Context, scope CustomerCredentialScope) (EncryptedCustomerCredential, error) {
	if r.activeErr != nil {
		return EncryptedCustomerCredential{}, r.activeErr
	}
	return r.active, nil
}

func newFixedCustomerWriter(t *testing.T, repository CustomerRepository, cipher CustomerEncryptor) *CustomerWriter {
	t.Helper()
	writer, err := newCustomerWriter(repository, cipher, func() (string, error) { return fixedCustomerHandle, nil })
	if err != nil {
		t.Fatalf("newCustomerWriter: %v", err)
	}
	return writer
}

func TestCustomerCreateBindsTheCompleteIdentityAtRevisionOne(t *testing.T) {
	repository := &recordingCustomerRepository{}
	cipher := &recordingCustomerCipher{}
	writer := newFixedCustomerWriter(t, repository, cipher)
	secret := []byte("customer-provider-secret")

	reference, err := writer.Create(context.Background(), customerTestIdentity, secret, "user:acc_customer_01")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	expectedScope := CustomerCredentialScope{
		CustomerCredentialIdentity: customerTestIdentity,
		CredentialHandle:           fixedCustomerHandle,
		Revision:                   1,
	}
	if cipher.encryptedScope != expectedScope {
		t.Fatalf("KMS scope = %#v, expected %#v", cipher.encryptedScope, expectedScope)
	}
	if repository.created.CustomerCredentialScope != expectedScope {
		t.Fatalf("database scope = %#v, expected %#v", repository.created.CustomerCredentialScope, expectedScope)
	}
	if bytes.Contains(repository.created.Ciphertext, secret) && bytes.Equal(repository.created.Ciphertext, secret) {
		t.Fatal("the repository received plaintext instead of ciphertext")
	}
	if reference.CredentialHandle != fixedCustomerHandle || reference.Revision != 1 {
		t.Fatalf("reference = %#v", reference)
	}
}

func TestCustomerCreateNeverRotatesAnExistingConnection(t *testing.T) {
	existing := &CustomerCredentialReference{CredentialHandle: fixedCustomerHandle, Revision: 7}
	repository := &recordingCustomerRepository{existing: existing}
	writer := newFixedCustomerWriter(t, repository, &recordingCustomerCipher{})

	reference, err := writer.Create(context.Background(), customerTestIdentity, []byte("different-secret"), "user:owner")
	if !errors.Is(err, ErrCustomerCredentialExists) {
		t.Fatalf("Create error = %v", err)
	}
	if reference != *existing {
		t.Fatalf("conflict reference = %#v, expected %#v", reference, *existing)
	}
}

func TestCustomerRotationIsExactAndReplaySafe(t *testing.T) {
	repository := &recordingCustomerRepository{rotateChanged: true}
	cipher := &recordingCustomerCipher{}
	writer := newFixedCustomerWriter(t, repository, cipher)
	scope := CustomerCredentialScope{
		CustomerCredentialIdentity: customerTestIdentity,
		CredentialHandle:           fixedCustomerHandle,
		Revision:                   4,
	}

	reference, err := writer.Rotate(context.Background(), scope, []byte("rotated-secret"), "user:owner")
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if cipher.encryptedScope.Revision != 5 || repository.rotated.Revision != 5 || repository.rotateExpected != 4 {
		t.Fatalf("rotation did not bind old revision 4 to new revision 5: encrypted=%d row=%d expected=%d",
			cipher.encryptedScope.Revision, repository.rotated.Revision, repository.rotateExpected)
	}
	if reference.Revision != 5 {
		t.Fatalf("reference revision = %d", reference.Revision)
	}

	repository.rotateChanged = false
	if _, err := writer.Rotate(context.Background(), scope, []byte("rotated-secret"), "user:owner"); !errors.Is(err, ErrCustomerCredentialConflict) {
		t.Fatalf("replayed Rotate error = %v", err)
	}
}

func TestCustomerRevokeRequiresTheExactActiveRevision(t *testing.T) {
	repository := &recordingCustomerRepository{revokeChanged: true}
	writer := newFixedCustomerWriter(t, repository, &recordingCustomerCipher{})
	scope := CustomerCredentialScope{
		CustomerCredentialIdentity: customerTestIdentity,
		CredentialHandle:           fixedCustomerHandle,
		Revision:                   9,
	}
	reference, err := writer.Revoke(context.Background(), scope, "user:owner")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if repository.revoked != scope || reference.Revision != 10 {
		t.Fatalf("revoke scope/reference = %#v / %#v", repository.revoked, reference)
	}

	repository.revokeChanged = false
	if _, err := writer.Revoke(context.Background(), scope, "user:owner"); !errors.Is(err, ErrCustomerCredentialConflict) {
		t.Fatalf("replayed Revoke error = %v", err)
	}
}

func TestCustomerResolverFailsClosedOnEveryIdentityMismatch(t *testing.T) {
	scope := CustomerCredentialScope{
		CustomerCredentialIdentity: customerTestIdentity,
		CredentialHandle:           fixedCustomerHandle,
		Revision:                   3,
	}
	row := EncryptedCustomerCredential{
		CustomerCredentialScope: scope,
		Ciphertext:              []byte("cipher:customer-provider-secret"),
		KMSKeyARN:               kmsTestKeyARN,
	}

	for name, mutate := range map[string]func(*EncryptedCustomerCredential){
		"provider":    func(value *EncryptedCustomerCredential) { value.Provider = "openai" },
		"owner":       func(value *EncryptedCustomerCredential) { value.OwnerAccountID = "acc_other" },
		"connection":  func(value *EncryptedCustomerCredential) { value.ConnectionID = "conn_other" },
		"environment": func(value *EncryptedCustomerCredential) { value.Environment = contract.EnvironmentStaging },
		"handle":      func(value *EncryptedCustomerCredential) { value.CredentialHandle = "kcred_22222222222222222222222222" },
		"revision":    func(value *EncryptedCustomerCredential) { value.Revision++ },
	} {
		t.Run(name, func(t *testing.T) {
			mismatched := row
			mutate(&mismatched)
			repository := &recordingCustomerRepository{active: mismatched}
			resolver, err := NewCustomerResolver(repository, &recordingCustomerCipher{})
			if err != nil {
				t.Fatalf("NewCustomerResolver: %v", err)
			}
			if _, err := resolver.ResolveForInference(context.Background(), scope); !errors.Is(err, ErrCustomerCredentialUnavailable) {
				t.Fatalf("ResolveForInference error = %v", err)
			}
		})
	}
}

func TestCustomerResolverReturnsPlaintextOnlyForExactInferenceScope(t *testing.T) {
	scope := CustomerCredentialScope{
		CustomerCredentialIdentity: customerTestIdentity,
		CredentialHandle:           fixedCustomerHandle,
		Revision:                   3,
	}
	repository := &recordingCustomerRepository{active: EncryptedCustomerCredential{
		CustomerCredentialScope: scope,
		Ciphertext:              []byte("cipher:customer-provider-secret"),
		KMSKeyARN:               kmsTestKeyARN,
	}}
	cipher := &recordingCustomerCipher{}
	resolver, err := NewCustomerResolver(repository, cipher)
	if err != nil {
		t.Fatalf("NewCustomerResolver: %v", err)
	}
	plaintext, err := resolver.ResolveForInference(context.Background(), scope)
	if err != nil {
		t.Fatalf("ResolveForInference: %v", err)
	}
	defer clear(plaintext)
	if string(plaintext) != "customer-provider-secret" || cipher.decryptedScope != scope {
		t.Fatalf("resolved secret/scope did not match the exact row")
	}
}

func TestCustomerIdentityValidationRefusesNamesAndMalformedIDs(t *testing.T) {
	writer := newFixedCustomerWriter(t, &recordingCustomerRepository{}, &recordingCustomerCipher{})
	for name, identity := range map[string]CustomerCredentialIdentity{
		"provider display name": {Provider: "Anthropic, Inc.", OwnerAccountID: "acc_01", ConnectionID: "conn_01", Environment: contract.EnvironmentProduction},
		"blank owner":           {Provider: "anthropic", ConnectionID: "conn_01", Environment: contract.EnvironmentProduction},
		"connection path":       {Provider: "anthropic", OwnerAccountID: "acc_01", ConnectionID: "../conn", Environment: contract.EnvironmentProduction},
		"unknown environment":   {Provider: "anthropic", OwnerAccountID: "acc_01", ConnectionID: "conn_01", Environment: "live"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := writer.Create(context.Background(), identity, []byte("secret"), "user:owner"); err == nil {
				t.Fatal("invalid identity was accepted")
			}
		})
	}
}
