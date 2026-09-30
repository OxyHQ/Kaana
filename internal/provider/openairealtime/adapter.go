// Package openairealtime opens OpenAI Realtime (GA) conversation sessions over
// a server-to-server WebSocket.
//
// It is a provider.RealtimeAdapter under its own slug, `openai-realtime`,
// bound to OpenAI's own origin exactly as `openai-audio` is. A session is not a
// request, so it is not served by the Chat Completions adapter under `openai`
// either: a deployment resolves to one adapter, and a realtime model published
// under this slug can only be opened by this code.
//
// Wire reviewed against OpenAI's documentation on 2026-09-30:
// https://developers.openai.com/api/docs/guides/voice-websockets,
// https://developers.openai.com/api/reference/resources/realtime/client-events,
// https://developers.openai.com/api/reference/resources/realtime/server-events.
package openairealtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// Slug is the provider name OpenAI's Realtime sessions are served under.
const Slug contract.ProviderSlug = "openai-realtime"

// openTimeout bounds the configuration half of opening: from the accepted
// handshake to OpenAI confirming the session's configuration.
const openTimeout = 20 * time.Second

// maxUpstreamEventBytes bounds one OpenAI server event. The largest are
// response.done and conversation.item.done, which carry a response's whole
// output text and transcripts but never its audio.
const maxUpstreamEventBytes = 16 << 20

// openEventID names the session.update that configures a new session, so the
// error it causes can be told apart from a later one.
const openEventID = "kaana-session-open"

type Config struct {
	Declarations []provider.KeyDeclaration
	Keys         provider.KeyPolicy
	HTTPClient   *http.Client
}

type Adapter struct {
	client      *http.Client
	credentials *provider.KeyPool
}

// New builds the adapter. Its origin is not configurable: the slug is bound to
// OpenAI's own API root, and the session endpoint to OpenAI's own WebSocket.
func New(config Config) (*Adapter, error) {
	if err := providerconfig.ValidateEndpointIdentity(Slug, providerconfig.OpenAIRealtimeBaseURL); err != nil {
		return nil, err
	}
	pool, err := provider.NewKeyPool(Slug, config.Declarations, config.Keys, provider.QuotaHeaders{})
	if err != nil {
		return nil, err
	}
	return &Adapter{client: provider.RefuseRedirects(config.HTTPClient), credentials: pool}, nil
}

func (a *Adapter) Provider() contract.ProviderSlug        { return Slug }
func (a *Adapter) PlatformCredentials() *provider.KeyPool { return a.credentials }

// RealtimeSessionKinds implements provider.RealtimeAdapter: conversations only.
func (a *Adapter) RealtimeSessionKinds() []contract.RealtimeSessionKind {
	return providerconfig.RealtimeSessionKinds(Slug, providerconfig.ProtocolOpenAIRealtime)
}

func (a *Adapter) refuseOpen(request provider.RealtimeOpenRequest) (*sessionConfig, error) {
	if request.Route.Provider != Slug || request.Route.UpstreamModelID == "" {
		return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "authorizedRoutes", Detail: "an exact OpenAI Realtime deployment is required"}
	}
	if !provider.OpensRealtime(a, request.Kind) {
		return nil, provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: "kind",
			Detail: fmt.Sprintf("the %s deployment holds %s sessions only; OpenAI's %s sessions are not served", Slug, contract.RealtimeConversation, request.Kind)}
	}
	return wireSession(request.Config)
}

// Open dials OpenAI, waits for the session it creates, configures it, and
// returns it once OpenAI has confirmed the configuration. That is what "open"
// means for failover: a session that dialled but refused its configuration
// never opened, and the credential rules read that refusal exactly as they
// read a refused request.
func (a *Adapter) Open(ctx context.Context, request provider.RealtimeOpenRequest, credentials *provider.KeyPool) (provider.RealtimeUpstream, provider.RealtimeOpened, error) {
	var opened provider.RealtimeOpened
	configuration, err := a.refuseOpen(request)
	if err != nil {
		return nil, opened, err
	}
	update, err := json.Marshal(clientEvent{Type: "session.update", EventID: openEventID, Session: configuration})
	if err != nil {
		return nil, opened, fmt.Errorf("openairealtime: encoding the session configuration: %w", err)
	}
	if credentials == nil {
		credentials = a.credentials
	}
	endpoint := providerconfig.OpenAIRealtimeSessionURL + "?model=" + url.QueryEscape(request.Route.UpstreamModelID)
	call := &provider.Call{RequestID: request.RequestID, Route: request.Route, Method: http.MethodGet, URL: endpoint}

	conn, key, err := provider.WalkAttempts(ctx, credentials, call, func(ctx context.Context, key provider.Key) (*websocket.Conn, provider.CredentialedAttempt) {
		attempt, dialled := a.dial(ctx, endpoint, key)
		if dialled == nil {
			return nil, attempt
		}
		if failure := configure(ctx, dialled, update, key); failure != nil {
			_ = dialled.CloseNow()
			attempt.Failure = failure
			var transport transportFailure
			if errors.As(failure, &transport) {
				attempt.Failure, attempt.Transport = transport.failure, true
			}
			return nil, attempt
		}
		attempt.Accepted = true
		attempt.Release = func() { _ = dialled.CloseNow() }
		return dialled, attempt
	})
	opened.KeyID, opened.KeyClass = key.ID, key.Class
	if err != nil {
		return nil, opened, err
	}
	inputFormat := request.Config.InputAudioFormat
	return newSession(conn, request, key, inputFormat), opened, nil
}

// dial performs the WebSocket handshake with one credential. A handshake
// OpenAI refused is classified from its status and body like any other
// refusal; one that never got an answer is a transport failure.
func (a *Adapter) dial(ctx context.Context, endpoint string, key provider.Key) (provider.CredentialedAttempt, *websocket.Conn) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+key.Secret())
	dialContext, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	conn, response, err := websocket.Dial(dialContext, endpoint, &websocket.DialOptions{HTTPClient: a.client, HTTPHeader: header})
	if err != nil {
		if response != nil && response.StatusCode != http.StatusSwitchingProtocols {
			var body []byte
			if response.Body != nil {
				body, _ = io.ReadAll(io.LimitReader(response.Body, 64<<10))
				_ = response.Body.Close()
			}
			return provider.CredentialedAttempt{Header: response.Header, Failure: classifyRefusal(response.StatusCode, response.Header, body, key)}, nil
		}
		return provider.CredentialedAttempt{Failure: transport(ctx, err), Transport: true}, nil
	}
	conn.SetReadLimit(maxUpstreamEventBytes)
	return provider.CredentialedAttempt{Header: response.Header}, conn
}

// transportFailure marks a configuration exchange that ended without OpenAI
// answering it, which says nothing about the credential.
type transportFailure struct{ failure error }

func (t transportFailure) Error() string { return t.failure.Error() }

// configure waits for session.created, sends the configuration and waits for
// OpenAI to confirm it. An error event in between is OpenAI's own answer about
// this session on this credential.
func configure(ctx context.Context, conn *websocket.Conn, update []byte, key provider.Key) error {
	openContext, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	created := false
	for {
		kind, data, err := conn.Read(openContext)
		if err != nil {
			return transportFailure{transport(openContext, err)}
		}
		if kind != websocket.MessageText {
			return transportFailure{invalidEvent()}
		}
		var event serverEvent
		if json.Unmarshal(data, &event) != nil {
			return transportFailure{invalidEvent()}
		}
		switch event.Type {
		case "error":
			if event.Error == nil {
				return transportFailure{invalidEvent()}
			}
			return classifyEvent(*event.Error, key)
		case "session.created":
			if created {
				return transportFailure{invalidEvent()}
			}
			created = true
			if err := conn.Write(openContext, websocket.MessageText, update); err != nil {
				return transportFailure{transport(openContext, err)}
			}
		case "session.updated":
			if created {
				return nil
			}
		}
		// Anything else before the confirmation (conversation.created,
		// rate_limits.updated) carries nothing the contract names.
	}
}

func invalidEvent() error {
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown,
		Detail: "OpenAI sent a Realtime event this adapter cannot read", Passthrough: &contract.ProviderErrorPassthrough{Provider: Slug}}
}

func transport(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	passthrough := &contract.ProviderErrorPassthrough{Provider: Slug}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return provider.ErrUpstream{Code: contract.CodeProviderTimeout, Category: contract.UpstreamTimeout, Detail: "OpenAI's Realtime session did not answer in time", Passthrough: passthrough}
	}
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamServerError, Detail: "the connection to OpenAI's Realtime API failed", Passthrough: passthrough}
}

// classifyRefusal reads a refused handshake by status and OpenAI's own error
// type and code, never by message prose
// (https://developers.openai.com/api/docs/guides/error-codes). The message is
// bounded, stripped of this key by exact match, and redacted.
func classifyRefusal(status int, header http.Header, body []byte, key provider.Key) error {
	var parsed struct {
		Error wireError `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	passthrough := passthroughFor(parsed.Error, key)
	passthrough.Status = &status
	code := ""
	if parsed.Error.Code != nil {
		code = *parsed.Error.Code
	}
	failure := provider.ErrUpstream{Passthrough: passthrough}
	switch {
	case status == http.StatusPaymentRequired,
		status == http.StatusTooManyRequests && quotaExhausted(parsed.Error.Type, code):
		failure.Code, failure.Category = contract.CodeProviderBillingRefused, contract.UpstreamQuota
		failure.Detail = "the platform's own OpenAI account cannot be billed for this session"
	case status == http.StatusTooManyRequests:
		failure.Code, failure.Category = contract.CodeRateLimited, contract.UpstreamRateLimit
		failure.Detail = "OpenAI rate-limited this session"
		failure.RetryAfterMs = provider.RetryAfterMs(header)
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		failure.Code, failure.Category = contract.CodeProviderCredentialInvalid, contract.UpstreamAuthentication
		failure.Detail = "OpenAI refused the platform's credential for this route"
	case status == http.StatusNotFound:
		failure.Code, failure.Category = contract.CodeModelNotFound, contract.UpstreamInvalidReq
		failure.Detail = "OpenAI does not serve the model this route names"
	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout:
		failure.Code, failure.Category = contract.CodeProviderTimeout, contract.UpstreamTimeout
		failure.Detail = "OpenAI timed out"
	case status == http.StatusServiceUnavailable:
		failure.Code, failure.Category = contract.CodeProviderOverloaded, contract.UpstreamOverloaded
		failure.Detail = "OpenAI is overloaded"
		failure.RetryAfterMs = provider.RetryAfterMs(header)
	case status >= 500:
		failure.Code, failure.Category = contract.CodeProviderError, contract.UpstreamServerError
		failure.Detail = "OpenAI returned an internal error"
	default:
		failure.Code, failure.Category = contract.CodeInvalidRequest, contract.UpstreamInvalidReq
		failure.Detail = "OpenAI refused the Realtime session"
	}
	return provider.CustomerCredentialFailure(key, failure)
}

// quotaExhausted recognises OpenAI's account-exhaustion vocabulary. The error
// codes page documents the 429 codes below and notes that the broader type can
// still be insufficient_quota.
func quotaExhausted(kind, code string) bool {
	switch {
	case kind == "insufficient_quota", code == "insufficient_quota", code == "credit_balance_exhausted",
		code == "organization_spend_limit_exceeded", code == "project_spend_limit_exceeded",
		code == "organization_usage_limit_exceeded":
		return true
	}
	return false
}

// classifyEvent reads an in-band error event by OpenAI's type and code. Most
// are refusals of one client event ("Most errors are recoverable and the
// session will stay open"); the account and credential vocabulary is the same
// one the HTTP API uses, and a server_error is OpenAI's own failure.
func classifyEvent(value wireError, key provider.Key) error {
	code := ""
	if value.Code != nil {
		code = *value.Code
	}
	failure := provider.ErrUpstream{Passthrough: passthroughFor(value, key)}
	switch {
	case quotaExhausted(value.Type, code):
		failure.Code, failure.Category = contract.CodeProviderBillingRefused, contract.UpstreamQuota
		failure.Detail = "the platform's own OpenAI account cannot be billed for this session"
	case code == "invalid_api_key", value.Type == "authentication_error":
		failure.Code, failure.Category = contract.CodeProviderCredentialInvalid, contract.UpstreamAuthentication
		failure.Detail = "OpenAI refused the platform's credential for this route"
	case value.Type == "rate_limit_error", code == "rate_limit_exceeded", code == "slow_down":
		failure.Code, failure.Category = contract.CodeRateLimited, contract.UpstreamRateLimit
		failure.Detail = "OpenAI rate-limited this session"
	case value.Type == "server_error":
		failure.Code, failure.Category = contract.CodeProviderError, contract.UpstreamServerError
		failure.Detail = "OpenAI's Realtime session failed"
	case value.Type == "invalid_request_error":
		failure.Code, failure.Category = contract.CodeInvalidRequest, contract.UpstreamInvalidReq
		failure.Detail = "OpenAI refused a Realtime event"
	default:
		failure.Code, failure.Category = contract.CodeProviderError, contract.UpstreamUnknown
		failure.Detail = "OpenAI reported a Realtime error this adapter does not classify"
	}
	return provider.CustomerCredentialFailure(key, failure)
}

func passthroughFor(value wireError, key provider.Key) *contract.ProviderErrorPassthrough {
	passthrough := &contract.ProviderErrorPassthrough{Provider: Slug}
	if value.Type != "" {
		kind := contract.SafeErrorText(provider.RedactSecret(value.Type, key.Secret()))
		passthrough.Code = &kind
	}
	if value.Message != "" {
		message := contract.SafeErrorText(provider.RedactSecret(value.Message, key.Secret()))
		passthrough.Message = &message
	}
	return passthrough
}

// Health never opens a session: a probe that held one would be a charge
// nobody asked for. Entitlement is proved by a signed canary session.
func (a *Adapter) Health(_ context.Context) provider.Health {
	now := time.Now()
	pool := a.credentials.Projection(now)
	health := provider.Health{Provider: Slug, CheckedAt: contract.NewTimestamp(now), Credentials: &pool, Status: provider.HealthDegraded,
		Detail: "credential configured; realtime entitlement requires a signed canary session"}
	if !a.credentials.Configured() {
		health.Status, health.Detail = provider.HealthUnconfigured, "no OpenAI Realtime credential configured"
	} else if pool.Usable == 0 {
		health.Status, health.Detail = provider.HealthUnavailable, "no usable OpenAI Realtime credential"
	}
	return health
}
