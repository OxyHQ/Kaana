package providerconfig

import (
	"slices"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// TranscriptionStatement is what a file transcription adapter takes, stated
// per slug from a reviewed reading of the provider's own documentation: the
// transcription counterpart of RealtimeStatement. The publisher writes it into
// each transcription deployment's inventory observation, the catalogue
// publishes it as the model's `audio`, the public API refuses a request it
// does not serve before a grant is asked for, and the adapter sends exactly
// what it states — so the catalogue cannot advertise an upload the adapter
// would refuse.
type TranscriptionStatement struct {
	// MediaTypes are the uploads the provider detects, sorted.
	MediaTypes []contract.AudioInputMediaType
	// Languages are the tags the provider takes as `language`; a client's tag
	// is normalized onto them (NormalizeLanguage), and one the provider takes
	// nothing of is not sent, so the model detects it.
	Languages []string
	// DetectsLanguage is a model that detects the spoken language when a
	// request names none.
	DetectsLanguage bool
	// Prompt is a provider that takes free-text context in `prompt`.
	Prompt bool
	// MinimumBilledMilliseconds is the least a provider bills one request
	// for, whatever the audio's length (Groq: 10 seconds); the adapter reports
	// the larger of the two, because that is the charge. Zero: none.
	MinimumBilledMilliseconds int
}

// TranscriptionCapabilities is the reviewed statement for a slug's file
// transcription; stated is false for a slug that transcribes no file.
func TranscriptionCapabilities(slug contract.ProviderSlug, protocol string) (statement TranscriptionStatement, stated bool) {
	if !slices.Contains(ExecutableAPIFormats(slug, protocol), contract.APIFormatAudioTranscriptions) {
		return TranscriptionStatement{}, false
	}
	switch {
	case protocol == ProtocolOpenAICompatible && slug == "xai":
		statement = xAITranscription
	case protocol == ProtocolOpenAICompatible && slug == "groq":
		statement = groqTranscription
	case protocol == ProtocolOpenAIAudio:
		statement = openAITranscription
	case protocol == ProtocolDeepgramVoice:
		statement = deepgramTranscription
	default:
		return TranscriptionStatement{}, false
	}
	statement.MediaTypes = slices.Clone(statement.MediaTypes)
	statement.Languages = slices.Clone(statement.Languages)
	return statement, true
}

// LanguageFor is the tag to send for the language a client named, or false to
// send none and let the model detect it.
func (s TranscriptionStatement) LanguageFor(tag string) (string, bool) {
	return NormalizeLanguage(tag, s.Languages, nil)
}

// xAITranscription is xAI's `POST /v1/stt`, read 2026-10-16 at
// https://docs.x.ai/developers/model-capabilities/audio/speech-to-text: it
// auto-detects WAV, MP3, OGG/Opus, FLAC, AAC, MP4/M4A and MKV (WebM is
// Matroska); `language` is optional ("detects the spoken language
// automatically") and takes the codes of its language table, where Spanish,
// Portuguese and Arabic also have regional variants; there is no prompt field
// (its `keyterm` is a list of terms, not context).
var xAITranscription = TranscriptionStatement{
	MediaTypes: []contract.AudioInputMediaType{"audio/aac", "audio/flac", "audio/mp4", "audio/mpeg", "audio/ogg", "audio/wav", "audio/webm"},
	Languages: []string{
		"ar", "ar-AE", "ar-EG", "ar-SA", "bg", "bs", "ca", "cs", "da", "de", "el", "en", "es", "es-ES", "es-MX",
		"fa", "fi", "fil", "fr", "hi", "hr", "hu", "id", "it", "ja", "ko", "mk", "ms", "nb", "nl", "pl", "pt",
		"pt-BR", "pt-PT", "ro", "ru", "sk", "sv", "th", "tr", "uk", "ur", "vi", "yue", "zh",
	},
	DetectsLanguage: true,
}

// groqTranscription is Groq's OpenAI-compatible
// `POST /openai/v1/audio/transcriptions`, read 2026-10-16 at
// https://console.groq.com/docs/speech-to-text: "flac, mp3, mp4, mpeg, mpga,
// m4a, ogg, wav, webm"; `language` is an ISO-639-1 code, sent for the
// languages Whisper transcribes (whisperLanguages); a `prompt` of up to 224
// tokens; "Minimum billed length: 10 seconds".
var groqTranscription = TranscriptionStatement{
	MediaTypes:                []contract.AudioInputMediaType{"audio/flac", "audio/mp4", "audio/mpeg", "audio/ogg", "audio/wav", "audio/webm"},
	Languages:                 whisperLanguages,
	DetectsLanguage:           true,
	Prompt:                    true,
	MinimumBilledMilliseconds: 10_000,
}

// openAITranscription is OpenAI's `POST /v1/audio/transcriptions`, read
// 2026-10-16 at https://developers.openai.com/api/docs/guides/speech-to-text:
// "mp3, mp4, mpeg, mpga, m4a, wav, and webm" (ogg and flac are what this
// adapter has served since it was reviewed, docs/openai-audio.md); `language`
// in "ISO 639-1 codes, such as en, es, and fr" (the same guide), sent for the
// languages Whisper transcribes (whisperLanguages); a `prompt`.
var openAITranscription = TranscriptionStatement{
	MediaTypes:      []contract.AudioInputMediaType{"audio/flac", "audio/mp4", "audio/mpeg", "audio/ogg", "audio/wav", "audio/webm"},
	Languages:       whisperLanguages,
	DetectsLanguage: true,
	Prompt:          true,
}

// deepgramTranscription is Deepgram's pre-recorded `POST /v1/listen` with a
// raw body, which this adapter sends only for self-describing containers
// (WAV, MP3, FLAC, Ogg); no language is sent, so the model detects it, and no
// prompt exists.
var deepgramTranscription = TranscriptionStatement{
	MediaTypes:      []contract.AudioInputMediaType{"audio/flac", "audio/mpeg", "audio/ogg", "audio/wav"},
	Languages:       []string{},
	DetectsLanguage: true,
}
