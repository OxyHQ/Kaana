package credentialstore

import (
	"bytes"
	"context"
	"errors"
	"strings"
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
	created          EncryptedCustomerCredential
	createdOperation CustomerCredentialOperation
	createConflict   bool
	rotateChanged    bool
	rotateOperation  CustomerCredentialOperation
	rotated          EncryptedCustomerCredential
	revokeChanged    bool
	revoked          CustomerCredentialOperation
	outcome          CustomerCredentialOutcome
	outcomeErr       error
	active           EncryptedCustomerCredential
	activeErr        error
	operationActor   string
}

func (r *recordingCustomerRepository) CreateCustomer(_ context.Context, operation CustomerCredentialOperation, row EncryptedCustomerCredential, actor string) (CustomerCredentialOutcome, error) {
	r.createdOperation = operation
	r.created = row
	r.operationActor = actor
	if r.createConflict {
		return CustomerCredentialOutcome{Operation: operation, Status: CustomerCredentialOutcomeConflict}, nil
	}
	return CustomerCredentialOutcome{
		Operation: operation, Status: CustomerCredentialOutcomeApplied,
		CredentialHandle: row.CredentialHandle, Revision: 1,
	}, nil
}

func (r *recordingCustomerRepository) RotateCustomer(_ context.Context, operation CustomerCredentialOperation, row EncryptedCustomerCredential, actor string) (CustomerCredentialOutcome, error) {
	r.rotateOperation = operation
	r.rotated = row
	r.operationActor = actor
	if !r.rotateChanged {
		return CustomerCredentialOutcome{Operation: operation, Status: CustomerCredentialOutcomeConflict}, nil
	}
	return CustomerCredentialOutcome{
		Operation: operation, Status: CustomerCredentialOutcomeApplied,
		CredentialHandle: operation.CredentialHandle, Revision: operation.ExpectedRevision + 1,
	}, nil
}

func (r *recordingCustomerRepository) RevokeCustomer(_ context.Context, operation CustomerCredentialOperation, actor string) (CustomerCredentialOutcome, error) {
	r.revoked = operation
	r.operationActor = actor
	if !r.revokeChanged {
		return CustomerCredentialOutcome{Operation: operation, Status: CustomerCredentialOutcomeConflict}, nil
	}
	return CustomerCredentialOutcome{
		Operation: operation, Status: CustomerCredentialOutcomeApplied,
		CredentialHandle: operation.CredentialHandle, Revision: operation.ExpectedRevision + 1,
	}, nil
}

func (r *recordingCustomerRepository) GetCustomerOutcome(_ context.Context, query CustomerCredentialOutcomeQuery) (CustomerCredentialOutcome, error) {
	if r.outcomeErr != nil {
		return CustomerCredentialOutcome{}, r.outcomeErr
	}
	if customerOutcomeQueryFor(r.outcome.Operation) != query {
		return CustomerCredentialOutcome{}, ErrCustomerCredentialOutcomeUnavailable
	}
	return r.outcome, nil
}

func customerOutcomeQueryFor(operation CustomerCredentialOperation) CustomerCredentialOutcomeQuery {
	return CustomerCredentialOutcomeQuery{
		OperationID: operation.OperationID, Action: operation.Action,
		CustomerCredentialIdentity: operation.CustomerCredentialIdentity,
		CredentialHandle:           operation.CredentialHandle,
		ExpectedRevision:           operation.ExpectedRevision,
	}
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

	outcome, err := writer.Create(context.Background(), "operation_create_01", customerTestIdentity, secret, "user:acc_customer_01")
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
	if len(repository.createdOperation.SecretSHA256) != 64 || repository.createdOperation.SecretSHA256 == string(secret) {
		t.Fatal("the operation ledger did not receive a fixed one-way secret fingerprint")
	}
	if outcome.CredentialHandle != fixedCustomerHandle || outcome.Revision != 1 || outcome.Operation.OperationID != "operation_create_01" {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestCustomerCreateNeverRotatesAnExistingConnection(t *testing.T) {
	repository := &recordingCustomerRepository{createConflict: true}
	writer := newFixedCustomerWriter(t, repository, &recordingCustomerCipher{})

	outcome, err := writer.Create(context.Background(), "operation_create_02", customerTestIdentity, []byte("different-secret"), "user:owner")
	if !errors.Is(err, ErrCustomerCredentialConflict) {
		t.Fatalf("Create error = %v", err)
	}
	if outcome.Status != CustomerCredentialOutcomeConflict || outcome.CredentialHandle != "" || outcome.Revision != 0 {
		t.Fatalf("conflict outcome = %#v", outcome)
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

	outcome, err := writer.Rotate(context.Background(), "operation_rotate_01", scope, []byte("rotated-secret"), "user:owner")
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if cipher.encryptedScope.Revision != 5 || repository.rotated.Revision != 5 || repository.rotateOperation.ExpectedRevision != 4 {
		t.Fatalf("rotation did not bind old revision 4 to new revision 5: encrypted=%d row=%d expected=%d",
			cipher.encryptedScope.Revision, repository.rotated.Revision, repository.rotateOperation.ExpectedRevision)
	}
	if outcome.Revision != 5 {
		t.Fatalf("outcome revision = %d", outcome.Revision)
	}

	repository.rotateChanged = false
	if _, err := writer.Rotate(context.Background(), "operation_rotate_02", scope, []byte("rotated-secret"), "user:owner"); !errors.Is(err, ErrCustomerCredentialConflict) {
		t.Fatalf("conflicting Rotate error = %v", err)
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
	outcome, err := writer.Revoke(context.Background(), "operation_revoke_01", scope, "user:owner")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if repository.revoked.CredentialHandle != scope.CredentialHandle || repository.revoked.ExpectedRevision != scope.Revision || outcome.Revision != 10 {
		t.Fatalf("revoke operation/outcome = %#v / %#v", repository.revoked, outcome)
	}

	repository.revokeChanged = false
	if _, err := writer.Revoke(context.Background(), "operation_revoke_02", scope, "user:owner"); !errors.Is(err, ErrCustomerCredentialConflict) {
		t.Fatalf("conflicting Revoke error = %v", err)
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
			if _, err := writer.Create(context.Background(), "operation_invalid", identity, []byte("secret"), "user:owner"); err == nil {
				t.Fatal("invalid identity was accepted")
			}
		})
	}
}

func TestCustomerOutcomeFailsClosedOnWrongIdentityAndMalformedOperationID(t *testing.T) {
	operation := CustomerCredentialOperation{
		OperationID:                "operation_rotate_exact",
		Action:                     CustomerCredentialActionRotate,
		CustomerCredentialIdentity: customerTestIdentity,
		CredentialHandle:           fixedCustomerHandle,
		ExpectedRevision:           4,
		SecretSHA256:               strings.Repeat("a", 64),
	}
	repository := &recordingCustomerRepository{outcome: CustomerCredentialOutcome{
		Operation: operation, Status: CustomerCredentialOutcomeApplied,
		CredentialHandle: fixedCustomerHandle, Revision: 5,
	}}
	writer := newFixedCustomerWriter(t, repository, &recordingCustomerCipher{})
	query := customerOutcomeQueryFor(operation)
	for name, mutate := range map[string]func(*CustomerCredentialOutcomeQuery){
		"operation id": func(value *CustomerCredentialOutcomeQuery) { value.OperationID = "operation_other" },
		"action": func(value *CustomerCredentialOutcomeQuery) {
			value.Action = CustomerCredentialActionRevoke
		},
		"provider":    func(value *CustomerCredentialOutcomeQuery) { value.Provider = "openai" },
		"owner":       func(value *CustomerCredentialOutcomeQuery) { value.OwnerAccountID = "acc_other" },
		"connection":  func(value *CustomerCredentialOutcomeQuery) { value.ConnectionID = "conn_other" },
		"environment": func(value *CustomerCredentialOutcomeQuery) { value.Environment = contract.EnvironmentStaging },
		"handle": func(value *CustomerCredentialOutcomeQuery) {
			value.CredentialHandle = "kcred_22222222222222222222222222"
		},
		"revision": func(value *CustomerCredentialOutcomeQuery) { value.ExpectedRevision++ },
	} {
		t.Run(name, func(t *testing.T) {
			wrong := query
			mutate(&wrong)
			if _, err := writer.Outcome(context.Background(), wrong); !errors.Is(err, ErrCustomerCredentialOutcomeUnavailable) {
				t.Fatalf("wrong operation outcome error = %v", err)
			}
		})
	}
	malformed := query
	malformed.OperationID = strings.Repeat("a", 129)
	if _, err := writer.Outcome(context.Background(), malformed); !errors.Is(err, ErrCustomerCredentialInvalid) {
		t.Fatalf("oversized operation id error = %v", err)
	}
	unsafeRevision := query
	unsafeRevision.ExpectedRevision = maxSafeJSONInteger
	if _, err := writer.Outcome(context.Background(), unsafeRevision); !errors.Is(err, ErrCustomerCredentialInvalid) {
		t.Fatalf("non-advanceable revision error = %v", err)
	}
}

func TestCustomerOutcomeTreatsCorruptRepositoryResultAsInternalFailure(t *testing.T) {
	operation := CustomerCredentialOperation{
		OperationID:                "operation_rotate_corrupt",
		Action:                     CustomerCredentialActionRotate,
		CustomerCredentialIdentity: customerTestIdentity,
		CredentialHandle:           fixedCustomerHandle,
		ExpectedRevision:           4,
		SecretSHA256:               strings.Repeat("a", 64),
	}
	repository := &recordingCustomerRepository{outcome: CustomerCredentialOutcome{
		Operation: operation, Status: CustomerCredentialOutcomeApplied,
		CredentialHandle: fixedCustomerHandle, Revision: 99,
	}}
	writer := newFixedCustomerWriter(t, repository, &recordingCustomerCipher{})
	if _, err := writer.Outcome(context.Background(), customerOutcomeQueryFor(operation)); err == nil || errors.Is(err, ErrCustomerCredentialOutcomeUnavailable) {
		t.Fatalf("corrupt outcome error = %v", err)
	}
}

func TestCustomerMutationPayloadLimitsAreExact(t *testing.T) {
	writer := newFixedCustomerWriter(t, &recordingCustomerRepository{}, &recordingCustomerCipher{})
	if _, err := writer.Create(context.Background(), "operation_secret_4096", customerTestIdentity, []byte(strings.Repeat("a", 4096)), "user:owner"); err != nil {
		t.Fatalf("4096-byte secret was refused: %v", err)
	}
	for name, secret := range map[string][]byte{
		"4097 bytes":        []byte(strings.Repeat("a", 4097)),
		"multiline":         []byte("first\nsecond"),
		"NUL control byte":  {'v', 'a', 'l', 'i', 'd', 0, 'x'},
		"tab control byte":  []byte("valid\tvalue"),
		"surrounding space": []byte(" customer-secret "),
		"non-ASCII":         []byte("credencial-ñ"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := writer.Create(context.Background(), "operation_secret_invalid", customerTestIdentity, secret, "user:owner"); !errors.Is(err, ErrCustomerCredentialInvalid) {
				t.Fatalf("invalid secret error = %v", err)
			}
		})
	}
}

func TestCustomerOutcomeQueryIsActionDiscriminatedWithoutSecretMaterial(t *testing.T) {
	writer := newFixedCustomerWriter(t, &recordingCustomerRepository{}, &recordingCustomerCipher{})
	create := CustomerCredentialOutcomeQuery{
		OperationID:                "operation_create_outcome",
		Action:                     CustomerCredentialActionCreate,
		CustomerCredentialIdentity: customerTestIdentity,
	}
	if _, err := writer.Outcome(context.Background(), create); !errors.Is(err, ErrCustomerCredentialOutcomeUnavailable) {
		t.Fatalf("valid create outcome query error = %v", err)
	}
	rotate := CustomerCredentialOutcomeQuery{
		OperationID: "operation_rotate_outcome", Action: CustomerCredentialActionRotate,
		CustomerCredentialIdentity: customerTestIdentity,
		CredentialHandle:           fixedCustomerHandle, ExpectedRevision: 1,
	}
	if _, err := writer.Outcome(context.Background(), rotate); !errors.Is(err, ErrCustomerCredentialOutcomeUnavailable) {
		t.Fatalf("valid rotate outcome query error = %v", err)
	}
	rotate.CredentialHandle = ""
	if _, err := writer.Outcome(context.Background(), rotate); !errors.Is(err, ErrCustomerCredentialInvalid) {
		t.Fatalf("rotate without handle error = %v", err)
	}
}
