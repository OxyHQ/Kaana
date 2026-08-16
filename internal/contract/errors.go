package contract

import (
	"fmt"
	"regexp"
	"strings"
)

// ErrorCode is the closed set of inference error codes, shared by the Oxy edge,
// the data plane and every SDK.
type ErrorCode string

const (
	CodeInvalidRequest             ErrorCode = "invalid_request"
	CodeAuthenticationFailed       ErrorCode = "authentication_failed"
	CodePermissionDenied           ErrorCode = "permission_denied"
	CodeInsufficientScope          ErrorCode = "insufficient_scope"
	CodeModelNotFound              ErrorCode = "model_not_found"
	CodeUnsupportedModality        ErrorCode = "unsupported_modality"
	CodeContextLengthExceeded      ErrorCode = "context_length_exceeded"
	CodeRequestTooLarge            ErrorCode = "request_too_large"
	CodeOutputLimitExceeded        ErrorCode = "output_limit_exceeded"
	CodeIdempotencyConflict        ErrorCode = "idempotency_conflict"
	CodeInsufficientBalance        ErrorCode = "insufficient_balance"
	CodeSpendingLimitExceeded      ErrorCode = "spending_limit_exceeded"
	CodeQuotaExceeded              ErrorCode = "quota_exceeded"
	CodeBYOKCredentialInvalid      ErrorCode = "byok_credential_invalid"
	CodePolicyViolation            ErrorCode = "policy_violation"
	CodeCommercialPermissionDenied ErrorCode = "commercial_permission_denied"
	CodeNoRouteAvailable           ErrorCode = "no_route_available"
	CodeUpstreamContentFiltered    ErrorCode = "upstream_content_filtered"
	CodeCancelled                  ErrorCode = "cancelled"
	CodeRateLimited                ErrorCode = "rate_limited"
	CodeDeploymentUnavailable      ErrorCode = "deployment_unavailable"
	CodeProviderError              ErrorCode = "provider_error"
	CodeProviderTimeout            ErrorCode = "provider_timeout"
	CodeProviderOverloaded         ErrorCode = "provider_overloaded"
	CodeServiceUnavailable         ErrorCode = "service_unavailable"
	CodeInternalError              ErrorCode = "internal_error"
)

var errorCodeValues = []ErrorCode{
	CodeInvalidRequest, CodeAuthenticationFailed, CodePermissionDenied, CodeInsufficientScope,
	CodeModelNotFound, CodeUnsupportedModality, CodeContextLengthExceeded, CodeRequestTooLarge,
	CodeOutputLimitExceeded, CodeIdempotencyConflict, CodeInsufficientBalance,
	CodeSpendingLimitExceeded, CodeQuotaExceeded, CodeBYOKCredentialInvalid, CodePolicyViolation,
	CodeCommercialPermissionDenied, CodeNoRouteAvailable, CodeUpstreamContentFiltered,
	CodeCancelled, CodeRateLimited, CodeDeploymentUnavailable, CodeProviderError,
	CodeProviderTimeout, CodeProviderOverloaded, CodeServiceUnavailable, CodeInternalError,
}

// nonRetryableErrorCodes are the codes for which an identical retried request
// cannot succeed. The contract constrains `retryable` by the code, so a producer
// that sets both is rejected at Oxy's parse — which means Relay must not be that
// producer, and NewError enforces it here rather than discovering it at the edge.
var nonRetryableErrorCodes = []ErrorCode{
	CodeInvalidRequest, CodeAuthenticationFailed, CodePermissionDenied, CodeInsufficientScope,
	CodeModelNotFound, CodeUnsupportedModality, CodeContextLengthExceeded, CodeRequestTooLarge,
	CodeOutputLimitExceeded, CodeIdempotencyConflict, CodeInsufficientBalance,
	CodeSpendingLimitExceeded, CodeQuotaExceeded, CodeBYOKCredentialInvalid, CodePolicyViolation,
	CodeCommercialPermissionDenied, CodeNoRouteAvailable, CodeUpstreamContentFiltered, CodeCancelled,
}

var nonRetryableSet = func() map[ErrorCode]struct{} {
	set := make(map[ErrorCode]struct{}, len(nonRetryableErrorCodes))
	for _, code := range nonRetryableErrorCodes {
		set[code] = struct{}{}
	}
	return set
}()

// Retryable reports whether an identical retry of this code could ever succeed.
func (c ErrorCode) Retryable() bool {
	_, nonRetryable := nonRetryableSet[c]
	return !nonRetryable
}

// maxSafeErrorTextLength is the contract's ceiling on customer-visible free text.
const maxSafeErrorTextLength = 2000

// credentialLikeText matches the shapes upstream providers actually echo back.
// It is the same deliberately narrow set the contract refuses on, restated here
// because Relay is the producer: an error body whose message trips this pattern
// is rejected wholesale by Oxy's parse, and the customer then sees nothing at
// all instead of the real cause. Redacting before emitting is what keeps the
// diagnostic and drops the credential.
var credentialLikeText = regexp.MustCompile(`(?i)bearer\s+[a-z0-9._~+/=-]{8,}|authorization\s*[:=]|api[_-]?key\s*[:=]|\bsk-[a-z0-9_-]{8,}|\bsk_(?:live|test)_[a-z0-9]{8,}`)

// redactionMarker replaces credential-shaped spans in text bound for a customer.
const redactionMarker = "[redacted]"

// SafeErrorText renders free text the contract will accept: credential-shaped
// spans replaced, length capped, never empty.
//
// It returns a value rather than an error because the alternative — refusing to
// build the error body — loses the diagnostic entirely, which is strictly worse
// for the customer than a redacted one.
func SafeErrorText(text string) string {
	cleaned := credentialLikeText.ReplaceAllString(strings.TrimSpace(text), redactionMarker)
	if cleaned == "" {
		cleaned = "no diagnostic text was available"
	}
	if len(cleaned) > maxSafeErrorTextLength {
		cleaned = cleaned[:maxSafeErrorTextLength]
	}
	return cleaned
}

// ProviderErrorPassthrough is what the upstream said, reduced to the four fields
// a customer can act on. It carries no headers and no request body: this is the
// single most likely place for an upstream credential to escape.
type ProviderErrorPassthrough struct {
	Provider ProviderSlug `json:"provider"`
	Status   *int         `json:"status,omitempty"`
	Code     *string      `json:"code,omitempty"`
	Message  *string      `json:"message,omitempty"`
}

// Error is the body every inference surface returns and every stream error event
// carries.
type Error struct {
	SchemaVersion    int                       `json:"schemaVersion"`
	Code             ErrorCode                 `json:"code"`
	Message          string                    `json:"message"`
	Retryable        bool                      `json:"retryable"`
	RequestID        RequestID                 `json:"requestId"`
	RetryAfterMs     *int                      `json:"retryAfterMs,omitempty"`
	Param            *string                   `json:"param,omitempty"`
	UpstreamCategory *UpstreamErrorCategory    `json:"upstreamCategory,omitempty"`
	ProviderError    *ProviderErrorPassthrough `json:"providerError,omitempty"`
}

// Error implements the error interface so a contract error can travel as one.
func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s (requestId=%s, retryable=%t)", e.Code, e.Message, e.RequestID, e.Retryable)
}

// NewError builds an error body that the contract will accept.
//
// Retryability is derived from the code rather than taken from the caller. The
// contract makes `retryable` a producer assertion constrained by the code, and
// deriving it is the only way to guarantee Relay never emits the combination
// that Oxy's parse rejects.
func NewError(requestID RequestID, code ErrorCode, message string) *Error {
	return &Error{
		SchemaVersion: SchemaVersion,
		Code:          code,
		Message:       SafeErrorText(message),
		Retryable:     code.Retryable(),
		RequestID:     requestID,
	}
}

// WithParam names the request field at fault. Only meaningful for invalid_request.
func (e *Error) WithParam(param string) *Error {
	e.Param = &param
	return e
}

// WithRetryAfter records how long to wait. Ignored for a non-retryable code,
// because the contract rejects the pair and a silently dropped hint is better
// than an unparseable error body.
func (e *Error) WithRetryAfter(milliseconds int) *Error {
	if !e.Retryable || milliseconds < 0 {
		return e
	}
	e.RetryAfterMs = &milliseconds
	return e
}

// WithUpstream records Oxy's classification of an upstream failure and what the
// upstream said. Both free-text fields go through SafeErrorText.
func (e *Error) WithUpstream(category UpstreamErrorCategory, passthrough *ProviderErrorPassthrough) *Error {
	e.UpstreamCategory = &category
	if passthrough != nil {
		if passthrough.Message != nil {
			safe := SafeErrorText(*passthrough.Message)
			passthrough.Message = &safe
		}
		if passthrough.Code != nil && len(*passthrough.Code) > 128 {
			truncated := (*passthrough.Code)[:128]
			passthrough.Code = &truncated
		}
		e.ProviderError = passthrough
	}
	return e
}
