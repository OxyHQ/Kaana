package credentialstore

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

const providerCredentialComparisonBytes = 4096

var (
	// ErrProviderCredentialAdminConflict means an operation id was already
	// bound to a different exact action, actor, provider, source, or target.
	ErrProviderCredentialAdminConflict = errors.New("provider credential admin operation conflicts")
	// ErrProviderCredentialSourceUnavailable deliberately covers an absent or
	// disabled exact source id. No name, pool position, or ordering fallback is
	// attempted.
	ErrProviderCredentialSourceUnavailable = errors.New("provider credential source is unavailable")
	// ErrProviderCredentialDestinationExists refuses an unrecorded rekey onto
	// any existing row, including a disabled one. Only the durable operation
	// receipt makes a destination an idempotent replay.
	ErrProviderCredentialDestinationExists = errors.New("provider credential destination already exists")
	// ErrProviderCredentialPrerequisiteUnavailable means a conditional rekey's
	// exact prerequisite receipt is absent or has another outcome.
	ErrProviderCredentialPrerequisiteUnavailable = errors.New("provider credential prerequisite operation is unavailable")
)

// CredentialIDOperation binds a one-shot admin operation to exact, non-secret
// identities. For rekey, SourceKeyID is the old id and DestinationKeyID the new
// opaque id. For deduplication, SourceKeyID is the candidate duplicate to
// disable and DestinationKeyID is the exact id to preserve. An optional exact
// prerequisite receipt makes multi-step cutovers fail closed and is itself part
// of the durable replay identity.
type CredentialIDOperation struct {
	OperationID             string
	Provider                contract.ProviderSlug
	SourceKeyID             string
	DestinationKeyID        string
	PrerequisiteOperationID string
	PrerequisiteOutcome     CredentialAdminOutcome
	Actor                   string
}

// CredentialAdminAction is the immutable meaning assigned to an operation id.
type CredentialAdminAction string

const (
	CredentialAdminActionRekeyID     CredentialAdminAction = "rekey_id"
	CredentialAdminActionDeduplicate CredentialAdminAction = "deduplicate"
)

// CredentialAdminOutcome is a terminal, non-secret operation result.
type CredentialAdminOutcome string

const (
	CredentialAdminOutcomeApplied      CredentialAdminOutcome = "applied"
	CredentialAdminOutcomeDeduplicated CredentialAdminOutcome = "deduplicated"
	CredentialAdminOutcomeDifferent    CredentialAdminOutcome = "different"
)

// CredentialAdminReceipt is safe to print. It contains neither ciphertext nor
// plaintext, and no value derived from plaintext beyond the intentional
// equality outcome of an exact deduplication request.
type CredentialAdminReceipt struct {
	OperationID             string                 `json:"operationId"`
	Action                  CredentialAdminAction  `json:"action"`
	Provider                contract.ProviderSlug  `json:"provider"`
	SourceKeyID             string                 `json:"sourceKeyId"`
	DestinationKeyID        string                 `json:"destinationKeyId"`
	Outcome                 CredentialAdminOutcome `json:"outcome"`
	PrerequisiteOperationID string                 `json:"prerequisiteOperationId,omitempty"`
	PrerequisiteOutcome     CredentialAdminOutcome `json:"prerequisiteOutcome,omitempty"`
	Replayed                bool                   `json:"replayed"`
}

// CredentialRekeyTransform re-encrypts one exact ciphertext row under another
// exact KMS context while the repository transaction remains open.
type CredentialRekeyTransform func(EncryptedCredential) (EncryptedCredential, error)

// CredentialCompareTransform compares two exact ciphertext rows without
// returning plaintext or a correlatable digest to the repository.
type CredentialCompareTransform func(EncryptedCredential, EncryptedCredential) (bool, error)

// RekeyID decrypts under the source context and encrypts the same bytes under
// the exact destination context while the repository holds its transaction
// and advisory lock. Plaintext exists only inside the callback and is cleared
// on every return path.
func (s *Store) RekeyID(ctx context.Context, operation CredentialIDOperation) (CredentialAdminReceipt, error) {
	if err := ValidateRekeyIDOperation(operation); err != nil {
		return CredentialAdminReceipt{}, err
	}
	return s.repository.RekeyID(ctx, operation, func(source EncryptedCredential) (EncryptedCredential, error) {
		if err := validateEncrypted(source); err != nil {
			return EncryptedCredential{}, err
		}
		if source.Provider != operation.Provider || source.KeyID != operation.SourceKeyID {
			return EncryptedCredential{}, errors.New("credential store: rekey repository returned another source identity")
		}
		plaintext, err := s.cipher.Decrypt(ctx, source.Scope, source.Ciphertext, source.KMSKeyARN)
		if err != nil {
			clear(plaintext)
			return EncryptedCredential{}, fmt.Errorf("credential store: decrypting exact rekey source: %w", err)
		}
		defer clear(plaintext)
		if err := provider.ValidateCustomerCredential(plaintext); err != nil {
			return EncryptedCredential{}, errors.New("credential store: exact rekey source decrypted to an invalid credential")
		}
		destination := source
		destination.Scope = Scope{Provider: operation.Provider, KeyID: operation.DestinationKeyID}
		destination.Ciphertext = nil
		destination.KMSKeyARN = ""
		destination.Ciphertext, destination.KMSKeyARN, err = s.cipher.Encrypt(ctx, destination.Scope, plaintext)
		clear(plaintext)
		if err != nil {
			clear(destination.Ciphertext)
			return EncryptedCredential{}, fmt.Errorf("credential store: encrypting exact rekey destination: %w", err)
		}
		if err := validateEncrypted(destination); err != nil {
			clear(destination.Ciphertext)
			return EncryptedCredential{}, err
		}
		return destination, nil
	})
}

// Deduplicate compares two exact active credentials in memory. PostgreSQL
// receives only the equality bit. Equal disables SourceKeyID; different leaves
// both rows untouched so an exact secondary rekey can follow.
func (s *Store) Deduplicate(ctx context.Context, operation CredentialIDOperation) (CredentialAdminReceipt, error) {
	if err := ValidateDeduplicationOperation(operation); err != nil {
		return CredentialAdminReceipt{}, err
	}
	return s.repository.Deduplicate(ctx, operation, func(keep, duplicate EncryptedCredential) (bool, error) {
		if err := validateEncrypted(keep); err != nil {
			return false, err
		}
		if err := validateEncrypted(duplicate); err != nil {
			return false, err
		}
		if keep.Provider != operation.Provider || keep.KeyID != operation.DestinationKeyID ||
			duplicate.Provider != operation.Provider || duplicate.KeyID != operation.SourceKeyID {
			return false, errors.New("credential store: deduplication repository returned another source identity")
		}

		keepPlaintext, err := s.cipher.Decrypt(ctx, keep.Scope, keep.Ciphertext, keep.KMSKeyARN)
		if err != nil {
			clear(keepPlaintext)
			return false, fmt.Errorf("credential store: decrypting exact deduplication keep id: %w", err)
		}
		defer clear(keepPlaintext)
		duplicatePlaintext, err := s.cipher.Decrypt(ctx, duplicate.Scope, duplicate.Ciphertext, duplicate.KMSKeyARN)
		if err != nil {
			clear(duplicatePlaintext)
			return false, fmt.Errorf("credential store: decrypting exact deduplication candidate: %w", err)
		}
		defer clear(duplicatePlaintext)
		if provider.ValidateCustomerCredential(keepPlaintext) != nil || provider.ValidateCustomerCredential(duplicatePlaintext) != nil {
			return false, errors.New("credential store: exact deduplication source decrypted to an invalid credential")
		}
		return providerCredentialsEqual(keepPlaintext, duplicatePlaintext), nil
	})
}

// ValidateRekeyIDOperation validates every non-secret selector before a CLI
// opens PostgreSQL or initializes KMS.
func ValidateRekeyIDOperation(operation CredentialIDOperation) error {
	return validateCredentialIDOperation(operation, true, true)
}

// ValidateDeduplicationOperation validates every non-secret selector before a
// CLI opens PostgreSQL or initializes KMS.
func ValidateDeduplicationOperation(operation CredentialIDOperation) error {
	return validateCredentialIDOperation(operation, false, false)
}

func validateCredentialIDOperation(operation CredentialIDOperation, requireOpaqueDestination, allowPrerequisite bool) error {
	if !validCredentialOperationID(operation.OperationID) {
		return errors.New("credential store: operation id must be kop_ followed by 32 lowercase hexadecimal characters")
	}
	if err := validateScope(Scope{Provider: operation.Provider, KeyID: operation.SourceKeyID}); err != nil {
		return err
	}
	if err := validateScope(Scope{Provider: operation.Provider, KeyID: operation.DestinationKeyID}); err != nil {
		return err
	}
	if operation.SourceKeyID == operation.DestinationKeyID {
		return errors.New("credential store: source and destination key ids must differ")
	}
	if requireOpaqueDestination && !validOpaqueCredentialID(operation.DestinationKeyID) {
		return errors.New("credential store: rekey destination must be an exact lowercase UUIDv4")
	}
	if !allowPrerequisite && (operation.PrerequisiteOperationID != "" || operation.PrerequisiteOutcome != "") {
		return errors.New("credential store: deduplication does not accept a prerequisite")
	}
	if operation.PrerequisiteOperationID == "" {
		if operation.PrerequisiteOutcome != "" {
			return errors.New("credential store: prerequisite outcome requires an exact operation id")
		}
	} else {
		if !validCredentialOperationID(operation.PrerequisiteOperationID) || operation.PrerequisiteOperationID == operation.OperationID {
			return errors.New("credential store: prerequisite operation id is invalid")
		}
		switch operation.PrerequisiteOutcome {
		case "", CredentialAdminOutcomeApplied, CredentialAdminOutcomeDeduplicated, CredentialAdminOutcomeDifferent:
		default:
			return errors.New("credential store: prerequisite outcome is invalid")
		}
	}
	return validateActor(operation.Actor)
}

func validCredentialOperationID(value string) bool {
	if len(value) != 36 || value[:4] != "kop_" {
		return false
	}
	for _, character := range value[4:] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validOpaqueCredentialID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' ||
		value[14] != '4' || (value[19] != '8' && value[19] != '9' && value[19] != 'a' && value[19] != 'b') {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func providerCredentialsEqual(left, right []byte) bool {
	var leftPadded, rightPadded [providerCredentialComparisonBytes]byte
	copy(leftPadded[:], left)
	copy(rightPadded[:], right)
	equalLength := subtle.ConstantTimeEq(int32(len(left)), int32(len(right)))
	equalBytes := subtle.ConstantTimeCompare(leftPadded[:], rightPadded[:])
	clear(leftPadded[:])
	clear(rightPadded[:])
	return equalLength&equalBytes == 1
}
