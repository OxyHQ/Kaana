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
	// sessionModalities is a session-level output-modality field (OpenAI's
	// `output_modalities`). xAI documents modalities only per response
	// (`response.create.response.modalities`), so there the session's
	// modalities are sent on every response.create the client does not
	// override — which, push-to-talk being the only mode served, is every
	// response the session has.
	sessionModalities bool
	// responseModalitiesField is the response.create field that carries them.
	responseModalitiesField string
	// oneModality is OpenAI GA's "one output modality per response".
	oneModality bool
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
		// xAI: "Sessions using the default server_vad turn detection are billed
		// for session duration. Push-to-talk sessions are billed only for audio
		// sent and received." The contract has no unit for a session's wall
		// clock, so a server_vad session's charge cannot be reported faithfully;
		// it is refused rather than metered as audio it is not (docs/realtime.md).
		contract.TurnDetectionServerVAD:   "xAI bills a server_vad session for its whole duration, which no contract usage unit can carry; open the session with turnDetection none (push-to-talk)",
		contract.TurnDetectionSemanticVAD: "xAI's Voice Agent API has no semantic_vad",
	},
	responseModalitiesField: "modalities",
	outputTextPart:          "text", outputAudioPart: "audio",
	aliases: map[string]string{
		// "Functionally identical ... Clients should handle both."
		"response.text.delta": "response.output_text.delta",
		// Named beside output_audio.delta for the JSON transport.
		"response.audio.delta": "response.output_audio.delta",
	},
	classifyEvent: classifyXAIEvent, refusalBody: xAIRefusalBody,
}
