// Package deepgram implements Deepgram's synchronous voice REST APIs.
package deepgram

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

const Slug contract.ProviderSlug = "deepgram"
const maxBody = 20 << 20

type Config struct {
	BaseURL      string
	Declarations []provider.KeyDeclaration
	Keys         provider.KeyPolicy
	HTTPClient   *http.Client
}

type Adapter struct {
	base        string
	client      *http.Client
	credentials *provider.KeyPool
}

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

func refuse(param, message string) (*provider.Call, error) {
	return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: param, Detail: message}
}

// Translate sends only controls represented by the voice REST protocol. A voice
// is the selected upstream model, never a silent replacement for that model.
func (a *Adapter) Translate(r *contract.Request, route provider.Route) (*provider.Call, error) {
	if r == nil || route.Provider != Slug || route.UpstreamModelID == "" {
		return refuse("model", "an exact Deepgram deployment is required")
	}
	if r.Stream {
		return refuse("stream", "Deepgram realtime WebSocket sessions are not supported by this REST adapter")
	}
	if r.Modality != contract.ModalityAudio {
		return refuse("modality", "Deepgram requires audio modality")
	}
	if r.Reasoning != nil || r.MaxOutputTokens != nil || len(r.Tools) != 0 || r.ToolChoice != nil || r.ResponseFormat != nil || !reflect.DeepEqual(r.Sampling, contract.SamplingParameters{}) {
		return refuse("parameters", "chat, reasoning and sampling controls are not supported by the voice API")
	}
	query := url.Values{"model": {route.UpstreamModelID}}
	call := &provider.Call{Route: route, Method: http.MethodPost, Header: make(http.Header)}
	switch r.Client.APIFormat {
	case contract.APIFormatAudioSpeech:
		if r.Input.Format != contract.InputText || r.Input.Text == nil || len(r.Input.Messages) != 0 || len(r.Input.Texts) != 0 || r.Speech == nil {
			return refuse("speech", "speech synthesis requires text and explicit speech parameters")
		}
		if !strings.HasPrefix(route.UpstreamModelID, "aura-") || r.Speech.Voice != route.UpstreamModelID {
			return refuse("speech.voice", "voice must equal the exact authorized Deepgram Aura model")
		}
		if r.Speech.ResponseFormat != "mp3" {
			return refuse("speech.responseFormat", "this adapter supports MP3 synthesis")
		}
		if r.Speech.Speed != nil {
			return refuse("speech.speed", "speed has not been reviewed for this adapter")
		}
		if n := utf8.RuneCountInString(*r.Input.Text); n == 0 || n > 2000 {
			return refuse("input", "Deepgram speech requires 1 to 2000 characters")
		}
		query.Set("encoding", "mp3")
		call.URL = a.base + "/speak?" + query.Encode()
		body, err := json.Marshal(struct {
			Text string `json:"text"`
		}{*r.Input.Text})
		if err != nil {
			return nil, fmt.Errorf("deepgram: encoding speech: %w", err)
		}
		call.Body = body
		call.Header.Set("Content-Type", "application/json")
		call.Header.Set("Accept", "audio/mpeg")
	case contract.APIFormatAudioTranscriptions:
		if r.Speech != nil || r.Input.Format != contract.InputMessages || len(r.Input.Messages) != 1 || r.Input.Text != nil || len(r.Input.Texts) != 0 {
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
		switch *part.Source.MediaType {
		case "audio/wav", "audio/x-wav", "audio/mpeg", "audio/flac", "audio/ogg":
		default:
			return refuse("input.mediaType", "audio must use a supported self-describing container")
		}
		if len(*part.Source.Data) > base64.StdEncoding.EncodedLen(maxBody) {
			return refuse("input", "audio exceeds 20 MiB")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(*part.Source.Data)
		if err != nil || len(data) == 0 || len(data) > maxBody {
			return refuse("input", "audio must be nonempty base64 of at most 20 MiB")
		}
		if strings.HasPrefix(route.UpstreamModelID, "aura-") {
			return refuse("model", "an Aura synthesis model cannot transcribe audio")
		}
		call.Body = data
		call.Header.Set("Content-Type", *part.Source.MediaType)
		call.Header.Set("Accept", "application/json")
		call.URL = a.base + "/listen?" + query.Encode()
	default:
		return refuse("client.apiFormat", "only audio_speech and audio_transcriptions are supported")
	}
	return call, nil
}

func invalidResponse() error {
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: "Deepgram returned an invalid or oversized voice response"}
}

func (a *Adapter) Stream(ctx context.Context, call *provider.Call, out provider.Emitter, credentials *provider.KeyPool) (provider.Outcome, error) {
	result := provider.Outcome{UsageSource: contract.UsageProviderReported}
	isSpeech := strings.HasPrefix(call.URL, a.base+"/speak?")
	if isSpeech {
		if _, ok := out.(provider.AudioEmitter); !ok {
			return result, errors.New("deepgram: synthesis requires an audio emitter")
		}
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
	// Capture billable synthesis units before reading the body: cancellation or
	// a disconnected downstream does not undo work already reported upstream.
	if isSpeech {
		count, parseErr := strconv.Atoi(response.Header.Get("dg-char-count"))
		if parseErr != nil || count <= 0 || count > 2000 {
			return result, invalidResponse()
		}
		result.Units = []contract.UsageQuantity{{Unit: contract.UnitCharacters, Quantity: count}}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return result, a.TransportFailure(ctx, err)
	}
	if len(data) > maxBody {
		return result, invalidResponse()
	}
	var transcript string
	if isSpeech {
		mediaType := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
		if mediaType != "audio/mpeg" || len(data) < 4 || (string(data[:3]) != "ID3" && (data[0] != 0xff || data[1]&0xe0 != 0xe0)) {
			return result, invalidResponse()
		}
	} else {
		var body struct {
			Metadata struct {
				Duration *float64 `json:"duration"`
				Channels *int     `json:"channels"`
			} `json:"metadata"`
			Results struct {
				Channels []struct {
					Alternatives []struct {
						Transcript *string `json:"transcript"`
					} `json:"alternatives"`
				} `json:"channels"`
			} `json:"results"`
		}
		if json.Unmarshal(data, &body) != nil || body.Metadata.Duration == nil || body.Metadata.Channels == nil || *body.Metadata.Channels != 1 {
			return result, invalidResponse()
		}
		duration := *body.Metadata.Duration
		if math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > 24*60*60 {
			return result, invalidResponse()
		}
		result.Units = []contract.UsageQuantity{{Unit: contract.UnitAudioInputMilliseconds, Quantity: int(math.Ceil(duration * 1000))}}
		if len(body.Results.Channels) != 1 || len(body.Results.Channels[0].Alternatives) != 1 || body.Results.Channels[0].Alternatives[0].Transcript == nil {
			return result, invalidResponse()
		}
		transcript = *body.Results.Channels[0].Alternatives[0].Transcript
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
	if isSpeech {
		for len(data) > 0 {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			n := min(len(data), 49152)
			if err := out.(provider.AudioEmitter).Audio(0, "audio/mpeg", data[:n]); err != nil {
				return result, err
			}
			data = data[n:]
		}
	} else if err := out.Delta(0, contract.ChannelOutputText, transcript); err != nil {
		return result, err
	}
	return result, nil
}

func (a *Adapter) Send(ctx context.Context, call *provider.Call, key provider.Key) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, call.Method, call.URL, bytes.NewReader(call.Body))
	if err != nil {
		return nil, fmt.Errorf("deepgram: creating request: %w", err)
	}
	r.Header = call.Header.Clone()
	r.Header.Set("Authorization", "Token "+key.Secret())
	return a.client.Do(r)
}

func (a *Adapter) TransportFailure(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return provider.ErrUpstream{Code: contract.CodeProviderTimeout, Category: contract.UpstreamTimeout, Detail: "Deepgram request timed out"}
	}
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamServerError, Detail: "Deepgram connection failed"}
}

// Refuse follows https://developers.deepgram.com/docs/errors. Free-form body
// fields are bounded and redacted; never use their prose to retire credentials.
func (a *Adapter) Refuse(response *http.Response, key provider.Key) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	_ = response.Body.Close()
	var parsed struct {
		Code    string `json:"err_code"`
		Message string `json:"err_msg"`
	}
	_ = json.Unmarshal(body, &parsed)
	code := contract.SafeErrorText(provider.RedactSecret(parsed.Code, key.Secret()))
	message := contract.SafeErrorText(provider.RedactSecret(parsed.Message, key.Secret()))
	status := response.StatusCode
	failure := provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: "Deepgram rejected the voice request", Passthrough: &contract.ProviderErrorPassthrough{Provider: Slug, Status: &status, Code: &code, Message: &message}}
	switch {
	case status == 402:
		failure.Code, failure.Category = contract.CodeProviderBillingRefused, contract.UpstreamQuota
	case parsed.Code == "INVALID_AUTH" || parsed.Code == "INSUFFICIENT_PERMISSIONS":
		failure.Code, failure.Category = contract.CodeProviderCredentialInvalid, contract.UpstreamAuthentication
	case status == 429:
		failure.Code, failure.Category, failure.RetryAfterMs = contract.CodeRateLimited, contract.UpstreamRateLimit, provider.RetryAfterMs(response.Header)
	case status == 408 || status == 504:
		failure.Code, failure.Category = contract.CodeProviderTimeout, contract.UpstreamTimeout
	case status >= 500:
		failure.Category = contract.UpstreamServerError
	case status == 413:
		failure.Code, failure.Category = contract.CodeRequestTooLarge, contract.UpstreamInvalidReq
	case status == 400 || status == 422:
		failure.Code, failure.Category = contract.CodeInvalidRequest, contract.UpstreamInvalidReq
	}
	return provider.CustomerCredentialFailure(key, failure)
}

// Do not bill a voice call as a health probe, or treat the public model list as
// proof of account entitlement. A live signed canary is the enablement gate.
func (a *Adapter) Health(_ context.Context) provider.Health {
	now := time.Now()
	pool := a.credentials.Projection(now)
	health := provider.Health{Provider: Slug, CheckedAt: contract.NewTimestamp(now), Credentials: &pool, Status: provider.HealthDegraded, Detail: "credential configured; voice entitlement requires a signed canary"}
	if !a.credentials.Configured() {
		health.Status, health.Detail = provider.HealthUnconfigured, "no Deepgram credential configured"
	} else if pool.Usable == 0 {
		health.Status, health.Detail = provider.HealthUnavailable, "no usable Deepgram credential"
	}
	return health
}
