package publicapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/sse"
)

// maxSpeechInputCharacters is MAX_SPEECH_INPUT_CHARACTERS.
const maxSpeechInputCharacters = 100_000

// maxSpeechAudioBytes bounds the audio one non-streamed answer may buffer
// (MAX_INFERENCE_AUDIO_BYTES).
const maxSpeechAudioBytes = 20 << 20

// handleSpeech serves POST /v1/audio/speech (ADR 0002, decisions 8 and 9):
// a whole file, raw bytes as they arrive (`stream_format: "audio"`) or typed
// SSE frames (`stream_format: "sse"`).
func (s *Server) handleSpeech(w http.ResponseWriter, r *http.Request) {
	requestID, receivedAt := NewRequestID(), s.now()
	if failure := requireJSON(r, requestID); failure != nil {
		writeError(w, requestID, failure)
		return
	}
	body, failure := readBody(w, r, requestID, MaxJSONBodyBytes)
	if failure != nil {
		writeError(w, requestID, failure)
		return
	}
	var request contract.SpeechRequest
	if failure := decodeRequest(body, requestID, &request); failure != nil {
		writeError(w, requestID, failure)
		return
	}
	if failure := s.validateSpeech(requestID, request); failure != nil {
		writeError(w, requestID, failure)
		return
	}
	stream := request.Stream != nil && *request.Stream
	characters := utf16Length(request.Input)
	target := contract.ModelTarget(request.Model)
	ask := authorization{
		operation: contract.GrantAudioSpeech, target: &target, body: body, endpoint: "/v1/audio/speech",
		facts: contract.GrantRequestFacts{InputCharacters: &characters, Stream: stream},
	}
	if request.User != nil {
		user := contract.UserID(*request.User)
		ask.onBehalfOf = &user
	}
	admitted, failure := s.admit(r.Context(), r, requestID, receivedAt, ask)
	if failure != nil {
		writeError(w, requestID, failure)
		return
	}
	format := speechFormatOf(request)
	envelope := admitted.request(contract.ModalityAudio, contract.APIFormatAudioSpeech, contract.Input{Format: contract.InputText, Text: &request.Input})
	envelope.Speech = &contract.SpeechParameters{Voice: request.Voice, ResponseFormat: format, Speed: request.Speed}
	mediaType := string(speechMediaType(format))
	if format == "pcm" {
		mediaType = "audio/pcm;rate=24000"
	}

	switch {
	case !stream:
		s.speechWhole(r.Context(), w, admitted, &envelope, mediaType)
	case request.StreamFormat != nil && *request.StreamFormat == contract.SpeechStreamSSE:
		s.speechSSE(r.Context(), w, admitted, &envelope)
	default:
		s.speechRaw(r.Context(), w, admitted, &envelope, mediaType)
	}
}

func (s *Server) validateSpeech(requestID contract.RequestID, request contract.SpeechRequest) *contract.PublicError {
	switch {
	case !request.Model.Valid():
		return invalid(requestID, "model", "model is a model reference (publisher/model[@revision]); a power level cannot name a voice")
	case request.Input == "" || utf16Length(request.Input) > maxSpeechInputCharacters:
		return invalid(requestID, "input", "input is 1 to 100000 characters")
	case request.Voice == "" || len(request.Voice) > 64:
		return invalid(requestID, "voice", "voice is 1 to 64 characters")
	case request.ResponseFormat != nil && !request.ResponseFormat.Valid():
		return invalid(requestID, "response_format", "response_format is mp3, opus, aac, flac, wav or pcm")
	case request.Speed != nil && (*request.Speed < 0.25 || *request.Speed > 4):
		return invalid(requestID, "speed", "speed is between 0.25 and 4")
	case request.StreamFormat != nil && (request.Stream == nil || !*request.Stream):
		return invalid(requestID, "stream_format", "stream_format applies only to a streamed request (stream: true)")
	case request.StreamFormat != nil && *request.StreamFormat != contract.SpeechStreamAudio && *request.StreamFormat != contract.SpeechStreamSSE:
		return invalid(requestID, "stream_format", "stream_format is audio or sse")
	case request.Stream != nil && *request.Stream && !slices.Contains([]contract.SpeechFormat{"mp3", "pcm"}, speechFormatOf(request)):
		return invalid(requestID, "response_format", "streamed speech is mp3 or pcm; "+string(speechFormatOf(request))+" exists only whole")
	case request.User != nil && (*request.User == "" || len(*request.User) > 64):
		return invalid(requestID, "user", "user is 1 to 64 characters")
	}
	if s.audio != nil {
		if published, known := s.audio.Audio(request.Model); known {
			if issues := checkSpeech(request, published); len(issues) > 0 {
				return invalid(requestID, issues[0].param, issues[0].message)
			}
		}
	}
	return nil
}

// speechWhole buffers the whole file: the status is decided at the end, so a
// failure at any point is an HTTP error and nothing partial is sent.
func (s *Server) speechWhole(ctx context.Context, w http.ResponseWriter, admitted *admission, envelope *contract.Request, mediaType string) {
	var audio []byte
	oversized := false
	result := s.execute(ctx, admitted, envelope, func(event contract.StreamEvent) error {
		if chunk, isAudio := event.(*contract.StreamAudioEvent); isAudio {
			decoded, err := base64.StdEncoding.DecodeString(chunk.Data)
			if err != nil {
				return err
			}
			if len(audio)+len(decoded) > maxSpeechAudioBytes {
				oversized = true
				return errAnswerTooLarge
			}
			audio = append(audio, decoded...)
		}
		return nil
	})
	switch {
	case result.Failure != nil && !oversized:
		writeError(w, admitted.requestID, contract.PublicErrorFrom(result.Failure))
		return
	case oversized:
		writeError(w, admitted.requestID, contract.NewPublicError(admitted.requestID, "output_limit_exceeded", "the synthesized audio exceeds 20 MiB"))
		return
	case result.Report == nil || len(audio) == 0:
		writeError(w, admitted.requestID, contract.NewPublicError(admitted.requestID, "provider_error", contract.PublicMessage("provider_error")))
		return
	}
	header := w.Header()
	header.Set(HeaderRequestID, string(admitted.requestID))
	header.Set(HeaderModel, string(result.Report.ResolvedModelReference))
	header.Set(HeaderUsage, usageHeader(result.Report.Units))
	header.Set("Content-Type", mediaType)
	header.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(audio)
}

// speechRaw relays the audio bytes as the provider produces them (the
// OpenAI-compatible stream). A failure before the first byte is an HTTP
// error; one after it cuts the connection, and the settlement keeps the code.
func (s *Server) speechRaw(ctx context.Context, w http.ResponseWriter, admitted *admission, envelope *contract.Request, mediaType string) {
	flusher, canFlush := w.(http.Flusher)
	started := false
	result := s.execute(ctx, admitted, envelope, func(event contract.StreamEvent) error {
		chunk, isAudio := event.(*contract.StreamAudioEvent)
		if !isAudio {
			return nil
		}
		decoded, err := base64.StdEncoding.DecodeString(chunk.Data)
		if err != nil {
			return err
		}
		if !started {
			started = true
			header := w.Header()
			header.Set(HeaderRequestID, string(admitted.requestID))
			header.Set("Content-Type", mediaType)
			header.Set("Cache-Control", "no-store")
			header.Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
		}
		if _, err := w.Write(decoded); err != nil {
			return errClientGone(err)
		}
		if canFlush {
			flusher.Flush()
		}
		return nil
	})
	if !started {
		failure := result.Failure
		if failure == nil {
			failure = contract.NewError(admitted.requestID, contract.CodeProviderError, "no audio")
		}
		writeError(w, admitted.requestID, contract.PublicErrorFrom(failure))
		return
	}
	if result.Failure != nil {
		// The status already sent stands; a cut connection is the only signal
		// left, and the client reads it as an incomplete body.
		panic(http.ErrAbortHandler)
	}
}

// speechSSE carries the audio as typed frames so a failure mid-stream arrives
// as a typed error (what @kaana.ai/sdk's speechStream uses).
func (s *Server) speechSSE(ctx context.Context, w http.ResponseWriter, admitted *admission, envelope *contract.Request) {
	var writer *sse.Writer
	result := s.execute(ctx, admitted, envelope, func(event contract.StreamEvent) error {
		chunk, isAudio := event.(*contract.StreamAudioEvent)
		if !isAudio {
			return nil
		}
		if writer == nil {
			opened, err := openSSE(w, admitted.requestID)
			if err != nil {
				return err
			}
			writer = opened
		}
		return writeFrame(writer, "speech.audio.delta", contract.SpeechAudioDeltaEvent{Type: "speech.audio.delta", Audio: chunk.Data})
	})
	if writer == nil {
		failure := result.Failure
		if failure == nil {
			failure = contract.NewError(admitted.requestID, contract.CodeProviderError, "no audio")
		}
		writeError(w, admitted.requestID, contract.PublicErrorFrom(failure))
		return
	}
	if result.Failure != nil || result.Report == nil {
		failure := contract.PublicErrorFrom(result.Failure)
		if failure == nil {
			failure = contract.NewPublicError(admitted.requestID, "internal_error", contract.PublicMessage("internal_error"))
		}
		_ = writeFrame(writer, "error", contract.SpeechStreamErrorEvent{Type: "error", Error: *failure})
		return
	}
	_ = writeFrame(writer, "speech.audio.done", contract.SpeechAudioDoneEvent{
		Type: "speech.audio.done", RequestID: admitted.requestID,
		Model: result.Report.ResolvedModelReference, Usage: nonNilUnits(result.Report.Units),
	})
}

// openSSE commits a 200 event stream.
func openSSE(w http.ResponseWriter, requestID contract.RequestID) (*sse.Writer, error) {
	w.Header().Set(HeaderRequestID, string(requestID))
	return sse.NewWriter(w)
}

// writeFrame writes one `event: <type>` frame whose data is payload.
func writeFrame(writer *sse.Writer, name string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := writer.WriteEvent(name, encoded); err != nil {
		return errClientGone(err)
	}
	return nil
}
