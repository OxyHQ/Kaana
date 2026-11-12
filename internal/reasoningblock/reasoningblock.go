// Package reasoningblock seals and opens the opaque continuation token of a
// reasoning content part (contract set 8.6.0, ADR 0009 decision 2).
//
// A model that reasons before calling a tool must be given its own reasoning
// back, unmodified and in order, to continue: Anthropic requires its thinking
// blocks and their signatures, OpenRouter an ordered `reasoning_details` array.
// A provider adapter reads that payload from the upstream response, seals it
// here, and the customer returns the sealed string on the assistant message of
// the next turn. The adapter of the route that serves that turn opens it, takes
// the blocks of its own provider family and drops every other.
//
// # What sealing is and is not
//
// The token is base64url of a JSON document `{"f": <family>, "p": <payload>}`.
// It is an encoding, not a signature: a customer who edits or forges one gets
// no more than the provider's own refusal of the request, and nothing may rely
// on it for trust. The family is what lets an adapter tell its own blocks from
// another provider's after a failover or a model switch.
//
// # Customer data
//
// The payload is the model's reasoning. It is never logged, persisted or put in
// an error: a failure to open a token says that it could not be read, never what
// it held.
package reasoningblock

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// Family names the provider protocol that issued a block and can continue it.
// It is internal: no public shape carries it, and it is a protocol family, not
// a deployment, so every route speaking the protocol reads the same blocks.
type Family string

const (
	// FamilyAnthropic is the Messages protocol's thinking and redacted_thinking
	// blocks.
	FamilyAnthropic Family = "anthropic"
	// FamilyOpenRouter is OpenRouter's `reasoning_details` entries.
	FamilyOpenRouter Family = "openrouter"
)

// ErrUnreadable is returned for a token that is not one this package sealed.
var ErrUnreadable = errors.New("reasoningblock: the token is not readable")

type envelope struct {
	Family  Family          `json:"f"`
	Payload json.RawMessage `json:"p"`
}

// Seal wraps an adapter's payload in a token tagged with its family.
func Seal(family Family, payload any) (string, error) {
	if family == "" {
		return "", fmt.Errorf("reasoningblock: a block needs a family")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("reasoningblock: encoding a payload: %w", err)
	}
	document, err := json.Marshal(envelope{Family: family, Payload: encoded})
	if err != nil {
		return "", fmt.Errorf("reasoningblock: encoding a token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(document), nil
}

// Open reads a token, returning its family and raw payload. The error never
// carries the token.
func Open(token string) (Family, json.RawMessage, error) {
	document, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", nil, ErrUnreadable
	}
	var sealed envelope
	if err := json.Unmarshal(document, &sealed); err != nil || sealed.Family == "" || len(sealed.Payload) == 0 {
		return "", nil, ErrUnreadable
	}
	return sealed.Family, sealed.Payload, nil
}

// Payloads returns the payloads of the family's own blocks among the tokens,
// in their original order. A token of another family, or one that cannot be
// read, is dropped: the continuation is lost, the conversation is not.
func Payloads(family Family, tokens []string) []json.RawMessage {
	var payloads []json.RawMessage
	for _, token := range tokens {
		opened, payload, err := Open(token)
		if err != nil || opened != family {
			continue
		}
		payloads = append(payloads, payload)
	}
	return payloads
}
