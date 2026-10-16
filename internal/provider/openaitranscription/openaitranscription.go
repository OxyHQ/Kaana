// Package openaitranscription is OpenAI's file transcription wire,
// `POST {base}/audio/transcriptions`: a multipart upload answered by one JSON
// document. OpenAI serves it (internal/provider/openaiaudio) and so does every
// provider that documents the same endpoint (Groq, through
// internal/provider/openaicompat); this package is the one place it is
// written and read, so the two cannot drift.
package openaitranscription

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// MaxResponseBytes bounds the transcript document. A transcript of 20 MiB of
// audio is far smaller; verbose_json segments are the largest shape asked for.
const MaxResponseBytes = 8 << 20

// fileNames is the upload name for each media type. The endpoint infers the
// container from the file name's extension, so the name is part of the wire
// contract rather than decoration.
var fileNames = map[contract.AudioInputMediaType]string{
	"audio/wav":  "audio.wav",
	"audio/mpeg": "audio.mp3",
	"audio/flac": "audio.flac",
	"audio/ogg":  "audio.ogg",
	"audio/webm": "audio.webm",
	"audio/mp4":  "audio.m4a",
}

// FileName is the upload name of a media type the statement accepts.
func FileName(mediaType contract.AudioInputMediaType) (string, bool) {
	name, ok := fileNames[mediaType]
	return name, ok
}

// responseFormat is the response this package reads. A Whisper model's plain
// `json` carries no duration, which is what it is billed by, so it is asked
// for `verbose_json`, whose top-level `duration` is; every other model reports
// its usage in `json`.
func responseFormat(upstreamModelID string) string {
	if strings.HasPrefix(strings.ToLower(upstreamModelID), "whisper") {
		return "verbose_json"
	}
	return "json"
}

// Call builds the upload at endpoint: the exact model, the response format
// read here, the caller's language and prompt where it gave them, and the
// audio. No temperature or chunking default is invented for a caller who
// never set one.
func Call(route provider.Route, endpoint string, upload provider.TranscriptionUpload) (*provider.Call, error) {
	fileName, ok := FileName(upload.MediaType)
	if !ok {
		return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "input.mediaType", Detail: "this deployment does not take " + string(upload.MediaType) + " audio"}
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	fields := [][2]string{{"model", route.UpstreamModelID}, {"response_format", responseFormat(route.UpstreamModelID)}}
	if upload.Language != nil {
		fields = append(fields, [2]string{"language", string(*upload.Language)})
	}
	if upload.Prompt != nil {
		fields = append(fields, [2]string{"prompt", *upload.Prompt})
	}
	for _, field := range fields {
		if err := form.WriteField(field[0], field[1]); err != nil {
			return nil, fmt.Errorf("openaitranscription: encoding %s: %w", field[0], err)
		}
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, fileName))
	header.Set("Content-Type", string(upload.MediaType))
	file, err := form.CreatePart(header)
	if err != nil {
		return nil, fmt.Errorf("openaitranscription: encoding the audio part: %w", err)
	}
	if _, err := file.Write(upload.Audio); err != nil {
		return nil, fmt.Errorf("openaitranscription: encoding the audio part: %w", err)
	}
	if err := form.Close(); err != nil {
		return nil, fmt.Errorf("openaitranscription: closing the upload: %w", err)
	}
	call := &provider.Call{Route: route, Method: http.MethodPost, URL: endpoint, Body: body.Bytes(), Header: make(http.Header)}
	call.Header.Set("Content-Type", form.FormDataContentType())
	call.Header.Set("Accept", "application/json")
	return call, nil
}

// Transcript is what a response reports.
type Transcript struct {
	Text  string
	Units []contract.UsageQuantity
}

// document is the part of the json and verbose_json responses read here.
type document struct {
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

// Read reads a response. ok is false for one that is not a transcript with a
// usage this package can settle.
//
// The usage is copied exactly, in one of two shapes:
//
//   - `duration` (Whisper, and per-minute-priced models): the audio length in
//     seconds, which is `audio_input_milliseconds`, rounded up and never below
//     the statement's per-request minimum (Groq bills at least 10 seconds).
//   - `tokens` (token-priced models): `input_tokens` and `output_tokens`. The
//     input count is mostly audio tokens and the contract has no audio-token
//     unit for a file, so the total is reported as `input_tokens` without a
//     split: the provider's own count, not an estimate.
//
// A response with neither is refused rather than settled on a guess, and the
// units are read before the text, because the audio was billed whether or not
// a transcript comes back.
func Read(data []byte, statement providerconfig.TranscriptionStatement) (Transcript, bool) {
	var body document
	if json.Unmarshal(data, &body) != nil {
		return Transcript{}, false
	}
	units, measured := body.units(statement)
	if !measured || body.Text == nil {
		return Transcript{Units: units}, false
	}
	return Transcript{Text: *body.Text, Units: units}, true
}

func (d document) units(statement providerconfig.TranscriptionStatement) ([]contract.UsageQuantity, bool) {
	seconds := d.Duration
	if d.Usage != nil {
		switch d.Usage.Type {
		case "duration":
			seconds = d.Usage.Seconds
		case "tokens":
			in, out := d.Usage.InputTokens, d.Usage.OutputTokens
			if in == nil || out == nil || *in < 0 || *out < 0 || (d.Usage.TotalTokens != nil && *d.Usage.TotalTokens != *in+*out) {
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
	if seconds == nil {
		return nil, false
	}
	milliseconds, ok := provider.BilledTranscriptionMilliseconds(*seconds, statement)
	if !ok {
		return nil, false
	}
	return []contract.UsageQuantity{{Unit: contract.UnitAudioInputMilliseconds, Quantity: milliseconds}}, true
}
