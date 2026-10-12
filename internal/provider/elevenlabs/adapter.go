// Package elevenlabs implements ElevenLabs' synchronous text-to-speech REST
// API (https://elevenlabs.io/docs/api-reference/text-to-speech/convert).
//
// One request family, audio_speech: the signed deployment names the
// ElevenLabs model (`model_id`), the caller names the voice. Nothing else
// ElevenLabs offers — streaming, timestamps, dialogue, voice changing, sound
// effects, transcription — is executed here, and every control the contract
// carries that this endpoint cannot express is refused before anything is
// spent (docs/elevenlabs.md).
package elevenlabs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// Slug is the only provider name this wire is served under.
const Slug contract.ProviderSlug = "elevenlabs"

// maxAudioBytes is the contract's MAX_INFERENCE_AUDIO_BYTES: the most the
// edge can fold into one response.
const maxAudioBytes = 20 << 20

// outputFormat is the encoding requested for `responseFormat: mp3`. It is
// ElevenLabs' documented default (`mp3_44100_128`, available on every tier),
// stated explicitly so the format Kaana validates is the format it asked for
// rather than whatever the default becomes.
const outputFormat = "mp3_44100_128"

// Speed bounds ElevenLabs documents for `voice_settings.speed`: "to a minimum
// of 0.7 ... to a maximum of 1.2"
// (https://elevenlabs.io/docs/overview/capabilities/text-to-speech/best-practices).
const (
	minSpeed = 0.7
	maxSpeed = 1.2
)

// ProductVoices maps the contract's two product voice names onto ElevenLabs
// premade voices, as the xAI speech adapter maps them onto eve/rex. Both are
// in ElevenLabs' public premade catalogue (`GET /v1/voices`, read
// 2026-10-12): Sarah is labelled female and George male, and both list
// Spanish and English among their verified languages. Any other value must be
// an exact ElevenLabs voice id (voiceIDPattern), which ElevenLabs validates
// against the workspace's own library.
var ProductVoices = map[string]string{
	"female": "EXAVITQu4vr4xnSDxMaL", // Sarah
	"male":   "JBFqnCBsd6RMkjVDRZzb", // George
}

// voiceIDPattern is the shape of every ElevenLabs voice id in its catalogue:
// twenty ASCII letters and digits. It bounds what reaches the URL path; it is
// not a claim that the voice exists.
var voiceIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{20}$`)

// MaxCharacters is the per-request text ceiling ElevenLabs documents for each
// attributed model (https://elevenlabs.io/docs/overview/models, "Character
// limits", read 2026-10-12). It is counted here in UTF-16 code units, the unit
// Oxy's public input ceiling uses, which is never fewer than ElevenLabs'
// characters, so a request this adapter admits is never one ElevenLabs refuses
// for length. A model without a row is refused: there is no limit to invent.
var MaxCharacters = map[string]int{
	"eleven_multilingual_v2": 10_000,
	"eleven_flash_v2_5":      40_000,
	"eleven_v3":              5_000,
}

// Config describes the provider.
type Config struct {
	BaseURL      string
	Declarations []provider.KeyDeclaration
	Keys         provider.KeyPolicy
	HTTPClient   *http.Client
}

// Adapter is the ElevenLabs text-to-speech adapter.
type Adapter struct {
	base        string
	client      *http.Client
	credentials *provider.KeyPool
}

// New builds the adapter on ElevenLabs' canonical API root.
func New(config Config) (*Adapter, error) {
	if err := providerconfig.ValidateEndpointIdentity(Slug, config.BaseURL); err != nil {
		return nil, err
	}
	pool, err := provider.NewKeyPool(Slug, config.Declarations, config.Keys, provider.QuotaHeaders{})
	if err != nil {
		return nil, err
	}
	return &Adapter{base: config.BaseURL, client: provider.RefuseRedirects(config.HTTPClient), credentials: pool}, nil
}

func (a *Adapter) Provider() contract.ProviderSlug        { return Slug }
func (a *Adapter) PlatformCredentials() *provider.KeyPool { return a.credentials }
func (a *Adapter) APIFormats() []contract.APIFormat {
	return providerconfig.ExecutableAPIFormats(Slug, providerconfig.ProtocolElevenLabsSpeech)
}

func refuse(param, message string) (*provider.Call, error) {
	return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: param, Detail: message}
}

type speechBody struct {
	Text          string         `json:"text"`
	ModelID       string         `json:"model_id"`
	VoiceSettings *voiceSettings `json:"voice_settings,omitempty"`
}

// voiceSettings carries only what the caller set. ElevenLabs applies the
// voice's stored settings to every field this object leaves out.
type voiceSettings struct {
	Speed *float64 `json:"speed,omitempty"`
}

// Voice resolves the contract's voice to the ElevenLabs voice id sent in the
// path, or reports that it is neither a product voice nor a voice id.
func Voice(requested string) (string, bool) {
	if id, product := ProductVoices[requested]; product {
		return id, true
	}
	return requested, voiceIDPattern.MatchString(requested)
}

// Characters is the measured size of a speech input: UTF-16 code units, as
// Oxy measures its public input ceiling and xAI's speech route meters.
func Characters(text string) int { return len(utf16.Encode([]rune(text))) }

// Translate builds `POST {base}/text-to-speech/{voice_id}?output_format=...`.
// The signed upstream model is `model_id`; nothing is defaulted that the
// caller did not send.
func (a *Adapter) Translate(r *contract.Request, route provider.Route) (*provider.Call, error) {
	if r == nil || route.Provider != Slug || route.UpstreamModelID == "" {
		return refuse("model", "an exact ElevenLabs deployment is required")
	}
	if r.Client.APIFormat != contract.APIFormatAudioSpeech {
		return nil, provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: "client.apiFormat", Detail: "ElevenLabs deployments execute speech synthesis only"}
	}
	if r.Stream {
		return refuse("stream", "streamed speech is not supported by this adapter")
	}
	if r.Modality != contract.ModalityAudio {
		return refuse("modality", "speech synthesis requires audio modality")
	}
	if r.AudioOutput != nil || r.Reasoning != nil || r.MaxOutputTokens != nil || len(r.Tools) != 0 || r.ToolChoice != nil || r.ResponseFormat != nil || !reflect.DeepEqual(r.Sampling, contract.SamplingParameters{}) {
		return refuse("parameters", "chat, reasoning and sampling controls are not supported by the speech API")
	}
	if r.Input.Format != contract.InputText || r.Input.Text == nil || len(r.Input.Messages) != 0 || len(r.Input.Texts) != 0 || r.Speech == nil {
		return refuse("speech", "speech synthesis requires text and explicit speech parameters")
	}
	limit, reviewed := MaxCharacters[route.UpstreamModelID]
	if !reviewed {
		return refuse("model", "this ElevenLabs model has no reviewed character limit")
	}
	if n := Characters(*r.Input.Text); n == 0 || n > limit {
		return refuse("input", fmt.Sprintf("this ElevenLabs model takes 1 to %d characters", limit))
	}
	if r.Speech.ResponseFormat != "mp3" {
		return refuse("speech.responseFormat", "this adapter supports mp3 output")
	}
	if r.Speech.Speed != nil && (*r.Speech.Speed < minSpeed || *r.Speech.Speed > maxSpeed) {
		return refuse("speech.speed", "ElevenLabs supports speeds from 0.7 to 1.2")
	}
	voice, ok := Voice(r.Speech.Voice)
	if !ok {
		return refuse("speech.voice", "voice must be female, male or an exact ElevenLabs voice id")
	}
	body := speechBody{Text: *r.Input.Text, ModelID: route.UpstreamModelID}
	if r.Speech.Speed != nil {
		body.VoiceSettings = &voiceSettings{Speed: r.Speech.Speed}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs: encoding speech: %w", err)
	}
	query := url.Values{"output_format": {outputFormat}}
	call := &provider.Call{
		Route:  route,
		Method: http.MethodPost,
		URL:    a.base + "/text-to-speech/" + url.PathEscape(voice) + "?" + query.Encode(),
		Body:   encoded,
		Header: make(http.Header),
	}
	call.Header.Set("Content-Type", "application/json")
	call.Header.Set("Accept", "audio/mpeg")
	return call, nil
}

func invalidResponse() error {
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: "ElevenLabs returned an invalid or oversized speech response"}
}

// Stream sends the call and emits the MP3 in bounded audio chunks.
//
// ElevenLabs documents no usage object or header for this endpoint; it bills
// the characters of the text it accepted. So the units are Kaana's own
// measurement of the text it sent, recorded as soon as ElevenLabs accepts the
// request (a 200) and before the body is read: a cut connection or a
// disconnected customer does not undo work ElevenLabs already billed.
func (a *Adapter) Stream(ctx context.Context, call *provider.Call, out provider.Emitter, credentials *provider.KeyPool) (provider.Outcome, error) {
	result := provider.Outcome{UsageSource: contract.UsageOxyMeasured}
	audio, ok := out.(provider.AudioEmitter)
	if !ok {
		return result, errors.New("elevenlabs: synthesis requires an audio emitter")
	}
	var sent speechBody
	if err := json.Unmarshal(call.Body, &sent); err != nil {
		return result, fmt.Errorf("elevenlabs: unreadable translated speech request: %w", err)
	}
	if credentials == nil {
		credentials = a.credentials
	}
	response, key, err := provider.Walk(ctx, credentials, call, a)
	result.KeyID, result.KeyClass = key.ID, key.Class
	if err != nil {
		return result, err
	}
	defer func() { _ = response.Body.Close() }()
	result.Units = []contract.UsageQuantity{{Unit: contract.UnitCharacters, Quantity: Characters(sent.Text)}}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxAudioBytes+1))
	if err != nil {
		return result, a.TransportFailure(ctx, err)
	}
	mediaType := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	if len(data) > maxAudioBytes || mediaType != "audio/mpeg" || len(data) < 4 || (string(data[:3]) != "ID3" && (data[0] != 0xff || data[1]&0xe0 != 0xe0)) {
		return result, invalidResponse()
	}
	result.FinishReason = contract.FinishStop
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := audio.Start(call.Route.ModelReference, time.Now()); err != nil {
		return result, err
	}
	if err := audio.Usage(result.Units, result.UsageSource); err != nil {
		return result, err
	}
	return result, provider.EmitAudio(ctx, audio, 0, "audio/mpeg", data)
}

// Send authenticates with `xi-api-key`, the header every ElevenLabs request
// carries (https://elevenlabs.io/docs/api-reference/authentication).
func (a *Adapter) Send(ctx context.Context, call *provider.Call, key provider.Key) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, call.Method, call.URL, bytes.NewReader(call.Body))
	if err != nil {
		return nil, fmt.Errorf("elevenlabs: creating request: %w", err)
	}
	r.Header = call.Header.Clone()
	r.Header.Set("xi-api-key", key.Secret())
	return a.client.Do(r)
}

func (a *Adapter) TransportFailure(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return provider.ErrUpstream{Code: contract.CodeProviderTimeout, Category: contract.UpstreamTimeout, Detail: "ElevenLabs request timed out"}
	}
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamServerError, Detail: "ElevenLabs connection failed"}
}

// errorBody is ElevenLabs' documented error object
// (https://elevenlabs.io/docs/developers/resources/errors): `detail.type` is
// the category, `detail.code` the specific error, `detail.status` a legacy
// field still sent (measured 2026-10-12: `invalid_api_key`,
// `needs_authorization`). A 422 validation error carries `detail` as a list
// instead, which leaves these fields empty and the status decides.
type errorBody struct {
	Detail json.RawMessage `json:"detail"`
}

type errorDetail struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// Refuse classifies by ElevenLabs' own error type and code, never by message
// prose. What each verdict means for the key is provider.Walk's:
//
//   - 402 `payment_required` / `insufficient_credits`, and the legacy
//     `quota_exceeded` status, are the account's credits running out: the
//     key is exhausted.
//   - 401 `authentication_error` is a refused key.
//   - 403 `authorization_error` is a refused key too, unless its code names
//     the request (`voice_access_denied`) or this account's entitlement to a
//     model or feature (`model_access_denied`, `feature_not_available`,
//     `subscription_required`). A key restricted to an IP allowlist or an
//     endpoint scope answers 403 from anywhere else
//     (https://elevenlabs.io/docs/overview/administration/workspaces/api-keys):
//     that is credential configuration, never the customer's fault.
//   - 429 is a throttle (`system_busy` an overload); 5xx the provider.
func (a *Adapter) Refuse(response *http.Response, key provider.Key) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	_ = response.Body.Close()
	var parsed errorBody
	var detail errorDetail
	if json.Unmarshal(body, &parsed) == nil {
		_ = json.Unmarshal(parsed.Detail, &detail)
	}
	code := detail.Code
	if code == "" {
		code = detail.Status
	}
	safeCode := contract.SafeErrorText(provider.RedactSecret(code, key.Secret()))
	message := detail.Message
	if message == "" && len(parsed.Detail) != 0 && parsed.Detail[0] == '[' {
		message = string(parsed.Detail)
	}
	safeMessage := contract.SafeErrorText(provider.RedactSecret(message, key.Secret()))
	status := response.StatusCode
	failure := provider.ErrUpstream{
		Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: "ElevenLabs refused the speech request",
		Passthrough: &contract.ProviderErrorPassthrough{Provider: Slug, Status: &status, Code: &safeCode, Message: &safeMessage},
	}
	switch {
	case status == http.StatusPaymentRequired || detail.Type == "payment_required" || detail.Code == "insufficient_credits" || detail.Code == "quota_exceeded" || detail.Status == "quota_exceeded":
		failure.Code, failure.Category = contract.CodeProviderBillingRefused, contract.UpstreamQuota
	case status == http.StatusUnauthorized || detail.Type == "authentication_error":
		failure.Code, failure.Category = contract.CodeProviderCredentialInvalid, contract.UpstreamAuthentication
	case status == http.StatusForbidden || detail.Type == "authorization_error":
		switch detail.Code {
		case "voice_access_denied", "model_access_denied", "feature_not_available", "subscription_required":
			failure.Code, failure.Category = contract.CodePermissionDenied, contract.UpstreamInvalidReq
		default:
			failure.Code, failure.Category = contract.CodeProviderCredentialInvalid, contract.UpstreamAuthentication
		}
	case status == http.StatusTooManyRequests || detail.Type == "rate_limit_error":
		if detail.Code == "system_busy" {
			failure.Code, failure.Category = contract.CodeProviderOverloaded, contract.UpstreamOverloaded
		} else {
			failure.Code, failure.Category, failure.RetryAfterMs = contract.CodeRateLimited, contract.UpstreamRateLimit, provider.RetryAfterMs(response.Header)
		}
	case status == http.StatusNotFound && detail.Code == "model_not_found":
		failure.Code, failure.Category = contract.CodeModelNotFound, contract.UpstreamInvalidReq
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		failure.Code, failure.Category = contract.CodeProviderTimeout, contract.UpstreamTimeout
	case status == http.StatusServiceUnavailable:
		failure.Code, failure.Category = contract.CodeProviderOverloaded, contract.UpstreamOverloaded
	case status >= 500:
		failure.Category = contract.UpstreamServerError
	case status == http.StatusRequestEntityTooLarge || detail.Code == "request_too_large":
		failure.Code, failure.Category = contract.CodeRequestTooLarge, contract.UpstreamInvalidReq
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity || status == http.StatusNotFound:
		// A validation error, or a voice the workspace does not hold
		// (`voice_not_found`): the request is at fault, wherever it is sent.
		failure.Code, failure.Category = contract.CodeInvalidRequest, contract.UpstreamInvalidReq
	}
	return provider.CustomerCredentialFailure(key, failure)
}

// Health never synthesizes speech: a health probe would spend credits. The
// route stays degraded until a signed canary has proved the key can speak.
func (a *Adapter) Health(_ context.Context) provider.Health {
	now := time.Now()
	pool := a.credentials.Projection(now)
	health := provider.Health{Provider: Slug, CheckedAt: contract.NewTimestamp(now), Credentials: &pool, Status: provider.HealthDegraded, Detail: "credential configured; speech entitlement requires a signed canary"}
	if !a.credentials.Configured() {
		health.Status, health.Detail = provider.HealthUnconfigured, "no ElevenLabs credential configured"
	} else if pool.Usable == 0 {
		health.Status, health.Detail = provider.HealthUnavailable, "no usable ElevenLabs credential"
	}
	return health
}
