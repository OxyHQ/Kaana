package publicapi

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strings"

	"github.com/OxyHQ/Kaana/internal/audioprobe"
	"github.com/OxyHQ/Kaana/internal/contract"
)

// Transcription bounds (`@kaana.ai/contracts` 4.1.0).
const (
	maxTranscriptionFileBytes    = 20 << 20
	maxTranscriptionAudioMs      = 30 * 60 * 1000
	maxTranscriptionPromptLength = 4096
	maxTranscriptionFieldBytes   = 16 << 10
)

// transcriptionUpload is one transcription request in either form.
type transcriptionUpload struct {
	fields    contract.TranscriptionRequest
	mediaType contract.AudioInputMediaType
	audio     []byte
}

// handleTranscriptions serves POST /v1/audio/transcriptions, as multipart (a
// `file` part and text fields) or as JSON with the audio inline in base64.
// Synchronous: the transcription is the response.
func (s *Server) handleTranscriptions(w http.ResponseWriter, r *http.Request) {
	requestID, receivedAt := NewRequestID(), s.now()
	mediaType, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var (
		body    []byte
		upload  transcriptionUpload
		failure *contract.PublicError
	)
	switch mediaType {
	case "multipart/form-data":
		body, failure = readBody(w, r, requestID, MaxTranscriptionMultipartBytes)
		if failure == nil {
			upload, failure = parseTranscriptionMultipart(requestID, body, params["boundary"])
		}
	case "application/json":
		body, failure = readBody(w, r, requestID, MaxTranscriptionJSONBytes)
		if failure == nil {
			upload, failure = parseTranscriptionJSON(requestID, body)
		}
	default:
		failure = invalid(requestID, "Content-Type", "the body is multipart/form-data or application/json")
	}
	if failure != nil {
		writeError(w, requestID, failure)
		return
	}
	if failure := validateTranscription(requestID, upload); failure != nil {
		writeError(w, requestID, failure)
		return
	}
	if failure := s.checkTranscription(requestID, upload); failure != nil {
		writeError(w, requestID, failure)
		return
	}
	// The duration is measured from the container before anything is held:
	// it sizes Oxy's hold, and an upload whose duration cannot be read is
	// refused rather than guessed from its byte count.
	measured, err := audioprobe.Milliseconds(string(upload.mediaType), upload.audio)
	if err != nil {
		writeError(w, requestID, invalid(requestID, "file", "the audio's duration cannot be read from its container; send a container that states it (WAV, FLAC, MP3, AAC, Ogg, WebM or MP4)"))
		return
	}
	if measured > maxTranscriptionAudioMs {
		writeError(w, requestID, contract.NewPublicError(requestID, "request_too_large",
			fmt.Sprintf("the audio is %d ms; a transcription takes at most %d", measured, maxTranscriptionAudioMs)).WithParam("file"))
		return
	}
	durationMs := &measured
	facts := contract.GrantRequestFacts{AudioInputMilliseconds: &measured}
	target := contract.ModelTarget(upload.fields.Model)
	admitted, failure := s.admit(r.Context(), r, requestID, receivedAt, authorization{
		operation: contract.GrantAudioTranscriptions, target: &target, body: body,
		endpoint: "/v1/audio/transcriptions", facts: facts,
	})
	if failure != nil {
		writeError(w, requestID, failure)
		return
	}
	data := base64.StdEncoding.EncodeToString(upload.audio)
	source := string(upload.mediaType)
	envelope := admitted.request(contract.ModalityAudio, contract.APIFormatAudioTranscriptions, contract.Input{
		Format: contract.InputMessages,
		Messages: []contract.Message{{
			Role: contract.RoleUser,
			Content: []contract.ContentPart{{
				Type:   contract.ContentPartAudio,
				Source: &contract.ContentSource{Kind: contract.ContentSourceInline, MediaType: &source, Data: &data},
			}},
		}},
	})
	if upload.fields.Language != nil || upload.fields.Prompt != nil {
		// Carried, never dropped: the adapter sends each in its provider's own
		// parameter (checkTranscription refused one it does not take).
		envelope.Transcription = &contract.TranscriptionParameters{Language: upload.fields.Language, Prompt: upload.fields.Prompt}
	}
	var text strings.Builder
	result := s.execute(r.Context(), admitted, &envelope, func(event contract.StreamEvent) error {
		if delta, isDelta := event.(*contract.StreamDeltaEvent); isDelta && delta.Channel == contract.ChannelOutputText {
			text.WriteString(delta.Text)
		}
		return nil
	})
	if result.Failure != nil || result.Report == nil {
		failure := contract.PublicErrorFrom(result.Failure)
		if failure == nil {
			failure = contract.NewPublicError(requestID, "internal_error", contract.PublicMessage("internal_error"))
		}
		writeError(w, requestID, failure)
		return
	}
	writeJSON(w, requestID, http.StatusOK, contract.Transcription{
		SchemaVersion: contract.SchemaVersion, RequestID: requestID, Model: result.Report.ResolvedModelReference,
		Text: text.String(), Language: result.DetectedLanguage, DurationMs: durationMs, Usage: nonNilUnits(result.Report.Units),
	})
}

// parseTranscriptionMultipart reads the form: exactly one `file` part and at
// most one of each text field, nothing else.
func parseTranscriptionMultipart(requestID contract.RequestID, body []byte, boundary string) (transcriptionUpload, *contract.PublicError) {
	var upload transcriptionUpload
	if boundary == "" {
		return upload, invalid(requestID, "Content-Type", "the multipart body names no boundary")
	}
	reader := multipart.NewReader(strings.NewReader(string(body)), boundary)
	seen := make(map[string]bool)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return upload, invalid(requestID, "", "the multipart body is malformed")
		}
		name := part.FormName()
		if seen[name] {
			return upload, invalid(requestID, name, name+" appears twice")
		}
		seen[name] = true
		switch name {
		case "file":
			declared, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
			upload.mediaType = contract.AudioInputMediaType(declared)
			audio, err := io.ReadAll(io.LimitReader(part, maxTranscriptionFileBytes+1))
			if err != nil {
				return upload, invalid(requestID, "file", "the file part could not be read")
			}
			upload.audio = audio
		case "model", "language", "prompt":
			value, err := io.ReadAll(io.LimitReader(part, maxTranscriptionFieldBytes+1))
			if err != nil || len(value) > maxTranscriptionFieldBytes {
				return upload, invalid(requestID, name, name+" is too long")
			}
			text := string(value)
			switch name {
			case "model":
				upload.fields.Model = contract.ModelReference(text)
			case "language":
				tag := contract.LanguageTag(text)
				upload.fields.Language = &tag
			case "prompt":
				upload.fields.Prompt = &text
			}
		default:
			return upload, invalid(requestID, name, fmt.Sprintf("%q is not a field of a transcription request", name))
		}
	}
	if !seen["file"] {
		return upload, invalid(requestID, "file", "the audio is the file part, and it is missing")
	}
	return upload, nil
}

// parseTranscriptionJSON reads the JSON form (transcriptionJsonRequestSchema).
func parseTranscriptionJSON(requestID contract.RequestID, body []byte) (transcriptionUpload, *contract.PublicError) {
	var request contract.TranscriptionJSONRequest
	if failure := decodeRequest(body, requestID, &request); failure != nil {
		return transcriptionUpload{}, failure
	}
	if len(request.Audio.Data) > base64.StdEncoding.EncodedLen(maxTranscriptionFileBytes) {
		return transcriptionUpload{}, contract.NewPublicError(requestID, "request_too_large", "the audio exceeds 20 MiB").WithParam("audio.data")
	}
	audio, err := base64.StdEncoding.Strict().DecodeString(request.Audio.Data)
	if err != nil {
		return transcriptionUpload{}, invalid(requestID, "audio.data", "audio.data is padded standard base64")
	}
	return transcriptionUpload{
		fields:    contract.TranscriptionRequest{Model: request.Model, Language: request.Language, Prompt: request.Prompt},
		mediaType: request.Audio.MediaType,
		audio:     audio,
	}, nil
}

// validateTranscription applies the contract's own bounds: what any model's
// request must satisfy. What one model takes is checkTranscription's.
func validateTranscription(requestID contract.RequestID, upload transcriptionUpload) *contract.PublicError {
	fields := upload.fields
	switch {
	case !fields.Model.Valid():
		return invalid(requestID, "model", "model is a model reference (publisher/model[@revision])")
	case !upload.mediaType.Valid():
		return invalid(requestID, "file", "the audio's media type is not one a transcription accepts")
	case len(upload.audio) == 0:
		return invalid(requestID, "file", "the audio is empty")
	case len(upload.audio) > maxTranscriptionFileBytes:
		return contract.NewPublicError(requestID, "request_too_large", "the audio exceeds 20 MiB").WithParam("file")
	case fields.Language != nil && !fields.Language.Valid():
		return invalid(requestID, "language", "language is a BCP 47 tag")
	case fields.Prompt != nil && utf16Length(*fields.Prompt) > maxTranscriptionPromptLength:
		return invalid(requestID, "prompt", "prompt is at most 4096 characters")
	}
	return nil
}

// checkTranscription is `checkTranscriptionRequest` of `@kaana.ai/contracts`,
// restated against the inventory's catalogue entry for the line (what every
// route of it serves): an upload in a format the model does not read is
// refused with invalid_request BEFORE a grant is asked for. The language and
// the prompt are never refused: the adapter sends the provider's spelling of
// the language, or none for the model to detect, and the prompt where the
// provider takes one, since the contract defines it as a hint (Kaana ADR
// 0003). A line whose audio is undeclared (an older snapshot) is not checked.
func (s *Server) checkTranscription(requestID contract.RequestID, upload transcriptionUpload) *contract.PublicError {
	entry, found := s.catalogueEntry(upload.fields.Model)
	if !found {
		return contract.NewPublicError(requestID, "model_not_found", contract.PublicMessage("model_not_found")).WithParam("model")
	}
	audio := entry.Audio
	if audio != nil && audio.InputMediaTypes != nil && !slices.Contains(audio.InputMediaTypes, upload.mediaType) {
		return invalid(requestID, "file", fmt.Sprintf("this model does not take %s audio", upload.mediaType))
	}
	return nil
}
