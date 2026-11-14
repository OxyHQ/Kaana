// Package openaierror reads OpenAI's error vocabulary for the session adapters
// that speak one of OpenAI's WebSockets: the Realtime API and its xAI dialect
// (internal/provider/openairealtime) and GPT-Live (internal/provider/openailive).
// A refused handshake is classified by its HTTP status, and an account that
// cannot be billed by OpenAI's own error type and code; an in-band `error`
// event by its type and code. Neither ever reads message prose
// (https://developers.openai.com/api/docs/guides/error-codes).
//
// The vocabulary is one fact about OpenAI's API, so it is written once: a second
// copy in each adapter would drift the day OpenAI adds a quota code.
package openaierror

import (
	"net/http"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// Session names whose session a failure is about, in its detail: the
// provider ("OpenAI", "xAI") and the API ("Realtime", "GPT-Live").
type Session struct {
	Provider string
	API      string
}

// QuotaExhausted recognises OpenAI's account-exhaustion vocabulary. The error
// codes page documents the 429 codes below and notes that the broader type can
// still be insufficient_quota.
func QuotaExhausted(kind, code string) bool {
	switch {
	case kind == "insufficient_quota", code == "insufficient_quota", code == "credit_balance_exhausted",
		code == "organization_spend_limit_exceeded", code == "project_spend_limit_exceeded",
		code == "organization_usage_limit_exceeded":
		return true
	}
	return false
}

// Passthrough is what the provider said, stripped of this key by exact match
// and redacted: its error type as the code, and its message.
func Passthrough(slug contract.ProviderSlug, kind, message string, key provider.Key) *contract.ProviderErrorPassthrough {
	passthrough := &contract.ProviderErrorPassthrough{Provider: slug}
	if kind != "" {
		redacted := contract.SafeErrorText(provider.RedactSecret(kind, key.Secret()))
		passthrough.Code = &redacted
	}
	if message != "" {
		redacted := contract.SafeErrorText(provider.RedactSecret(message, key.Secret()))
		passthrough.Message = &redacted
	}
	return passthrough
}

// Handshake classifies a refused WebSocket handshake by its status, with the
// provider's error type and code read only to tell an account that cannot be
// billed from a throttle: 402, or 429 with OpenAI's quota vocabulary, is the
// account refusing to be billed (the key is retired); 429 otherwise is a
// throttle with Retry-After; 401/403 refuse the credential; 404 is a model the
// provider does not serve; 408/504 a timeout; 503 overloaded; other 5xx the
// provider's own failure; anything else an invalid request.
func Handshake(session Session, status int, header http.Header, kind, code string, passthrough *contract.ProviderErrorPassthrough, key provider.Key) error {
	passthrough.Status = &status
	failure := provider.ErrUpstream{Passthrough: passthrough}
	switch {
	case status == http.StatusPaymentRequired,
		status == http.StatusTooManyRequests && QuotaExhausted(kind, code):
		failure.Code, failure.Category = contract.CodeProviderBillingRefused, contract.UpstreamQuota
		failure.Detail = "the platform's own " + session.Provider + " account cannot be billed for this session"
	case status == http.StatusTooManyRequests:
		failure.Code, failure.Category = contract.CodeRateLimited, contract.UpstreamRateLimit
		failure.Detail = session.Provider + " rate-limited this session"
		failure.RetryAfterMs = provider.RetryAfterMs(header)
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		failure.Code, failure.Category = contract.CodeProviderCredentialInvalid, contract.UpstreamAuthentication
		failure.Detail = session.Provider + " refused the platform's credential for this route"
	case status == http.StatusNotFound:
		failure.Code, failure.Category = contract.CodeModelNotFound, contract.UpstreamInvalidReq
		failure.Detail = session.Provider + " does not serve the model this route names"
	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout:
		failure.Code, failure.Category = contract.CodeProviderTimeout, contract.UpstreamTimeout
		failure.Detail = session.Provider + " timed out"
	case status == http.StatusServiceUnavailable:
		failure.Code, failure.Category = contract.CodeProviderOverloaded, contract.UpstreamOverloaded
		failure.Detail = session.Provider + " is overloaded"
		failure.RetryAfterMs = provider.RetryAfterMs(header)
	case status >= 500:
		failure.Code, failure.Category = contract.CodeProviderError, contract.UpstreamServerError
		failure.Detail = session.Provider + " returned an internal error"
	default:
		failure.Code, failure.Category = contract.CodeInvalidRequest, contract.UpstreamInvalidReq
		failure.Detail = session.Provider + " refused the " + session.API + " session"
	}
	return provider.CustomerCredentialFailure(key, failure)
}

// InBand classifies an in-band `error` event by OpenAI's type and code. Most
// are refusals of one client event, after which the session stays open
// (`invalid_request_error`, category invalid_request); the account and
// credential vocabulary is the one the HTTP API uses, and a `server_error` is
// OpenAI's own failure.
func InBand(session Session, kind, code string, passthrough *contract.ProviderErrorPassthrough, key provider.Key) error {
	failure := provider.ErrUpstream{Passthrough: passthrough}
	switch {
	case QuotaExhausted(kind, code):
		failure.Code, failure.Category = contract.CodeProviderBillingRefused, contract.UpstreamQuota
		failure.Detail = "the platform's own " + session.Provider + " account cannot be billed for this session"
	case code == "invalid_api_key", kind == "authentication_error":
		failure.Code, failure.Category = contract.CodeProviderCredentialInvalid, contract.UpstreamAuthentication
		failure.Detail = session.Provider + " refused the platform's credential for this route"
	case kind == "rate_limit_error", code == "rate_limit_exceeded", code == "slow_down":
		failure.Code, failure.Category = contract.CodeRateLimited, contract.UpstreamRateLimit
		failure.Detail = session.Provider + " rate-limited this session"
	case kind == "server_error":
		failure.Code, failure.Category = contract.CodeProviderError, contract.UpstreamServerError
		failure.Detail = session.Provider + "'s " + session.API + " session failed"
	case kind == "invalid_request_error":
		failure.Code, failure.Category = contract.CodeInvalidRequest, contract.UpstreamInvalidReq
		failure.Detail = session.Provider + " refused a " + session.API + " event"
	default:
		failure.Code, failure.Category = contract.CodeProviderError, contract.UpstreamUnknown
		failure.Detail = session.Provider + " reported a " + session.API + " error this adapter does not classify"
	}
	return provider.CustomerCredentialFailure(key, failure)
}
