// Package openairealtime opens OpenAI-Realtime-shaped conversation sessions
// over a server-to-server WebSocket: OpenAI's Realtime (GA) API, and xAI's
// Voice Agent API, which xAI documents as compatible with it.
//
// Each is a provider.RealtimeAdapter under its own slug bound to its own
// origin: `openai-realtime` (as `openai-audio` is) and `xai-realtime` (beside
// the `xai` request adapter). A session is not a request, so neither is served
// by a Chat Completions adapter: a deployment resolves to one adapter, and a
// realtime model published under one of these slugs can only be opened by this
// code, in that provider's dialect (dialect.go).
//
// Wire reviewed on 2026-09-30:
// https://developers.openai.com/api/docs/guides/voice-websockets,
// https://developers.openai.com/api/reference/resources/realtime/client-events,
// https://developers.openai.com/api/reference/resources/realtime/server-events,
// https://docs.x.ai/developers/model-capabilities/audio/speech-to-speech,
// https://docs.x.ai/voice-realtime.ws.json.
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
// handshake to the provider confirming the session's configuration.
const openTimeout = 20 * time.Second

// maxUpstreamEventBytes bounds one server event. The largest are
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
	// Now is the clock a session billed by its duration is measured with;
	// nil is time.Now, whose readings carry the monotonic clock.
	Now func() time.Time
}

type Adapter struct {
	dialect     *dialect
	client      *http.Client
	credentials *provider.KeyPool
	now         func() time.Time
}

// New builds the OpenAI Realtime adapter. Its origin is not configurable: the
// slug is bound to OpenAI's own API root, and the session endpoint to OpenAI's
// own WebSocket.
func New(config Config) (*Adapter, error) { return buildAdapter(openAIDialect, config) }

// NewXAI builds the xAI Voice Agent adapter, bound to xAI's own API root and
// session endpoint exactly as New is to OpenAI's.
func NewXAI(config Config) (*Adapter, error) { return buildAdapter(xAIDialect, config) }

func buildAdapter(d *dialect, config Config) (*Adapter, error) {
	if err := providerconfig.ValidateEndpointIdentity(d.slug, d.baseURL); err != nil {
		return nil, err
	}
	pool, err := provider.NewKeyPool(d.slug, config.Declarations, config.Keys, provider.QuotaHeaders{})
	if err != nil {
		return nil, err
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Adapter{dialect: d, client: provider.RefuseRedirects(config.HTTPClient), credentials: pool, now: now}, nil
}

func (a *Adapter) Provider() contract.ProviderSlug        { return a.dialect.slug }
func (a *Adapter) PlatformCredentials() *provider.KeyPool { return a.credentials }

// RealtimeSessionKinds implements provider.RealtimeAdapter: conversations only.
func (a *Adapter) RealtimeSessionKinds() []contract.RealtimeSessionKind {
	return providerconfig.RealtimeSessionKinds(a.dialect.slug, a.dialect.protocol)
}

func (a *Adapter) refuseOpen(request provider.RealtimeOpenRequest) (*sessionConfig, error) {
	d := a.dialect
	if request.Route.Provider != d.slug || request.Route.UpstreamModelID == "" {
		return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "authorizedRoutes", Detail: "an exact " + d.name + " Realtime deployment is required"}
	}
	if !provider.OpensRealtime(a, request.Kind) {
		return nil, provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: "kind",
			Detail: fmt.Sprintf("the %s deployment holds %s sessions only; %s's %s sessions are not served", d.slug, contract.RealtimeConversation, d.name, request.Kind)}
	}
	return d.wireSession(request.Config)
}

// Open dials the provider, waits for the session it creates, configures it,
// and returns it once the provider has confirmed the configuration. That is what "open"
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
	d := a.dialect
	endpoint := d.sessionURL + "?model=" + url.QueryEscape(request.Route.UpstreamModelID)
	call := &provider.Call{RequestID: request.RequestID, Route: request.Route, Method: http.MethodGet, URL: endpoint}

	// handshakeAt is when the provider accepted the handshake of the attempt
	// that opened: a provider billing session time starts its clock there.
	var handshakeAt time.Time
	conn, key, err := provider.WalkAttempts(ctx, credentials, call, func(ctx context.Context, key provider.Key) (*websocket.Conn, provider.CredentialedAttempt) {
		attempt, dialled := a.dial(ctx, endpoint, key)
		if dialled == nil {
			return nil, attempt
		}
		handshakeAt = a.now()
		if failure := d.configure(ctx, dialled, update, key); failure != nil {
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
	upstream := newSession(d, conn, request, key, request.Config.InputAudioFormat)
	if d.tokenUsage {
		return upstream, opened, nil
	}
	// A provider that bills by what Kaana can measure, not by the tokens it
	// reports, is metered (meter.go): by its wall clock when the session's turn
	// detection is the one the provider bills by duration, else by its audio.
	clock := d.sessionClock != "" && request.Config.TurnDetection.Type == d.sessionClock
	upstream.meter = newMeter(request.Config, clock, handshakeAt, a.now)
	return meteredSession{upstream}, opened, nil
}

// dial performs the WebSocket handshake with one credential. A handshake the
// provider refused is classified from its status and body like any other
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
			return provider.CredentialedAttempt{Header: response.Header, Failure: a.dialect.classifyRefusal(response.StatusCode, response.Header, body, key)}, nil
		}
		return provider.CredentialedAttempt{Failure: a.dialect.transport(ctx, err), Transport: true}, nil
	}
	conn.SetReadLimit(maxUpstreamEventBytes)
	return provider.CredentialedAttempt{Header: response.Header}, conn
}

// transportFailure marks a configuration exchange that ended without the
// provider answering it, which says nothing about the credential.
type transportFailure struct{ failure error }

func (t transportFailure) Error() string { return t.failure.Error() }

// configure waits for session.created, sends the configuration and waits for
// the provider to confirm it. An error event in between is the provider's own
// answer about this session on this credential.
func (d *dialect) configure(ctx context.Context, conn *websocket.Conn, update []byte, key provider.Key) error {
	openContext, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	created := false
	for {
		kind, data, err := conn.Read(openContext)
		if err != nil {
			return transportFailure{d.transport(openContext, err)}
		}
		if kind != websocket.MessageText {
			return transportFailure{d.invalidEvent()}
		}
		var event serverEvent
		if json.Unmarshal(data, &event) != nil {
			return transportFailure{d.invalidEvent()}
		}
		switch event.Type {
		case "error":
			if event.Error == nil {
				return transportFailure{d.invalidEvent()}
			}
			return d.classifyEvent(d, *event.Error, key)
		case "session.created":
			if created {
				return transportFailure{d.invalidEvent()}
			}
			created = true
			if err := conn.Write(openContext, websocket.MessageText, update); err != nil {
				return transportFailure{d.transport(openContext, err)}
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

func (d *dialect) invalidEvent() error {
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown,
		Detail: d.name + " sent a Realtime event this adapter cannot read", Passthrough: &contract.ProviderErrorPassthrough{Provider: d.slug}}
}

func (d *dialect) transport(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	passthrough := &contract.ProviderErrorPassthrough{Provider: d.slug}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return provider.ErrUpstream{Code: contract.CodeProviderTimeout, Category: contract.UpstreamTimeout, Detail: d.name + "'s Realtime session did not answer in time", Passthrough: passthrough}
	}
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamServerError, Detail: "the connection to " + d.name + "'s Realtime API failed", Passthrough: passthrough}
}

// classifyRefusal reads a refused handshake by status and the provider's own
// error type and code, never by message prose
// (https://developers.openai.com/api/docs/guides/error-codes). The message is
// bounded, stripped of this key by exact match, and redacted.
//
// xAI documents no status table for the WebSocket handshake. Probed on
// 2026-09-30 without a credential it answered 401, and with an invalid one 400
// `{"code":"Client specified an invalid argument","error":"Incorrect API key
// provided..."}`: a gRPC status description, not a credential-specific code,
// so that 400 is read as the invalid request it is labelled and the key is not
// retired on prose (docs/realtime.md).
func (d *dialect) classifyRefusal(status int, header http.Header, body []byte, key provider.Key) error {
	reported := d.refusalBody(body)
	passthrough := d.passthroughFor(reported, key)
	passthrough.Status = &status
	code := ""
	if reported.Code != nil {
		code = *reported.Code
	}
	failure := provider.ErrUpstream{Passthrough: passthrough}
	switch {
	case status == http.StatusPaymentRequired,
		status == http.StatusTooManyRequests && quotaExhausted(reported.Type, code):
		failure.Code, failure.Category = contract.CodeProviderBillingRefused, contract.UpstreamQuota
		failure.Detail = "the platform's own " + d.name + " account cannot be billed for this session"
	case status == http.StatusTooManyRequests:
		failure.Code, failure.Category = contract.CodeRateLimited, contract.UpstreamRateLimit
		failure.Detail = d.name + " rate-limited this session"
		failure.RetryAfterMs = provider.RetryAfterMs(header)
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		failure.Code, failure.Category = contract.CodeProviderCredentialInvalid, contract.UpstreamAuthentication
		failure.Detail = d.name + " refused the platform's credential for this route"
	case status == http.StatusNotFound:
		failure.Code, failure.Category = contract.CodeModelNotFound, contract.UpstreamInvalidReq
		failure.Detail = d.name + " does not serve the model this route names"
	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout:
		failure.Code, failure.Category = contract.CodeProviderTimeout, contract.UpstreamTimeout
		failure.Detail = d.name + " timed out"
	case status == http.StatusServiceUnavailable:
		failure.Code, failure.Category = contract.CodeProviderOverloaded, contract.UpstreamOverloaded
		failure.Detail = d.name + " is overloaded"
		failure.RetryAfterMs = provider.RetryAfterMs(header)
	case status >= 500:
		failure.Code, failure.Category = contract.CodeProviderError, contract.UpstreamServerError
		failure.Detail = d.name + " returned an internal error"
	default:
		failure.Code, failure.Category = contract.CodeInvalidRequest, contract.UpstreamInvalidReq
		failure.Detail = d.name + " refused the Realtime session"
	}
	return provider.CustomerCredentialFailure(key, failure)
}

// openAIRefusalBody reads OpenAI's `{"error": {type, code, message}}`.
func openAIRefusalBody(body []byte) wireError {
	var parsed struct {
		Error wireError `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	return parsed.Error
}

// xAIRefusalBody reads the body xAI's handshake answered with when probed:
// `{"code": "<status description>", "error": "<message>"}`, two strings. The
// code is carried as the passthrough's code and classifies nothing.
func xAIRefusalBody(body []byte) wireError {
	var parsed struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	return wireError{Type: parsed.Code, Message: parsed.Error}
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

// classifyOpenAIEvent reads an in-band error event by OpenAI's type and code.
// Most are refusals of one client event ("Most errors are recoverable and the
// session will stay open"); the account and credential vocabulary is the same
// one the HTTP API uses, and a server_error is OpenAI's own failure.
func classifyOpenAIEvent(d *dialect, value wireError, key provider.Key) error {
	code := ""
	if value.Code != nil {
		code = *value.Code
	}
	failure := provider.ErrUpstream{Passthrough: d.passthroughFor(value, key)}
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

// classifyXAIEvent reads an in-band error event by xAI's documented types:
// `invalid_request_error` and `invalid_event` refuse one client event and the
// session stays open ("Most errors are recoverable"); `internal_error` is
// xAI's own failure; `timeout` (inactivity) and `max_duration` (its 120-minute
// session ceiling) end the session. xAI documents `code` as "same as type" and
// then gives a code beside a type in its own example, so the type decides.
func classifyXAIEvent(d *dialect, value wireError, key provider.Key) error {
	failure := provider.ErrUpstream{Passthrough: d.passthroughFor(value, key)}
	switch value.Type {
	case "invalid_request_error", "invalid_event":
		failure.Code, failure.Category = contract.CodeInvalidRequest, contract.UpstreamInvalidReq
		failure.Detail = "xAI refused a Realtime event"
	case "internal_error":
		failure.Code, failure.Category = contract.CodeProviderError, contract.UpstreamServerError
		failure.Detail = "xAI's Realtime session failed"
	case "timeout", "max_duration":
		failure.Code, failure.Category = contract.CodeProviderTimeout, contract.UpstreamTimeout
		failure.Detail = "xAI ended the Realtime session at its own inactivity or duration limit"
	default:
		failure.Code, failure.Category = contract.CodeProviderError, contract.UpstreamUnknown
		failure.Detail = "xAI reported a Realtime error this adapter does not classify"
	}
	return provider.CustomerCredentialFailure(key, failure)
}

func (d *dialect) passthroughFor(value wireError, key provider.Key) *contract.ProviderErrorPassthrough {
	passthrough := &contract.ProviderErrorPassthrough{Provider: d.slug}
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
	health := provider.Health{Provider: a.dialect.slug, CheckedAt: contract.NewTimestamp(now), Credentials: &pool, Status: provider.HealthDegraded,
		Detail: "credential configured; realtime entitlement requires a signed canary session"}
	if !a.credentials.Configured() {
		health.Status, health.Detail = provider.HealthUnconfigured, "no "+a.dialect.name+" Realtime credential configured"
	} else if pool.Usable == 0 {
		health.Status, health.Detail = provider.HealthUnavailable, "no usable "+a.dialect.name+" Realtime credential"
	}
	return health
}
