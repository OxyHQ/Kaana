package credentialstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
)

const (
	customerCredentialHandlePrefix       = "kcred_"
	maxSafeJSONInteger             int64 = 1<<53 - 1
)

var (
	// ErrCustomerCredentialInvalid means a signed mutation is structurally
	// invalid. Callers may map it to a generic 400 without returning details.
	ErrCustomerCredentialInvalid = errors.New("customer provider credential mutation is invalid")
	// ErrCustomerCredentialConflict covers a stale revision, revoked handle, or
	// mismatch between an operation id and any member of its exact request.
	ErrCustomerCredentialConflict = errors.New("customer provider credential identity or revision conflicts")
	// ErrCustomerCredentialOutcomeUnavailable is deliberately returned for both
	// an absent operation and an exact-operation query whose identity differs.
	ErrCustomerCredentialOutcomeUnavailable = errors.New("customer provider credential outcome is unavailable")
	// ErrCustomerCredentialUnavailable is the deliberately indistinguishable
	// inference result for an absent, revoked, mismatched, or undecryptable row.
	ErrCustomerCredentialUnavailable = errors.New("customer provider credential is unavailable")
)

// CustomerCredentialAction is the exact mutation meaning bound to an Oxy
// operation id. An id can never move between actions.
type CustomerCredentialAction string

const (
	CustomerCredentialActionCreate CustomerCredentialAction = "create"
	CustomerCredentialActionRotate CustomerCredentialAction = "rotate"
	CustomerCredentialActionRevoke CustomerCredentialAction = "revoke"
)

// CustomerCredentialOutcomeStatus is the durable terminal answer for one
// operation. A conflict carries no credential reference.
type CustomerCredentialOutcomeStatus string

const (
	CustomerCredentialOutcomeApplied  CustomerCredentialOutcomeStatus = "applied"
	CustomerCredentialOutcomeConflict CustomerCredentialOutcomeStatus = "conflict"
)

// CustomerCredentialIdentity is the immutable Oxy identity a BYOK secret is
// bound to. Kaana carries these values as opaque strings and never resolves
// them into accounts or connections of its own.
type CustomerCredentialIdentity struct {
	Provider       contract.ProviderSlug
	OwnerAccountID string
	ConnectionID   string
	Environment    contract.Environment
}

// CustomerCredentialScope adds Kaana's opaque handle and the exact ciphertext
// revision. Every member is authenticated by the KMS encryption context.
type CustomerCredentialScope struct {
	CustomerCredentialIdentity
	CredentialHandle string
	Revision         int64
}

// CustomerCredentialOperation is the complete replay identity of a BYOK
// mutation. Create has no requested handle or revision; rotate and revoke have
// both. Create and rotate also carry a one-way secret fingerprint. OperationID
// is supplied by Oxy and treated as one exact opaque string.
type CustomerCredentialOperation struct {
	OperationID string
	Action      CustomerCredentialAction
	CustomerCredentialIdentity
	CredentialHandle string
	ExpectedRevision int64
	SecretSHA256     string
}

// CustomerCredentialOutcome is an immutable terminal operation result. The
// internal replay bit lets callers validate a freshly proposed create handle
// without placing that transport detail on the HTTP wire.
type CustomerCredentialOutcome struct {
	Operation        CustomerCredentialOperation
	Status           CustomerCredentialOutcomeStatus
	CredentialHandle string
	Revision         int64
	Replayed         bool
}

// EncryptedCustomerCredential is the ciphertext-only row exchanged with
// PostgreSQL. Plaintext has no database representation.
type EncryptedCustomerCredential struct {
	CustomerCredentialScope
	Ciphertext []byte
	KMSKeyARN  string
}

// CustomerRepository is the least database authority needed by the BYOK
// boundary. Implementations perform mutations through SECURITY DEFINER
// functions rather than granting table DML to the control process.
type CustomerRepository interface {
	CreateCustomer(context.Context, CustomerCredentialOperation, EncryptedCustomerCredential, string) (CustomerCredentialOutcome, error)
	RotateCustomer(context.Context, CustomerCredentialOperation, EncryptedCustomerCredential, string) (CustomerCredentialOutcome, error)
	RevokeCustomer(context.Context, CustomerCredentialOperation, string) (CustomerCredentialOutcome, error)
	GetCustomerOutcome(context.Context, CustomerCredentialOperation) (CustomerCredentialOutcome, error)
	GetActiveCustomer(context.Context, CustomerCredentialScope) (EncryptedCustomerCredential, error)
}

// CustomerEncryptor is the only KMS authority the credential-control process
// needs. Its task role gets Encrypt and never Decrypt.
type CustomerEncryptor interface {
	EncryptCustomer(context.Context, CustomerCredentialScope, []byte) ([]byte, string, error)
}

// CustomerDecryptor is the inverse inference-only authority. The serving task
// gets Decrypt and never Encrypt.
type CustomerDecryptor interface {
	DecryptCustomer(context.Context, CustomerCredentialScope, []byte, string) ([]byte, error)
}

type customerHandleGenerator func() (string, error)

// CustomerWriter creates, rotates, and revokes BYOK ciphertext without holding
// decrypt authority.
type CustomerWriter struct {
	repository CustomerRepository
	encryptor  CustomerEncryptor
	newHandle  customerHandleGenerator
}

// NewCustomerWriter builds the mutation half of the BYOK boundary.
func NewCustomerWriter(repository CustomerRepository, encryptor CustomerEncryptor) (*CustomerWriter, error) {
	return newCustomerWriter(repository, encryptor, randomCustomerCredentialHandle)
}

func newCustomerWriter(repository CustomerRepository, encryptor CustomerEncryptor, newHandle customerHandleGenerator) (*CustomerWriter, error) {
	if repository == nil {
		return nil, errors.New("credential store: customer repository is required")
	}
	if encryptor == nil {
		return nil, errors.New("credential store: customer encryptor is required")
	}
	if newHandle == nil {
		return nil, errors.New("credential store: customer handle generator is required")
	}
	return &CustomerWriter{repository: repository, encryptor: encryptor, newHandle: newHandle}, nil
}

// Create allocates a Kaana-owned opaque handle and creates one immutable
// identity at revision one. Repeating the same operation returns its first
// terminal outcome; a different operation for that identity conflicts.
func (w *CustomerWriter) Create(ctx context.Context, operationID string, identity CustomerCredentialIdentity, plaintext []byte, actor string) (CustomerCredentialOutcome, error) {
	if err := validateCustomerIdentity(identity); err != nil {
		return CustomerCredentialOutcome{}, invalidCustomerCredential(err)
	}
	if err := validateCustomerSecret(plaintext); err != nil {
		return CustomerCredentialOutcome{}, invalidCustomerCredential(err)
	}
	if err := validateActor(actor); err != nil {
		return CustomerCredentialOutcome{}, invalidCustomerCredential(err)
	}
	secretDigest := sha256.Sum256(plaintext)
	operation := CustomerCredentialOperation{
		OperationID:                operationID,
		Action:                     CustomerCredentialActionCreate,
		CustomerCredentialIdentity: identity,
		SecretSHA256:               hex.EncodeToString(secretDigest[:]),
	}
	if err := validateCustomerOperation(operation); err != nil {
		return CustomerCredentialOutcome{}, invalidCustomerCredential(err)
	}
	handle, err := w.newHandle()
	if err != nil {
		return CustomerCredentialOutcome{}, fmt.Errorf("credential store: allocating customer credential handle: %w", err)
	}
	scope := CustomerCredentialScope{CustomerCredentialIdentity: identity, CredentialHandle: handle, Revision: 1}
	if err := validateCustomerScope(scope); err != nil {
		return CustomerCredentialOutcome{}, invalidCustomerCredential(err)
	}
	ciphertext, keyARN, err := w.encryptor.EncryptCustomer(ctx, scope, plaintext)
	if err != nil {
		return CustomerCredentialOutcome{}, fmt.Errorf("credential store: encrypting customer provider credential: %w", err)
	}
	outcome, err := w.repository.CreateCustomer(ctx, operation, EncryptedCustomerCredential{
		CustomerCredentialScope: scope,
		Ciphertext:              ciphertext,
		KMSKeyARN:               keyARN,
	}, actor)
	if err != nil {
		return CustomerCredentialOutcome{}, fmt.Errorf("credential store: creating customer provider credential: %w", err)
	}
	if err := validateCustomerOutcome(operation, outcome); err != nil {
		return CustomerCredentialOutcome{}, err
	}
	if outcome.Status == CustomerCredentialOutcomeConflict {
		return outcome, ErrCustomerCredentialConflict
	}
	if !outcome.Replayed && (outcome.CredentialHandle != handle || outcome.Revision != 1) {
		return CustomerCredentialOutcome{}, errors.New("credential store: fresh create outcome does not match its proposed handle")
	}
	return outcome, nil
}

// Rotate replaces the ciphertext only when the handle, complete identity, and
// expected revision all match one active row. Repeating the same operation id
// returns its original terminal outcome and never rotates twice.
func (w *CustomerWriter) Rotate(ctx context.Context, operationID string, scope CustomerCredentialScope, plaintext []byte, actor string) (CustomerCredentialOutcome, error) {
	if err := validateCustomerScope(scope); err != nil {
		return CustomerCredentialOutcome{}, invalidCustomerCredential(err)
	}
	if err := validateCustomerSecret(plaintext); err != nil {
		return CustomerCredentialOutcome{}, invalidCustomerCredential(err)
	}
	if err := validateActor(actor); err != nil {
		return CustomerCredentialOutcome{}, invalidCustomerCredential(err)
	}
	secretDigest := sha256.Sum256(plaintext)
	operation := operationForScope(operationID, CustomerCredentialActionRotate, scope)
	operation.SecretSHA256 = hex.EncodeToString(secretDigest[:])
	if err := validateCustomerOperation(operation); err != nil {
		return CustomerCredentialOutcome{}, invalidCustomerCredential(err)
	}
	next := scope
	next.Revision++
	ciphertext, keyARN, err := w.encryptor.EncryptCustomer(ctx, next, plaintext)
	if err != nil {
		return CustomerCredentialOutcome{}, fmt.Errorf("credential store: encrypting rotated customer provider credential: %w", err)
	}
	outcome, err := w.repository.RotateCustomer(ctx, operation, EncryptedCustomerCredential{
		CustomerCredentialScope: next,
		Ciphertext:              ciphertext,
		KMSKeyARN:               keyARN,
	}, actor)
	if err != nil {
		return CustomerCredentialOutcome{}, fmt.Errorf("credential store: rotating customer provider credential: %w", err)
	}
	if err := validateCustomerOutcome(operation, outcome); err != nil {
		return CustomerCredentialOutcome{}, err
	}
	if outcome.Status == CustomerCredentialOutcomeConflict {
		return outcome, ErrCustomerCredentialConflict
	}
	return outcome, nil
}

// Revoke terminally removes a handle from inference resolution. It is
// optimistic-concurrency controlled for the same replay property as rotation.
func (w *CustomerWriter) Revoke(ctx context.Context, operationID string, scope CustomerCredentialScope, actor string) (CustomerCredentialOutcome, error) {
	if err := validateCustomerScope(scope); err != nil {
		return CustomerCredentialOutcome{}, invalidCustomerCredential(err)
	}
	if err := validateActor(actor); err != nil {
		return CustomerCredentialOutcome{}, invalidCustomerCredential(err)
	}
	operation := operationForScope(operationID, CustomerCredentialActionRevoke, scope)
	if err := validateCustomerOperation(operation); err != nil {
		return CustomerCredentialOutcome{}, invalidCustomerCredential(err)
	}
	outcome, err := w.repository.RevokeCustomer(ctx, operation, actor)
	if err != nil {
		return CustomerCredentialOutcome{}, fmt.Errorf("credential store: revoking customer provider credential: %w", err)
	}
	if err := validateCustomerOutcome(operation, outcome); err != nil {
		return CustomerCredentialOutcome{}, err
	}
	if outcome.Status == CustomerCredentialOutcomeConflict {
		return outcome, ErrCustomerCredentialConflict
	}
	return outcome, nil
}

// Outcome reads one terminal result only when the operation id, action,
// complete immutable identity, handle and expected revision all match exactly.
func (w *CustomerWriter) Outcome(ctx context.Context, operation CustomerCredentialOperation) (CustomerCredentialOutcome, error) {
	if err := validateCustomerOperation(operation); err != nil {
		return CustomerCredentialOutcome{}, invalidCustomerCredential(err)
	}
	outcome, err := w.repository.GetCustomerOutcome(ctx, operation)
	if err != nil {
		return CustomerCredentialOutcome{}, err
	}
	if err := validateCustomerOutcome(operation, outcome); err != nil {
		return CustomerCredentialOutcome{}, err
	}
	return outcome, nil
}

// CustomerResolver is intentionally mutation-free. Its only caller is the
// inference executor after Oxy has signed an exact credential binding.
type CustomerResolver struct {
	repository CustomerRepository
	decryptor  CustomerDecryptor
}

// NewCustomerResolver builds the inference-only half of the BYOK boundary.
func NewCustomerResolver(repository CustomerRepository, decryptor CustomerDecryptor) (*CustomerResolver, error) {
	if repository == nil {
		return nil, errors.New("credential store: customer repository is required")
	}
	if decryptor == nil {
		return nil, errors.New("credential store: customer decryptor is required")
	}
	return &CustomerResolver{repository: repository, decryptor: decryptor}, nil
}

// ResolveForInference returns plaintext only after one exact active row is
// selected by handle plus every immutable identity member. It deliberately has
// no HTTP wrapper, list operation, or metadata-only fallback.
func (r *CustomerResolver) ResolveForInference(ctx context.Context, scope CustomerCredentialScope) ([]byte, error) {
	if err := validateCustomerScope(scope); err != nil {
		return nil, ErrCustomerCredentialUnavailable
	}
	row, err := r.repository.GetActiveCustomer(ctx, scope)
	if err != nil {
		return nil, ErrCustomerCredentialUnavailable
	}
	if row.CustomerCredentialScope != scope || len(row.Ciphertext) == 0 || strings.TrimSpace(row.KMSKeyARN) == "" {
		return nil, ErrCustomerCredentialUnavailable
	}
	plaintext, err := r.decryptor.DecryptCustomer(ctx, scope, row.Ciphertext, row.KMSKeyARN)
	if err != nil || len(bytes.TrimSpace(plaintext)) == 0 {
		clear(plaintext)
		return nil, ErrCustomerCredentialUnavailable
	}
	return plaintext, nil
}

func randomCustomerCredentialHandle() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return customerCredentialHandlePrefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)), nil
}

func validateCustomerIdentity(identity CustomerCredentialIdentity) error {
	if !identity.Provider.Valid() {
		return fmt.Errorf("credential store: %q is not a provider slug", identity.Provider)
	}
	if err := validateOpaqueCustomerID("owner account id", identity.OwnerAccountID, 64); err != nil {
		return err
	}
	if err := validateOpaqueCustomerID("connection id", identity.ConnectionID, 128); err != nil {
		return err
	}
	switch identity.Environment {
	case contract.EnvironmentDevelopment, contract.EnvironmentStaging, contract.EnvironmentProduction:
	default:
		return fmt.Errorf("credential store: %q is not a credential environment", identity.Environment)
	}
	return nil
}

func validateCustomerScope(scope CustomerCredentialScope) error {
	if err := validateCustomerIdentity(scope.CustomerCredentialIdentity); err != nil {
		return err
	}
	if !validCustomerCredentialHandle(scope.CredentialHandle) {
		return errors.New("credential store: invalid customer credential handle")
	}
	if scope.Revision <= 0 || scope.Revision > maxSafeJSONInteger {
		return errors.New("credential store: customer credential revision must be positive")
	}
	return nil
}

func operationForScope(operationID string, action CustomerCredentialAction, scope CustomerCredentialScope) CustomerCredentialOperation {
	return CustomerCredentialOperation{
		OperationID:                operationID,
		Action:                     action,
		CustomerCredentialIdentity: scope.CustomerCredentialIdentity,
		CredentialHandle:           scope.CredentialHandle,
		ExpectedRevision:           scope.Revision,
	}
}

func validateCustomerOperation(operation CustomerCredentialOperation) error {
	if err := validateOpaqueCustomerID("operation id", operation.OperationID, 128); err != nil {
		return err
	}
	if err := validateCustomerIdentity(operation.CustomerCredentialIdentity); err != nil {
		return err
	}
	switch operation.Action {
	case CustomerCredentialActionCreate:
		if operation.CredentialHandle != "" || operation.ExpectedRevision != 0 {
			return errors.New("credential store: create operation carries a credential reference")
		}
		if !validSecretSHA256(operation.SecretSHA256) {
			return errors.New("credential store: create operation has an invalid secret fingerprint")
		}
	case CustomerCredentialActionRotate:
		if err := validateCustomerScope(CustomerCredentialScope{
			CustomerCredentialIdentity: operation.CustomerCredentialIdentity,
			CredentialHandle:           operation.CredentialHandle,
			Revision:                   operation.ExpectedRevision,
		}); err != nil {
			return err
		}
		if operation.ExpectedRevision >= maxSafeJSONInteger {
			return errors.New("credential store: customer credential revision cannot be advanced safely")
		}
		if !validSecretSHA256(operation.SecretSHA256) {
			return errors.New("credential store: rotate operation has an invalid secret fingerprint")
		}
	case CustomerCredentialActionRevoke:
		if err := validateCustomerScope(CustomerCredentialScope{
			CustomerCredentialIdentity: operation.CustomerCredentialIdentity,
			CredentialHandle:           operation.CredentialHandle,
			Revision:                   operation.ExpectedRevision,
		}); err != nil {
			return err
		}
		if operation.ExpectedRevision >= maxSafeJSONInteger {
			return errors.New("credential store: customer credential revision cannot be advanced safely")
		}
		if operation.SecretSHA256 != "" {
			return errors.New("credential store: revoke operation carries a secret fingerprint")
		}
	default:
		return errors.New("credential store: invalid customer credential action")
	}
	return nil
}

func validSecretSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validateCustomerOutcome(operation CustomerCredentialOperation, outcome CustomerCredentialOutcome) error {
	if outcome.Operation != operation {
		return errors.New("credential store: outcome operation does not match the request")
	}
	switch outcome.Status {
	case CustomerCredentialOutcomeApplied:
		if !validCustomerCredentialHandle(outcome.CredentialHandle) {
			return errors.New("credential store: applied outcome has an invalid handle")
		}
		expectedRevision := int64(1)
		if operation.Action != CustomerCredentialActionCreate {
			expectedRevision = operation.ExpectedRevision + 1
			if outcome.CredentialHandle != operation.CredentialHandle {
				return errors.New("credential store: applied outcome changed the requested handle")
			}
		}
		if outcome.Revision != expectedRevision {
			return errors.New("credential store: applied outcome has an invalid revision")
		}
	case CustomerCredentialOutcomeConflict:
		if outcome.CredentialHandle != "" || outcome.Revision != 0 {
			return errors.New("credential store: conflict outcome exposes a credential reference")
		}
	default:
		return errors.New("credential store: outcome has an invalid status")
	}
	return nil
}

func validateOpaqueCustomerID(label, value string, maximum int) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maximum {
		return fmt.Errorf("credential store: invalid %s", label)
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') &&
			character != '_' && character != '-' {
			return fmt.Errorf("credential store: invalid %s", label)
		}
	}
	return nil
}

func validCustomerCredentialHandle(handle string) bool {
	if !strings.HasPrefix(handle, customerCredentialHandlePrefix) || len(handle) != len(customerCredentialHandlePrefix)+26 {
		return false
	}
	for _, character := range handle[len(customerCredentialHandlePrefix):] {
		if (character < 'a' || character > 'z') && (character < '2' || character > '7') {
			return false
		}
	}
	return true
}

func validateCustomerSecret(secret []byte) error {
	if len(secret) == 0 || len(secret) > 4096 || len(bytes.TrimSpace(secret)) == 0 || bytes.ContainsAny(secret, "\r\n") {
		return errors.New("credential store: provider credential must be one non-empty line of at most 4096 bytes")
	}
	return nil
}

func invalidCustomerCredential(err error) error {
	return fmt.Errorf("%w: %w", ErrCustomerCredentialInvalid, err)
}
