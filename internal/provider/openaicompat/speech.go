package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

const maxSpeechAudioBytes = 20 * 1024 * 1024

type speechRequest struct {
	Text         string       `json:"text"`
	Voice        string       `json:"voice_id"`
	Language     string       `json:"language"`
	OutputFormat speechFormat `json:"output_format"`
	Speed        *float64     `json:"speed,omitempty"`
}
type speechFormat struct {
	Codec      string `json:"codec"`
	SampleRate int    `json:"sample_rate"`
	BitRate    int    `json:"bit_rate"`
}

func (a *Adapter) translateSpeech(request *contract.Request, route provider.Route) (*provider.Call, error) {
	refuse := func(param, detail string) (*provider.Call, error) {
		return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: param, Detail: detail}
	}
	if a.config.Provider != "xai" || route.Provider != "xai" {
		return nil, provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: "modality", Detail: "this deployment does not expose speech synthesis"}
	}
	if request.Modality != contract.ModalityAudio || request.Input.Format != contract.InputText || request.Input.Text == nil || request.Speech == nil || request.Stream {
		return refuse("speech", "speech requires audio modality, text input, parameters and non-streaming output")
	}
	if route.UpstreamModelID != "tts" {
		return refuse("model", "this deployment is not a speech endpoint")
	}
	text := *request.Input.Text
	// Oxy's declared character ceiling is JavaScript UTF-16 length. Use the same
	// measured unit, including two units for supplementary Unicode characters.
	characters := len(utf16.Encode([]rune(text)))
	if characters == 0 || characters > 15000 {
		return refuse("input", "speech input must contain 1 to 15000 characters")
	}
	if request.Speech.ResponseFormat != "mp3" {
		return refuse("speech.responseFormat", "this speech deployment supports mp3 output")
	}
	if request.Speech.Speed != nil && (*request.Speech.Speed < 0.7 || *request.Speech.Speed > 1.5) {
		return refuse("speech.speed", "this speech deployment supports speeds from 0.7 to 1.5")
	}
	voice := request.Speech.Voice
	switch voice {
	case "female":
		voice = "eve"
	case "male":
		voice = "rex"
	case "eve", "rex", "ara", "sal", "leo":
	default:
		return refuse("speech.voice", "this speech deployment does not support the requested voice")
	}
	body, err := json.Marshal(speechRequest{Text: text, Voice: voice, Language: "auto", OutputFormat: speechFormat{Codec: "mp3", SampleRate: 24000, BitRate: 128000}, Speed: request.Speech.Speed})
	if err != nil {
		return nil, fmt.Errorf("openaicompat: encoding speech request: %w", err)
	}
	headers := make(http.Header)
	for name, value := range a.config.Headers {
		headers.Set(name, value)
	}
	return &provider.Call{Route: route, Method: http.MethodPost, URL: a.config.BaseURL + "/tts", Body: body, Header: headers}, nil
}

func (a *Adapter) streamSpeech(ctx context.Context, call *provider.Call, out provider.Emitter, credentials *provider.KeyPool) (provider.Outcome, error) {
	outcome := provider.Outcome{UsageSource: contract.UsageOxyMeasured}
	audio, ok := out.(provider.AudioEmitter)
	if !ok {
		return outcome, fmt.Errorf("openaicompat: speech requires an audio emitter")
	}
	var input speechRequest
	if err := json.Unmarshal(call.Body, &input); err != nil {
		return outcome, fmt.Errorf("openaicompat: unreadable translated speech request: %w", err)
	}
	if credentials == nil {
		credentials = a.credentials
	}
	response, key, err := provider.Walk(ctx, credentials, call, a)
	outcome.KeyID, outcome.KeyClass = key.ID, key.Class
	if err != nil {
		return outcome, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxSpeechAudioBytes+1))
	if err != nil {
		return outcome, fmt.Errorf("openaicompat: reading speech audio: %w", err)
	}
	mediaType := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	if len(data) < 4 || len(data) > maxSpeechAudioBytes || mediaType != "audio/mpeg" ||
		(string(data[:3]) != "ID3" && (data[0] != 0xff || data[1]&0xe0 != 0xe0)) {
		return outcome, provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: "speech provider returned invalid or oversized MP3 audio"}
	}
	outcome.Units = []contract.UsageQuantity{{Unit: contract.UnitCharacters, Quantity: len(utf16.Encode([]rune(input.Text)))}}
	outcome.FinishReason = contract.FinishStop
	if err := ctx.Err(); err != nil {
		return outcome, err
	}
	if err := audio.Start(call.Route.ModelReference, time.Now()); err != nil {
		return outcome, err
	}
	if err := audio.Usage(outcome.Units, outcome.UsageSource); err != nil {
		return outcome, err
	}
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return outcome, err
		}
		size := min(len(data), 49152)
		if err := audio.Audio(0, mediaType, data[:size]); err != nil {
			return outcome, err
		}
		data = data[size:]
	}
	return outcome, nil
}
