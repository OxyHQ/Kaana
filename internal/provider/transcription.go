package provider

import (
	"encoding/base64"
	"math"
	"reflect"
	"slices"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// MaxTranscriptionAudioBytes is the contract's inline audio ceiling for one
// file transcription (MAX_TRANSCRIPTION_FILE_BYTES, 20 MiB). Every provider
// Kaana transcribes with takes larger files; this is the binding bound.
const MaxTranscriptionAudioBytes = 20 << 20

// maxTranscriptionSeconds bounds a duration a provider reports. The public API
// refuses audio over 30 minutes before a grant; a provider claiming a day is a
// broken response, not a bill.
const maxTranscriptionSeconds = 24 * 60 * 60

// TranscriptionUpload is what one file transcription carries, read and
// decoded: the audio and what the caller asked of it.
type TranscriptionUpload struct {
	MediaType contract.AudioInputMediaType
	Audio     []byte
	Language  *contract.LanguageTag
	Prompt    *string
}

// ReadTranscription reads a file transcription request against the adapter's
// reviewed statement (providerconfig.TranscriptionCapabilities): one user
// message holding one inline audio part, in a container the provider
// detects, with a language and a prompt only where the statement takes them.
// Every transcription adapter reads its request here, so they refuse the same
// things the same way, and the public API's check before the grant
// (publicapi.checkTranscription) refuses them first.
func ReadTranscription(r *contract.Request, statement providerconfig.TranscriptionStatement) (TranscriptionUpload, error) {
	refuse := func(param, detail string) (TranscriptionUpload, error) {
		return TranscriptionUpload{}, ErrUnsupported{Code: contract.CodeInvalidRequest, Param: param, Detail: detail}
	}
	switch {
	case r.Stream:
		return refuse("stream", "streamed transcription is not supported by this deployment")
	case r.Modality != contract.ModalityAudio:
		return refuse("modality", "transcription requires audio modality")
	case r.AudioOutput != nil:
		return refuse("audioOutput", "spoken output is a chat_completions request")
	case r.Speech != nil || r.Reasoning != nil || r.MaxOutputTokens != nil || len(r.Tools) != 0 || r.ToolChoice != nil || r.ResponseFormat != nil || !reflect.DeepEqual(r.Sampling, contract.SamplingParameters{}):
		return refuse("parameters", "chat, speech, reasoning and sampling controls are not supported by the transcription API")
	case r.Input.Format != contract.InputMessages || len(r.Input.Messages) != 1 || r.Input.Text != nil || len(r.Input.Texts) != 0:
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
	mediaType := contract.AudioInputMediaType(*part.Source.MediaType)
	if !slices.Contains(statement.MediaTypes, mediaType) {
		return refuse("input.mediaType", "this deployment does not take "+string(mediaType)+" audio")
	}
	if len(*part.Source.Data) > base64.StdEncoding.EncodedLen(MaxTranscriptionAudioBytes) {
		return refuse("input", "audio exceeds 20 MiB")
	}
	audio, err := base64.StdEncoding.Strict().DecodeString(*part.Source.Data)
	if err != nil || len(audio) == 0 || len(audio) > MaxTranscriptionAudioBytes {
		return refuse("input", "audio must be nonempty base64 of at most 20 MiB")
	}
	upload := TranscriptionUpload{MediaType: mediaType, Audio: audio}
	if parameters := r.Transcription; parameters != nil {
		if parameters.Language != nil {
			if !slices.Contains(statement.Languages, string(*parameters.Language)) {
				return refuse("transcription.language", "this deployment names a language only from its declared list; omit it to let the model detect it")
			}
			upload.Language = parameters.Language
		}
		if parameters.Prompt != nil {
			if !statement.Prompt {
				return refuse("transcription.prompt", "this deployment takes no prompt")
			}
			upload.Prompt = parameters.Prompt
		}
	}
	return upload, nil
}

// BilledTranscriptionMilliseconds is what a provider bills for a transcription
// it reports as lasting seconds: rounded up to a whole millisecond, and never
// less than the statement's per-request minimum. ok is false for a reading
// that is not a duration.
func BilledTranscriptionMilliseconds(seconds float64, statement providerconfig.TranscriptionStatement) (milliseconds int, ok bool) {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds > maxTranscriptionSeconds {
		return 0, false
	}
	return max(int(math.Ceil(seconds*1000)), statement.MinimumBilledMilliseconds), true
}

// DetectedLanguage is the language a provider says it heard, when it says so
// as a BCP 47 tag; nil otherwise (Whisper's verbose_json names "english",
// which is a name, not a tag, and is not passed off as one).
func DetectedLanguage(reported *string) *contract.LanguageTag {
	if reported == nil {
		return nil
	}
	tag := contract.LanguageTag(*reported)
	if !tag.Valid() {
		return nil
	}
	return &tag
}
