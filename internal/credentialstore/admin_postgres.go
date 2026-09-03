package credentialstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/jackc/pgx/v5"
)

const (
	credentialAdminStateExecute                 = "execute"
	credentialAdminStateApplied                 = "applied"
	credentialAdminStateReplayed                = "replayed"
	credentialAdminStateConflict                = "conflict"
	credentialAdminStateSourceUnavailable       = "source_unavailable"
	credentialAdminStateDestinationExists       = "destination_exists"
	credentialAdminStatePrerequisiteUnavailable = "prerequisite_unavailable"
)

// RekeyID holds one database transaction and its advisory/table locks across
// the KMS transform. The old ciphertext row is disabled and the new-context
// ciphertext row, audit records, and durable receipt either commit together or
// all roll back.
func (p *Postgres) RekeyID(ctx context.Context, operation CredentialIDOperation, transform CredentialRekeyTransform) (CredentialAdminReceipt, error) {
	if transform == nil {
		return CredentialAdminReceipt{}, errors.New("credential store: rekey transform is required")
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return CredentialAdminReceipt{}, fmt.Errorf("credential store: beginning provider credential rekey: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var (
		state      string
		outcome    *string
		ciphertext []byte
		kmsKeyARN  *string
		keyClass   *string
		budgetUSD  *float64
		position   *int
	)
	err = tx.QueryRow(ctx, `SELECT outcome_state, stored_outcome,
			source_encrypted_secret, source_kms_key_arn, source_key_class,
			source_budget_usd, source_position
		FROM kaana_prepare_provider_credential_rekey($1, $2, $3, $4, $5, $6, $7)`,
		operation.OperationID, operation.Provider, operation.SourceKeyID,
		operation.DestinationKeyID, operation.Actor,
		nullableCredentialAdminString(operation.PrerequisiteOperationID),
		nullableCredentialAdminOutcome(operation.PrerequisiteOutcome),
	).Scan(&state, &outcome, &ciphertext, &kmsKeyARN, &keyClass, &budgetUSD, &position)
	if err != nil {
		return CredentialAdminReceipt{}, fmt.Errorf("credential store: preparing provider credential rekey: %w", err)
	}
	defer clear(ciphertext)

	if state == credentialAdminStateReplayed {
		receipt, err := credentialAdminReceipt(operation, CredentialAdminActionRekeyID, outcome, true)
		if err != nil {
			return CredentialAdminReceipt{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return CredentialAdminReceipt{}, fmt.Errorf("credential store: committing provider credential rekey replay: %w", err)
		}
		return receipt, nil
	}
	if err := credentialAdminStateError(state); err != nil {
		return CredentialAdminReceipt{}, err
	}
	if state != credentialAdminStateExecute || kmsKeyARN == nil || keyClass == nil || position == nil {
		return CredentialAdminReceipt{}, errors.New("credential store: incomplete provider credential rekey preparation")
	}
	source := EncryptedCredential{
		Scope:      Scope{Provider: operation.Provider, KeyID: operation.SourceKeyID},
		Ciphertext: ciphertext,
		KMSKeyARN:  *kmsKeyARN,
		Class:      provider.KeyClass(*keyClass),
		BudgetUSD:  budgetUSD,
		Position:   *position,
	}
	destination, err := transform(source)
	if err != nil {
		return CredentialAdminReceipt{}, err
	}
	defer clear(destination.Ciphertext)

	outcome = nil
	err = tx.QueryRow(ctx, `SELECT outcome_state, stored_outcome
		FROM kaana_complete_provider_credential_rekey($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		operation.OperationID, operation.Provider, operation.SourceKeyID,
		operation.DestinationKeyID, operation.Actor, destination.Ciphertext,
		destination.KMSKeyARN, nullableCredentialAdminString(operation.PrerequisiteOperationID),
		nullableCredentialAdminOutcome(operation.PrerequisiteOutcome),
	).Scan(&state, &outcome)
	if err != nil {
		return CredentialAdminReceipt{}, fmt.Errorf("credential store: completing provider credential rekey: %w", err)
	}
	if state != credentialAdminStateApplied {
		if err := credentialAdminStateError(state); err != nil {
			return CredentialAdminReceipt{}, err
		}
		return CredentialAdminReceipt{}, fmt.Errorf("credential store: unexpected provider credential rekey state %q", state)
	}
	receipt, err := credentialAdminReceipt(operation, CredentialAdminActionRekeyID, outcome, false)
	if err != nil {
		return CredentialAdminReceipt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CredentialAdminReceipt{}, fmt.Errorf("credential store: committing provider credential rekey: %w", err)
	}
	return receipt, nil
}

// Deduplicate holds the same exact transaction boundary while the two
// ciphertexts are decrypted and compared. PostgreSQL sees only equality; a
// different outcome is durable but mutates neither credential.
func (p *Postgres) Deduplicate(ctx context.Context, operation CredentialIDOperation, compare CredentialCompareTransform) (CredentialAdminReceipt, error) {
	if compare == nil {
		return CredentialAdminReceipt{}, errors.New("credential store: credential comparison is required")
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return CredentialAdminReceipt{}, fmt.Errorf("credential store: beginning provider credential deduplication: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var (
		state               string
		outcome             *string
		keepCiphertext      []byte
		keepKMSKeyARN       *string
		keepKeyClass        *string
		keepBudgetUSD       *float64
		keepPosition        *int
		duplicateCiphertext []byte
		duplicateKMSKeyARN  *string
		duplicateKeyClass   *string
		duplicateBudgetUSD  *float64
		duplicatePosition   *int
	)
	err = tx.QueryRow(ctx, `SELECT outcome_state, stored_outcome,
			keep_encrypted_secret, keep_kms_key_arn, keep_key_class,
			keep_budget_usd, keep_position, duplicate_encrypted_secret,
			duplicate_kms_key_arn, duplicate_key_class,
			duplicate_budget_usd, duplicate_position
		FROM kaana_prepare_provider_credential_deduplication($1, $2, $3, $4, $5)`,
		operation.OperationID, operation.Provider, operation.SourceKeyID,
		operation.DestinationKeyID, operation.Actor,
	).Scan(
		&state, &outcome, &keepCiphertext, &keepKMSKeyARN, &keepKeyClass,
		&keepBudgetUSD, &keepPosition, &duplicateCiphertext,
		&duplicateKMSKeyARN, &duplicateKeyClass, &duplicateBudgetUSD,
		&duplicatePosition,
	)
	if err != nil {
		return CredentialAdminReceipt{}, fmt.Errorf("credential store: preparing provider credential deduplication: %w", err)
	}
	defer clear(keepCiphertext)
	defer clear(duplicateCiphertext)

	if state == credentialAdminStateReplayed {
		receipt, err := credentialAdminReceipt(operation, CredentialAdminActionDeduplicate, outcome, true)
		if err != nil {
			return CredentialAdminReceipt{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return CredentialAdminReceipt{}, fmt.Errorf("credential store: committing provider credential deduplication replay: %w", err)
		}
		return receipt, nil
	}
	if err := credentialAdminStateError(state); err != nil {
		return CredentialAdminReceipt{}, err
	}
	if state != credentialAdminStateExecute || keepKMSKeyARN == nil || keepKeyClass == nil || keepPosition == nil ||
		duplicateKMSKeyARN == nil || duplicateKeyClass == nil || duplicatePosition == nil {
		return CredentialAdminReceipt{}, errors.New("credential store: incomplete provider credential deduplication preparation")
	}
	keep := EncryptedCredential{
		Scope:      Scope{Provider: operation.Provider, KeyID: operation.DestinationKeyID},
		Ciphertext: keepCiphertext,
		KMSKeyARN:  *keepKMSKeyARN,
		Class:      provider.KeyClass(*keepKeyClass),
		BudgetUSD:  keepBudgetUSD,
		Position:   *keepPosition,
	}
	duplicate := EncryptedCredential{
		Scope:      Scope{Provider: operation.Provider, KeyID: operation.SourceKeyID},
		Ciphertext: duplicateCiphertext,
		KMSKeyARN:  *duplicateKMSKeyARN,
		Class:      provider.KeyClass(*duplicateKeyClass),
		BudgetUSD:  duplicateBudgetUSD,
		Position:   *duplicatePosition,
	}
	equal, err := compare(keep, duplicate)
	if err != nil {
		return CredentialAdminReceipt{}, err
	}

	outcome = nil
	err = tx.QueryRow(ctx, `SELECT outcome_state, stored_outcome
		FROM kaana_complete_provider_credential_deduplication($1, $2, $3, $4, $5, $6)`,
		operation.OperationID, operation.Provider, operation.SourceKeyID,
		operation.DestinationKeyID, operation.Actor, equal,
	).Scan(&state, &outcome)
	if err != nil {
		return CredentialAdminReceipt{}, fmt.Errorf("credential store: completing provider credential deduplication: %w", err)
	}
	if state != credentialAdminStateApplied {
		if err := credentialAdminStateError(state); err != nil {
			return CredentialAdminReceipt{}, err
		}
		return CredentialAdminReceipt{}, fmt.Errorf("credential store: unexpected provider credential deduplication state %q", state)
	}
	receipt, err := credentialAdminReceipt(operation, CredentialAdminActionDeduplicate, outcome, false)
	if err != nil {
		return CredentialAdminReceipt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CredentialAdminReceipt{}, fmt.Errorf("credential store: committing provider credential deduplication: %w", err)
	}
	return receipt, nil
}

func credentialAdminStateError(state string) error {
	switch state {
	case credentialAdminStateExecute, credentialAdminStateApplied, credentialAdminStateReplayed:
		return nil
	case credentialAdminStateConflict:
		return ErrProviderCredentialAdminConflict
	case credentialAdminStateSourceUnavailable:
		return ErrProviderCredentialSourceUnavailable
	case credentialAdminStateDestinationExists:
		return ErrProviderCredentialDestinationExists
	case credentialAdminStatePrerequisiteUnavailable:
		return ErrProviderCredentialPrerequisiteUnavailable
	default:
		return fmt.Errorf("credential store: unknown provider credential admin state %q", state)
	}
}

func credentialAdminReceipt(operation CredentialIDOperation, action CredentialAdminAction, outcome *string, replayed bool) (CredentialAdminReceipt, error) {
	if outcome == nil {
		return CredentialAdminReceipt{}, errors.New("credential store: provider credential admin outcome is absent")
	}
	parsed := CredentialAdminOutcome(*outcome)
	switch action {
	case CredentialAdminActionRekeyID:
		if parsed != CredentialAdminOutcomeApplied {
			return CredentialAdminReceipt{}, errors.New("credential store: invalid provider credential rekey outcome")
		}
	case CredentialAdminActionDeduplicate:
		if parsed != CredentialAdminOutcomeDeduplicated && parsed != CredentialAdminOutcomeDifferent {
			return CredentialAdminReceipt{}, errors.New("credential store: invalid provider credential deduplication outcome")
		}
	default:
		return CredentialAdminReceipt{}, errors.New("credential store: invalid provider credential admin action")
	}
	return CredentialAdminReceipt{
		OperationID:             operation.OperationID,
		Action:                  action,
		Provider:                operation.Provider,
		SourceKeyID:             operation.SourceKeyID,
		DestinationKeyID:        operation.DestinationKeyID,
		Outcome:                 parsed,
		PrerequisiteOperationID: operation.PrerequisiteOperationID,
		PrerequisiteOutcome:     operation.PrerequisiteOutcome,
		Replayed:                replayed,
	}, nil
}

func nullableCredentialAdminString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableCredentialAdminOutcome(value CredentialAdminOutcome) any {
	if value == "" {
		return nil
	}
	return value
}
