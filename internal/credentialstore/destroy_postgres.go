package credentialstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const (
	destructionStateAlreadyDestroyed = "already_destroyed"
	destructionStateEnabled          = "enabled"
	destructionStatePinned           = "pinned"
	destructionRoleCandidate         = "candidate"
	destructionRoleReference         = "reference"
)

// DestroyCredential runs prepare, the in-memory decision and complete in one
// transaction. Prepare takes the provider's admin advisory lock and a SHARE
// ROW EXCLUSIVE table lock, so no put, disable or rekey can change the
// candidate or its references between the comparison and the write.
func (p *Postgres) DestroyCredential(ctx context.Context, operation DestroyOperation, decide DestructionDecide) (DestructionReceipt, error) {
	if decide == nil {
		return DestructionReceipt{}, errors.New("credential store: a destruction decision is required")
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return DestructionReceipt{}, fmt.Errorf("credential store: beginning provider credential destruction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	rows, err := tx.Query(ctx, `SELECT outcome_state, stored_proof, stored_duplicate_of_key_id,
			row_role, row_key_id, row_encrypted_secret, row_kms_key_arn
		FROM kaana_prepare_provider_credential_destruction($1, $2, $3, $4)`,
		operation.OperationID, operation.Provider, operation.KeyID, operation.Actor)
	if err != nil {
		return DestructionReceipt{}, fmt.Errorf("credential store: preparing provider credential destruction: %w", err)
	}
	var (
		state       string
		storedProof *string
		storedDup   *string
		candidate   *EncryptedCredential
		references  []EncryptedCredential
	)
	defer func() {
		if candidate != nil {
			clear(candidate.Ciphertext)
		}
		for _, reference := range references {
			clear(reference.Ciphertext)
		}
	}()
	for rows.Next() {
		var (
			rowState   string
			role       *string
			keyID      *string
			ciphertext []byte
			kmsKeyARN  *string
		)
		if err := rows.Scan(&rowState, &storedProof, &storedDup, &role, &keyID, &ciphertext, &kmsKeyARN); err != nil {
			rows.Close()
			return DestructionReceipt{}, fmt.Errorf("credential store: reading provider credential destruction preparation: %w", err)
		}
		if state != "" && rowState != state {
			rows.Close()
			return DestructionReceipt{}, errors.New("credential store: inconsistent provider credential destruction preparation")
		}
		state = rowState
		if state != credentialAdminStateExecute {
			continue
		}
		if role == nil || keyID == nil || kmsKeyARN == nil || len(ciphertext) == 0 {
			rows.Close()
			return DestructionReceipt{}, errors.New("credential store: incomplete provider credential destruction preparation")
		}
		row := EncryptedCredential{
			Scope:      Scope{Provider: operation.Provider, KeyID: *keyID},
			Ciphertext: ciphertext,
			KMSKeyARN:  *kmsKeyARN,
		}
		switch *role {
		case destructionRoleCandidate:
			if candidate != nil {
				rows.Close()
				return DestructionReceipt{}, errors.New("credential store: provider credential destruction prepared two candidates")
			}
			candidate = &row
		case destructionRoleReference:
			references = append(references, row)
		default:
			rows.Close()
			return DestructionReceipt{}, errors.New("credential store: unknown provider credential destruction row")
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return DestructionReceipt{}, fmt.Errorf("credential store: preparing provider credential destruction: %w", err)
	}

	if state == credentialAdminStateReplayed {
		receipt, err := destructionReceipt(operation, storedProof, storedDup, true)
		if err != nil {
			return DestructionReceipt{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return DestructionReceipt{}, fmt.Errorf("credential store: committing provider credential destruction replay: %w", err)
		}
		return receipt, nil
	}
	if err := destructionStateError(state); err != nil {
		return DestructionReceipt{}, err
	}
	if state != credentialAdminStateExecute || candidate == nil {
		return DestructionReceipt{}, errors.New("credential store: incomplete provider credential destruction preparation")
	}

	decision, err := decide(*candidate, references)
	if err != nil {
		return DestructionReceipt{}, err
	}
	if err := validDestructionDecision(decision, operation, references); err != nil {
		return DestructionReceipt{}, err
	}
	var duplicateOf *string
	if decision.DuplicateOfKeyID != "" {
		duplicateOf = &decision.DuplicateOfKeyID
	}

	storedProof, storedDup = nil, nil
	if err := tx.QueryRow(ctx, `SELECT outcome_state, stored_proof, stored_duplicate_of_key_id
		FROM kaana_complete_provider_credential_destruction($1, $2, $3, $4, $5, $6)`,
		operation.OperationID, operation.Provider, operation.KeyID, operation.Actor,
		string(decision.Proof), duplicateOf,
	).Scan(&state, &storedProof, &storedDup); err != nil {
		return DestructionReceipt{}, fmt.Errorf("credential store: completing provider credential destruction: %w", err)
	}
	if state != credentialAdminStateApplied {
		if err := destructionStateError(state); err != nil {
			return DestructionReceipt{}, err
		}
		return DestructionReceipt{}, fmt.Errorf("credential store: unexpected provider credential destruction state %q", state)
	}
	receipt, err := destructionReceipt(operation, storedProof, storedDup, false)
	if err != nil {
		return DestructionReceipt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DestructionReceipt{}, fmt.Errorf("credential store: committing provider credential destruction: %w", err)
	}
	return receipt, nil
}

func validDestructionDecision(decision DestructionDecision, operation DestroyOperation, references []EncryptedCredential) error {
	switch decision.Proof {
	case DestructionProofProviderRejected:
		if decision.DuplicateOfKeyID == "" {
			return nil
		}
	case DestructionProofDuplicateOfEnabled:
		for _, reference := range references {
			if reference.KeyID == decision.DuplicateOfKeyID && reference.KeyID != operation.KeyID {
				return nil
			}
		}
	}
	return errors.New("credential store: invalid provider credential destruction decision")
}

func destructionStateError(state string) error {
	switch state {
	case destructionStateAlreadyDestroyed:
		return ErrCredentialAlreadyDestroyed
	case destructionStateEnabled:
		return ErrCredentialEnabled
	case destructionStatePinned:
		return ErrCredentialPinned
	default:
		return credentialAdminStateError(state)
	}
}

func destructionReceipt(operation DestroyOperation, proof, duplicateOf *string, replayed bool) (DestructionReceipt, error) {
	if proof == nil {
		return DestructionReceipt{}, errors.New("credential store: provider credential destruction proof is absent")
	}
	receipt := DestructionReceipt{
		OperationID: operation.OperationID,
		Provider:    operation.Provider,
		KeyID:       operation.KeyID,
		Proof:       DestructionProof(*proof),
		Replayed:    replayed,
	}
	if duplicateOf != nil {
		receipt.DuplicateOfKeyID = *duplicateOf
	}
	switch {
	case receipt.Proof == DestructionProofProviderRejected && receipt.DuplicateOfKeyID == "":
	case receipt.Proof == DestructionProofDuplicateOfEnabled && receipt.DuplicateOfKeyID != "":
	default:
		return DestructionReceipt{}, errors.New("credential store: invalid provider credential destruction proof")
	}
	return receipt, nil
}
