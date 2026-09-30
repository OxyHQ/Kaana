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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"reflect"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/spokenchat"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// Slug is the provider name OpenAI's audio APIs are served under.
const Slug contract.ProviderSlug = "openai-audio"

// maxAudioBytes is the smaller of the contract's inline audio ceiling
// (MAX_INFERENCE_AUDIO_BYTES, 20 MiB) and OpenAI's documented 25 MB upload
// limit. The contract's is the binding one.
const maxAudioBytes = 20 << 20

// maxResponseBytes bounds the transcript document. A transcript of 20 MiB of
// audio is far smaller; verbose_json segments are the largest shape asked for.
const maxResponseBytes = 8 << 20

// whisperModel is the one reviewed model whose plain `json` response carries no
// duration. It is asked for `verbose_json`, whose top-level duration is the
// billing quantity OpenAI prices it by.
const whisperModel = "whisper-1"

// fileNames is the upload name for each accepted container. OpenAI infers the
// container from the file name's extension, so the name is part of the wire
// contract rather than decoration. Only containers OpenAI documents for this
// endpoint and the contract can name are accepted.
var fileNames = map[string]string{
	"audio/wav":   "audio.wav",
	"audio/x-wav": "audio.wav",
	"audio/mpeg":  "audio.mp3",
	"audio/flac":  "audio.flac",
	"audio/ogg":   "audio.ogg",
	"audio/webm":  "audio.webm",
	"audio/mp4":   "audio.m4a",
}

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

// Translate builds the multipart upload. It sends the exact signed model, the
// audio, and the response format this adapter reads — no language, prompt,
// temperature or chunking default is invented for a caller who never set one.
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
	if r.AudioOutput != nil {
		return refuse("audioOutput", "spoken output is a chat_completions request")
	}
	if r.Stream {
		return refuse("stream", "streamed transcription events are not supported by this adapter")
	}
	if r.Modality != contract.ModalityAudio {
		return refuse("modality", "transcription requires audio modality")
	}
	if r.Speech != nil || r.Reasoning != nil || r.MaxOutputTokens != nil || len(r.Tools) != 0 || r.ToolChoice != nil || r.ResponseFormat != nil || !reflect.DeepEqual(r.Sampling, contract.SamplingParameters{}) {
		return refuse("parameters", "chat, speech, reasoning and sampling controls are not supported by the transcription API")
	}
	if r.Input.Format != contract.InputMessages || len(r.Input.Messages) != 1 || r.Input.Text != nil || len(r.Input.Texts) != 0 {
		return refuse("input", "transcription requires one user message containing one inline audio part")
	}
	message := r.Input.Messages[0]
	if message.Role != contract.RoleUser || len(message.Content) != 1 || len(message.ToolCalls) != 0 || message.Name != nil || message.ToolCallID != nil {
		return refuse("input", "transcription requires one user audio part without message metadata")
	}
	part := message.Content[0]
	if part.Type != contract.ContentPartAudio || part.Source == nil || part.Source.Kind != contract.ContentSourceInline || part.Source.URL != nil || part.Source.Data == nil || part.Source.MediaType == nil || part.Text != nil || part.Detail != nil || part.Filename != nil {
		return refuse("input", "only inline audio is supported; remote audio is not fetched")
	}
	fileName, supported := fileNames[*part.Source.MediaType]
	if !supported {
		return refuse("input.mediaType", "audio must use a container OpenAI documents for transcription")
	}
	if len(*part.Source.Data) > base64.StdEncoding.EncodedLen(maxAudioBytes) {
		return refuse("input", "audio exceeds 20 MiB")
	}
	audio, err := base64.StdEncoding.Strict().DecodeString(*part.Source.Data)
	if err != nil || len(audio) == 0 || len(audio) > maxAudioBytes {
		return refuse("input", "audio must be nonempty base64 of at most 20 MiB")
	}

	responseFormat := "json"
	if route.UpstreamModelID == whisperModel {
		responseFormat = "verbose_json"
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for _, field := range [][2]string{{"model", route.UpstreamModelID}, {"response_format", responseFormat}} {
		if err := form.WriteField(field[0], field[1]); err != nil {
			return nil, fmt.Errorf("openaiaudio: encoding %s: %w", field[0], err)
		}
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, fileName))
	header.Set("Content-Type", *part.Source.MediaType)
	file, err := form.CreatePart(header)
	if err != nil {
		return nil, fmt.Errorf("openaiaudio: encoding the audio part: %w", err)
	}
	if _, err := file.Write(audio); err != nil {
		return nil, fmt.Errorf("openaiaudio: encoding the audio part: %w", err)
	}
	if err := form.Close(); err != nil {
		return nil, fmt.Errorf("openaiaudio: closing the upload: %w", err)
	}
	call := &provider.Call{Route: route, Method: http.MethodPost, URL: a.base + "/audio/transcriptions", Body: body.Bytes(), Header: make(http.Header)}
	call.Header.Set("Content-Type", form.FormDataContentType())
	call.Header.Set("Accept", "application/json")
	return call, nil
}

func invalidResponse() error {
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: "OpenAI returned an invalid or oversized transcription response",
		Passthrough: &contract.ProviderErrorPassthrough{Provider: Slug}}
}

// transcription is the part of OpenAI's json and verbose_json responses this
// adapter reads.
type transcription struct {
	Text     *string  `json:"text"`
	Duration *float64 `json:"duration"`
	Usage    *struct {
		Type         string   `json:"type"`
		Seconds      *float64 `json:"seconds"`
		InputTokens  *int     `json:"input_tokens"`
		OutputTokens *int     `json:"output_tokens"`
		TotalTokens  *int     `json:"total_tokens"`
	} `json:"usage"`
}

// units is what OpenAI reported, in the contract's units.
//
// OpenAI reports one of two usage shapes and each is copied exactly:
//
//   - `duration` (whisper-1, and per-minute-priced models): the audio length
//     in seconds, which is `audio_input_milliseconds`, rounded up.
//   - `tokens` (token-priced models): `input_tokens` and `output_tokens`.
//     OpenAI's input count is mostly audio tokens (`input_token_details`
//     splits audio from text), and the contract has no audio-token unit, so
//     the total is reported as `input_tokens` without a split. That is the
//     provider's own count, not an estimate; what is lost is only the price
//     distinction, which a rate card for these deployments must price knowing
//     the input is audio.
//
// A response with neither is refused rather than settled on a guess.
func (t transcription) units() ([]contract.UsageQuantity, bool) {
	seconds := t.Duration
	if t.Usage != nil {
		switch t.Usage.Type {
		case "duration":
			seconds = t.Usage.Seconds
		case "tokens":
			in, out := t.Usage.InputTokens, t.Usage.OutputTokens
			if in == nil || out == nil || *in < 0 || *out < 0 || (t.Usage.TotalTokens != nil && *t.Usage.TotalTokens != *in+*out) {
				return nil, false
			}
			return []contract.UsageQuantity{
				{Unit: contract.UnitInputTokens, Quantity: *in},
				{Unit: contract.UnitOutputTokens, Quantity: *out},
			}, true
		default:
			return nil, false
		}
	}
	if seconds == nil || math.IsNaN(*seconds) || math.IsInf(*seconds, 0) || *seconds <= 0 || *seconds > 24*60*60 {
		return nil, false
	}
	return []contract.UsageQuantity{{Unit: contract.UnitAudioInputMilliseconds, Quantity: int(math.Ceil(*seconds * 1000))}}, true
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
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return result, a.TransportFailure(ctx, err)
	}
	if len(data) > maxResponseBytes {
		return result, invalidResponse()
	}
	var body transcription
	if json.Unmarshal(data, &body) != nil {
		return result, invalidResponse()
	}
	// Units come before the transcript is validated or delivered: OpenAI bills
	// the audio it processed whether or not the text reaches the customer.
	units, measured := body.units()
	if !measured {
		return result, invalidResponse()
	}
	result.Units = units
	if body.Text == nil {
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
	if err := out.Delta(0, contract.ChannelOutputText, *body.Text); err != nil {
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
