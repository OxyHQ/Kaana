package openairealtime

import (
	"encoding/json"
	"slices"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// dialect is how a provider that speaks an OpenAI-Realtime-shaped WebSocket
// spells it: OpenAI's GA Realtime API itself, and xAI's Voice Agent API, which
// xAI documents as OpenAI-Realtime-compatible with a reviewed list of
// differences
// (https://docs.x.ai/developers/model-capabilities/audio/speech-to-speech,
// "OpenAI Realtime API Compatibility"). The handshake, the configure-then-
// confirm open, the command and event vocabulary, the item model, the tool
// call accumulation and the audio framing are shared.
//
// What a session may SET is not here: it is the protocol's reviewed statement
// (providerconfig.RealtimeCapabilities), which the catalogue publishes and
// every request is translated onto (statement, refuseUnservable). This type holds
// only how a served value is written on the provider's wire, so the catalogue
// and the adapter cannot disagree about what is accepted.
type dialect struct {
	slug       contract.ProviderSlug
	name       string
	protocol   string
	baseURL    string
	sessionURL string

	// sessionType is the session.update `type` OpenAI GA requires; xAI's
	// session schema has no such field.
	sessionType string
	// flatSession is xAI's session shape: `voice` and `turn_detection` on the
	// session itself, where OpenAI GA nests them as `audio.output.voice` and
	// `audio.input.turn_detection`.
	flatSession bool
	// turnDetectionNone is how "the client commits its own turns" is spelled:
	// OpenAI's `turn_detection: null`, xAI's `turn_detection: {"type": null}`.
	turnDetectionNone json.RawMessage
	// sessionModalities is a session-level output-modality field (OpenAI's
	// `output_modalities`). xAI documents modalities only per response
	// (`response.create.response.modalities`), so there the session's
	// modalities are sent on every response.create the client does not
	// override — which, under push-to-talk, is every response the session
	// has. Under server_vad xAI creates the responses itself, with modalities
	// no client field sets.
	sessionModalities bool
	// responseModalitiesField is the response.create field that carries them.
	responseModalitiesField string
	// toolChoiceField is a provider whose session and response have a
	// `tool_choice` field. Without one, `auto` — the only choice such a
	// provider serves — is sent as nothing.
	toolChoiceField bool
	// statusDetailsText is a provider whose `response.status_details` may be
	// a string (xAI: "unimplemented", measured on response.created and
	// response.done) rather than OpenAI's object or null. Such a string
	// carries no reason (statusReason).
	statusDetailsText bool
	// outputTextPart and outputAudioPart are the wire names of an assistant's
	// text and audio content parts.
	outputTextPart, outputAudioPart string
	// aliases are event names a provider documents as equivalents of the
	// shared ones.
	aliases map[string]string
	// transcription is how the provider transcribes the caller's speech
	// inside a conversation (config.inputAudioTranscription); nil refuses it.
	transcription *transcriptionDialect
	// classifyEvent reads an in-band error event in the provider's vocabulary.
	classifyEvent func(d *dialect, value wireError, key provider.Key) error
	// refusalBody reads a refused handshake's body into the shared error shape.
	refusalBody func(body []byte) wireError
}

// statement is what this dialect's protocol serves: the one reviewed
// statement the publisher writes into the inventory and the catalogue
// publishes. It is read where it is used, never copied into the dialect.
func (d *dialect) statement() providerconfig.RealtimeStatement {
	statement, _ := providerconfig.RealtimeCapabilities(d.slug, d.protocol)
	return statement
}

// refuseUnservable refuses the first field of a configuration, an update or a
// response.create that no translation onto this wire can serve, in the
// contract's terms (the same check the public API runs before a grant:
// contract.RealtimeParameters.Unservable). Everything else is translated or
// emulated (Kaana ADR 0003).
func (d *dialect) refuseUnservable(prefix string, fields contract.RealtimeConfigFields) error {
	if issues := d.statement().Parameters.Unservable(prefix, fields); len(issues) > 0 {
		return refused(issues[0].Param, issues[0].Message)
	}
	return nil
}

// clockBilled reports a turn detection under which the provider bills a
// session for its wall-clock duration rather than for what it carried (xAI:
// server_vad). Such a session is metered in `session_milliseconds`
// (meter.go), and a session never changes billing mode once open.
func (d *dialect) clockBilled(kind contract.RealtimeTurnDetectionType) bool {
	served, ok := d.statement().Parameters.TurnDetectionOf(kind)
	return ok && slices.Contains(served.MeteredUnits, contract.UnitSessionMilliseconds)
}

// billsClock reports a provider that bills some session by its clock.
func (d *dialect) billsClock() bool {
	return slices.ContainsFunc(d.statement().Parameters.TurnDetection, func(t contract.RealtimeTurnDetectionParameters) bool {
		return d.clockBilled(t.Type)
	})
}

// tokenUsage is a provider that bills the tokens `response.done` reports
// (OpenAI). A provider that does not (xAI bills audio by the minute) is
// metered instead (meter.go), and its token counts are never settled.
func (d *dialect) tokenUsage() bool {
	return slices.ContainsFunc(d.statement().Parameters.TurnDetection, func(t contract.RealtimeTurnDetectionParameters) bool {
		return slices.Contains(t.MeteredUnits, contract.UnitOutputTokens)
	})
}

// transcriptionDialect is input transcription inside a conversation on one
// provider's wire.
type transcriptionDialect struct {
	// model is the deployment's transcription model. The contract leaves it to
	// "the deployment's own, declared where the deployment is"; every
	// conversation model of a slug shares it, so it is declared here, with the
	// evidence for it, and a change to it is a reviewed change to what the
	// slug's sessions are billed for.
	model string
	// languageHint is xAI's field name (`language_hint`) for OpenAI's
	// `language`.
	languageHint bool
	// billed is a provider that bills the transcription model apart from the
	// session model and reports its usage on each completed transcription
	// (OpenAI). Otherwise the session's own price includes it (xAI) and it
	// adds no unit.
	billed bool
	// continuesInterruptedTurns is a provider whose transcriber keeps running
	// across the caller's items while no response has ended: an item the caller
	// starts by talking over a response is transcribed together with the items
	// before it (xAI, measured below). Kaana relays each item only the text of
	// its own audio (session.ownText).
	continuesInterruptedTurns bool
}

// openAITranscription is OpenAI's input transcription inside a Realtime
// conversation, reviewed 2026-10-15:
//
//   - `session.audio.input.transcription {model, language, prompt}`; the model
//     enum names `gpt-transcribe` beside whisper-1 and the 4o transcribers
//     (https://developers.openai.com/api/reference/resources/realtime/client-events),
//     and the transcription guide states that "gpt-transcribe performs input
//     transcription in a Realtime API session"
//     (https://developers.openai.com/api/docs/guides/realtime-transcription).
//   - It is billed "according to the ASR model's pricing rather than the
//     realtime model's"; `conversation.item.input_audio_transcription.completed`
//     carries that usage as `{type: "tokens", ...}` or `{type: "duration",
//     seconds}` (server-events reference).
//   - `gpt-transcribe` is priced per minute of audio ($0.0045/min,
//     https://developers.openai.com/api/docs/models/gpt-transcribe) and its
//     file transcriptions report `duration` usage (docs/openai-audio.md), so it
//     is declared: its charge is `input_transcription_milliseconds`. A token
//     usage for it is a charge the contract's unit does not express and ends
//     the session (transcriptionUnits). UNVERIFIED until the first signed
//     canary: that the in-session usage is `duration` as it is for files.
//   - OpenAI publishes no list of languages, so the statement names none and
//     the model detects the language (providerconfig.RealtimeCapabilities).
var openAITranscription = &transcriptionDialect{model: "gpt-transcribe", billed: true}

// xAITranscription is xAI's input transcription inside a Voice Agent session,
// reviewed 2026-10-15 at
// https://docs.x.ai/developers/model-capabilities/audio/speech-to-speech and
// https://docs.x.ai/voice-realtime.ws.json: `session.audio.input.transcription`
// with `language_hint` (a BCP 47 code from its documented list; "for Spanish
// and Portuguese, you must specify a regional variant"; an unrecognised code
// is "silently ignored", which is why only the statement's languages are
// sent) and the model `grok-transcribe`. No prompt field exists (its
// `keyterms` are a list of terms, not context). xAI's pricing names two Voice
// Agent charges — "duration and text events sent without audio"
// (https://docs.x.ai/developers/models/speech-to-speech) — and no
// transcription line, so it adds no unit to the session's meter. UNVERIFIED
// until the first signed canary: that xAI's invoice shows no separate
// transcription charge.
//
// Measured on production sessions of grok-voice-3.0 (2026-10-16, server_vad,
// one recording of "Hello Alia. … Please tell me, in one short sentence, what
// is the capital of France?"): when the caller goes on talking while the
// response to "Hello Alia." is being generated, xAI commits "Hello Alia." as
// one item and the rest as a second item whose audio starts after the first
// one's (`audio_start_ms`), yet every transcript of the second item —
// running and completed — begins with "Hello Alia."; xAI then drops the
// interrupted response without a response.done. With the same audio and a
// 4 s pause, the response ends with its response.done before the caller goes
// on, and the second item's transcript holds only its own words. So xAI's
// transcriber restarts when a response ends, not per item
// (continuesInterruptedTurns). testdata/xai-interrupted-turn.jsonl replays it.
var xAITranscription = &transcriptionDialect{model: "grok-transcribe", languageHint: true, continuesInterruptedTurns: true}

// contractPartType maps a provider's content part type to the contract's. xAI
// names an assistant's parts `text` and `audio` (and documents `text` as
// "general text" for other roles too), where OpenAI and the contract say
// output_text/output_audio for the assistant and input_text/input_audio for
// everyone else.
func (d *dialect) contractPartType(wire, role string) string {
	assistant := role == "assistant"
	switch {
	case wire == d.outputTextPart && d.outputTextPart != "output_text":
		if assistant {
			return "output_text"
		}
		return "input_text"
	case wire == d.outputAudioPart && d.outputAudioPart != "output_audio":
		if assistant {
			return "output_audio"
		}
		return "input_audio"
	}
	return wire
}

var openAIDialect = &dialect{
	slug: Slug, name: "OpenAI", protocol: providerconfig.ProtocolOpenAIRealtime,
	baseURL: providerconfig.OpenAIRealtimeBaseURL, sessionURL: providerconfig.OpenAIRealtimeSessionURL,
	sessionType: "realtime", turnDetectionNone: json.RawMessage("null"),
	sessionModalities: true, responseModalitiesField: "output_modalities", toolChoiceField: true,
	outputTextPart: "output_text", outputAudioPart: "output_audio",
	transcription: openAITranscription, classifyEvent: classifyOpenAIEvent, refusalBody: openAIRefusalBody,
}

// XAISlug is the provider name xAI's Voice Agent sessions are served under.
const XAISlug contract.ProviderSlug = "xai-realtime"

// xAIDialect is xAI's Voice Agent API, reviewed on 2026-09-30 against
// https://docs.x.ai/developers/model-capabilities/audio/speech-to-speech,
// https://docs.x.ai/developers/rest-api-reference/inference/voice and the
// machine-readable schema https://docs.x.ai/voice-realtime.ws.json.
var xAIDialect = &dialect{
	slug: XAISlug, name: "xAI", protocol: providerconfig.ProtocolXAIRealtime,
	baseURL: providerconfig.XAIRealtimeBaseURL, sessionURL: providerconfig.XAIRealtimeSessionURL,
	flatSession: true, turnDetectionNone: json.RawMessage(`{"type":null}`),
	responseModalitiesField: "modalities", statusDetailsText: true,
	outputTextPart: "text", outputAudioPart: "audio",
	aliases: map[string]string{
		// "Functionally identical ... Clients should handle both."
		"response.text.delta": "response.output_text.delta",
		// Named beside output_audio.delta for the JSON transport.
		"response.audio.delta": "response.output_audio.delta",
	},
	transcription: xAITranscription, classifyEvent: classifyXAIEvent, refusalBody: xAIRefusalBody,
}
