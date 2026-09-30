package openairealtime

import (
	"encoding/json"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// dialect is what differs between the providers that speak an
// OpenAI-Realtime-shaped WebSocket: OpenAI's GA Realtime API itself, and xAI's
// Voice Agent API, which xAI documents as OpenAI-Realtime-compatible with a
// reviewed list of differences
// (https://docs.x.ai/developers/model-capabilities/audio/speech-to-speech,
// "OpenAI Realtime API Compatibility"). The handshake, the configure-then-
// confirm open, the command and event vocabulary, the item model, the tool
// call accumulation and the audio framing are shared; every difference below
// is a documented one, and a field one provider has and the other lacks is
// refused on the one that lacks it rather than dropped.
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
	// refusedTurnDetection names the turn detection types this provider cannot
	// serve faithfully, with why.
	refusedTurnDetection map[contract.RealtimeTurnDetectionType]string
	// sessionClock is the turn detection under which the provider bills a
	// session for its wall-clock duration rather than for the audio it
	// carries (xAI: server_vad). Such a session is metered in
	// `session_milliseconds` (meter.go), and a session never changes billing
	// mode once open. Empty: no turn detection is billed by the clock.
	sessionClock contract.RealtimeTurnDetectionType
	// vad is the provider's server_vad parameter vocabulary and bounds.
	vad vadDialect
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
	// oneModality is OpenAI GA's "one output modality per response".
	oneModality bool
	// alwaysSpeaks is a provider that answers aloud whatever output
	// modalities it is sent, and bills that audio. Measured on xAI on
	// 2026-09-30: a session set to `modalities: ["text"]` (which xAI echoed
	// back on session.updated) still answered with an `audio` part, output
	// audio deltas and their transcript, and reported the audio as billable.
	// A text-only session there would receive, and pay for, audio it has no
	// format for and the meter no rate for, so any modality set without
	// audio is refused (wireModalities).
	alwaysSpeaks bool
	// statusDetailsText is a provider whose `response.status_details` may be
	// a string (xAI: "unimplemented", measured on response.created and
	// response.done) rather than OpenAI's object or null. Such a string
	// carries no reason (statusReason).
	statusDetailsText bool
	// maxOutputTokens is the documented ceiling; 0 means the provider has no
	// such field.
	maxOutputTokens int
	// toolChoice is whether the provider documents `tool_choice`.
	toolChoice bool
	// outputTextPart and outputAudioPart are the wire names of an assistant's
	// text and audio content parts.
	outputTextPart, outputAudioPart string
	// aliases are event names a provider documents as equivalents of the
	// shared ones.
	aliases map[string]string
	// tokenUsage is a provider that bills the tokens `response.done` reports.
	// A provider that does not (xAI bills audio by the minute) is metered
	// instead (meter.go), and its token counts are never settled.
	tokenUsage bool
	// classifyEvent reads an in-band error event in the provider's vocabulary.
	classifyEvent func(d *dialect, value wireError, key provider.Key) error
	// refusalBody reads a refused handshake's body into the shared error shape.
	refusalBody func(body []byte) wireError
}

// vadDialect is how a provider spells server_vad. OpenAI documents
// `create_response` and `interrupt_response`; xAI documents neither — its
// server_vad always answers a turn it detected, and a caller's speech
// interrupts playback by default — and bounds the other three parameters more
// tightly than the contract does.
type vadDialect struct {
	responseFields bool
	minThreshold   float64
	maxThreshold   float64
	// maxDurationMs bounds prefix_padding_ms and silence_duration_ms; 0 is
	// the contract's own bound.
	maxDurationMs int
}

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
	sessionModalities: true, responseModalitiesField: "output_modalities", oneModality: true,
	maxOutputTokens: 4096, toolChoice: true,
	outputTextPart: "output_text", outputAudioPart: "output_audio",
	vad:        vadDialect{responseFields: true, maxThreshold: 1},
	tokenUsage: true, classifyEvent: classifyOpenAIEvent, refusalBody: openAIRefusalBody,
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
	refusedTurnDetection: map[contract.RealtimeTurnDetectionType]string{
		// xAI's session schema names `"server_vad"` or null and nothing else
		// (https://docs.x.ai/voice-realtime.ws.json, session.turn_detection.type).
		contract.TurnDetectionSemanticVAD: "xAI's Voice Agent API has no semantic_vad",
	},
	// xAI: "Sessions using the default server_vad turn detection are billed
	// for session duration. Push-to-talk sessions are billed only for audio
	// sent and received." (https://docs.x.ai/developers/pricing). A server_vad
	// session is therefore metered by its wall clock, `session_milliseconds`
	// (contract set 3.3.0), and never by the audio it carried (meter.go).
	sessionClock: contract.TurnDetectionServerVAD,
	// session.turn_detection: threshold 0.1-0.9, silence_duration_ms and
	// prefix_padding_ms 0-10000; no create_response or interrupt_response.
	vad:                     vadDialect{minThreshold: 0.1, maxThreshold: 0.9, maxDurationMs: 10_000},
	responseModalitiesField: "modalities", alwaysSpeaks: true, statusDetailsText: true,
	outputTextPart: "text", outputAudioPart: "audio",
	aliases: map[string]string{
		// "Functionally identical ... Clients should handle both."
		"response.text.delta": "response.output_text.delta",
		// Named beside output_audio.delta for the JSON transport.
		"response.audio.delta": "response.output_audio.delta",
	},
	classifyEvent: classifyXAIEvent, refusalBody: xAIRefusalBody,
}
