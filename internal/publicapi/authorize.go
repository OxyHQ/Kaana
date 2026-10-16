package publicapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
)

// Authorizer is Oxy's internal authorization API as Kaana uses it. Each call
// either returns what Oxy decided or the public error the caller is owed.
type Authorizer interface {
	// Authorize asks for a grant for one inference operation. The grant it
	// returns is verified: signed under oxy-kaana-grant:v1, for this request,
	// operation and body, and still executable.
	Authorize(ctx context.Context, request contract.InferenceAuthorizationRequest) (*contract.Grant, *contract.PublicError)
	// Catalogue asks what a credential may see of the catalogue.
	Catalogue(ctx context.Context, request contract.CatalogueAuthorizationRequest) (*contract.CatalogueAccess, *contract.PublicError)
	// Receipt asks Oxy for a generation record, which Kaana relays verbatim.
	Receipt(ctx context.Context, request contract.ReceiptAuthorizationRequest) (json.RawMessage, *contract.PublicError)
}

// TokenSource is Kaana's own Oxy service token (Oxy ADR 0026): how Kaana
// authenticates to Oxy, never the caller's credential.
type TokenSource func(context.Context) (string, error)

// OxyAuthorizerConfig wires the HTTP authorizer.
type OxyAuthorizerConfig struct {
	// URL is POST …/internal/kaana/v1/authorizations.
	URL string
	// Token authenticates Kaana as a workload.
	Token TokenSource
	// Grants verifies the grant's Ed25519 signature (KAANA_EDGE_PUBLIC_KEYS).
	Grants *edgeauth.Verifier
	Client *http.Client
	// Timeout bounds one authorization. Oxy's p99 adds to every request's
	// time to first byte (ADR 0002, risk 6), so it is short.
	Timeout time.Duration
	Logger  *slog.Logger
	Now     func() time.Time
}

// OxyAuthorizer calls Oxy's authorization API over HTTPS.
type OxyAuthorizer struct {
	url     string
	token   TokenSource
	grants  *edgeauth.Verifier
	client  *http.Client
	timeout time.Duration
	logger  *slog.Logger
	now     func() time.Time

	mu        sync.Mutex
	catalogue map[[32]byte]cachedCatalogue
}

type cachedCatalogue struct {
	access  contract.CatalogueAccess
	expires time.Time
}

// maxAuthorizationResponseBytes bounds what Oxy may answer; a grant is small.
const maxAuthorizationResponseBytes = 1 << 20

// catalogueCacheTTL is the most a catalogue answer is reused (ADR 0002).
const catalogueCacheTTL = 60 * time.Second

// NewOxyAuthorizer builds the authorizer. The URL must be https, except a
// loopback origin for local development.
func NewOxyAuthorizer(config OxyAuthorizerConfig) (*OxyAuthorizer, error) {
	parsed, err := url.Parse(config.URL)
	switch {
	case err != nil || !secureOrLoopback(parsed):
		return nil, fmt.Errorf("publicapi: the Oxy authorization URL %q is not an https URL", config.URL)
	case config.Token == nil:
		return nil, errors.New("publicapi: no Oxy service token source")
	case config.Grants == nil:
		return nil, errors.New("publicapi: no grant signature verifier")
	}
	client := config.Client
	if client == nil {
		client = &http.Client{}
	}
	copied := *client
	copied.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &OxyAuthorizer{
		url: parsed.String(), token: config.Token, grants: config.Grants, client: &copied,
		timeout: timeout, logger: logger, now: now, catalogue: make(map[[32]byte]cachedCatalogue),
	}, nil
}

// secureOrLoopback admits an https URL, or http only to a loopback host for
// local development: Kaana's service token never crosses plain HTTP.
func secureOrLoopback(parsed *url.URL) bool {
	if parsed.Host == "" {
		return false
	}
	return parsed.Scheme == "https" || (parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname()))
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// Authorize implements Authorizer.
func (a *OxyAuthorizer) Authorize(ctx context.Context, request contract.InferenceAuthorizationRequest) (*contract.Grant, *contract.PublicError) {
	body, header, failure := a.post(ctx, request.RequestID, request)
	if failure != nil {
		return nil, failure
	}
	// The signature covers the exact bytes; they are verified before a single
	// field of them is believed.
	if err := a.grants.Verify(header, body); err != nil {
		a.logger.Error("Oxy answered an authorization with a grant that does not verify", "requestId", request.RequestID)
		return nil, contract.NewPublicError(request.RequestID, "service_unavailable", contract.PublicMessage("service_unavailable"))
	}
	grant, err := contract.DecodeGrant(body)
	if err != nil {
		a.logger.Error("Oxy signed a grant this build cannot read", "requestId", request.RequestID, "error", err)
		return nil, contract.NewPublicError(request.RequestID, "service_unavailable", contract.PublicMessage("service_unavailable"))
	}
	if failure := checkGrant(grant, request, a.now()); failure != nil {
		a.logger.Error("Oxy signed a grant for something other than this request", "requestId", request.RequestID, "reason", failure.Error())
		return nil, contract.NewPublicError(request.RequestID, "service_unavailable", contract.PublicMessage("service_unavailable"))
	}
	return grant, nil
}

// checkGrant binds a verified grant to the request it answers: the same
// request id, operation and body, and still inside its execution window.
func checkGrant(grant *contract.Grant, request contract.InferenceAuthorizationRequest, now time.Time) error {
	switch {
	case grant.RequestID != request.RequestID:
		return errors.New("the grant names another request")
	case grant.Operation != request.Operation:
		return errors.New("the grant authorizes another operation")
	case grant.BodySHA256 != request.BodySHA256:
		return errors.New("the grant authorizes another body")
	case !grant.Executable(now):
		return errors.New("the grant's execution window has passed")
	case grant.IdempotencyKey != nil && (request.IdempotencyKey == nil || *grant.IdempotencyKey != *request.IdempotencyKey):
		return errors.New("the grant names another idempotency key")
	}
	return nil
}

// Catalogue implements Authorizer, caching each credential's answer for at
// most 60 seconds and never past the expiry Oxy states.
func (a *OxyAuthorizer) Catalogue(ctx context.Context, request contract.CatalogueAuthorizationRequest) (*contract.CatalogueAccess, *contract.PublicError) {
	digest := sha256.Sum256([]byte(request.Credential))
	now := a.now()
	a.mu.Lock()
	cached, hit := a.catalogue[digest]
	a.mu.Unlock()
	if hit && now.Before(cached.expires) {
		access := cached.access
		return &access, nil
	}
	body, _, failure := a.post(ctx, request.RequestID, request)
	if failure != nil {
		return nil, failure
	}
	var access contract.CatalogueAccess
	if err := decodeStrict(body, &access); err != nil || access.Validate() != nil {
		return nil, contract.NewPublicError(request.RequestID, "service_unavailable", contract.PublicMessage("service_unavailable"))
	}
	expires := now.Add(catalogueCacheTTL)
	if stated, err := time.Parse(time.RFC3339Nano, string(access.ExpiresAt)); err == nil && stated.Before(expires) {
		expires = stated
	}
	a.mu.Lock()
	if len(a.catalogue) > credentialDigestCacheLimit {
		clear(a.catalogue)
	}
	a.catalogue[digest] = cachedCatalogue{access: access, expires: expires}
	a.mu.Unlock()
	return &access, nil
}

// Receipt implements Authorizer.
func (a *OxyAuthorizer) Receipt(ctx context.Context, request contract.ReceiptAuthorizationRequest) (json.RawMessage, *contract.PublicError) {
	body, _, failure := a.post(ctx, request.RequestID, request)
	if failure != nil {
		return nil, failure
	}
	var access contract.ReceiptAccess
	if err := decodeStrict(body, &access); err != nil || access.SchemaVersion != contract.AuthorizationSchemaVersion ||
		access.Kind != contract.AuthorizationKindReceipt || len(access.Record) == 0 {
		return nil, contract.NewPublicError(request.RequestID, "service_unavailable", contract.PublicMessage("service_unavailable"))
	}
	return access.Record, nil
}

// post sends one authorization and maps every failure to the public error the
// caller is owed. Oxy being unreachable is service_unavailable: there is no
// degraded mode without a grant (ADR 0002, consequences).
func (a *OxyAuthorizer) post(ctx context.Context, requestID contract.RequestID, payload any) ([]byte, http.Header, *contract.PublicError) {
	unavailable := func() *contract.PublicError {
		return contract.NewPublicError(requestID, "service_unavailable", contract.PublicMessage("service_unavailable")).WithRetryAfter(1000)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, contract.NewPublicError(requestID, "internal_error", contract.PublicMessage("internal_error"))
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	token, err := a.token(ctx)
	if err != nil {
		a.logger.Error("Kaana could not mint its Oxy service token for an authorization", "requestId", requestID, "errorType", "oxy_identity")
		return nil, nil, unavailable()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url, bytes.NewReader(encoded))
	if err != nil {
		return nil, nil, unavailable()
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Request-Id", string(requestID))
	response, err := a.client.Do(request)
	if err != nil {
		if ctx.Err() != nil && errors.Is(context.Cause(ctx), context.Canceled) {
			return nil, nil, contract.NewPublicError(requestID, "cancelled", contract.PublicMessage("cancelled"))
		}
		a.logger.Warn("Oxy's authorization API is unreachable", "requestId", requestID, "errorType", "oxy_unreachable")
		return nil, nil, unavailable()
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxAuthorizationResponseBytes+1))
	if err != nil || len(body) > maxAuthorizationResponseBytes {
		return nil, nil, unavailable()
	}
	if response.StatusCode == http.StatusOK {
		return body, response.Header, nil
	}
	return nil, nil, a.refusal(requestID, response.StatusCode, response.Header, body)
}

// refusal reads Oxy's answer to an authorization it did not grant. Oxy
// answers with the error shape both surfaces share (code, retryable,
// retryAfterMs, param); its message may be about the account and is replaced
// by the code's public text, which links to Oxy Console where it matters.
//
// Anything else is not the caller's refusal — it is Oxy, or something in front
// of it, turning Kaana away — so it is service_unavailable, retryable after the
// Retry-After Oxy sent, or a second when it sent none. One such answer Kaana
// reads by name: Oxy's API error `RATE_LIMITED` (`{"error":"RATE_LIMITED",…}`),
// which its global limiters answer when Kaana's own service credential is over
// its budget. That budget is all of Kaana's control traffic, never the
// caller's, so it is still service_unavailable for Oxy's stated wait; what
// changes is that it is logged as what it is, an operator's problem.
func (a *OxyAuthorizer) refusal(requestID contract.RequestID, status int, header http.Header, body []byte) *contract.PublicError {
	var answer struct {
		Code         contract.ErrorCode `json:"code"`
		RetryAfterMs *int               `json:"retryAfterMs"`
		Param        *string            `json:"param"`
		// The shared shape nested under `error`, or Oxy's API error code.
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(body, &answer)
	var apiCode string
	if answer.Code == "" && len(answer.Error) > 0 && json.Unmarshal(answer.Error, &apiCode) != nil {
		var nested struct {
			Code         contract.ErrorCode `json:"code"`
			RetryAfterMs *int               `json:"retryAfterMs"`
			Param        *string            `json:"param"`
		}
		if json.Unmarshal(answer.Error, &nested) == nil {
			answer.Code, answer.RetryAfterMs, answer.Param = nested.Code, nested.RetryAfterMs, nested.Param
		}
	}
	if answer.Code == "" {
		if status == http.StatusTooManyRequests && apiCode == oxyRateLimited {
			a.logger.Error("Oxy is rate-limiting Kaana's own authorizations", "requestId", requestID, "retryAfterMs", retryAfterMs(header), "errorType", "oxy_rate_limited")
		} else {
			a.logger.Warn("Oxy refused an authorization without a readable error", "requestId", requestID, "status", status, "oxyCode", apiCode)
		}
		if status >= 500 || status == http.StatusTooManyRequests {
			return contract.NewPublicError(requestID, "service_unavailable", contract.PublicMessage("service_unavailable")).WithRetryAfter(retryAfterMs(header))
		}
		return contract.NewPublicError(requestID, "internal_error", contract.PublicMessage("internal_error"))
	}
	internal := contract.NewError(requestID, answer.Code, "refused by Oxy")
	if answer.Param != nil {
		internal = internal.WithParam(*answer.Param)
	}
	if answer.RetryAfterMs != nil {
		internal = internal.WithRetryAfter(*answer.RetryAfterMs)
	}
	return contract.PublicErrorFrom(internal)
}

// oxyRateLimited is the code Oxy's API errors carry when one of its limiters
// refused the request (oxy packages/api/src/middleware/rateLimiter.ts).
const oxyRateLimited = "RATE_LIMITED"

// maxStatedRetryAfter bounds the wait an unreadable refusal's Retry-After can
// pass on: Oxy's longest rate-limit window.
const maxStatedRetryAfter = 15 * time.Minute

// retryAfterMs is the wait a Retry-After header states in delay-seconds, or a
// second when it states none Kaana can use.
func retryAfterMs(header http.Header) int {
	seconds, err := strconv.Atoi(strings.TrimSpace(header.Get("Retry-After")))
	if err != nil || seconds <= 0 {
		return 1000
	}
	return int(min(time.Duration(seconds)*time.Second, maxStatedRetryAfter).Milliseconds())
}

func decodeStrict(body []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}
