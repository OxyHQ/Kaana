// Package openailive opens OpenAI GPT-Live sessions over a server-to-server
// WebSocket: the native adapter of Kaana's `live` session kind (Kaana ADR 0012
// §2). GPT-Live is a voice that listens, speaks and decides when a turn ends,
// and hands every turn to its client ("client delegation"); Kaana relays that
// turn to the caller's backend as `delegation.requested` and speaks its
// `delegation.reply` back through the voice.
//
// It is a provider.RealtimeAdapter under its own slug, `openai-live`, bound to
// OpenAI's own origin: GPT-Live is neither a request nor the Realtime protocol
// (internal/provider/openairealtime), and a slug resolves to exactly one
// adapter.
//
// Wire reviewed on 2026-11-14:
// https://developers.openai.com/api/docs/guides/live-conversations,
// https://developers.openai.com/api/docs/guides/live-delegation,
// https://developers.openai.com/api/docs/guides/voice-websockets?api=live,
// https://developers.openai.com/api/reference/resources/live/primary-websocket,
// https://developers.openai.com/api/docs/models/gpt-live-1.
package openailive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/openaierror"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// Slug is the provider name OpenAI's GPT-Live sessions are served under.
const Slug contract.ProviderSlug = "openai-live"

// openTimeout bounds each stage of opening: the handshake, and from the
// accepted handshake to OpenAI's `session.started`.
const openTimeout = 20 * time.Second

// maxUpstreamEventBytes bounds one server event. The largest are
// `session.started` and `session.closed`, which carry the session's resolved
// configuration (instructions of up to 16 384 tokens) but never audio beyond
// one output delta.
const maxUpstreamEventBytes = 16 << 20

// startEventID names the session.start, so the error it causes and the
// session.started that answers it can be told apart from later ones.
const startEventID = "kaana-session-start"

// vocabulary names this adapter's sessions in a failure's detail.
var vocabulary = openaierror.Session{Provider: "OpenAI", API: "GPT-Live"}

type Config struct {
	Declarations []provider.KeyDeclaration
	Keys         provider.KeyPolicy
	HTTPClient   *http.Client
	// Now is the clock a session that ends without OpenAI's final usage is
	// measured with; nil is time.Now, whose readings carry the monotonic clock.
	Now func() time.Time
}

type Adapter struct {
	client      *http.Client
	credentials *provider.KeyPool
	now         func() time.Time
	// outputGap is how long the voice is silent before what it said is one
	// finished utterance (outputTurnGap; tests shorten it).
	outputGap time.Duration
	// closeTimeout bounds the wait for OpenAI's session.closed at settlement.
	closeTimeout time.Duration
}

// New builds the GPT-Live adapter. Its origin is not configurable: the slug is
// bound to OpenAI's own API root, and the session endpoint to GPT-Live's own
// WebSocket.
func New(config Config) (*Adapter, error) {
	if err := providerconfig.ValidateEndpointIdentity(Slug, providerconfig.OpenAILiveBaseURL); err != nil {
		return nil, err
	}
	pool, err := provider.NewKeyPool(Slug, config.Declarations, config.Keys, provider.QuotaHeaders{})
	if err != nil {
		return nil, err
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Adapter{client: provider.RefuseRedirects(config.HTTPClient), credentials: pool, now: now,
		outputGap: outputTurnGap, closeTimeout: closeTimeout}, nil
}

func (a *Adapter) Provider() contract.ProviderSlug        { return Slug }
func (a *Adapter) PlatformCredentials() *provider.KeyPool { return a.credentials }

// RealtimeSessionKinds implements provider.RealtimeAdapter: `live` only.
func (a *Adapter) RealtimeSessionKinds() []contract.RealtimeSessionKind {
	return providerconfig.RealtimeSessionKinds(Slug, providerconfig.ProtocolOpenAILive)
}

// statement is what the protocol serves: the one reviewed statement the
// publisher writes into the inventory and the catalogue publishes.
func statement() providerconfig.RealtimeStatement {
	statement, _ := providerconfig.RealtimeCapabilities(Slug, providerconfig.ProtocolOpenAILive)
	return statement
}

// refuseOpen refuses, before anything is dialled, a session this wire cannot
// hold. The contract already requires a live session's voice, one audio
// format both ways, a detected turn and its delegation; what is left is what
// GPT-Live itself names.
func (a *Adapter) refuseOpen(request provider.RealtimeOpenRequest) error {
	if request.Route.Provider != Slug || request.Route.UpstreamModelID == "" {
		return provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "authorizedRoutes", Detail: "an exact OpenAI GPT-Live deployment is required"}
	}
	if !provider.OpensRealtime(a, request.Kind) {
		return provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: "kind",
			Detail: fmt.Sprintf("the %s deployment holds %s sessions only; GPT-Live's %s sessions are not served", Slug, contract.RealtimeLive, request.Kind)}
	}
	config := request.Config
	if config.Delegation == nil || config.Voice == nil || config.OutputAudioFormat == nil || *config.OutputAudioFormat != config.InputAudioFormat {
		return provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "config", Detail: "a live session names its voice, one audio format both ways and its delegation"}
	}
	if issues := statement().Parameters.Unservable("config", config.Fields()); len(issues) > 0 {
		return provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: issues[0].Param, Detail: issues[0].Message}
	}
	if !slices.ContainsFunc(statement().Voices, func(voice providerconfig.Voice) bool { return voice.ID == *config.Voice }) {
		return provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "config.voice", Detail: "this model has no voice " + *config.Voice}
	}
	if _, err := wireFormat(config.InputAudioFormat); err != nil {
		return err
	}
	return nil
}

// Open dials GPT-Live, starts the session and returns it once OpenAI has
// answered `session.start` with `session.started`: only then is audio sent
// (voice-websockets guide, "Wait for session.started before sending audio or
// commands"). A session that dialled but whose start OpenAI refused never
// opened, and the credential rules read that refusal exactly as they read a
// refused request.
func (a *Adapter) Open(ctx context.Context, request provider.RealtimeOpenRequest, credentials *provider.KeyPool) (provider.RealtimeUpstream, provider.RealtimeOpened, error) {
	var opened provider.RealtimeOpened
	if err := a.refuseOpen(request); err != nil {
		return nil, opened, err
	}
	start, err := json.Marshal(startEvent(request))
	if err != nil {
		return nil, opened, fmt.Errorf("openailive: encoding session.start: %w", err)
	}
	if credentials == nil {
		credentials = a.credentials
	}
	call := &provider.Call{RequestID: request.RequestID, Route: request.Route, CredentialAttempts: request.CredentialAttempts,
		Method: http.MethodGet, URL: providerconfig.OpenAILiveSessionURL}

	// handshakeAt is when OpenAI accepted the handshake of the attempt that
	// opened: the earliest instant its session can exist, so the wall clock a
	// session without OpenAI's final usage settles never under-counts it.
	var handshakeAt time.Time
	conn, key, err := provider.WalkAttempts(ctx, credentials, call, func(ctx context.Context, key provider.Key) (*websocket.Conn, provider.CredentialedAttempt) {
		attempt, dialled := a.dial(ctx, key)
		if dialled == nil {
			return nil, attempt
		}
		handshakeAt = a.now()
		if failure := startSession(ctx, dialled, start, key); failure != nil {
			_ = dialled.CloseNow()
			attempt.Failure = failure
			var transport transportFailure
			if errors.As(failure, &transport) {
				attempt.Failure, attempt.Transport = transport.failure, true
			}
			return nil, attempt
		}
		attempt.Accepted = true
		return dialled, attempt
	})
	opened.KeyID, opened.KeyClass = key.ID, key.Class
	if err != nil {
		return nil, opened, err
	}
	return newSession(ctx, a, conn, request, key, handshakeAt), opened, nil
}

// dial performs the WebSocket handshake with one credential: `Authorization:
// Bearer` and no query parameters. A handshake OpenAI refused is classified
// from its status and body; one that never got an answer is a transport
// failure.
func (a *Adapter) dial(ctx context.Context, key provider.Key) (provider.CredentialedAttempt, *websocket.Conn) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+key.Secret())
	dialContext, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	conn, response, err := websocket.Dial(dialContext, providerconfig.OpenAILiveSessionURL, &websocket.DialOptions{HTTPClient: a.client, HTTPHeader: header})
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

// transportFailure marks a start that ended without OpenAI answering it, which
// says nothing about the credential.
type transportFailure struct{ failure error }

func (t transportFailure) Error() string { return t.failure.Error() }

// startSession sends session.start as the connection's first frame and waits
// for session.started. An `error` before it is OpenAI's own answer about this
// session on this credential.
func startSession(ctx context.Context, conn *websocket.Conn, start []byte, key provider.Key) error {
	startContext, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	if err := conn.Write(startContext, websocket.MessageText, start); err != nil {
		return transportFailure{transport(startContext, err)}
	}
	for {
		kind, data, err := conn.Read(startContext)
		if err != nil {
			return transportFailure{transport(startContext, err)}
		}
		if kind != websocket.MessageText {
			return transportFailure{invalidEvent("", "a binary frame")}
		}
		event, err := decode(data)
		if err != nil {
			return transportFailure{err}
		}
		switch event.Type {
		case "error":
			if event.Error == nil {
				return transportFailure{invalidEvent(event.Type, "error is absent")}
			}
			return classifyEvent(*event.Error, key)
		case "session.started":
			return nil
		}
		// Nothing else is documented before session.started; anything that
		// arrives carries nothing the contract names.
	}
}

// decode reads one server event. An event this adapter cannot read is refused
// naming its type and the field that did not fit, never a value OpenAI sent.
func decode(data []byte) (serverEvent, error) {
	var event serverEvent
	err := json.Unmarshal(data, &event)
	if err == nil {
		return event, nil
	}
	var typeError *json.UnmarshalTypeError
	if !errors.As(err, &typeError) {
		return event, invalidEvent("", "not a JSON object")
	}
	var named struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(data, &named)
	kind, _, _ := strings.Cut(typeError.Value, " ")
	return event, invalidEvent(named.Type, typeError.Field+" is a JSON "+kind)
}

// maxLoggedEventType bounds an event type named in an error.
const maxLoggedEventType = 64

// invalidEvent is a server event this adapter cannot read. Its detail names
// the event's type (kept only if it looks like one) and what did not fit in
// this adapter's own wire vocabulary, so the next mismatch is diagnosable from
// the error alone, which reaches the session's log line and the customer.
func invalidEvent(eventType, problem string) error {
	detail := "OpenAI sent a GPT-Live event this adapter cannot read"
	if len(eventType) > maxLoggedEventType || strings.IndexFunc(eventType, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '_'
	}) >= 0 {
		eventType = "an unrecognisable type"
	}
	switch {
	case eventType != "" && problem != "":
		detail += " (" + eventType + ": " + problem + ")"
	case eventType != "":
		detail += " (" + eventType + ")"
	case problem != "":
		detail += " (" + problem + ")"
	}
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown,
		Detail: detail, Passthrough: &contract.ProviderErrorPassthrough{Provider: Slug}}
}

func transport(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	passthrough := &contract.ProviderErrorPassthrough{Provider: Slug}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return provider.ErrUpstream{Code: contract.CodeProviderTimeout, Category: contract.UpstreamTimeout, Detail: "OpenAI's GPT-Live session did not answer in time", Passthrough: passthrough}
	}
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamServerError, Detail: "the connection to OpenAI's GPT-Live API failed", Passthrough: passthrough}
}

// classifyRefusal reads a refused handshake by status and OpenAI's own error
// type and code, never by prose (openaierror.Handshake). OpenAI documents no
// status table for the GPT-Live handshake (UNVERIFIED: that it answers a
// refused key, an unfunded account or a throttle with the statuses and the
// `{"error": {type, code, message}}` body its HTTP API uses; the shared table
// reads them that way).
func classifyRefusal(status int, header http.Header, body []byte, key provider.Key) error {
	var parsed struct {
		Error wireError `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	return openaierror.Handshake(vocabulary, status, header, parsed.Error.Type, parsed.Error.code(), passthroughFor(parsed.Error, key), key)
}

// classifyEvent reads an in-band `error {error: {type, code, message, param,
// client_event_id}}` by OpenAI's type and code (openaierror.InBand). The
// reference gives `invalid_request_error` / `unknown_parameter` as its only
// example and enumerates neither field (UNVERIFIED: the rest of GPT-Live's
// vocabulary; an unknown type is OpenAI's unclassified failure).
func classifyEvent(value wireError, key provider.Key) error {
	return openaierror.InBand(vocabulary, value.Type, value.code(), passthroughFor(value, key), key)
}

func passthroughFor(value wireError, key provider.Key) *contract.ProviderErrorPassthrough {
	return openaierror.Passthrough(Slug, value.Type, value.Message, key)
}

// Health never opens a session: a probe that held one would be a charge
// nobody asked for. Entitlement is proved by a signed canary session.
func (a *Adapter) Health(_ context.Context) provider.Health {
	now := time.Now()
	pool := a.credentials.Projection(now)
	health := provider.Health{Provider: Slug, CheckedAt: contract.NewTimestamp(now), Credentials: &pool, Status: provider.HealthDegraded,
		Detail: "credential configured; GPT-Live entitlement requires a signed canary session"}
	if !a.credentials.Configured() {
		health.Status, health.Detail = provider.HealthUnconfigured, "no OpenAI GPT-Live credential configured"
	} else if pool.Usable == 0 {
		health.Status, health.Detail = provider.HealthUnavailable, "no usable OpenAI GPT-Live credential"
	}
	return health
}
