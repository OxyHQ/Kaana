package providerconfig

import "github.com/OxyHQ/Kaana/internal/contract"

// RealtimeStatement is what a realtime session adapter can serve, stated per
// protocol from a reviewed reading of the provider's own documentation: the
// session counterpart of ReasoningEfforts. The publisher writes it into each
// realtime deployment's inventory observation (internal/publisher), the
// catalogue serves it, and the adapter enforces the same facts — so the
// catalogue cannot advertise what a session would refuse (Kaana ADR 0002
// decision 9; Alia ADR 0017 §2 and §5).
//
// Nothing here is read from a model list: neither provider's
// `GET /v1/models` says anything about sessions, voices or languages.
type RealtimeStatement struct {
	Transports   []contract.RealtimeSessionTransport
	SessionKinds []contract.RealtimeSessionKind
	// InputAudioTranscription is whether a conversation transcribes the
	// caller's speech when asked (config.inputAudioTranscription), each turn
	// arriving as transcript.done with source input_audio.
	InputAudioTranscription bool
	// InputAudioFormats and OutputAudioFormats are the realtime encodings the
	// adapter translates for the provider.
	InputAudioFormats  []contract.RealtimeAudioFormat
	OutputAudioFormats []contract.RealtimeAudioFormat
	// Voices are the provider's documented built-in voices. Every voice speaks
	// every one of Languages (both providers document voice and language as
	// independent).
	Voices []RealtimeVoice
	// Languages are the BCP 47 tags the provider documents the model
	// conversing in. Empty: the provider documents none, which is published as
	// an empty list rather than guessed.
	Languages []string
}

// RealtimeVoice is one documented voice.
type RealtimeVoice struct {
	ID   string
	Name string
	// Gender is "female" or "male" where it has been reviewed; empty is
	// unknown and is never inferred from a name.
	Gender string
}

// realtimeFormats is every encoding the contract names: both adapters map all
// three onto the provider's own (`audio/pcm` at 24 kHz, `audio/pcmu`,
// `audio/pcma`; openairealtime.wireFormat).
var realtimeFormats = []contract.RealtimeAudioFormat{contract.RealtimePCM16, contract.RealtimeULaw, contract.RealtimeALaw}

// RealtimeCapabilities is the reviewed statement for a slug's realtime
// protocol; stated is false for a protocol that holds no session.
func RealtimeCapabilities(slug contract.ProviderSlug, protocol string) (statement RealtimeStatement, stated bool) {
	kinds := RealtimeSessionKinds(slug, protocol)
	if len(kinds) == 0 {
		return RealtimeStatement{}, false
	}
	statement = RealtimeStatement{
		Transports:              []contract.RealtimeSessionTransport{contract.RealtimeWebSocket},
		SessionKinds:            kinds,
		InputAudioTranscription: true,
		InputAudioFormats:       append([]contract.RealtimeAudioFormat(nil), realtimeFormats...),
		OutputAudioFormats:      append([]contract.RealtimeAudioFormat(nil), realtimeFormats...),
	}
	switch protocol {
	case ProtocolOpenAIRealtime:
		statement.Voices = cloneVoices(openAIRealtimeVoices)
		statement.Languages = []string{}
	case ProtocolXAIRealtime:
		statement.Voices = cloneVoices(xAIRealtimeVoices)
		statement.Languages = append([]string{}, XAIRealtimeLanguages...)
	default:
		return RealtimeStatement{}, false
	}
	return statement, true
}

func cloneVoices(voices []RealtimeVoice) []RealtimeVoice {
	return append([]RealtimeVoice(nil), voices...)
}

// openAIRealtimeVoices is OpenAI's documented Realtime voice list, read
// 2026-10-15 at https://developers.openai.com/api/docs/guides/realtime-conversations
// ("Current voice options are alloy, ash, ballad, coral, echo, sage, shimmer,
// verse, marin, and cedar"). OpenAI documents neither a gender for any of them
// nor the languages its Realtime models converse in, so neither is published
// (docs/realtime.md, "What the catalogue says about a realtime model").
var openAIRealtimeVoices = []RealtimeVoice{
	{ID: "alloy", Name: "Alloy"}, {ID: "ash", Name: "Ash"}, {ID: "ballad", Name: "Ballad"},
	{ID: "coral", Name: "Coral"}, {ID: "echo", Name: "Echo"}, {ID: "sage", Name: "Sage"},
	{ID: "shimmer", Name: "Shimmer"}, {ID: "verse", Name: "Verse"},
	{ID: "marin", Name: "Marin"}, {ID: "cedar", Name: "Cedar"},
}

// xAIRealtimeVoices is xAI's built-in voice list, read 2026-10-15 at
// https://docs.x.ai/developers/rest-api-reference/inference/voice (the 26 ids
// `GET /v1/tts/voices` returns), which the speech-to-speech guide names as the
// Voice Agent's voices ("the full list ... is in the text-to-speech voice
// table"; ids are lowercase, displayed capitalised: "e.g. Eve, Ara"). xAI
// publishes no gender for a built-in voice. The two published here are Kaana's
// existing reviewed product voices for xAI — `female` is eve and `male` is rex
// (internal/provider/openaicompat/speech.go) — and every other voice's gender
// stays unknown.
var xAIRealtimeVoices = []RealtimeVoice{
	{ID: "altair", Name: "Altair"}, {ID: "ara", Name: "Ara"}, {ID: "atlas", Name: "Atlas"},
	{ID: "carina", Name: "Carina"}, {ID: "castor", Name: "Castor"}, {ID: "celeste", Name: "Celeste"},
	{ID: "cosmo", Name: "Cosmo"}, {ID: "eve", Name: "Eve", Gender: "female"}, {ID: "helios", Name: "Helios"},
	{ID: "helix", Name: "Helix"}, {ID: "iris", Name: "Iris"}, {ID: "kepler", Name: "Kepler"},
	{ID: "leo", Name: "Leo"}, {ID: "lumen", Name: "Lumen"}, {ID: "luna", Name: "Luna"},
	{ID: "lux", Name: "Lux"}, {ID: "naksh", Name: "Naksh"}, {ID: "orion", Name: "Orion"},
	{ID: "perseus", Name: "Perseus"}, {ID: "rex", Name: "Rex", Gender: "male"}, {ID: "rigel", Name: "Rigel"},
	{ID: "sal", Name: "Sal"}, {ID: "sirius", Name: "Sirius"}, {ID: "ursa", Name: "Ursa"},
	{ID: "zagan", Name: "Zagan"}, {ID: "zenith", Name: "Zenith"},
}

// XAIRealtimeLanguages is the language list xAI's speech-to-speech guide
// documents, read 2026-10-15 at
// https://docs.x.ai/developers/model-capabilities/audio/speech-to-speech
// ("The Speech to Speech API supports 20+ languages"), with xAI's own codes.
// The same codes are what its input transcription takes as `language_hint`,
// where "for Spanish and Portuguese, you must specify a regional variant" and
// an unrecognised code is "silently ignored", which is why the adapter takes
// exactly these and refuses anything else (openairealtime, wireTranscription).
var XAIRealtimeLanguages = []string{
	"ar-AE", "ar-EG", "ar-SA", "bn", "de", "en", "es-ES", "es-MX", "fr", "hi",
	"id", "it", "ja", "ko", "pt-BR", "pt-PT", "ru", "tr", "vi", "zh",
}
