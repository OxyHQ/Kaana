package openaicompat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// xAI file transcription: POST {base}/stt, a multipart upload answered by one
// JSON document
// (https://docs.x.ai/developers/model-capabilities/audio/speech-to-text). It
// lives beside xAI's speech synthesis because both are the `xai` slug's own
// origin and key; nothing else in this adapter reaches /stt.

// maxTranscriptionAudioBytes is the contract's inline audio ceiling (20 MiB),
// the binding one: xAI itself takes files up to 500 MB.
const maxTranscriptionAudioBytes = 20 << 20

// maxTranscriptionResponseBytes bounds the transcript document. Word timings
// are its largest part, and 20 MiB of compressed speech is far smaller.
const maxTranscriptionResponseBytes = 8 << 20

// maxTranscriptionSeconds bounds the duration xAI may report. The public API
// refuses audio over 30 minutes before a grant; a provider claiming a day is
// a broken response, not a bill.
const maxTranscriptionSeconds = 24 * 60 * 60

// transcriptionFileNames is the upload name for each container xAI detects
// (WAV, MP3, OGG/Opus, FLAC, AAC, MP4/M4A, MKV — WebM is Matroska). The
// extension is a hint for the upstream's container detection; the part's
// Content-Type says the same thing.
var transcriptionFileNames = map[string]string{
	"audio/wav":  "audio.wav",
	"audio/mpeg": "audio.mp3",
	"audio/ogg":  "audio.ogg",
	"audio/flac": "audio.flac",
	"audio/aac":  "audio.aac",
	"audio/mp4":  "audio.m4a",
	"audio/webm": "audio.webm",
}

// isTranscriptionModel reports an xAI deployment that only transcribes.
func isTranscriptionModel(provider contract.ProviderSlug, upstreamModelID string) bool {
	if provider != "xai" {
		return false
	}
	family, classified, _ := providerconfig.ClassifyModel("xai", upstreamModelID)
	return classified && family.Format == contract.APIFormatAudioTranscriptions
}

// translateTranscription builds the upload: the exact model, the caller's
// language when one was given, and the audio as the LAST field, which xAI
// requires. No formatting, diarization or filler-word default is invented for
// a caller who never asked for one.
func (a *Adapter) translateTranscription(request *contract.Request, route provider.Route) (*provider.Call, error) {
	refuse := func(param, detail string) (*provider.Call, error) {
		return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: param, Detail: detail}
	}
	if !isTranscriptionModel(a.config.Provider, route.UpstreamModelID) || route.Provider != "xai" {
		return nil, provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: "modality", Detail: "this deployment does not transcribe audio"}
	}
	if request.Stream {
		return refuse("stream", "streamed transcription is not supported by this deployment")
	}
	if request.Modality != contract.ModalityAudio {
		return refuse("modality", "transcription requires audio modality")
	}
	if request.Speech != nil || request.AudioOutput != nil || request.Reasoning != nil || request.MaxOutputTokens != nil || len(request.Tools) != 0 || request.ToolChoice != nil || request.ResponseFormat != nil || !reflect.DeepEqual(request.Sampling, contract.SamplingParameters{}) {
		return refuse("parameters", "chat, speech, reasoning and sampling controls are not supported by the transcription API")
	}
	if request.Input.Format != contract.InputMessages || len(request.Input.Messages) != 1 || request.Input.Text != nil || len(request.Input.Texts) != 0 {
		return refuse("input", "transcription requires one user message containing one inline audio part")
	}
	message := request.Input.Messages[0]
	if message.Role != contract.RoleUser || len(message.Content) != 1 || len(message.ToolCalls) != 0 || message.Name != nil || message.ToolCallID != nil {
		return refuse("input", "transcription requires one user audio part without message metadata")
	}
	part := message.Content[0]
	if part.Type != contract.ContentPartAudio || part.Source == nil || part.Source.Kind != contract.ContentSourceInline || part.Source.URL != nil || part.Source.Data == nil || part.Source.MediaType == nil || part.Text != nil || part.Detail != nil || part.Filename != nil {
		return refuse("input", "only inline audio is supported; remote audio is not fetched")
	}
	fileName, supported := transcriptionFileNames[*part.Source.MediaType]
	if !supported {
		return refuse("input.mediaType", "audio must use a container xAI detects for transcription")
	}
	if len(*part.Source.Data) > base64.StdEncoding.EncodedLen(maxTranscriptionAudioBytes) {
		return refuse("input", "audio exceeds 20 MiB")
	}
	audio, err := base64.StdEncoding.Strict().DecodeString(*part.Source.Data)
	if err != nil || len(audio) == 0 || len(audio) > maxTranscriptionAudioBytes {
		return refuse("input", "audio must be nonempty base64 of at most 20 MiB")
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	fields := [][2]string{{"model", route.UpstreamModelID}}
	if request.Transcription != nil && request.Transcription.Language != nil {
		fields = append(fields, [2]string{"language", string(*request.Transcription.Language)})
	}
	for _, field := range fields {
		if err := form.WriteField(field[0], field[1]); err != nil {
			return nil, fmt.Errorf("openaicompat: encoding %s: %w", field[0], err)
		}
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, fileName))
	header.Set("Content-Type", *part.Source.MediaType)
	file, err := form.CreatePart(header)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: encoding the audio part: %w", err)
	}
	if _, err := file.Write(audio); err != nil {
		return nil, fmt.Errorf("openaicompat: encoding the audio part: %w", err)
	}
	if err := form.Close(); err != nil {
		return nil, fmt.Errorf("openaicompat: closing the upload: %w", err)
	}
	headers := make(http.Header)
	for name, value := range a.config.Headers {
		headers.Set(name, value)
	}
	headers.Set("Content-Type", form.FormDataContentType())
	headers.Set("Accept", "application/json")
	return &provider.Call{Route: route, Method: http.MethodPost, URL: a.config.BaseURL + "/stt", Body: body.Bytes(), Header: headers}, nil
}

func invalidTranscription() error {
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: "xAI returned an invalid or oversized transcription response"}
}

// streamTranscription sends the upload and emits the transcript as one text
// delta. The billed quantity is the duration xAI reports, which is what it
// prices speech-to-text by; it is read before the transcript is believed, so a
// response without one is a provider error rather than a free request.
func (a *Adapter) streamTranscription(ctx context.Context, call *provider.Call, out provider.Emitter, credentials *provider.KeyPool) (provider.Outcome, error) {
	outcome := provider.Outcome{UsageSource: contract.UsageProviderReported}
	if credentials == nil {
		credentials = a.credentials
	}
	response, key, err := provider.Walk(ctx, credentials, call, a)
	outcome.KeyID, outcome.KeyClass = key.ID, key.Class
	if err != nil {
		return outcome, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxTranscriptionResponseBytes+1))
	if err != nil {
		return outcome, fmt.Errorf("openaicompat: reading the transcription: %w", err)
	}
	if len(data) > maxTranscriptionResponseBytes {
		return outcome, invalidTranscription()
	}
	var body struct {
		Text     *string  `json:"text"`
		Duration *float64 `json:"duration"`
	}
	if json.Unmarshal(data, &body) != nil || body.Text == nil || body.Duration == nil {
		return outcome, invalidTranscription()
	}
	duration := *body.Duration
	if math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > maxTranscriptionSeconds {
		return outcome, invalidTranscription()
	}
	outcome.Units = []contract.UsageQuantity{{Unit: contract.UnitAudioInputMilliseconds, Quantity: int(math.Ceil(duration * 1000))}}
	outcome.FinishReason = contract.FinishStop
	if err := ctx.Err(); err != nil {
		return outcome, err
	}
	if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
		return outcome, err
	}
	if err := out.Usage(outcome.Units, outcome.UsageSource); err != nil {
		return outcome, err
	}
	return outcome, out.Delta(0, contract.ChannelOutputText, *body.Text)
}
