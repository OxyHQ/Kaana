package credentialstore

import (
	"context"
	"errors"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrCredentialNotActive is returned when the exact (provider, keyId) has no
// active row. It never falls back to another key of the same provider.
var (
	ErrCredentialNotActive   = errors.New("credential store: exact credential is absent or disabled")
	ErrCredentialUnavailable = errors.New("credential store: exact credential is unavailable")
	ErrCredentialUseFailed   = errors.New("credential store: credential use failed")
)

type exactCredentialRepository interface {
	GetEnabled(context.Context, Scope) (EncryptedCredential, error)
}

// WithCredential decrypts exactly one active credential and lends its
// plaintext to use. The buffer is cleared on every return path; use must not
// retain it. No other row is selected or decrypted, and a missing row is an
// error rather than a choice among the provider's other keys. Errors are fixed
// sentinels: database, KMS and callback errors may contain secrets and are never
// retained in the returned error chain.
func (s *Store) WithCredential(ctx context.Context, scope Scope, use func(secret []byte) error) error {
	if use == nil {
		return ErrCredentialUnavailable
	}
	if err := validateScope(scope); err != nil {
		return ErrCredentialUnavailable
	}
	repository, ok := s.repository.(exactCredentialRepository)
	if !ok {
		return ErrCredentialUnavailable
	}
	row, err := repository.GetEnabled(ctx, scope)
	if err != nil {
		if errors.Is(err, ErrCredentialNotActive) {
			return ErrCredentialNotActive
		}
		return ErrCredentialUnavailable
	}
	if row.Scope != scope {
		return ErrCredentialUnavailable
	}
	if err := validateEncrypted(row); err != nil {
		return ErrCredentialUnavailable
	}
	plaintext, err := s.cipher.Decrypt(ctx, row.Scope, row.Ciphertext, row.KMSKeyARN)
	defer clear(plaintext)
	if err != nil {
		return ErrCredentialUnavailable
	}
	if err := provider.ValidateCustomerCredential(plaintext); err != nil {
		return ErrCredentialUnavailable
	}
	if err := use(plaintext); err != nil {
		return ErrCredentialUseFailed
	}
	return nil
}

// GetEnabled reads one active row by its exact identity. The view excludes
// disabled history, and the primary key makes a second row impossible; a
// second row is still refused rather than trusted.
func (p *Postgres) GetEnabled(ctx context.Context, scope Scope) (EncryptedCredential, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT provider_slug, key_id, encrypted_secret, kms_key_arn,
		       key_class, budget_usd::double precision, position
		FROM active_provider_credentials
		WHERE provider_slug = $1 AND key_id = $2`, string(scope.Provider), scope.KeyID)
	if err != nil {
		return EncryptedCredential{}, ErrCredentialUnavailable
	}
	defer rows.Close()
	var found []EncryptedCredential
	for rows.Next() {
		var (
			row          EncryptedCredential
			providerSlug string
			class        string
			budget       pgtype.Float8
		)
		if err := rows.Scan(&providerSlug, &row.KeyID, &row.Ciphertext, &row.KMSKeyARN, &class, &budget, &row.Position); err != nil {
			return EncryptedCredential{}, ErrCredentialUnavailable
		}
		row.Provider = contract.ProviderSlug(providerSlug)
		row.Class = provider.KeyClass(class)
		if budget.Valid {
			value := budget.Float64
			row.BudgetUSD = &value
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		return EncryptedCredential{}, ErrCredentialUnavailable
	}
	switch len(found) {
	case 0:
		return EncryptedCredential{}, ErrCredentialNotActive
	case 1:
		return found[0], nil
	default:
		return EncryptedCredential{}, ErrCredentialUnavailable
	}
}
