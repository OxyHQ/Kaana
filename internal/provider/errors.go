package provider

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// An adapter classifies its own failures. The executor believes that
// classification and refuses to invent one, because inferring a code from an
// HTTP status is exactly the guess the contract forbids: a 429 from a provider
// whose daily quota is exhausted and a 429 from a momentary burst limit are the
// same status and different answers.
//
// The two error types below are the whole vocabulary an adapter needs. Anything
// else it returns is treated as an unclassified provider error, which is
// non-retryable — a failure nobody can classify is not safe to retry.

// ErrUnsupported is returned by Translate when the provider cannot express the
// request. It is a refusal before any upstream spend, which is the only reason
// translation is a separate method.
type ErrUnsupported struct {
	// Code is the contract code the customer sees. It must be one that can
	// never succeed on a retry.
	Code contract.ErrorCode
	// Param names the request field at fault, when one field is.
	Param  string
	Detail string
}

func (e ErrUnsupported) Error() string {
	if e.Param != "" {
		return fmt.Sprintf("%s (%s): %s", e.Code, e.Param, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

// ErrUpstream is returned by Stream when the upstream provider failed.
type ErrUpstream struct {
	Code     contract.ErrorCode
	Category contract.UpstreamErrorCategory
	Detail   string
	// RetryAfterMs is honoured only when Code is retryable; the contract
	// rejects the other combination.
	RetryAfterMs int
	// Passthrough is what the upstream said, already reduced to the four fields
	// a customer can act on. Its free text is redacted when the error body is
	// built, so an upstream that echoes a credential cannot leak one here.
	Passthrough *contract.ProviderErrorPassthrough
}

func (e ErrUpstream) Error() string {
	return fmt.Sprintf("%s (%s): %s", e.Code, e.Category, e.Detail)
}

// AttributableCategory reports whether an upstream failure of this category
// says something about the DEPLOYMENT rather than about the request.
//
// It is the single decision that same-model failover and the circuit breakers
// both turn on, which is why it is one function on the error vocabulary rather
// than a rule restated in each. The categories that qualify are the ones where
// sending the identical request to another deployment could plausibly succeed.
// The rest would be refused again wherever they were sent, and counting them
// against a deployment would let one customer's malformed traffic take a
// healthy route out of rotation for everybody.
func AttributableCategory(category contract.UpstreamErrorCategory) bool {
	switch category {
	case contract.UpstreamRateLimit, contract.UpstreamQuota, contract.UpstreamTimeout,
		contract.UpstreamOverloaded, contract.UpstreamServerError, contract.UpstreamAuthentication:
		// Authentication belongs here because the credential a provider
		// refused is RELAY's, not the customer's: another deployment holds a
		// different one, and this deployment cannot serve anything until an
		// operator rotates a key.
		return true
	case contract.UpstreamInvalidReq, contract.UpstreamContentFilter, contract.UpstreamUnknown:
		return false
	}
	return false
}

// secretRedactionMarker replaces a credential found in text bound outward.
const secretRedactionMarker = "[redacted]"

// RedactSecret removes an exact secret from text before it can be shown to
// anyone.
//
// The contract's own refusal pattern is a SHAPE heuristic — `bearer <token>`,
// `authorization:`, `api_key=`, `sk-…` — and it was written against providers
// that authenticate with a bearer token. It is not enough on its own for a
// provider that authenticates with a differently named header: an upstream
// echoing `x-api-key: <value>` matches the marker and not the value, so
// redacting the marker leaves the credential in a message the contract then
// accepts, because the thing that made it look like a credential is the thing
// that was removed.
//
// An adapter is holding the exact bytes it sent, so it does not need a
// heuristic. This is the exact match, applied first, and the shape-based
// redaction still runs after it for the credentials Kaana is not holding —
// another tenant's key quoted back by a shared gateway, for instance.
//
// A short secret would eat ordinary text, and that is the right trade: text a
// customer cannot read is recoverable, and a leaked upstream credential is not.
func RedactSecret(text, secret string) string {
	if secret == "" {
		return text
	}
	return strings.ReplaceAll(text, secret, secretRedactionMarker)
}

// RetryAfterMs reads HTTP's own Retry-After header, in either of the two forms
// the standard defines.
//
// It lives here rather than in one adapter because the header is HTTP's and not
// any provider's, and because the alternative to reading it is inventing a
// number and presenting it to the customer as the provider's advice. A provider
// that sends none leaves the client to its own backoff, which is honest.
func RetryAfterMs(header http.Header) int {
	value := header.Get("Retry-After")
	if value == "" {
		return 0
	}
	if seconds, err := time.ParseDuration(value + "s"); err == nil && seconds > 0 {
		return int(seconds.Milliseconds())
	}
	if at, err := http.ParseTime(value); err == nil {
		if wait := time.Until(at); wait > 0 {
			return int(wait.Milliseconds())
		}
	}
	return 0
}

// DeploymentAttributable reports whether a failure is attributable to the
// deployment that produced it.
//
// An error an adapter did not classify is never attributable: nobody can say
// whether the upstream did the work before it failed, so a second attempt might
// pay for it twice.
func DeploymentAttributable(err error) bool {
	var upstream ErrUpstream
	if !errors.As(err, &upstream) {
		return false
	}
	return AttributableCategory(upstream.Category)
}
