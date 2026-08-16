package provider

import (
	"fmt"

	"github.com/OxyHQ/Relay/internal/contract"
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
