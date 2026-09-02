package credentialstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
)

const customerCredentialHandlePrefix = "kcred_"

var (
	// ErrCustomerCredentialInvalid means a signed mutation is structurally
	// invalid. Callers may map it to a generic 400 without returning details.
	ErrCustomerCredentialInvalid = errors.New("customer provider credential mutation is invalid")
	// ErrCustomerCredentialExists means the exact Oxy connection identity is
	// already bound to a Kaana credential handle. Creation never rotates it.
	ErrCustomerCredentialExists = errors.New("customer provider credential already exists")
	// ErrCustomerCredentialConflict covers a stale revision, revoked handle, or
	// mismatch between the handle and any member of its exact identity.
	ErrCustomerCredentialConflict = errors.New("customer provider credential identity or revision conflicts")
	// ErrCustomerCredentialUnavailable is the deliberately indistinguishable
	// inference result for an absent, revoked, mismatched, or undecryptable row.
	ErrCustomerCredentialUnavailable = errors.New("customer provider credential is unavailable")
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

// EncryptedCustomerCredential is the ciphertext-only row exchanged with
// PostgreSQL. Plaintext has no database representation.
type EncryptedCustomerCredential struct {
	CustomerCredentialScope
	Ciphertext []byte
	KMSKeyARN  string
}

// CustomerCredentialReference is the only value Oxy needs to retain beside its
// own connection metadata.
type CustomerCredentialReference struct {
	CredentialHandle string `json:"credentialHandle"`
	Revision         int64  `json:"revision"`
}

// CustomerRepository is the least database authority needed by the BYOK
// boundary. Implementations perform mutations through SECURITY DEFINER
// functions rather than granting table DML to the control process.
type CustomerRepository interface {
	CreateCustomer(context.Context, EncryptedCustomerCredential, string) (*CustomerCredentialReference, error)
	RotateCustomer(context.Context, EncryptedCustomerCredential, int64, string) (bool, error)
	RevokeCustomer(context.Context, CustomerCredentialScope, string) (bool, error)
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
// identity at revision one. A duplicate identity is reported, never upserted.
func (w *CustomerWriter) Create(ctx context.Context, identity CustomerCredentialIdentity, plaintext []byte, actor string) (CustomerCredentialReference, error) {
	if err := validateCustomerIdentity(identity); err != nil {
		return CustomerCredentialReference{}, invalidCustomerCredential(err)
	}
	if err := validateCustomerSecret(plaintext); err != nil {
		return CustomerCredentialReference{}, invalidCustomerCredential(err)
	}
	if err := validateActor(actor); err != nil {
		return CustomerCredentialReference{}, invalidCustomerCredential(err)
	}
	handle, err := w.newHandle()
	if err != nil {
		return CustomerCredentialReference{}, fmt.Errorf("credential store: allocating customer credential handle: %w", err)
	}
	scope := CustomerCredentialScope{CustomerCredentialIdentity: identity, CredentialHandle: handle, Revision: 1}
	if err := validateCustomerScope(scope); err != nil {
		return CustomerCredentialReference{}, invalidCustomerCredential(err)
	}
	ciphertext, keyARN, err := w.encryptor.EncryptCustomer(ctx, scope, plaintext)
	if err != nil {
		return CustomerCredentialReference{}, fmt.Errorf("credential store: encrypting customer provider credential: %w", err)
	}
	existing, err := w.repository.CreateCustomer(ctx, EncryptedCustomerCredential{
		CustomerCredentialScope: scope,
		Ciphertext:              ciphertext,
		KMSKeyARN:               keyARN,
	}, actor)
	if err != nil {
		return CustomerCredentialReference{}, fmt.Errorf("credential store: creating customer provider credential: %w", err)
	}
	if existing != nil {
		if !validCustomerCredentialHandle(existing.CredentialHandle) || existing.Revision <= 0 {
			return CustomerCredentialReference{}, ErrCustomerCredentialConflict
		}
		return *existing, ErrCustomerCredentialExists
	}
	return CustomerCredentialReference{CredentialHandle: handle, Revision: 1}, nil
}

// Rotate replaces the ciphertext only when the handle, complete identity, and
// expected revision all match one active row. A retry of a successful rotation
// is therefore a conflict rather than a second rotation.
func (w *CustomerWriter) Rotate(ctx context.Context, scope CustomerCredentialScope, plaintext []byte, actor string) (CustomerCredentialReference, error) {
	if err := validateCustomerScope(scope); err != nil {
		return CustomerCredentialReference{}, invalidCustomerCredential(err)
	}
	if err := validateCustomerSecret(plaintext); err != nil {
		return CustomerCredentialReference{}, invalidCustomerCredential(err)
	}
	if err := validateActor(actor); err != nil {
		return CustomerCredentialReference{}, invalidCustomerCredential(err)
	}
	next := scope
	next.Revision++
	ciphertext, keyARN, err := w.encryptor.EncryptCustomer(ctx, next, plaintext)
	if err != nil {
		return CustomerCredentialReference{}, fmt.Errorf("credential store: encrypting rotated customer provider credential: %w", err)
	}
	changed, err := w.repository.RotateCustomer(ctx, EncryptedCustomerCredential{
		CustomerCredentialScope: next,
		Ciphertext:              ciphertext,
		KMSKeyARN:               keyARN,
	}, scope.Revision, actor)
	if err != nil {
		return CustomerCredentialReference{}, fmt.Errorf("credential store: rotating customer provider credential: %w", err)
	}
	if !changed {
		return CustomerCredentialReference{}, ErrCustomerCredentialConflict
	}
	return CustomerCredentialReference{CredentialHandle: scope.CredentialHandle, Revision: next.Revision}, nil
}

// Revoke terminally removes a handle from inference resolution. It is
// optimistic-concurrency controlled for the same replay property as rotation.
func (w *CustomerWriter) Revoke(ctx context.Context, scope CustomerCredentialScope, actor string) (CustomerCredentialReference, error) {
	if err := validateCustomerScope(scope); err != nil {
		return CustomerCredentialReference{}, invalidCustomerCredential(err)
	}
	if err := validateActor(actor); err != nil {
		return CustomerCredentialReference{}, invalidCustomerCredential(err)
	}
	changed, err := w.repository.RevokeCustomer(ctx, scope, actor)
	if err != nil {
		return CustomerCredentialReference{}, fmt.Errorf("credential store: revoking customer provider credential: %w", err)
	}
	if !changed {
		return CustomerCredentialReference{}, ErrCustomerCredentialConflict
	}
	return CustomerCredentialReference{CredentialHandle: scope.CredentialHandle, Revision: scope.Revision + 1}, nil
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
	if scope.Revision <= 0 || scope.Revision == math.MaxInt64 {
		return errors.New("credential store: customer credential revision must be positive")
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
