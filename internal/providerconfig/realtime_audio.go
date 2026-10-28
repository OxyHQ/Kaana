package providerconfig

import (
	"slices"

	"github.com/OxyHQ/Kaana/internal/contract"
)

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
	// conversing in, and exactly what config.inputAudioTranscription.language
	// may name. Empty: the provider documents none, which is published as an
	// empty list rather than guessed.
	Languages []string
	// LanguageDefaults is the variant sent for a language the client names in
	// a region the provider does not list (`es-AR` → `es-MX`), keyed by the
	// language alone (NormalizeLanguage).
	LanguageDefaults map[string]string
	// DetectsLanguage is a model that detects the spoken language when a
	// session names none: a client's language the provider takes nothing of
	// is not sent, and the model detects it.
	DetectsLanguage bool
	// Parameters is what the provider's wire takes. Kaana's own knowledge: the
	// dialect (internal/provider/openairealtime) translates and emulates every
	// contract value onto it and refuses only what cannot be served; it never
	// reaches a client (Kaana ADR 0003).
	Parameters contract.RealtimeParameters
}

// LanguageFor is the tag to send for the language a client named, or false to
// send none and let the model detect it.
func (s RealtimeStatement) LanguageFor(tag string) (string, bool) {
	return NormalizeLanguage(tag, s.Languages, s.LanguageDefaults)
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
	if protocol == ProtocolXAISTT {
		return xAISTTStatement(kinds), true
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
		statement.DetectsLanguage = true
		statement.Parameters = openAIRealtimeParameters()
	case ProtocolXAIRealtime:
		statement.Voices = cloneVoices(xAIRealtimeVoices)
		statement.Languages = append([]string{}, XAIRealtimeLanguages...)
		statement.LanguageDefaults = map[string]string{"ar": "ar-SA", "es": "es-MX", "pt": "pt-BR"}
		statement.DetectsLanguage = true
		statement.Parameters = xAIRealtimeParameters()
	default:
		return RealtimeStatement{}, false
	}
	return statement, true
}

// openAIRealtimeTokenUnits is how OpenAI bills a Realtime session under every
// turn detection: the tokens response.done reports, split as the contract
// partitions them (openairealtime, usage.units), and the input transcription
// model's minutes, billed apart.
var openAIRealtimeTokenUnits = []contract.UsageUnit{
	contract.UnitAudioInputTokens, contract.UnitAudioOutputTokens, contract.UnitCachedAudioInputTokens,
	contract.UnitCachedInputTokens, contract.UnitInputTokens, contract.UnitInputTranscriptionMilliseconds,
	contract.UnitOutputTokens,
}

// openAIRealtimeParameters is OpenAI's GA Realtime session, reviewed
// 2026-10-15 against
// https://developers.openai.com/api/reference/resources/realtime/client-events:
// push-to-talk (`turn_detection: null`), server_vad and semantic_vad, each
// with `create_response` and `interrupt_response`; exactly one of `text` or
// `audio` per response; `tool_choice` as a mode or a named function;
// `max_output_tokens` up to 4096; an input transcription `prompt`. No
// `temperature` (removed from the GA schema).
func openAIRealtimeParameters() contract.RealtimeParameters {
	maxOutputTokens := 4096
	detection := func(kind contract.RealtimeTurnDetectionType) contract.RealtimeTurnDetectionParameters {
		return contract.RealtimeTurnDetectionParameters{Type: kind, MeteredUnits: slices.Clone(openAIRealtimeTokenUnits)}
	}
	return contract.RealtimeParameters{
		TurnDetection: []contract.RealtimeTurnDetectionParameters{
			detection(contract.TurnDetectionNone), detection(contract.TurnDetectionServerVAD), detection(contract.TurnDetectionSemanticVAD),
		},
		OutputModalities: [][]contract.RealtimeOutputModality{{contract.RealtimeOutputAudio}, {contract.RealtimeOutputText}},
		ToolChoice: []contract.RealtimeToolChoice{
			contract.RealtimeToolChoiceAuto, contract.RealtimeToolChoiceNone, contract.RealtimeToolChoiceRequired, contract.RealtimeToolChoiceFunction,
		},
		MaxOutputTokens:          &maxOutputTokens,
		InputTranscriptionPrompt: true,
	}
}

// xAIRealtimeParameters is xAI's Voice Agent session, reviewed 2026-09-30
// against https://docs.x.ai/voice-realtime.ws.json and
// https://docs.x.ai/developers/pricing:
//
//   - `turn_detection` is `{"type": null}` or `server_vad` with `threshold`
//     0.1-0.9 and `prefix_padding_ms`/`silence_duration_ms` 0-10000, and no
//     `create_response` or `interrupt_response`: its VAD always answers and is
//     interruptible.
//   - "Push-to-talk sessions are billed only for audio sent and received";
//     "sessions using the default server_vad turn detection are billed for
//     session duration"; each text conversation.item.create is a request.
//   - It answers every response aloud whatever modalities it is sent
//     (measured 2026-09-30), so a set without audio is not served.
//   - No `tool_choice` (auto, its behaviour, is served by sending nothing), no
//     `max_output_tokens`, no transcription prompt.
func xAIRealtimeParameters() contract.RealtimeParameters {
	return contract.RealtimeParameters{
		TurnDetection: []contract.RealtimeTurnDetectionParameters{
			{
				Type:         contract.TurnDetectionNone,
				MeteredUnits: []contract.UsageUnit{contract.UnitAudioInputMilliseconds, contract.UnitAudioOutputMilliseconds, contract.UnitRequests},
			},
			{
				Type: contract.TurnDetectionServerVAD, FixedResponses: true,
				Threshold:    &contract.NumberRange{Min: 0.1, Max: 0.9},
				MeteredUnits: []contract.UsageUnit{contract.UnitRequests, contract.UnitSessionMilliseconds},
			},
		},
		OutputModalities: [][]contract.RealtimeOutputModality{{contract.RealtimeOutputAudio}, {contract.RealtimeOutputAudio, contract.RealtimeOutputText}},
		ToolChoice:       []contract.RealtimeToolChoice{contract.RealtimeToolChoiceAuto},
	}
}

// xAISTTStatement is xAI's streaming speech-to-text, reviewed 2026-10-28
// against https://docs.x.ai/developers/model-capabilities/audio/speech-to-text:
//
//   - one WebSocket, `wss://api.x.ai/v1/stt`, configured by query parameters
//     only; raw binary audio frames in `pcm` (8 000 to 48 000 Hz), `mulaw` or
//     `alaw`, so every contract encoding is served (24 kHz PCM16, G.711 at
//     8 kHz);
//   - it transcribes and never answers: no output audio, no responses, no
//     tools, no output token cap;
//   - `endpointing` (0 to 5000 ms of silence) ends an utterance, which serves
//     server_vad's `silenceDurationMs`, and `{"type":"finalize"}` ends one
//     at once, which serves push-to-talk; no threshold or prefix padding, and
//     `smart_turn` is not semantic_vad;
//   - `language` takes the codes of the same table as `POST /v1/stt`
//     (TranscriptionCapabilities), and is optional: the model detects the
//     language; no prompt field (its `keyterm` is a term list).
//
// Billing: "Speech to Text $0.20 / hr (Streaming)" (https://docs.x.ai/developers/models)
// without saying whether audio or connection time is counted, so a session is
// metered by its wall clock, `session_milliseconds`, which bounds either
// (Kaana ADR 0006 §4).
//
// It states no voices and no audio of its own: the languages it transcribes
// are the line's file transcription's statement, which the catalogue already
// publishes, and a second audio statement would only narrow the line's upload
// types (inventory.aggregateAudio).
func xAISTTStatement(kinds []contract.RealtimeSessionKind) RealtimeStatement {
	sessionClock := []contract.UsageUnit{contract.UnitSessionMilliseconds}
	return RealtimeStatement{
		Transports:         []contract.RealtimeSessionTransport{contract.RealtimeWebSocket},
		SessionKinds:       kinds,
		InputAudioFormats:  append([]contract.RealtimeAudioFormat(nil), realtimeFormats...),
		OutputAudioFormats: []contract.RealtimeAudioFormat{},
		Languages:          append([]string{}, xAITranscription.Languages...),
		DetectsLanguage:    true,
		Parameters: contract.RealtimeParameters{
			TurnDetection: []contract.RealtimeTurnDetectionParameters{
				{Type: contract.TurnDetectionNone, MeteredUnits: sessionClock},
				{Type: contract.TurnDetectionServerVAD, MeteredUnits: slices.Clone(sessionClock)},
			},
			OutputModalities: [][]contract.RealtimeOutputModality{},
			ToolChoice:       []contract.RealtimeToolChoice{},
		},
	}
}

// PublishesAudio is whether the statement's voices and languages are the
// model's audio observation. A statement without voices is a transcriber's,
// whose languages are the line's file transcription's to state.
func (s RealtimeStatement) PublishesAudio() bool { return len(s.Voices) > 0 }

func cloneVoices(voices []RealtimeVoice) []RealtimeVoice {
	return append([]RealtimeVoice(nil), voices...)
}

// openAIRealtimeVoices is OpenAI's documented Realtime voice list, read
// 2026-10-15 at https://developers.openai.com/api/docs/guides/realtime-conversations
// ("Current voice options are alloy, ash, ballad, coral, echo, sage, shimmer,
// verse, marin, and cedar"). OpenAI documents neither a gender for any of them
// nor the languages its Realtime models converse in, so neither is published
// (docs/realtime.md, "What the catalogue says about a realtime model").
//
// The languages: OpenAI's guides publish no list of the languages a Realtime
// model converses or transcribes in (read 2026-10-16 at
// https://developers.openai.com/api/docs/guides/speech-to-text, which names
// only the code formats), so none is published and a session names none: the
// model detects the language (DetectsLanguage).
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
