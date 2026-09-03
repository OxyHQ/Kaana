package contract

import (
	"bytes"
	"encoding/base64"
	"errors"
	"regexp"
)

var (
	kaanaCredentialHandlePattern      = regexp.MustCompile(`^kcred_[a-z2-7]{26}$`)
	kaanaCredentialOperationIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// KaanaCredentialHandle is the opaque ciphertext generation handle Kaana
// minted. It is never a KMS, Vault, SSM or database locator.
type KaanaCredentialHandle string

// KaanaCredentialOperationID is Oxy's case-sensitive replay identity for one
// exact customer-credential mutation.
type KaanaCredentialOperationID string

// KaanaCredentialOperationAction is the mutation meaning bound to an operation
// id. An id can never move between actions.
type KaanaCredentialOperationAction string

// KaanaCredentialValidationOutcomeState is the closed lifecycle vocabulary
// for one exact-generation validation operation.
type KaanaCredentialValidationOutcomeState string

// KaanaCredentialValidationFailureCode is the closed, non-secret reason
// vocabulary for invalid or inconclusive validation outcomes.
type KaanaCredentialValidationFailureCode string

const (
	KaanaCredentialCreate KaanaCredentialOperationAction = "create"
	KaanaCredentialRotate KaanaCredentialOperationAction = "rotate"
	KaanaCredentialRevoke KaanaCredentialOperationAction = "revoke"
)

var kaanaCredentialOperationActionValues = []KaanaCredentialOperationAction{
	KaanaCredentialCreate,
	KaanaCredentialRotate,
	KaanaCredentialRevoke,
}

const (
	KaanaCredentialValidationPending      KaanaCredentialValidationOutcomeState = "pending"
	KaanaCredentialValidationValid        KaanaCredentialValidationOutcomeState = "valid"
	KaanaCredentialValidationInvalid      KaanaCredentialValidationOutcomeState = "invalid"
	KaanaCredentialValidationInconclusive KaanaCredentialValidationOutcomeState = "inconclusive"
)

var kaanaCredentialValidationOutcomeStateValues = []KaanaCredentialValidationOutcomeState{
	KaanaCredentialValidationPending,
	KaanaCredentialValidationValid,
	KaanaCredentialValidationInvalid,
	KaanaCredentialValidationInconclusive,
}

const (
	KaanaCredentialValidationUnauthorized KaanaCredentialValidationFailureCode = "unauthorized"
	KaanaCredentialValidationForbidden    KaanaCredentialValidationFailureCode = "forbidden"
	KaanaCredentialValidationNotFound     KaanaCredentialValidationFailureCode = "not_found"
	KaanaCredentialValidationRateLimited  KaanaCredentialValidationFailureCode = "rate_limited"
	KaanaCredentialValidationNetwork      KaanaCredentialValidationFailureCode = "network"
	KaanaCredentialValidationUnknown      KaanaCredentialValidationFailureCode = "unknown"
)

var kaanaCredentialValidationFailureCodeValues = []KaanaCredentialValidationFailureCode{
	KaanaCredentialValidationUnauthorized,
	KaanaCredentialValidationForbidden,
	KaanaCredentialValidationNotFound,
	KaanaCredentialValidationRateLimited,
	KaanaCredentialValidationNetwork,
	KaanaCredentialValidationUnknown,
}

// KaanaCredentialIdentity is the exact immutable Oxy identity bound into a
// customer-provider credential. Every id is opaque to Kaana.
type KaanaCredentialIdentity struct {
	Provider       ProviderSlug `json:"provider"`
	OwnerAccountID AccountID    `json:"ownerAccountId"`
	ConnectionID   string       `json:"connectionId"`
	Environment    Environment  `json:"environment"`
}

// KaanaCredentialSecret is a transient decoded provider credential. Its JSON
// representation is strict canonical base64, but the value held by Go is a
// clearable byte slice rather than an immutable string.
type KaanaCredentialSecret []byte

// UnmarshalJSON accepts exactly the contract's canonical padded-base64 wire
// representation. Semantic credential validation remains at the provider-key
// custody boundary, immediately before encryption.
func (s *KaanaCredentialSecret) UnmarshalJSON(data []byte) error {
	if len(data) < 2 || data[0] != '"' || data[len(data)-1] != '"' || len(data) > 8194 {
		return errors.New("contract: invalid customer credential encoding")
	}
	encoded := data[1 : len(data)-1]
	if len(encoded) == 0 || bytes.ContainsAny(encoded, "\\\r\n") {
		return errors.New("contract: invalid customer credential encoding")
	}
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(encoded)))
	written, err := base64.StdEncoding.Strict().Decode(decoded, encoded)
	if err != nil {
		clear(decoded)
		return errors.New("contract: invalid customer credential encoding")
	}
	if written < 1 || written > 4096 {
		clear(decoded)
		return errors.New("contract: customer credential must contain 1 to 4096 visible ASCII bytes")
	}
	for _, character := range decoded[:written] {
		if character < 0x21 || character > 0x7e {
			clear(decoded)
			return errors.New("contract: customer credential must contain 1 to 4096 visible ASCII bytes")
		}
	}
	canonical := make([]byte, base64.StdEncoding.EncodedLen(written))
	base64.StdEncoding.Encode(canonical, decoded[:written])
	if !bytes.Equal(canonical, encoded) {
		clear(canonical)
		clear(decoded)
		return errors.New("contract: invalid customer credential encoding")
	}
	clear(canonical)
	*s = decoded[:written]
	return nil
}

// Clear overwrites the transient decoded bytes after the mutation writer has
// encrypted or refused them.
func (s KaanaCredentialSecret) Clear() { clear(s) }

// KaanaCredentialCreateMutation is the signed create instruction Oxy sends to
// the credential-control task. Its wire schemaVersion remains 1.
type KaanaCredentialCreateMutation struct {
	SchemaVersion  int                            `json:"schemaVersion"`
	Action         KaanaCredentialOperationAction `json:"action"`
	OperationID    KaanaCredentialOperationID     `json:"operationId"`
	OperationActor string                         `json:"operationActor"`
	Provider       ProviderSlug                   `json:"provider"`
	OwnerAccountID AccountID                      `json:"ownerAccountId"`
	ConnectionID   string                         `json:"connectionId"`
	Environment    Environment                    `json:"environment"`
	SecretBase64   KaanaCredentialSecret          `json:"secretBase64"`
}

// KaanaCredentialRotateMutation is the signed exact-generation rotation
// instruction. ExpectedRevision is the generation being replaced.
type KaanaCredentialRotateMutation struct {
	SchemaVersion    int                            `json:"schemaVersion"`
	Action           KaanaCredentialOperationAction `json:"action"`
	OperationID      KaanaCredentialOperationID     `json:"operationId"`
	OperationActor   string                         `json:"operationActor"`
	Provider         ProviderSlug                   `json:"provider"`
	OwnerAccountID   AccountID                      `json:"ownerAccountId"`
	ConnectionID     string                         `json:"connectionId"`
	Environment      Environment                    `json:"environment"`
	CredentialHandle KaanaCredentialHandle          `json:"credentialHandle"`
	ExpectedRevision int64                          `json:"expectedRevision"`
	SecretBase64     KaanaCredentialSecret          `json:"secretBase64"`
}

// KaanaCredentialRevokeMutation is the signed exact-generation revocation
// instruction.
type KaanaCredentialRevokeMutation struct {
	SchemaVersion    int                            `json:"schemaVersion"`
	Action           KaanaCredentialOperationAction `json:"action"`
	OperationID      KaanaCredentialOperationID     `json:"operationId"`
	OperationActor   string                         `json:"operationActor"`
	Provider         ProviderSlug                   `json:"provider"`
	OwnerAccountID   AccountID                      `json:"ownerAccountId"`
	ConnectionID     string                         `json:"connectionId"`
	Environment      Environment                    `json:"environment"`
	CredentialHandle KaanaCredentialHandle          `json:"credentialHandle"`
	ExpectedRevision int64                          `json:"expectedRevision"`
}

// KaanaCredentialCreateOutcomeRequest is the metadata-only exact selector for
// a create outcome whose first response may have been lost.
type KaanaCredentialCreateOutcomeRequest struct {
	SchemaVersion  int                            `json:"schemaVersion"`
	Action         KaanaCredentialOperationAction `json:"action"`
	OperationID    KaanaCredentialOperationID     `json:"operationId"`
	Provider       ProviderSlug                   `json:"provider"`
	OwnerAccountID AccountID                      `json:"ownerAccountId"`
	ConnectionID   string                         `json:"connectionId"`
	Environment    Environment                    `json:"environment"`
}

// KaanaCredentialRotateOutcomeRequest repeats every non-secret selector of the
// rotate mutation.
type KaanaCredentialRotateOutcomeRequest struct {
	SchemaVersion    int                            `json:"schemaVersion"`
	Action           KaanaCredentialOperationAction `json:"action"`
	OperationID      KaanaCredentialOperationID     `json:"operationId"`
	Provider         ProviderSlug                   `json:"provider"`
	OwnerAccountID   AccountID                      `json:"ownerAccountId"`
	ConnectionID     string                         `json:"connectionId"`
	Environment      Environment                    `json:"environment"`
	CredentialHandle KaanaCredentialHandle          `json:"credentialHandle"`
	ExpectedRevision int64                          `json:"expectedRevision"`
}

// KaanaCredentialRevokeOutcomeRequest repeats every selector of the revoke
// mutation.
type KaanaCredentialRevokeOutcomeRequest struct {
	SchemaVersion    int                            `json:"schemaVersion"`
	Action           KaanaCredentialOperationAction `json:"action"`
	OperationID      KaanaCredentialOperationID     `json:"operationId"`
	Provider         ProviderSlug                   `json:"provider"`
	OwnerAccountID   AccountID                      `json:"ownerAccountId"`
	ConnectionID     string                         `json:"connectionId"`
	Environment      Environment                    `json:"environment"`
	CredentialHandle KaanaCredentialHandle          `json:"credentialHandle"`
	ExpectedRevision int64                          `json:"expectedRevision"`
}

// KaanaCredentialAppliedOutcome is the terminal successful answer. It never
// contains a secret, ciphertext or secret-derived value.
type KaanaCredentialAppliedOutcome struct {
	SchemaVersion    int                            `json:"schemaVersion"`
	OperationID      KaanaCredentialOperationID     `json:"operationId"`
	Action           KaanaCredentialOperationAction `json:"action"`
	Status           string                         `json:"status"`
	CredentialHandle KaanaCredentialHandle          `json:"credentialHandle"`
	Revision         int64                          `json:"revision"`
}

// KaanaCredentialConflictOutcome is deliberately reference-free.
type KaanaCredentialConflictOutcome struct {
	SchemaVersion int                            `json:"schemaVersion"`
	OperationID   KaanaCredentialOperationID     `json:"operationId"`
	Action        KaanaCredentialOperationAction `json:"action"`
	Status        string                         `json:"status"`
}

// KaanaCredentialValidationTask is a separately authenticated bootstrap probe.
// It binds one quarantined generation to one exact application and deployment;
// it is never accepted by the normal inference endpoint.
type KaanaCredentialValidationTask struct {
	SchemaVersion      int                        `json:"schemaVersion"`
	OperationID        KaanaCredentialOperationID `json:"operationId"`
	ApplicationID      ApplicationID              `json:"applicationId"`
	Provider           ProviderSlug               `json:"provider"`
	OwnerAccountID     AccountID                  `json:"ownerAccountId"`
	ConnectionID       string                     `json:"connectionId"`
	Environment        Environment                `json:"environment"`
	CredentialHandle   KaanaCredentialHandle      `json:"credentialHandle"`
	CredentialRevision int64                      `json:"credentialRevision"`
	DeploymentID       DeploymentID               `json:"deploymentId"`
}

// KaanaCredentialValidationOutcome is the closed, non-secret durable result.
// Inconclusive is not evidence against the credential and must leave it in
// quarantine.
type KaanaCredentialValidationOutcome struct {
	SchemaVersion      int                                   `json:"schemaVersion"`
	OperationID        KaanaCredentialOperationID            `json:"operationId"`
	ApplicationID      ApplicationID                         `json:"applicationId"`
	Provider           ProviderSlug                          `json:"provider"`
	OwnerAccountID     AccountID                             `json:"ownerAccountId"`
	ConnectionID       string                                `json:"connectionId"`
	Environment        Environment                           `json:"environment"`
	CredentialHandle   KaanaCredentialHandle                 `json:"credentialHandle"`
	CredentialRevision int64                                 `json:"credentialRevision"`
	DeploymentID       DeploymentID                          `json:"deploymentId"`
	State              KaanaCredentialValidationOutcomeState `json:"state"`
	FailureCode        *KaanaCredentialValidationFailureCode `json:"failureCode,omitempty"`
}
