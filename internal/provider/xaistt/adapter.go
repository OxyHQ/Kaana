// Package xaistt opens transcription sessions on xAI's streaming
// speech-to-text WebSocket (Kaana ADR 0006). It is a provider.RealtimeAdapter
// under its own slug, `xai-stt`, bound to xAI's own origin: the wire is
// neither the OpenAI-compatible requests of `xai` nor the Voice Agent of
// `xai-realtime`, and a slug resolves to exactly one adapter.
//
// Wire reviewed on 2026-10-28:
// https://docs.x.ai/developers/model-capabilities/audio/speech-to-text
// (`wss://api.x.ai/v1/stt`, query-parameter configuration, raw binary audio
// frames, `{"type":"finalize"}` and `{"type":"audio.done"}`,
// `transcript.created`, `transcript.partial` with `is_final` and
// `speech_final`, `transcript.done`, `error`) and
// https://docs.x.ai/developers/models ("Speech to Text $0.20 / hr
// (Streaming)").
package xaistt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// Slug is the provider name xAI's streaming speech-to-text is served under.
const Slug contract.ProviderSlug = "xai-stt"

// openTimeout bounds opening: from dialling to xAI's transcript.created,
// which xAI says to wait for before sending audio.
const openTimeout = 20 * time.Second

// maxUpstreamEventBytes bounds one server event: a partial or final
// transcript of one utterance with its words.
const maxUpstreamEventBytes = 4 << 20

// pushToTalkEndpointingMs is xAI's longest `endpointing`: with turn detection
// `none` the caller commits each utterance (finalize), and xAI ends one on its
// own only after five seconds of silence.
const pushToTalkEndpointingMs = 5000

// Config wires the adapter.
type Config struct {
	Declarations []provider.KeyDeclaration
	Keys         provider.KeyPolicy
	HTTPClient   *http.Client
	// Now is the clock a session is metered by; nil is time.Now, whose
	// readings carry the monotonic clock.
	Now func() time.Time
}

// Adapter opens xAI streaming speech-to-text sessions.
type Adapter struct {
	client      *http.Client
	credentials *provider.KeyPool
	statement   providerconfig.RealtimeStatement
	now         func() time.Time
}

// New builds the adapter. Its origin is not configurable.
func New(config Config) (*Adapter, error) {
	if err := providerconfig.ValidateEndpointIdentity(Slug, providerconfig.XAISTTBaseURL); err != nil {
		return nil, err
	}
	statement, stated := providerconfig.RealtimeCapabilities(Slug, providerconfig.ProtocolXAISTT)
	if !stated {
		return nil, errors.New("xaistt: no reviewed statement for xAI streaming speech-to-text")
	}
	pool, err := provider.NewKeyPool(Slug, config.Declarations, config.Keys, provider.QuotaHeaders{})
	if err != nil {
		return nil, err
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Adapter{client: provider.RefuseRedirects(config.HTTPClient), credentials: pool, statement: statement, now: now}, nil
}

func (a *Adapter) Provider() contract.ProviderSlug        { return Slug }
func (a *Adapter) PlatformCredentials() *provider.KeyPool { return a.credentials }

// RealtimeSessionKinds implements provider.RealtimeAdapter: transcription only.
func (a *Adapter) RealtimeSessionKinds() []contract.RealtimeSessionKind {
	return providerconfig.RealtimeSessionKinds(Slug, providerconfig.ProtocolXAISTT)
}

// endpoint is the session URL for one open: every setting xAI takes is a
// query parameter, and nothing is sent after the handshake but audio and the
// two control messages.
func (a *Adapter) endpoint(request provider.RealtimeOpenRequest) (string, error) {
	if request.Route.Provider != Slug || request.Route.UpstreamModelID == "" {
		return "", provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "authorizedRoutes", Detail: "an exact xAI speech-to-text deployment is required"}
	}
	if !provider.OpensRealtime(a, request.Kind) {
		return "", provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: "kind",
			Detail: fmt.Sprintf("the %s deployment holds %s sessions only", Slug, contract.RealtimeTranscription)}
	}
	config := request.Config
	query := url.Values{}
	query.Set("model", request.Route.UpstreamModelID)
	switch config.InputAudioFormat {
	case contract.RealtimePCM16:
		query.Set("encoding", "pcm")
		query.Set("sample_rate", "24000")
	case contract.RealtimeULaw:
		query.Set("encoding", "mulaw")
		query.Set("sample_rate", "8000")
	case contract.RealtimeALaw:
		query.Set("encoding", "alaw")
		query.Set("sample_rate", "8000")
	default:
		return "", provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "config.inputAudioFormat", Detail: "xAI's speech-to-text does not take this encoding"}
	}
	detection := config.TurnDetection
	switch detection.Type {
	case contract.TurnDetectionNone:
		query.Set("endpointing", strconv.Itoa(pushToTalkEndpointingMs))
	case contract.TurnDetectionServerVAD, contract.TurnDetectionSemanticVAD:
		// semantic_vad runs as server_vad, as on every wire without semantic
		// detection (contract.RealtimeParameters.WireTurnDetection, Kaana ADR
		// 0003): it names no silence, so Kaana's default ends an utterance.
		silence := contract.DefaultSilenceDurationMs
		if detection.SilenceDurationMs != nil {
			silence = *detection.SilenceDurationMs
		}
		query.Set("endpointing", strconv.Itoa(min(max(silence, 0), pushToTalkEndpointingMs)))
		if detection.Threshold != nil {
			query.Set("vad_threshold", strconv.FormatFloat(*detection.Threshold, 'f', -1, 64))
		}
	default:
		return "", provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "config.turnDetection",
			Detail: "xAI's speech-to-text ends an utterance on silence or on commit"}
	}
	transcription := config.InputAudioTranscription
	if transcription.WantsInterim() {
		query.Set("interim_results", "true")
	}
	if transcription != nil && transcription.Language != nil {
		if language, sent := a.statement.LanguageFor(*transcription.Language); sent {
			query.Set("language", language)
		}
	}
	return providerconfig.XAISTTSessionURL + "?" + query.Encode(), nil
}

// Open dials xAI and returns the session once xAI has said it is ready
// (`transcript.created`). That is what "open" means for failover: a session
// whose handshake or readiness xAI refused never opened.
func (a *Adapter) Open(ctx context.Context, request provider.RealtimeOpenRequest, credentials *provider.KeyPool) (provider.RealtimeUpstream, provider.RealtimeOpened, error) {
	var opened provider.RealtimeOpened
	endpoint, err := a.endpoint(request)
	if err != nil {
		return nil, opened, err
	}
	if credentials == nil {
		credentials = a.credentials
	}
	call := &provider.Call{RequestID: request.RequestID, Route: request.Route, CredentialAttempts: request.CredentialAttempts, Method: http.MethodGet, URL: endpoint}
	var readyAt time.Time
	conn, key, err := provider.WalkAttempts(ctx, credentials, call, func(ctx context.Context, key provider.Key) (*websocket.Conn, provider.CredentialedAttempt) {
		attempt, dialled := a.dial(ctx, endpoint, key)
		if dialled == nil {
			return nil, attempt
		}
		if failure := awaitReady(ctx, dialled, key); failure != nil {
			_ = dialled.CloseNow()
			attempt.Failure = failure
			var transport transportFailure
			if errors.As(failure, &transport) {
				attempt.Failure, attempt.Transport = transport.failure, true
			}
			return nil, attempt
		}
		readyAt = a.now()
		attempt.Accepted = true
		return dialled, attempt
	})
	opened.KeyID, opened.KeyClass = key.ID, key.Class
	if err != nil {
		return nil, opened, err
	}
	return newSession(conn, request, key, readyAt, a.now), opened, nil
}

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

// transportFailure marks an open that ended without xAI answering it, which
// says nothing about the credential.
type transportFailure struct{ failure error }

func (t transportFailure) Error() string { return t.failure.Error() }

// awaitReady reads until transcript.created. An error event before it is
// xAI's own answer about this session on this credential.
func awaitReady(ctx context.Context, conn *websocket.Conn, key provider.Key) error {
	openContext, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	for {
		kind, data, err := conn.Read(openContext)
		if err != nil {
			return transportFailure{transport(openContext, err)}
		}
		if kind != websocket.MessageText {
			return transportFailure{invalidEvent("", "a binary frame")}
		}
		event, err := decode(data)
		if err != nil {
			return transportFailure{err}
		}
		switch event.Type {
		case "transcript.created":
			return nil
		case "error":
			return classifyEvent(event.Message, key)
		}
	}
}

// serverEvent is every server event xAI documents, in one shape.
type serverEvent struct {
	Type        string   `json:"type"`
	Text        string   `json:"text"`
	IsFinal     bool     `json:"is_final"`
	SpeechFinal bool     `json:"speech_final"`
	Duration    *float64 `json:"duration"`
	Message     string   `json:"message"`
}

func decode(data []byte) (serverEvent, error) {
	var event serverEvent
	if err := json.Unmarshal(data, &event); err != nil {
		var named struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(data, &named)
		var typeError *json.UnmarshalTypeError
		if errors.As(err, &typeError) {
			return event, invalidEvent(named.Type, typeError.Field+" has the wrong JSON type")
		}
		return event, invalidEvent(named.Type, "not a JSON object")
	}
	return event, nil
}

// maxLoggedEventType bounds an event type named in an error.
const maxLoggedEventType = 64

// invalidEvent is a server event this adapter cannot read. Its detail names
// the event's type and what did not fit, never a value xAI sent.
func invalidEvent(eventType, problem string) error {
	detail := "xAI sent a speech-to-text event this adapter cannot read"
	if len(eventType) > maxLoggedEventType {
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
		return provider.ErrUpstream{Code: contract.CodeProviderTimeout, Category: contract.UpstreamTimeout, Detail: "xAI's speech-to-text session did not answer in time", Passthrough: passthrough}
	}
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamServerError, Detail: "the connection to xAI's speech-to-text API failed", Passthrough: passthrough}
}

// classifyRefusal reads a refused handshake by its status alone. xAI
// documents no status table for this WebSocket; its REST speech-to-text
// answers 429 to back off, and its handshakes answered 401 without a
// credential and 400 with an invalid one when probed (docs/realtime.md). The
// body's `code` and `error` are carried, bounded and redacted, and classify
// nothing: they are prose, not a type.
func classifyRefusal(status int, header http.Header, body []byte, key provider.Key) error {
	var reported struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &reported)
	passthrough := passthroughFor(reported.Code, reported.Error, key)
	passthrough.Status = &status
	failure := provider.ErrUpstream{Passthrough: passthrough}
	switch {
	case status == http.StatusPaymentRequired:
		failure.Code, failure.Category = contract.CodeProviderBillingRefused, contract.UpstreamQuota
		failure.Detail = "the platform's own xAI account cannot be billed for this session"
	case status == http.StatusTooManyRequests:
		failure.Code, failure.Category = contract.CodeRateLimited, contract.UpstreamRateLimit
		failure.Detail = "xAI rate-limited this session"
		failure.RetryAfterMs = provider.RetryAfterMs(header)
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		failure.Code, failure.Category = contract.CodeProviderCredentialInvalid, contract.UpstreamAuthentication
		failure.Detail = "xAI refused the platform's credential for this route"
	case status == http.StatusNotFound:
		failure.Code, failure.Category = contract.CodeModelNotFound, contract.UpstreamInvalidReq
		failure.Detail = "xAI does not serve the model this route names"
	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout:
		failure.Code, failure.Category = contract.CodeProviderTimeout, contract.UpstreamTimeout
		failure.Detail = "xAI timed out"
	case status == http.StatusServiceUnavailable:
		failure.Code, failure.Category = contract.CodeProviderOverloaded, contract.UpstreamOverloaded
		failure.Detail = "xAI is overloaded"
		failure.RetryAfterMs = provider.RetryAfterMs(header)
	case status >= 500:
		failure.Code, failure.Category = contract.CodeProviderError, contract.UpstreamServerError
		failure.Detail = "xAI returned an internal error"
	default:
		failure.Code, failure.Category = contract.CodeInvalidRequest, contract.UpstreamInvalidReq
		failure.Detail = "xAI refused the speech-to-text session"
	}
	return provider.CustomerCredentialFailure(key, failure)
}

// classifyEvent reads an in-band `error`. xAI documents only its `message`,
// so nothing classifies it: it is xAI's failure of unknown cause, and the
// message is carried bounded and redacted. Most such errors close the
// connection (xAI: "Most errors, including undecodable audio frames, close
// the connection").
func classifyEvent(message string, key provider.Key) error {
	failure := provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown,
		Detail: "xAI's speech-to-text session reported an error", Passthrough: passthroughFor("", message, key)}
	return provider.CustomerCredentialFailure(key, failure)
}

func passthroughFor(code, message string, key provider.Key) *contract.ProviderErrorPassthrough {
	passthrough := &contract.ProviderErrorPassthrough{Provider: Slug}
	if code != "" {
		value := contract.SafeErrorText(provider.RedactSecret(code, key.Secret()))
		passthrough.Code = &value
	}
	if message != "" {
		value := contract.SafeErrorText(provider.RedactSecret(message, key.Secret()))
		passthrough.Message = &value
	}
	return passthrough
}

// Health never opens a session: a probe that held one would be a charge
// nobody asked for. Entitlement is proved by a signed canary session.
func (a *Adapter) Health(_ context.Context) provider.Health {
	now := time.Now()
	pool := a.credentials.Projection(now)
	health := provider.Health{Provider: Slug, CheckedAt: contract.NewTimestamp(now), Credentials: &pool, Status: provider.HealthDegraded,
		Detail: "credential configured; speech-to-text entitlement requires a signed canary session"}
	if !a.credentials.Configured() {
		health.Status, health.Detail = provider.HealthUnconfigured, "no xAI speech-to-text credential configured"
	} else if pool.Usable == 0 {
		health.Status, health.Detail = provider.HealthUnavailable, "no usable xAI speech-to-text credential"
	}
	return health
}
