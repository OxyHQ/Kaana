package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/openaitranscription"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// File transcription on an OpenAI-compatible slug. Two wires, chosen by the
// slug's own documentation:
//
//   - xAI's `POST {base}/stt`, a multipart upload answered by one JSON
//     document (https://docs.x.ai/developers/model-capabilities/audio/speech-to-text);
//   - OpenAI's `POST {base}/audio/transcriptions`, which Groq documents
//     (https://console.groq.com/docs/speech-to-text) and
//     internal/provider/openaitranscription writes and reads.
//
// What either takes is the slug's reviewed statement
// (providerconfig.TranscriptionCapabilities), read by provider.ReadTranscription.

// sttPath is xAI's speech-to-text endpoint.
const sttPath = "/stt"

// sttFileNames is the upload name for each container xAI detects. The
// extension is a hint for its container detection; the part's Content-Type
// says the same thing.
var sttFileNames = map[contract.AudioInputMediaType]string{
	"audio/wav":  "audio.wav",
	"audio/mpeg": "audio.mp3",
	"audio/ogg":  "audio.ogg",
	"audio/flac": "audio.flac",
	"audio/aac":  "audio.aac",
	"audio/mp4":  "audio.m4a",
	"audio/webm": "audio.webm",
}

// transcriptionStatement is what this slug transcribes; stated is false for a
// slug that transcribes no file.
func (a *Adapter) transcriptionStatement() (providerconfig.TranscriptionStatement, bool) {
	return providerconfig.TranscriptionCapabilities(a.config.Provider, providerconfig.ProtocolOpenAICompatible)
}

// isTranscriptionModel reports a deployment of this slug that only
// transcribes.
func isTranscriptionModel(slug contract.ProviderSlug, upstreamModelID string) bool {
	family, classified, _ := providerconfig.ClassifyModel(slug, upstreamModelID)
	return classified && family.Format == contract.APIFormatAudioTranscriptions
}

// translateTranscription builds the slug's upload from the request read
// against its statement.
func (a *Adapter) translateTranscription(request *contract.Request, route provider.Route) (*provider.Call, error) {
	statement, stated := a.transcriptionStatement()
	if !stated || route.Provider != a.config.Provider || !isTranscriptionModel(a.config.Provider, route.UpstreamModelID) {
		return nil, provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: "modality", Detail: "this deployment does not transcribe audio"}
	}
	upload, err := provider.ReadTranscription(request, statement)
	if err != nil {
		return nil, err
	}
	var call *provider.Call
	if a.config.Provider == "xai" {
		call, err = sttCall(route, a.config.BaseURL+sttPath, upload)
	} else {
		call, err = openaitranscription.Call(route, a.config.BaseURL+"/audio/transcriptions", upload)
	}
	if err != nil {
		return nil, err
	}
	// The slug's configured headers, under the upload's own.
	headers := make(http.Header)
	for name, value := range a.config.Headers {
		headers.Set(name, value)
	}
	for name, values := range call.Header {
		headers[name] = values
	}
	call.Header = headers
	return call, nil
}

// isTranscriptionCall reports a call translateTranscription built.
func (a *Adapter) isTranscriptionCall(call *provider.Call) bool {
	if _, stated := a.transcriptionStatement(); !stated || !isTranscriptionModel(call.Route.Provider, call.Route.UpstreamModelID) {
		return false
	}
	return strings.HasSuffix(call.URL, sttPath) || strings.HasSuffix(call.URL, "/audio/transcriptions")
}

// sttCall is xAI's upload: the exact model, the caller's language when one
// was given, and the audio as the LAST field, which xAI requires. No
// formatting, diarization or filler-word default is invented for a caller who
// never asked for one.
func sttCall(route provider.Route, endpoint string, upload provider.TranscriptionUpload) (*provider.Call, error) {
	fileName, ok := sttFileNames[upload.MediaType]
	if !ok {
		return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "input.mediaType", Detail: "this deployment does not take " + string(upload.MediaType) + " audio"}
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	fields := [][2]string{{"model", route.UpstreamModelID}}
	if upload.Language != nil {
		fields = append(fields, [2]string{"language", string(*upload.Language)})
	}
	for _, field := range fields {
		if err := form.WriteField(field[0], field[1]); err != nil {
			return nil, fmt.Errorf("openaicompat: encoding %s: %w", field[0], err)
		}
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, fileName))
	header.Set("Content-Type", string(upload.MediaType))
	file, err := form.CreatePart(header)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: encoding the audio part: %w", err)
	}
	if _, err := file.Write(upload.Audio); err != nil {
		return nil, fmt.Errorf("openaicompat: encoding the audio part: %w", err)
	}
	if err := form.Close(); err != nil {
		return nil, fmt.Errorf("openaicompat: closing the upload: %w", err)
	}
	call := &provider.Call{Route: route, Method: http.MethodPost, URL: endpoint, Body: body.Bytes(), Header: make(http.Header)}
	call.Header.Set("Content-Type", form.FormDataContentType())
	call.Header.Set("Accept", "application/json")
	return call, nil
}

func invalidTranscription(slug contract.ProviderSlug) error {
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: string(slug) + " returned an invalid or oversized transcription response"}
}

// readSTT reads xAI's document: the transcript, the duration it bills by, and
// the language it detected.
func readSTT(data []byte, statement providerconfig.TranscriptionStatement) (text string, units []contract.UsageQuantity, language *contract.LanguageTag, ok bool) {
	var body struct {
		Text     *string  `json:"text"`
		Duration *float64 `json:"duration"`
		Language *string  `json:"language"`
	}
	if json.Unmarshal(data, &body) != nil || body.Duration == nil {
		return "", nil, nil, false
	}
	milliseconds, measured := provider.BilledTranscriptionMilliseconds(*body.Duration, statement)
	if !measured {
		return "", nil, nil, false
	}
	units = []contract.UsageQuantity{{Unit: contract.UnitAudioInputMilliseconds, Quantity: milliseconds}}
	if body.Text == nil {
		return "", units, nil, false
	}
	return *body.Text, units, provider.DetectedLanguage(body.Language), true
}

// streamTranscription sends the upload and emits the transcript as one text
// delta. The billed quantity is what the provider reports it bills, read
// before the transcript is believed, so a response without one is a provider
// error rather than a free request.
func (a *Adapter) streamTranscription(ctx context.Context, call *provider.Call, out provider.Emitter, credentials *provider.KeyPool) (provider.Outcome, error) {
	outcome := provider.Outcome{UsageSource: contract.UsageProviderReported}
	statement, _ := a.transcriptionStatement()
	if credentials == nil {
		credentials = a.credentials
	}
	response, key, err := provider.Walk(ctx, credentials, call, a)
	outcome.KeyID, outcome.KeyClass = key.ID, key.Class
	if err != nil {
		return outcome, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, openaitranscription.MaxResponseBytes+1))
	if err != nil {
		return outcome, fmt.Errorf("openaicompat: reading the transcription: %w", err)
	}
	if len(data) > openaitranscription.MaxResponseBytes {
		return outcome, invalidTranscription(call.Route.Provider)
	}
	var (
		text string
		ok   bool
	)
	if strings.HasSuffix(call.URL, sttPath) {
		text, outcome.Units, outcome.DetectedLanguage, ok = readSTT(data, statement)
	} else {
		var transcript openaitranscription.Transcript
		transcript, ok = openaitranscription.Read(data, statement)
		text, outcome.Units = transcript.Text, transcript.Units
	}
	if !ok {
		return outcome, invalidTranscription(call.Route.Provider)
	}
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
	return outcome, out.Delta(0, contract.ChannelOutputText, text)
}
