// Package openaiaudio implements OpenAI's audio APIs: synchronous file
// transcription, and Chat Completions answered aloud (chat.go).
//
// It is a separate adapter, under its own provider slug, from the Chat
// Completions adapter that serves the `openai` slug. The two share an origin and
// nothing else: a transcription is a multipart upload answered by one JSON
// document, and a spoken answer is audio plus its transcript, neither of which
// the text adapter can carry. Branching the text adapter on them would make one
// adapter's refusals depend on which endpoint a request happened to name. A
// separate slug is also what makes the capability boundary structural: a
// deployment is routed to exactly one adapter, so an audio model published
// under `openai-audio` can only be executed by this code, and a text chat model
// can never reach it — this adapter refuses any chat request that does not ask
// to be answered aloud.
package openaiaudio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/openaitranscription"
	"github.com/OxyHQ/Kaana/internal/provider/spokenchat"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// Slug is the provider name OpenAI's audio APIs are served under.
const Slug contract.ProviderSlug = "openai-audio"

type Config struct {
	Declarations []provider.KeyDeclaration
	Keys         provider.KeyPolicy
	HTTPClient   *http.Client
}

type Adapter struct {
	base        string
	client      *http.Client
	credentials *provider.KeyPool
}

// New builds the adapter. Its origin is not configurable: the slug is bound to
// OpenAI's own API root by providerconfig.
func New(config Config) (*Adapter, error) {
	if err := providerconfig.ValidateEndpointIdentity(Slug, providerconfig.OpenAIAudioBaseURL); err != nil {
		return nil, err
	}
	pool, err := provider.NewKeyPool(Slug, config.Declarations, config.Keys, provider.QuotaHeaders{})
	if err != nil {
		return nil, err
	}
	return &Adapter{base: providerconfig.OpenAIAudioBaseURL, client: provider.RefuseRedirects(config.HTTPClient), credentials: pool}, nil
}

func (a *Adapter) Provider() contract.ProviderSlug        { return Slug }
func (a *Adapter) PlatformCredentials() *provider.KeyPool { return a.credentials }

// APIFormats implements provider.Adapter: file transcription, and Chat
// Completions answered aloud (chat.go). A text chat is refused in Translate.
func (a *Adapter) APIFormats() []contract.APIFormat {
	return providerconfig.ExecutableAPIFormats(Slug, providerconfig.ProtocolOpenAIAudio)
}

func refuse(param, message string) (*provider.Call, error) {
	return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: param, Detail: message}
}

// Translate builds the multipart upload (internal/provider/openaitranscription)
// from the request read against the slug's reviewed statement
// (providerconfig.TranscriptionCapabilities), or a spoken chat (chat.go).
func (a *Adapter) Translate(r *contract.Request, route provider.Route) (*provider.Call, error) {
	if r == nil || route.Provider != Slug || route.UpstreamModelID == "" {
		return refuse("model", "an exact OpenAI audio deployment is required")
	}
	if r.Client.APIFormat == contract.APIFormatChatCompletions {
		return a.translateChat(r, route)
	}
	if r.Client.APIFormat != contract.APIFormatAudioTranscriptions {
		return refuse("client.apiFormat", "only audio_transcriptions and spoken chat_completions are supported")
	}
	upload, err := provider.ReadTranscription(r, transcriptionStatement())
	if err != nil {
		return nil, err
	}
	return openaitranscription.Call(route, a.base+"/audio/transcriptions", upload)
}

// transcriptionStatement is what this slug transcribes.
func transcriptionStatement() providerconfig.TranscriptionStatement {
	statement, _ := providerconfig.TranscriptionCapabilities(Slug, providerconfig.ProtocolOpenAIAudio)
	return statement
}

func invalidResponse() error {
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: "OpenAI returned an invalid or oversized transcription response",
		Passthrough: &contract.ProviderErrorPassthrough{Provider: Slug}}
}

func (a *Adapter) Stream(ctx context.Context, call *provider.Call, out provider.Emitter, credentials *provider.KeyPool) (provider.Outcome, error) {
	if spokenchat.IsCall(call) {
		return a.streamChat(ctx, call, out, credentials)
	}
	result := provider.Outcome{UsageSource: contract.UsageProviderReported}
	if credentials == nil {
		credentials = a.credentials
	}
	response, key, err := provider.Walk(ctx, credentials, call, a)
	result.KeyID, result.KeyClass = key.ID, key.Class
	if err != nil {
		return result, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, openaitranscription.MaxResponseBytes+1))
	if err != nil {
		return result, a.TransportFailure(ctx, err)
	}
	if len(data) > openaitranscription.MaxResponseBytes {
		return result, invalidResponse()
	}
	// Units come before the transcript is delivered: OpenAI bills the audio it
	// processed whether or not the text reaches the customer.
	transcript, ok := openaitranscription.Read(data, transcriptionStatement())
	result.Units = transcript.Units
	if !ok {
		return result, invalidResponse()
	}
	result.FinishReason = contract.FinishStop
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
		return result, err
	}
	if err := out.Usage(result.Units, result.UsageSource); err != nil {
		return result, err
	}
	if err := out.Delta(0, contract.ChannelOutputText, transcript.Text); err != nil {
		return result, err
	}
	return result, nil
}

func (a *Adapter) Send(ctx context.Context, call *provider.Call, key provider.Key) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, call.Method, call.URL, bytes.NewReader(call.Body))
	if err != nil {
		return nil, fmt.Errorf("openaiaudio: creating request: %w", err)
	}
	r.Header = call.Header.Clone()
	r.Header.Set("Authorization", "Bearer "+key.Secret())
	return a.client.Do(r)
}

func (a *Adapter) TransportFailure(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	passthrough := &contract.ProviderErrorPassthrough{Provider: Slug}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return provider.ErrUpstream{Code: contract.CodeProviderTimeout, Category: contract.UpstreamTimeout, Detail: "OpenAI transcription timed out", Passthrough: passthrough}
	}
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamServerError, Detail: "the connection to OpenAI failed", Passthrough: passthrough}
}

// Refuse classifies by status and OpenAI's own error type and code, never by
// message prose (https://platform.openai.com/docs/guides/error-codes). The
// message is bounded, stripped of this key by exact match, and redacted.
func (a *Adapter) Refuse(response *http.Response, key provider.Key) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	_ = response.Body.Close()
	var parsed struct {
		Error struct {
			Type    string  `json:"type"`
			Code    *string `json:"code"`
			Message string  `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	status := response.StatusCode
	passthrough := &contract.ProviderErrorPassthrough{Provider: Slug, Status: &status}
	if parsed.Error.Type != "" {
		kind := contract.SafeErrorText(provider.RedactSecret(parsed.Error.Type, key.Secret()))
		passthrough.Code = &kind
	}
	if parsed.Error.Message != "" {
		message := contract.SafeErrorText(provider.RedactSecret(parsed.Error.Message, key.Secret()))
		passthrough.Message = &message
	}
	code := ""
	if parsed.Error.Code != nil {
		code = *parsed.Error.Code
	}
	failure := provider.ErrUpstream{Passthrough: passthrough}
	switch {
	case status == http.StatusPaymentRequired,
		status == http.StatusTooManyRequests && (parsed.Error.Type == "insufficient_quota" || code == "insufficient_quota"):
		// The platform's OpenAI account cannot be billed. A quota is an
		// account ceiling only a human raises, unlike a rate limit.
		failure.Code, failure.Category = contract.CodeProviderBillingRefused, contract.UpstreamQuota
		failure.Detail = "the platform's own OpenAI account cannot be billed for this request"
	case status == http.StatusTooManyRequests:
		failure.Code, failure.Category = contract.CodeRateLimited, contract.UpstreamRateLimit
		failure.Detail = "OpenAI rate-limited this request"
		failure.RetryAfterMs = provider.RetryAfterMs(response.Header)
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
		failure.RetryAfterMs = provider.RetryAfterMs(response.Header)
	case status >= 500:
		failure.Code, failure.Category = contract.CodeProviderError, contract.UpstreamServerError
		failure.Detail = "OpenAI returned an internal error"
	case status == http.StatusRequestEntityTooLarge:
		failure.Code, failure.Category = contract.CodeRequestTooLarge, contract.UpstreamInvalidReq
		failure.Detail = "the request is larger than OpenAI accepts"
	default:
		failure.Code, failure.Category = contract.CodeInvalidRequest, contract.UpstreamInvalidReq
		failure.Detail = "OpenAI rejected the request"
	}
	return provider.CustomerCredentialFailure(key, failure)
}

// Health never uploads audio or asks for speech: a probe that spent audio
// credit would be a charge nobody asked for. Entitlement is proved by a signed
// canary.
func (a *Adapter) Health(_ context.Context) provider.Health {
	now := time.Now()
	pool := a.credentials.Projection(now)
	health := provider.Health{Provider: Slug, CheckedAt: contract.NewTimestamp(now), Credentials: &pool, Status: provider.HealthDegraded, Detail: "credential configured; audio entitlement requires a signed canary"}
	if !a.credentials.Configured() {
		health.Status, health.Detail = provider.HealthUnconfigured, "no OpenAI audio credential configured"
	} else if pool.Usable == 0 {
		health.Status, health.Detail = provider.HealthUnavailable, "no usable OpenAI audio credential"
	}
	return health
}
