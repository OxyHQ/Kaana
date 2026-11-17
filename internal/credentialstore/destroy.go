package credentialstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// DestructionProof is why a disabled key may lose its secret. There are
// exactly two, and both mean no working credential is lost:
//   - the provider itself rejected the secret as an invalid credential;
//   - the secret is byte-identical to an ENABLED key of the same provider,
//     which keeps serving with it.
//
// A secret the provider still accepts and no enabled key holds is a live
// credential; destroying the only copy Kaana has would hide it, not revoke
// it. That is refused: revoke it at the provider first.
type DestructionProof string

const (
	DestructionProofProviderRejected   DestructionProof = "provider_rejected"
	DestructionProofDuplicateOfEnabled DestructionProof = "duplicate_of_enabled"
)

// LivenessVerdict is what one authenticated, non-billable provider request
// says about a secret.
type LivenessVerdict string

const (
	// LivenessRejected: the provider refused the secret as an invalid credential,
	// by its own documented error, never inferred from a status alone.
	LivenessRejected LivenessVerdict = "rejected"
	// LivenessAccepted: the provider authenticated the secret.
	LivenessAccepted LivenessVerdict = "accepted"
	// LivenessUnknown: anything else (no reviewed probe for the provider, a
	// throttle, an outage, an IP restriction, an unrecognised answer).
	LivenessUnknown LivenessVerdict = "unknown"
)

// LivenessProbe asks a provider whether a secret is still a credential. The
// error, when any, must be a sentinel that carries no part of the secret.
type LivenessProbe interface {
	Probe(context.Context, contract.ProviderSlug, []byte) (LivenessVerdict, error)
}

var (
	// ErrCredentialStillValid: the provider accepts the secret and no enabled
	// key holds it. Revoke it upstream, then run destroy again.
	ErrCredentialStillValid = errors.New("credential store: the provider still accepts this secret and no enabled key holds it; revoke it at the provider first")
	// ErrCredentialLivenessUnproven: nothing proved the secret dead.
	ErrCredentialLivenessUnproven = errors.New("credential store: the provider did not prove this secret invalid; nothing was destroyed")
	// ErrCredentialEnabled: only a disabled key may be destroyed.
	ErrCredentialEnabled = errors.New("credential store: the key is enabled; disable it before destroying it")
	// ErrCredentialPinned: a deployment pin still names the key.
	ErrCredentialPinned = errors.New("credential store: a deployment is pinned to this key; unpin it first")
	// ErrCredentialAlreadyDestroyed: another operation destroyed the key.
	ErrCredentialAlreadyDestroyed = errors.New("credential store: the key was already destroyed by another operation")
)

// DestroyOperation names one disabled key. OperationID makes it replayable.
type DestroyOperation struct {
	OperationID string
	Scope
	Actor string
}

// DestructionReceipt is safe to print: identities and the proof only.
type DestructionReceipt struct {
	OperationID      string                `json:"operationId"`
	Provider         contract.ProviderSlug `json:"provider"`
	KeyID            string                `json:"keyId"`
	Proof            DestructionProof      `json:"proof"`
	DuplicateOfKeyID string                `json:"duplicateOfKeyId,omitempty"`
	Replayed         bool                  `json:"replayed"`
}

// DestructionDecision is what the admin task proved about the candidate.
type DestructionDecision struct {
	Proof            DestructionProof
	DuplicateOfKeyID string
}

// DestructionDecide receives the disabled candidate and every enabled key of
// its provider, under the repository's lock, and returns a proof or an error.
type DestructionDecide func(candidate EncryptedCredential, references []EncryptedCredential) (DestructionDecision, error)

type destructionRepository interface {
	DestroyCredential(context.Context, DestroyOperation, DestructionDecide) (DestructionReceipt, error)
}

// Destroy removes the ciphertext of one disabled key once it has proved that
// no working credential is lost. It first compares the secret with every
// enabled key of the provider (no network), and only for a secret no enabled
// key holds asks the provider. Plaintext exists only inside the callback and
// is cleared on every return path.
func (s *Store) Destroy(ctx context.Context, operation DestroyOperation, probe LivenessProbe) (DestructionReceipt, error) {
	if err := ValidateDestroyOperation(operation); err != nil {
		return DestructionReceipt{}, err
	}
	if probe == nil {
		return DestructionReceipt{}, errors.New("credential store: a liveness probe is required")
	}
	repository, ok := s.repository.(destructionRepository)
	if !ok {
		return DestructionReceipt{}, errors.New("credential store: repository cannot destroy credentials")
	}
	return repository.DestroyCredential(ctx, operation, func(candidate EncryptedCredential, references []EncryptedCredential) (DestructionDecision, error) {
		return s.decideDestruction(ctx, operation, candidate, references, probe)
	})
}

func (s *Store) decideDestruction(ctx context.Context, operation DestroyOperation, candidate EncryptedCredential, references []EncryptedCredential, probe LivenessProbe) (DestructionDecision, error) {
	if candidate.Scope != operation.Scope {
		return DestructionDecision{}, errors.New("credential store: destruction repository returned another candidate identity")
	}
	if len(candidate.Ciphertext) == 0 || candidate.KMSKeyARN == "" {
		return DestructionDecision{}, errors.New("credential store: destruction candidate has no ciphertext")
	}
	secret, err := s.cipher.Decrypt(ctx, candidate.Scope, candidate.Ciphertext, candidate.KMSKeyARN)
	if err != nil {
		clear(secret)
		return DestructionDecision{}, fmt.Errorf("credential store: decrypting destruction candidate: %w", err)
	}
	defer clear(secret)
	if provider.ValidateCustomerCredential(secret) != nil {
		return DestructionDecision{}, errors.New("credential store: destruction candidate decrypted to an invalid credential")
	}

	for _, reference := range references {
		if reference.Provider != operation.Provider || reference.KeyID == operation.KeyID ||
			len(reference.Ciphertext) == 0 || reference.KMSKeyARN == "" {
			return DestructionDecision{}, errors.New("credential store: destruction repository returned an invalid reference key")
		}
		equal, err := s.equalsReference(ctx, reference, secret)
		if err != nil {
			return DestructionDecision{}, err
		}
		if equal {
			return DestructionDecision{Proof: DestructionProofDuplicateOfEnabled, DuplicateOfKeyID: reference.KeyID}, nil
		}
	}

	verdict, err := probe.Probe(ctx, operation.Provider, secret)
	if err != nil {
		return DestructionDecision{}, fmt.Errorf("%w: %w", ErrCredentialLivenessUnproven, err)
	}
	switch verdict {
	case LivenessRejected:
		return DestructionDecision{Proof: DestructionProofProviderRejected}, nil
	case LivenessAccepted:
		return DestructionDecision{}, ErrCredentialStillValid
	default:
		return DestructionDecision{}, ErrCredentialLivenessUnproven
	}
}

func (s *Store) equalsReference(ctx context.Context, reference EncryptedCredential, secret []byte) (bool, error) {
	plaintext, err := s.cipher.Decrypt(ctx, reference.Scope, reference.Ciphertext, reference.KMSKeyARN)
	defer clear(plaintext)
	if err != nil {
		return false, fmt.Errorf("credential store: decrypting enabled key %q for comparison: %w", reference.KeyID, err)
	}
	if provider.ValidateCustomerCredential(plaintext) != nil {
		return false, fmt.Errorf("credential store: enabled key %q decrypted to an invalid credential", reference.KeyID)
	}
	return providerCredentialsEqual(plaintext, secret), nil
}

// ValidateDestroyOperation validates every selector before a CLI opens
// PostgreSQL or initializes KMS.
func ValidateDestroyOperation(operation DestroyOperation) error {
	if !validDestructionOperationID(operation.OperationID) {
		return errors.New("credential store: destruction operation id must be kcd_ followed by 32 lowercase hexadecimal characters")
	}
	if err := validateScope(operation.Scope); err != nil {
		return err
	}
	return validateActor(operation.Actor)
}

func validDestructionOperationID(value string) bool {
	if len(value) != 36 || value[:4] != "kcd_" {
		return false
	}
	for _, character := range value[4:] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
