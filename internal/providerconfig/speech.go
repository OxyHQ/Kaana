package providerconfig

import (
	"fmt"
	"slices"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// SpeechStatement is what a speech synthesis adapter takes, stated per
// deployment from a reviewed reading of the provider's own documentation: the
// speech counterpart of TranscriptionStatement. The publisher writes it into
// each speech deployment's inventory observation, the catalogue publishes it
// as the model's `audio`, the public API refuses a request it does not serve
// before a grant is asked for (checkSpeech), and the adapter enforces exactly
// it (Refusal) — so the catalogue cannot advertise a voice, a format or a
// speed the route would refuse, and a client can choose a voice from it.
type SpeechStatement struct {
	// Voices are the voices a request may name, by id.
	Voices []Voice
	// Aliases are names a request may send for a voice that are not voices of
	// their own (a product's `female`/`male`), mapped to the voice id. They
	// are never published: the catalogue lists only Voices.
	Aliases map[string]string
	// Languages are the BCP 47 tags the provider documents the voices
	// speaking. Empty: the provider documents none to choose from, published
	// as an empty list rather than guessed.
	Languages []string
	// OutputMediaTypes are the media types the adapter can produce, sorted.
	OutputMediaTypes []contract.AudioMediaType
	// Speed is the inclusive range of `speed` the provider takes; nil takes
	// no speed control.
	Speed *contract.NumberRange
	// MaxInputCharacters is the longest input one request may carry, in
	// UTF-16 code units, as the contract counts it.
	MaxInputCharacters int
}

// SpeechCapabilities is the reviewed statement for one speech deployment;
// stated is false for a deployment that synthesizes no speech.
func SpeechCapabilities(slug contract.ProviderSlug, protocol string, upstreamModelID string) (statement SpeechStatement, stated bool) {
	if !slices.Contains(ExecutableAPIFormats(slug, protocol), contract.APIFormatAudioSpeech) {
		return SpeechStatement{}, false
	}
	switch {
	case protocol == ProtocolOpenAICompatible && slug == "xai" && upstreamModelID == XAISpeechEndpoint:
		statement = xAISpeech()
	default:
		return SpeechStatement{}, false
	}
	return statement, true
}

// VoiceFor is the voice id a request's voice names (itself, or the voice an
// alias stands for), or false for a voice the statement does not take.
func (s SpeechStatement) VoiceFor(requested string) (string, bool) {
	if alias, aliased := s.Aliases[requested]; aliased {
		return alias, true
	}
	if slices.ContainsFunc(s.Voices, func(voice Voice) bool { return voice.ID == requested }) {
		return requested, true
	}
	return "", false
}

// Refusal is the first request field the statement does not serve and why,
// in the order the adapter has always refused them: input, format, speed,
// voice. characters is the input's UTF-16 length. refused is false when the
// statement serves the request.
func (s SpeechStatement) Refusal(characters int, parameters contract.SpeechParameters) (param, detail string, refused bool) {
	switch {
	case characters < 1 || characters > s.MaxInputCharacters:
		return "input", fmt.Sprintf("speech input must contain 1 to %d characters", s.MaxInputCharacters), true
	case !slices.Contains(s.OutputMediaTypes, parameters.ResponseFormat.MediaType()):
		return "speech.responseFormat", fmt.Sprintf("this speech deployment does not produce %s", parameters.ResponseFormat), true
	case parameters.Speed != nil && s.Speed == nil:
		return "speech.speed", "this speech deployment accepts no speed control", true
	case parameters.Speed != nil && (*parameters.Speed < s.Speed.Min || *parameters.Speed > s.Speed.Max):
		return "speech.speed", fmt.Sprintf("this speech deployment supports speeds from %g to %g", s.Speed.Min, s.Speed.Max), true
	}
	if _, known := s.VoiceFor(parameters.Voice); !known {
		return "speech.voice", "this speech deployment does not support the requested voice", true
	}
	return "", "", false
}

// XAISpeechEndpoint is Kaana's upstream identity for xAI's model-less
// `POST /v1/tts`: xAI's `/models` does not list it, and the publisher adds it
// from the authenticated `/tts/voices` read (xai_models_and_speech).
const XAISpeechEndpoint = "tts"

// xAISpeechVoiceIDs are the voices Kaana's xAI speech route takes: the voice
// ids reviewed with the route on 2026-09-13
// (https://docs.x.ai/developers/model-capabilities/audio/text-to-speech;
// docs/adapters.md, "xAI speech candidate"), each one of the built-in ids xAI
// documents. Their names and genders are the reviewed voice table's
// (xAIRealtimeVoices), the same xAI ids: gender is published for eve and rex
// only.
var xAISpeechVoiceIDs = []string{"ara", "eve", "leo", "rex", "sal"}

// xAISpeechAliases are Oxy's product voice names, which Gemies still sends
// through Oxy's signed lane (its `/v1/audio` takes `female` or `male`): Kaana's
// reviewed product voices for xAI. They are why eve is `female` and rex
// `male` in the voice table.
var xAISpeechAliases = map[string]string{"female": "eve", "male": "rex"}

// xAISpeech is xAI's `POST /v1/tts`, reviewed 2026-09-13 against
// https://docs.x.ai/developers/model-capabilities/audio/text-to-speech and a
// real Spanish probe that returned a decodable MP3: the adapter asks for MP3
// (24 kHz, 128 kbps) and nothing else, `speed` from 0.7 to 1.5, and `text` of
// at most 15 000 characters. `language` is sent as `auto`, xAI's detection,
// so no language is published for a request to choose.
func xAISpeech() SpeechStatement {
	voices := make([]Voice, 0, len(xAISpeechVoiceIDs))
	for _, voice := range xAIRealtimeVoices {
		if slices.Contains(xAISpeechVoiceIDs, voice.ID) {
			voices = append(voices, voice)
		}
	}
	aliases := make(map[string]string, len(xAISpeechAliases))
	for alias, voice := range xAISpeechAliases {
		aliases[alias] = voice
	}
	return SpeechStatement{
		Voices:             voices,
		Aliases:            aliases,
		Languages:          []string{},
		OutputMediaTypes:   []contract.AudioMediaType{"audio/mpeg"},
		Speed:              &contract.NumberRange{Min: 0.7, Max: 1.5},
		MaxInputCharacters: 15000,
	}
}
