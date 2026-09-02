package credentialstore

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// CreateCustomer atomically reserves an exact operation id, creates its
// ciphertext row, and records the terminal result. A repeated exact operation
// returns the original result; every other reuse or existing identity conflicts.
func (p *Postgres) CreateCustomer(ctx context.Context, operation CustomerCredentialOperation, row EncryptedCustomerCredential, actor string) (CustomerCredentialOutcome, error) {
	return p.scanCustomerOutcome(ctx, operation, `SELECT outcome_status, resolved_handle, resolved_revision, was_replayed
		FROM kaana_apply_customer_provider_credential_create($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		operation.OperationID, row.CredentialHandle, row.Provider, row.OwnerAccountID,
		row.ConnectionID, row.Environment, row.Ciphertext, row.KMSKeyARN,
		operation.SecretSHA256, actor)
}

// RotateCustomer advances one exact active generation and commits its outcome
// under the same transaction as the ciphertext and audit row.
func (p *Postgres) RotateCustomer(ctx context.Context, operation CustomerCredentialOperation, row EncryptedCustomerCredential, actor string) (CustomerCredentialOutcome, error) {
	return p.scanCustomerOutcome(ctx, operation, `SELECT outcome_status, resolved_handle, resolved_revision, was_replayed
		FROM kaana_apply_customer_provider_credential_rotate($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		operation.OperationID, row.CredentialHandle, row.Provider, row.OwnerAccountID,
		row.ConnectionID, row.Environment, operation.ExpectedRevision, row.Ciphertext,
		row.KMSKeyARN, operation.SecretSHA256, actor)
}

// RevokeCustomer terminally disables one exact active generation and records
// the outcome atomically.
func (p *Postgres) RevokeCustomer(ctx context.Context, operation CustomerCredentialOperation, actor string) (CustomerCredentialOutcome, error) {
	return p.scanCustomerOutcome(ctx, operation, `SELECT outcome_status, resolved_handle, resolved_revision, was_replayed
		FROM kaana_apply_customer_provider_credential_revoke($1, $2, $3, $4, $5, $6, $7, $8)`,
		operation.OperationID, operation.CredentialHandle, operation.Provider,
		operation.OwnerAccountID, operation.ConnectionID, operation.Environment,
		operation.ExpectedRevision, actor)
}

// GetCustomerOutcome returns a terminal result only for one exact operation
// request. A mismatched field is indistinguishable from an absent operation.
func (p *Postgres) GetCustomerOutcome(ctx context.Context, operation CustomerCredentialOperation) (CustomerCredentialOutcome, error) {
	row := p.pool.QueryRow(ctx, `SELECT outcome_status, resolved_handle, resolved_revision
		FROM kaana_get_customer_provider_credential_outcome($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		operation.OperationID, operation.Action, operation.Provider,
		operation.OwnerAccountID, operation.ConnectionID, operation.Environment,
		nullableCredentialHandle(operation), nullableExpectedRevision(operation),
		nullableSecretSHA256(operation))
	outcome, err := scanCustomerOutcome(operation, false, row.Scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return CustomerCredentialOutcome{}, ErrCustomerCredentialOutcomeUnavailable
	}
	return outcome, err
}

func (p *Postgres) scanCustomerOutcome(ctx context.Context, operation CustomerCredentialOperation, query string, arguments ...any) (CustomerCredentialOutcome, error) {
	return scanCustomerOutcome(operation, true, p.pool.QueryRow(ctx, query, arguments...).Scan)
}

func scanCustomerOutcome(operation CustomerCredentialOperation, includeReplay bool, scan func(...any) error) (CustomerCredentialOutcome, error) {
	var (
		status   CustomerCredentialOutcomeStatus
		handle   *string
		revision *int64
		replayed bool
	)
	destinations := []any{&status, &handle, &revision}
	if includeReplay {
		destinations = append(destinations, &replayed)
	}
	if err := scan(destinations...); err != nil {
		return CustomerCredentialOutcome{}, err
	}
	outcome := CustomerCredentialOutcome{Operation: operation, Status: status, Replayed: replayed}
	if handle != nil {
		outcome.CredentialHandle = *handle
	}
	if revision != nil {
		outcome.Revision = *revision
	}
	return outcome, nil
}

func nullableCredentialHandle(operation CustomerCredentialOperation) any {
	if operation.Action == CustomerCredentialActionCreate {
		return nil
	}
	return operation.CredentialHandle
}

func nullableExpectedRevision(operation CustomerCredentialOperation) any {
	if operation.Action == CustomerCredentialActionCreate {
		return nil
	}
	return operation.ExpectedRevision
}

func nullableSecretSHA256(operation CustomerCredentialOperation) any {
	if operation.Action == CustomerCredentialActionRevoke {
		return nil
	}
	return operation.SecretSHA256
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
