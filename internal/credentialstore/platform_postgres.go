package credentialstore

import (
	"context"
	"errors"
	"fmt"
)

// PutPlatformCredential invokes the schema-owned atomic mutation. The function
// binds an operation id to every selector and the write-only secret fingerprint
// and commits the credential plus audit record as one transaction.
func (p *Postgres) PutPlatformCredential(ctx context.Context, write PlatformCredentialWrite) (PlatformCredentialReceipt, error) {
	var state string
	err := p.pool.QueryRow(ctx, `SELECT outcome_state
		FROM kaana_put_platform_provider_credential($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		write.OperationID, write.Provider, write.KeyID, write.Ciphertext, write.KMSKeyARN,
		write.Class, write.Position, write.OperationActor, write.SecretFingerprint[:],
	).Scan(&state)
	if err != nil {
		return PlatformCredentialReceipt{}, fmt.Errorf("credential store: importing platform credential: %w", err)
	}
	receipt := PlatformCredentialReceipt{
		OperationID: write.OperationID, Provider: write.Provider, KeyID: write.KeyID,
		Outcome: "applied", Replayed: state == "replayed",
	}
	switch state {
	case "applied", "replayed":
		return receipt, nil
	case "conflict":
		return receipt, ErrPlatformCredentialConflict
	case "invalid":
		return PlatformCredentialReceipt{}, ErrPlatformCredentialInvalid
	default:
		return PlatformCredentialReceipt{}, errors.New("credential store: unknown platform credential mutation state")
	}
}
