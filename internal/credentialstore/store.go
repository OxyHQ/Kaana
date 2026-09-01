// Package credentialstore owns Kaana's upstream provider credentials.
//
// A provider secret has exactly one durable home: PostgreSQL. The database
// stores only KMS ciphertext, and the KMS encryption context binds that
// ciphertext to its provider and operator-facing key id. Moving a ciphertext
// to another row therefore does not move the authority it decrypts to.
package credentialstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// Scope is the authenticated identity of one provider credential.
type Scope struct {
	Provider contract.ProviderSlug
	KeyID    string
}

// EncryptedCredential is one row as stored. Secret is deliberately absent.
type EncryptedCredential struct {
	Scope
	Ciphertext []byte
	KMSKeyARN  string
	Class      provider.KeyClass
	BudgetUSD  *float64
	Position   int
}

// Metadata is the non-secret projection an operator may list.
type Metadata struct {
	Provider  contract.ProviderSlug `json:"provider"`
	KeyID     string                `json:"keyId"`
	KMSKeyARN string                `json:"kmsKeyArn"`
	Class     provider.KeyClass     `json:"class"`
	BudgetUSD *float64              `json:"budgetUsd,omitempty"`
	Position  int                   `json:"position"`
	Enabled   bool                  `json:"enabled"`
}

// Repository is the durable, ciphertext-only side of the store.
type Repository interface {
	ListEnabled(context.Context, []contract.ProviderSlug) ([]EncryptedCredential, error)
	Put(context.Context, EncryptedCredential, string) error
	Disable(context.Context, Scope, string) (bool, error)
	ListMetadata(context.Context) ([]Metadata, error)
}

// Cipher is the KMS boundary. Plaintext crosses it only in process memory.
type Cipher interface {
	Encrypt(context.Context, Scope, []byte) ([]byte, string, error)
	Decrypt(context.Context, Scope, []byte, string) ([]byte, error)
}

// Store composes PostgreSQL and KMS without letting either concern leak into
// the provider adapters.
type Store struct {
	repository Repository
	cipher     Cipher
}

// New builds a store from explicit dependencies.
func New(repository Repository, cipher Cipher) (*Store, error) {
	if repository == nil {
		return nil, errors.New("credential store: repository is required")
	}
	if cipher == nil {
		return nil, errors.New("credential store: cipher is required")
	}
	return &Store{repository: repository, cipher: cipher}, nil
}

// Load decrypts the active pools for exactly the requested providers.
//
// Every requested provider must have a pool. Starting a green task that cannot
// authenticate to one of its declared upstreams is a deployment failure, not a
// degraded steady state.
func (s *Store) Load(ctx context.Context, requested []contract.ProviderSlug) (map[contract.ProviderSlug][]provider.KeyDeclaration, []string, error) {
	providers, err := validateRequested(requested)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.repository.ListEnabled(ctx, providers)
	if err != nil {
		return nil, nil, fmt.Errorf("credential store: listing active credentials: %w", err)
	}
	sort.Slice(rows, func(left, right int) bool {
		if rows[left].Provider != rows[right].Provider {
			return rows[left].Provider < rows[right].Provider
		}
		if rows[left].Position != rows[right].Position {
			return rows[left].Position < rows[right].Position
		}
		return rows[left].KeyID < rows[right].KeyID
	})

	declarations := make(map[contract.ProviderSlug][]provider.KeyDeclaration, len(providers))
	budgets := make([]string, 0)
	seenKey := make(map[Scope]struct{}, len(rows))
	seenPosition := make(map[contract.ProviderSlug]map[int]string, len(providers))
	requestedSet := make(map[contract.ProviderSlug]struct{}, len(providers))
	for _, slug := range providers {
		requestedSet[slug] = struct{}{}
		seenPosition[slug] = make(map[int]string)
	}

	for _, row := range rows {
		if _, wanted := requestedSet[row.Provider]; !wanted {
			return nil, nil, fmt.Errorf("credential store: database returned provider %q outside the requested set", row.Provider)
		}
		if err := validateEncrypted(row); err != nil {
			return nil, nil, err
		}
		if _, duplicate := seenKey[row.Scope]; duplicate {
			return nil, nil, fmt.Errorf("credential store: provider %q returned key id %q twice", row.Provider, row.KeyID)
		}
		seenKey[row.Scope] = struct{}{}
		if other, duplicate := seenPosition[row.Provider][row.Position]; duplicate {
			return nil, nil, fmt.Errorf("credential store: provider %q assigns position %d to both %q and %q", row.Provider, row.Position, other, row.KeyID)
		}
		seenPosition[row.Provider][row.Position] = row.KeyID

		plaintext, err := s.cipher.Decrypt(ctx, row.Scope, row.Ciphertext, row.KMSKeyARN)
		if err != nil {
			return nil, nil, fmt.Errorf("credential store: decrypting provider %q key %q: %w", row.Provider, row.KeyID, err)
		}
		if len(bytes.TrimSpace(plaintext)) == 0 {
			clear(plaintext)
			return nil, nil, fmt.Errorf("credential store: provider %q key %q decrypted to an empty credential", row.Provider, row.KeyID)
		}
		secret := string(plaintext)
		clear(plaintext)
		declarations[row.Provider] = append(declarations[row.Provider], provider.KeyDeclaration{
			KeyID:  row.KeyID,
			Secret: secret,
			Class:  row.Class,
		})
		if row.BudgetUSD != nil {
			budgets = append(budgets, string(row.Provider)+"/"+row.KeyID)
		}
	}

	for _, slug := range providers {
		if len(declarations[slug]) == 0 {
			return nil, nil, fmt.Errorf("credential store: provider %q has no enabled credential", slug)
		}
	}
	sort.Strings(budgets)
	return declarations, budgets, nil
}

// Put encrypts plaintext before the repository sees it.
func (s *Store) Put(ctx context.Context, input EncryptedCredential, plaintext []byte, actor string) error {
	if len(input.Ciphertext) != 0 || input.KMSKeyARN != "" {
		return errors.New("credential store: Put accepts plaintext separately; ciphertext metadata must be empty")
	}
	if len(bytes.TrimSpace(plaintext)) == 0 {
		return errors.New("credential store: refusing an empty provider credential")
	}
	if err := validateMetadata(input.Scope, input.Class, input.BudgetUSD, input.Position); err != nil {
		return err
	}
	if err := validateActor(actor); err != nil {
		return err
	}
	ciphertext, keyARN, err := s.cipher.Encrypt(ctx, input.Scope, plaintext)
	if err != nil {
		return fmt.Errorf("credential store: encrypting provider %q key %q: %w", input.Provider, input.KeyID, err)
	}
	input.Ciphertext = ciphertext
	input.KMSKeyARN = keyARN
	if err := s.repository.Put(ctx, input, actor); err != nil {
		return fmt.Errorf("credential store: saving provider %q key %q: %w", input.Provider, input.KeyID, err)
	}
	return nil
}

// Disable takes a credential out of the active pool without deleting its
// ciphertext or its operator identity.
func (s *Store) Disable(ctx context.Context, scope Scope, actor string) (bool, error) {
	if err := validateScope(scope); err != nil {
		return false, err
	}
	if err := validateActor(actor); err != nil {
		return false, err
	}
	changed, err := s.repository.Disable(ctx, scope, actor)
	if err != nil {
		return false, fmt.Errorf("credential store: disabling provider %q key %q: %w", scope.Provider, scope.KeyID, err)
	}
	return changed, nil
}

func validateActor(actor string) error {
	if actor == "" || actor != strings.TrimSpace(actor) || len(actor) > 256 || strings.ContainsAny(actor, "\r\n") {
		return errors.New("credential store: mutation actor must be one trimmed line of at most 256 bytes")
	}
	return nil
}

// ListMetadata returns no ciphertext and no data derived from plaintext.
func (s *Store) ListMetadata(ctx context.Context) ([]Metadata, error) {
	rows, err := s.repository.ListMetadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("credential store: listing metadata: %w", err)
	}
	return rows, nil
}

func validateRequested(requested []contract.ProviderSlug) ([]contract.ProviderSlug, error) {
	if len(requested) == 0 {
		return nil, errors.New("credential store: at least one provider is required")
	}
	seen := make(map[contract.ProviderSlug]struct{}, len(requested))
	providers := make([]contract.ProviderSlug, 0, len(requested))
	for _, slug := range requested {
		if !slug.Valid() {
			return nil, fmt.Errorf("credential store: %q is not a provider slug", slug)
		}
		if _, duplicate := seen[slug]; duplicate {
			return nil, fmt.Errorf("credential store: provider %q was requested twice", slug)
		}
		seen[slug] = struct{}{}
		providers = append(providers, slug)
	}
	return providers, nil
}

func validateEncrypted(row EncryptedCredential) error {
	if err := validateMetadata(row.Scope, row.Class, row.BudgetUSD, row.Position); err != nil {
		return err
	}
	if len(row.Ciphertext) == 0 {
		return fmt.Errorf("credential store: provider %q key %q has empty ciphertext", row.Provider, row.KeyID)
	}
	if strings.TrimSpace(row.KMSKeyARN) == "" {
		return fmt.Errorf("credential store: provider %q key %q has no KMS key ARN", row.Provider, row.KeyID)
	}
	return nil
}

func validateMetadata(scope Scope, class provider.KeyClass, budget *float64, position int) error {
	if err := validateScope(scope); err != nil {
		return err
	}
	switch class {
	case provider.KeyClassUnstated, provider.KeyClassFree, provider.KeyClassPaid:
	default:
		return fmt.Errorf("credential store: provider %q key %q declares class %q; it is %q, %q, or absent", scope.Provider, scope.KeyID, class, provider.KeyClassFree, provider.KeyClassPaid)
	}
	if budget != nil && (*budget < 0 || math.IsNaN(*budget) || math.IsInf(*budget, 0)) {
		return fmt.Errorf("credential store: provider %q key %q declares a negative budget", scope.Provider, scope.KeyID)
	}
	if position <= 0 {
		return fmt.Errorf("credential store: provider %q key %q declares position %d; positions start at 1", scope.Provider, scope.KeyID, position)
	}
	return nil
}

func validateScope(scope Scope) error {
	if !scope.Provider.Valid() {
		return fmt.Errorf("credential store: %q is not a provider slug", scope.Provider)
	}
	keyID := strings.TrimSpace(scope.KeyID)
	if keyID == "" || len(keyID) > 128 || keyID != scope.KeyID {
		return fmt.Errorf("credential store: provider %q has invalid key id %q", scope.Provider, scope.KeyID)
	}
	for _, character := range keyID {
		if (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') &&
			!strings.ContainsRune("._:-", character) {
			return fmt.Errorf("credential store: provider %q has invalid key id %q", scope.Provider, scope.KeyID)
		}
	}
	first := rune(keyID[0])
	if (first < 'a' || first > 'z') &&
		(first < 'A' || first > 'Z') &&
		(first < '0' || first > '9') {
		return fmt.Errorf("credential store: provider %q has invalid key id %q", scope.Provider, scope.KeyID)
	}
	return nil
}
