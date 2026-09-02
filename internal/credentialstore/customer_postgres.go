package credentialstore

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// CreateCustomer inserts one new exact identity. On conflict it returns the
// already-bound opaque handle only when the full provider identity also
// matches; a provider mismatch remains an opaque conflict.
func (p *Postgres) CreateCustomer(ctx context.Context, row EncryptedCustomerCredential, actor string) (*CustomerCredentialReference, error) {
	var (
		created  bool
		handle   pgtype.Text
		revision pgtype.Int8
	)
	err := p.pool.QueryRow(ctx, `SELECT was_created, resolved_handle, resolved_revision
		FROM kaana_create_customer_provider_credential($1, $2, $3, $4, $5, $6, $7, $8)`,
		row.CredentialHandle, row.Provider, row.OwnerAccountID, row.ConnectionID,
		row.Environment, row.Ciphertext, row.KMSKeyARN, actor,
	).Scan(&created, &handle, &revision)
	if err != nil {
		return nil, err
	}
	if created {
		return nil, nil
	}
	if !handle.Valid || !revision.Valid {
		return &CustomerCredentialReference{}, nil
	}
	return &CustomerCredentialReference{CredentialHandle: handle.String, Revision: revision.Int64}, nil
}

// RotateCustomer updates only one exact active identity at the expected
// revision. The new row already carries expected+1 in its KMS context.
func (p *Postgres) RotateCustomer(ctx context.Context, row EncryptedCustomerCredential, expectedRevision int64, actor string) (bool, error) {
	var changed bool
	err := p.pool.QueryRow(ctx, `SELECT kaana_rotate_customer_provider_credential($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		row.CredentialHandle, row.Provider, row.OwnerAccountID, row.ConnectionID,
		row.Environment, expectedRevision, row.Ciphertext, row.KMSKeyARN, actor,
	).Scan(&changed)
	return changed, err
}

// RevokeCustomer terminally disables one exact active identity.
func (p *Postgres) RevokeCustomer(ctx context.Context, scope CustomerCredentialScope, actor string) (bool, error) {
	var changed bool
	err := p.pool.QueryRow(ctx, `SELECT kaana_revoke_customer_provider_credential($1, $2, $3, $4, $5, $6, $7)`,
		scope.CredentialHandle, scope.Provider, scope.OwnerAccountID, scope.ConnectionID,
		scope.Environment, scope.Revision, actor,
	).Scan(&changed)
	return changed, err
}

// GetActiveCustomer selects ciphertext through the exact SECURITY DEFINER
// function available to the inference runtime. No metadata/list resolver exists.
func (p *Postgres) GetActiveCustomer(ctx context.Context, scope CustomerCredentialScope) (EncryptedCustomerCredential, error) {
	row := EncryptedCustomerCredential{CustomerCredentialScope: scope}
	err := p.pool.QueryRow(ctx, `SELECT encrypted_secret, kms_key_arn, resolved_revision
		FROM kaana_get_active_customer_provider_credential($1, $2, $3, $4, $5, $6)`,
		scope.CredentialHandle, scope.Provider, scope.OwnerAccountID, scope.ConnectionID,
		scope.Environment, scope.Revision,
	).Scan(&row.Ciphertext, &row.KMSKeyARN, &row.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return EncryptedCustomerCredential{}, ErrCustomerCredentialUnavailable
	}
	return row, err
}
