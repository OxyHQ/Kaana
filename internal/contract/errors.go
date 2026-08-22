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
	// CodeProviderCredentialInvalid is an upstream refusing the PLATFORM's own
	// credential — the counterpart of byok_credential_invalid on the other side
	// of the BYOK boundary. It is the platform group's one non-retryable code,
	// because no retry reaches the operator who has to rotate a key.
	CodeProviderCredentialInvalid ErrorCode = "provider_credential_invalid"
	// CodeProviderBillingRefused is an upstream declining to bill OXY — its
	// account with the provider, not the customer's balance. It sits beside
	// provider_credential_invalid for the same reason: only an operator can act
	// on it, so reporting it as the customer's own exhausted quota is
	// retryability-correct and diagnostically wrong, which reads as actionable
	// while the action does nothing.
	CodeProviderBillingRefused ErrorCode = "provider_billing_refused"
	CodeServiceUnavailable     ErrorCode = "service_unavailable"
	CodeInternalError          ErrorCode = "internal_error"
)

var errorCodeValues = []ErrorCode{
	CodeInvalidRequest, CodeAuthenticationFailed, CodePermissionDenied, CodeInsufficientScope,
	CodeModelNotFound, CodeUnsupportedModality, CodeContextLengthExceeded, CodeRequestTooLarge,
	CodeOutputLimitExceeded, CodeIdempotencyConflict, CodeInsufficientBalance,
	CodeSpendingLimitExceeded, CodeQuotaExceeded, CodeBYOKCredentialInvalid, CodePolicyViolation,
	CodeCommercialPermissionDenied, CodeNoRouteAvailable, CodeUpstreamContentFiltered,
	CodeCancelled, CodeRateLimited, CodeDeploymentUnavailable, CodeProviderError,
	CodeProviderTimeout, CodeProviderOverloaded, CodeProviderCredentialInvalid,
	CodeProviderBillingRefused, CodeServiceUnavailable, CodeInternalError,
}

// nonRetryableErrorCodes are the codes for which an identical retried request
// cannot succeed. The contract constrains `retryable` by the code, so a producer
// that sets both is rejected at Oxy's parse — which means Kaana must not be that
// producer, and NewError enforces it here rather than discovering it at the edge.
var nonRetryableErrorCodes = []ErrorCode{
	CodeInvalidRequest, CodeAuthenticationFailed, CodePermissionDenied, CodeInsufficientScope,
	CodeModelNotFound, CodeUnsupportedModality, CodeContextLengthExceeded, CodeRequestTooLarge,
	CodeOutputLimitExceeded, CodeIdempotencyConflict, CodeInsufficientBalance,
	CodeSpendingLimitExceeded, CodeQuotaExceeded, CodeBYOKCredentialInvalid, CodePolicyViolation,
	CodeCommercialPermissionDenied, CodeNoRouteAvailable, CodeUpstreamContentFiltered, CodeCancelled,
	CodeProviderCredentialInvalid, CodeProviderBillingRefused,
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

// The published refusal is FOUR independent signals rather than one pattern, and
// they are restated here because Kaana is a producer: text that trips any of
// them is rejected wholesale by Oxy's parse, and the customer then sees nothing
// at all instead of the real cause.
//
// They are not character-identical to the published regexes and cannot be: the
// contract's first two use a negative lookahead to exclude a placeholder at the
// value position, and Go's RE2 engine has none. The lookahead is expressed here
// as a capture plus placeholderPrefix, which is why credentialShaped is pinned
// BEHAVIOURALLY — descriptor_test.go asserts a table of strings the published
// pattern refuses and a table it accepts, in both directions.
const (
	opaqueAlphabet = `[A-Za-z0-9][A-Za-z0-9._~+/=-]`
	// credentialName is the prefix group that matters most: `authorization` and
	// `api_key` matched literally miss `x-api-key`, `anthropic-api-key` and
	// `proxy-authorization` — the spellings an upstream actually echoes.
	credentialName = `(?:[a-z0-9]{1,20}[-_]){0,3}(?:api[-_]?(?:key|token|secret)|authorization|auth[-_]?(?:token|key)?|access[-_]?token|id[-_]?token|refresh[-_]?token|bearer[-_]?token|secret[-_]?key|private[-_]?key|client[-_]?secret|session[-_]?(?:id|key|token)|passwords?|passwd|cookie|credentials?|tokens?|secrets?)`
	authScheme     = `(?:(?:bearer|basic|token|apikey|digest)\s+)?`
	// placeholderWords are what a producer substitutes FOR a credential. A value
	// position holding one has been redacted correctly and is accepted.
	placeholderWords = `redacted|removed|hidden|masked|scrubbed|elided|omitted|filtered|sanitized|sanitised|none|null|undefined|empty`
)

var (
	// 1. A credential-bearing name ASSIGNED a value long enough to be a
	//    credential. The value is captured rather than excluded in-pattern
	//    because RE2 cannot express "not a placeholder here".
	credentialAssignment = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])` + credentialName + `["']?\s*[:=]\s*["']?` + authScheme + `(` + opaqueAlphabet + `{6,})`)
	// 2. A bearer token with no marker in front of it, which is how an upstream
	//    quotes the header value alone.
	bareBearerToken = regexp.MustCompile(`(?i)\bbearer\s+(` + opaqueAlphabet + `{6,})`)
	// 3. Token grammars that ARE credentials wherever they appear, marker or
	//    not. This is the layer that survives a producer stripping the marker.
	//    Case-SENSITIVE on purpose: these prefixes are issued in exactly this
	//    case, and matching them loosely would fire on ordinary words.
	issuedTokenGrammar = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{8,}|[sprk]k_(?:live|test)_[A-Za-z0-9]{8,}|AKIA[0-9A-Z]{12,}|ASIA[0-9A-Z]{12,}|AIza[0-9A-Za-z_-]{20,}|gh[pousr]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{20,}|xox[abeprs]-[A-Za-z0-9-]{10,}|glpat-[A-Za-z0-9_-]{16,}|npm_[A-Za-z0-9]{20,}|eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{4,})`)
	// 4. A placeholder standing NEXT TO a surviving opaque value — the residue a
	//    span redaction leaves, and the reason this package no longer performs
	//    one. A correct redaction puts the placeholder WHERE the value was, so
	//    the two never appear side by side.
	redactedMarkerBesideValue = regexp.MustCompile(`(?i)(?:[\[<({]\s*(?:` + placeholderWords + `)[^\])}>]{0,16}[\])}>]|\*{3,})[^A-Za-z0-9]{0,4}` + opaqueAlphabet + `{10,}`)
	// placeholderPrefix is the lookahead the two assignment patterns cannot
	// carry: a captured value that BEGINS with a placeholder word was redacted
	// correctly, and refusing it is what pushed producers into redacting the
	// marker instead.
	placeholderPrefix = regexp.MustCompile(`(?i)^(?:` + placeholderWords + `)\b`)
)

// CredentialShaped reports whether text still looks like it carries a
// credential, by the published contract's own reading.
func CredentialShaped(text string) bool {
	for _, pattern := range []*regexp.Regexp{credentialAssignment, bareBearerToken} {
		for _, match := range pattern.FindAllStringSubmatch(text, -1) {
			if !placeholderPrefix.MatchString(match[1]) {
				return true
			}
		}
	}
	return issuedTokenGrammar.MatchString(text) || redactedMarkerBesideValue.MatchString(text)
}

// WithheldErrorText replaces a message that still looks like it carries a
// credential. It is a whole-string replacement and never a span one — see
// SafeErrorText.
//
// Exported so a caller can tell "the upstream said nothing useful" from "the
// upstream's diagnostic was thrown away to keep a credential out of it". The
// conformance suite reads it for exactly that: an adapter that redacted its own
// key correctly keeps the message, and one that relied on this refusal loses it.
const WithheldErrorText = "the provider's message was withheld: it still looked like it carried a credential"

// SafeErrorText renders free text the contract will accept: bounded, never
// empty, and withheld entirely when it still looks like it carries a credential.
//
// It does NOT redact the span that matched, and that is the whole point of this
// function's shape. The span is the MARKER; the secret is what follows it, so
// replacing the span produces `{x-[redacted] <key>}` — a string that carries the
// key and no longer looks like it does. Kaana measured that (OxyHQ/Kaana#3) and
// the contract now refuses the residue explicitly, so a span redaction here
// would turn a message Oxy rejects into one it accepts with the credential
// intact.
//
// The control that actually keeps a credential out of an error is
// provider.RedactSecret, applied by the adapter that is still holding the exact
// bytes it sent. This function is what catches what that missed: a credential
// belonging to somebody else, echoed by a shared gateway. Losing the diagnostic
// is the price of not being able to tell which bytes are secret.
func SafeErrorText(text string) string {
	cleaned := strings.TrimSpace(text)
	if cleaned == "" {
		return "no diagnostic text was available"
	}
	if len(cleaned) > maxSafeErrorTextLength {
		// On a rune boundary: a truncation that split a multi-byte character
		// would emit invalid UTF-8, which is not what the customer's client is
		// expecting to render.
		cleaned = strings.ToValidUTF8(cleaned[:maxSafeErrorTextLength], "")
	}
	if CredentialShaped(cleaned) {
		return WithheldErrorText
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
// deriving it is the only way to guarantee Kaana never emits the combination
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
